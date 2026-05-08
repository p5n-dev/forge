package env

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

// connectFlagBash drops the user into a login bash shell instead of
// launching RAGE/Claude Code. Mirrors `cage env connect --bash`.
var connectFlagBash bool

// execSSH is the indirection point used by syscall.Exec. Tests swap in
// a stub that captures argv without actually replacing the test
// process.
//
// Signature matches syscall.Exec: argv0 is the binary path, argv is the
// full argv (including argv[0]), envv is the environment.
var execSSH = syscall.Exec

// lookSSHPath resolves the ssh binary on PATH. Wrapped so tests can
// stub the lookup if they ever need to.
var lookSSHPath = func() (string, error) {
	return exec.LookPath("ssh")
}

var connectCmd = &cobra.Command{
	Use:   "connect [name]",
	Short: "Connect to a running FORGE environment",
	Long: "Open a session inside the named environment. By default the " +
		"session immediately launches RAGE which starts the Claude Code " +
		"agent inside the VM. Use --bash to drop to a plain bash shell instead.",
	Args: cobra.ExactArgs(1),
	// "env not found" / "env not running" / "no ssh.sock" all surface
	// here as RunE errors. They're operational, not "you typed the
	// command wrong" — don't make cobra print the help text after them.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConnect(args[0], connectFlagBash)
	},
}

func init() {
	connectCmd.Flags().BoolVar(&connectFlagBash, "bash", false,
		"open a bash login shell instead of launching RAGE/Claude Code")
	Cmd.AddCommand(connectCmd)
}

// runConnect implements `forge env connect <name>`.
//
// It loads the env state, verifies the VM is `running` (using the
// Manager's live PID liveness check), then replaces the FORGE process
// with an ssh subprocess so the user gets full PTY control. The remote
// command is `rage` by default, or `bash -l` when --bash is passed.
//
// Connection rides the env's vsock-bridged Unix socket via ssh's
// ProxyCommand — no IP routing involved, so the host can reach the VM
// regardless of what a corp VPN has done to the routing table.
func runConnect(name string, bash bool) error {
	envDir := filepath.Join(envsBaseDir, name)

	// Confirm the env exists at all before doing anything else. A
	// missing state.json gives the cleanest "not found" signal.
	if _, err := os.Stat(filepath.Join(envDir, "state.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("env %q not found (looked in %s)", name, envDir)
		}
		return fmt.Errorf("checking env %q: %w", name, err)
	}

	// Manager.Status performs the lazy stale-PID check, so a "running"
	// or "starting" status here genuinely means the vfkit process is
	// alive. We accept both because sshd is enabled at boot, well
	// before forge-bootstrap finishes — connecting during a long
	// bootstrap to peek at /var/log/forge-bootstrap.log is exactly
	// when the user most wants to ssh in.
	manager := vm.NewManager(vm.NewVfkitRunner(), envsBaseDir)
	status, err := manager.Status(name)
	if err != nil {
		return fmt.Errorf("loading env %q: %w", name, err)
	}
	if status != vm.StatusRunning && status != vm.StatusStarting {
		return fmt.Errorf(
			"env %q is %s — connect requires running or starting (start it with 'forge env start %s')",
			name, status, name,
		)
	}

	state, err := vm.LoadState(envDir)
	if err != nil {
		return fmt.Errorf("loading env %q: %w", name, err)
	}

	sshSock := envpkg.SSHSocketPath(envDir)
	if _, err := os.Stat(sshSock); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"env %q has no ssh.sock at %s — recreate it (forge env destroy %s && forge env create %s) to enable vsock-bridged SSH",
				name, sshSock, name, name,
			)
		}
		return fmt.Errorf("checking ssh socket: %w", err)
	}

	sshPath, err := lookSSHPath()
	if err != nil {
		return fmt.Errorf("locating ssh: %w", err)
	}

	args := buildSSHArgs(state, envDir, bash)
	// argv[0] in syscall.Exec must match the binary's name (or path);
	// the helper put "ssh" there, so swap it for the resolved absolute
	// path now without touching the rest of the argv ordering.
	args[0] = sshPath

	// Replace this process with ssh. On success this call never
	// returns; on failure it returns the syscall error.
	return execSSH(sshPath, args, os.Environ())
}

