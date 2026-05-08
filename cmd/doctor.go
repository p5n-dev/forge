package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check FORGE host setup and report blockers to env reachability",
	Long: `Reports the health of FORGE's host-side plumbing: vfkit
availability, gvproxy userspace netstack per env, and per-env
reachability over the vsock-bridged ssh
socket.

Exits non-zero if any check FAILs.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDoctor(cmd.Context(), cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// doctorStatus is the outcome of a single check. It maps directly to
// the rendered glyph (✓ / ✗ / ·).
type doctorStatus int

const (
	statusPass doctorStatus = iota
	statusFail
	statusSkip
)

type doctorCheck struct {
	name   string
	status doctorStatus
	msg    string // one-line summary shown next to the glyph
	hint   string // optional follow-up shown indented under FAIL/SKIP
}

type doctorReport struct {
	checks []doctorCheck
}

func (r *doctorReport) add(c doctorCheck) { r.checks = append(r.checks, c) }

func (r *doctorReport) anyFailed() bool {
	for _, c := range r.checks {
		if c.status == statusFail {
			return true
		}
	}
	return false
}

// errChecksFailed is the sentinel returned to cobra when one or more
// checks failed. We rely on rootCmd.Execute to translate that into
// exit code 1 — SilenceErrors keeps the message off the user's screen
// since the rendered report is already informative.
var errChecksFailed = fmt.Errorf("one or more checks failed")

func runDoctor(ctx context.Context, out io.Writer) error {
	var report doctorReport

	report.add(checkVfkit(ctx))

	if home, err := os.UserHomeDir(); err == nil {
		envBase := filepath.Join(home, ".forge", "envs")
		report.checks = append(report.checks, checkEnvs(envBase)...)
	}

	renderDoctor(out, report)
	if report.anyFailed() {
		return errChecksFailed
	}
	return nil
}

func checkVfkit(ctx context.Context) doctorCheck {
	bin, err := exec.LookPath("vfkit")
	if err != nil {
		return doctorCheck{
			name:   "vfkit",
			status: statusFail,
			msg:    "not found on PATH",
			hint:   "Install with `brew install vfkit`.",
		}
	}
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		return doctorCheck{
			name:   "vfkit",
			status: statusFail,
			msg:    fmt.Sprintf("`vfkit --version` failed: %v", err),
		}
	}
	return doctorCheck{
		name:   "vfkit",
		status: statusPass,
		msg:    strings.TrimSpace(string(out)),
	}
}

func checkEnvs(envBase string) []doctorCheck {
	entries, err := os.ReadDir(envBase)
	if err != nil {
		// No envs dir yet is a normal first-run state — nothing to report.
		return nil
	}
	runner := vm.NewVfkitRunner()
	var checks []doctorCheck
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(envBase, e.Name())
		state, err := vm.LoadState(dir)
		if err != nil {
			// Not a managed env dir — silently skip.
			continue
		}
		checks = append(checks, checkSingleEnv(runner, dir, state))
	}
	return checks
}

func checkSingleEnv(runner *vm.VfkitRunner, envDir string, state *vm.State) doctorCheck {
	name := "env " + state.Name
	if state.Status != vm.StatusRunning {
		return doctorCheck{
			name:   name,
			status: statusSkip,
			msg:    fmt.Sprintf("%s — skipping reachability checks", state.Status),
		}
	}
	alive, err := runner.IsAlive(envDir)
	if err != nil {
		return doctorCheck{name: name, status: statusFail, msg: "vfkit alive check: " + err.Error()}
	}
	if !alive {
		return doctorCheck{
			name:   name,
			status: statusFail,
			msg:    "state.json says running but vfkit pid is gone",
			hint:   "Run `forge env start " + state.Name + "` to recover.",
		}
	}

	// SSH rides the vsock-bridged Unix socket (see internal/env/paths.go),
	// not TCP. Probe by Dial-ing the socket — a successful connect proves
	// vfkit's listener is alive on this end of the bridge.
	sshSock := envpkg.SSHSocketPath(envDir)
	if _, err := os.Stat(sshSock); err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{
				name:   name,
				status: statusFail,
				msg:    "no ssh.sock — env predates vsock-bridged SSH",
				hint:   "Recreate the env: forge env destroy " + state.Name + " && forge env create " + state.Name,
			}
		}
		return doctorCheck{name: name, status: statusFail, msg: "stat ssh.sock: " + err.Error()}
	}
	conn, err := net.DialTimeout("unix", sshSock, 1500*time.Millisecond)
	if err != nil {
		return doctorCheck{
			name:   name,
			status: statusFail,
			msg:    fmt.Sprintf("ssh.sock unreachable (%v)", err),
			hint:   "vfkit may be wedged. Try `forge env stop " + state.Name + " && forge env start " + state.Name + "`.",
		}
	}
	_ = conn.Close()

	// gvproxy (userspace netstack) is what gives the VM internet —
	// vfkit's virtio-net device dials this unix socket at boot. If
	// it's missing on a running env, the VM has no NIC even though
	// SSH is fine (SSH rides a separate vsock channel).
	netSock := envpkg.NetSocketPath(envDir)
	if _, err := os.Stat(netSock); err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{
				name:   name,
				status: statusFail,
				msg:    "no net.sock — gvproxy isn't running, VM has no internet",
				hint:   "Run `forge env stop " + state.Name + " && forge env start " + state.Name + "` to relaunch gvproxy.",
			}
		}
		return doctorCheck{name: name, status: statusFail, msg: "stat net.sock: " + err.Error()}
	}

	return doctorCheck{
		name:   name,
		status: statusPass,
		msg:    "running, ssh.sock + net.sock reachable",
	}
}

var (
	doctorOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	doctorBad  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	doctorSkip = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	doctorDim  = lipgloss.NewStyle().Faint(true)
)

func renderDoctor(out io.Writer, r doctorReport) {
	for _, c := range r.checks {
		var glyph string
		switch c.status {
		case statusPass:
			glyph = doctorOK.Render("✓")
		case statusFail:
			glyph = doctorBad.Render("✗")
		case statusSkip:
			glyph = doctorSkip.Render("·")
		}
		_, _ = fmt.Fprintf(out, "%s %-13s %s\n", glyph, c.name, c.msg)
		if c.hint != "" && c.status != statusPass {
			_, _ = fmt.Fprintf(out, "    %s\n", doctorDim.Render(c.hint))
		}
	}
}