// buildSSHArgs assembles the ssh argv for connecting to a FORGE env.
//
// argv[0] is set to "ssh" so tests can assert the shape of the command
// without caring where ssh resolves on the host. The caller (runConnect)
// overwrites it with the absolute path resolved via exec.LookPath right
// before handing the argv to syscall.Exec.
//
// The returned argv has the following layout:
//
//	ssh
//	-i <envDir>/id_ed25519
//	-o StrictHostKeyChecking=accept-new
//	-o UserKnownHostsFile=<envDir>/known_hosts
//	-o ProxyCommand=nc -U '<envDir>/ssh.sock'
//	forge@<envname>
//	<remote command...>
//
// The destination hostname is the env name, not its IP — IP is
// irrelevant once ProxyCommand owns connection setup. ssh uses the
// hostname as a known_hosts key only.
//
// Remote command is "rage" by default, "bash -l" when bash is true.
func buildSSHArgs(state *vm.State, envDir string, bash bool) []string {
	keyPath := filepath.Join(envDir, "id_ed25519")
	knownHosts := filepath.Join(envDir, "known_hosts")
	sshSock := envpkg.SSHSocketPath(envDir)

	args := []string{
		"ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "ProxyCommand=nc -U " + shellQuote(sshSock),
		// Force PTY allocation. ssh only allocates a PTY by default
		// when there's no remote command — but `forge env connect`
		// always passes one (rage or bash -l), and both are
		// interactive. Without -t, the remote command runs without a
		// controlling terminal and bash -l in particular looks
		// completely hung (it sits in non-interactive read on stdin).
		"-t",
		"forge@" + state.Name,
	}

	// Both variants:
	//   1. start a login bash so /etc/profile.d/*.sh sources (KUBECONFIG
	//      from k3s.sh; the rest of /usr/local/bin is already on the
	//      default Debian PATH so claude — which we symlink there
	//      during bootstrap — and rage are reachable either way).
	//   2. cd into /home/forge/workspace — the virtio-fs share with the
	//      env's Forgejo repo. Claude Code expects to be launched
	//      inside the project; without this it lands in $HOME and warns
	//      "launch it in a project directory instead", AND any agent
	//      action would push to the wrong place.
	//   3. exec the user-visible command (rage or interactive bash) so
	//      no stray bash parent process lingers.
	//
	// Critically this MUST be ONE arg to ssh, not three. ssh joins
	// remote-command args with spaces and sends as a single string;
	// the remote outer shell then tokenizes once. The internal
	// single-quotes survive that pass and bash's -c receives the
	// payload intact. Passing three args (bash, -lc, "cd … && exec
	// rage") collapses on the wire to `bash -lc cd … && exec rage`,
	// where bash reads -c="cd" with $0="…" and silently exits.
	if bash {
		args = append(args, "bash -l -c 'cd /home/forge/workspace && exec bash -i'")
	} else {
		args = append(args, "bash -l -c 'cd /home/forge/workspace && exec rage'")
	}

	return args
}

// shellQuote single-quotes s for safe inclusion in a shell command line.
// ssh hands ProxyCommand to /bin/sh -c, so any spaces or specials in
// envDir would break interpretation otherwise. Single-quote-escape the
// classic POSIX way: close the quote, emit `\'`, reopen the quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// BuildSSHArgsForTest exposes buildSSHArgs to the external test
// package. Production code uses buildSSHArgs directly.
func BuildSSHArgsForTest(state *vm.State, envDir string, bash bool) []string {
	return buildSSHArgs(state, envDir, bash)
}

// SetExecSSHForTest swaps the execSSH indirection and returns a
// restore function. Tests use this to capture the argv that would be
// passed to syscall.Exec without actually replacing the test binary.
func SetExecSSHForTest(fn func(argv0 string, argv []string, envv []string) error) func() {
	prev := execSSH
	execSSH = fn
	return func() { execSSH = prev }
}
