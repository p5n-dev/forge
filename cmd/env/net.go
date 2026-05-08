package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/gvproxy"
)

// netCmd is the in-process side of the per-env userspace networking
// stack: `forge env start` (and `create`) re-execs the running binary
// with this hidden subcommand to keep gvproxy alive past their own
// exit. Same pattern as `forge env _proxy` and the vfkit detach.
//
// Hidden because there's no scenario where a human runs this directly
// — it's an implementation detail of env start.
var netCmd = &cobra.Command{
	Use:    "_net",
	Short:  "(internal) run the per-env gvproxy userspace netstack",
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		sock, _ := cmd.Flags().GetString("socket")
		capture, _ := cmd.Flags().GetString("capture")
		if sock == "" {
			return fmt.Errorf("--socket is required")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()

		return gvproxy.Config{
			SocketPath:  sock,
			CaptureFile: capture,
		}.Run(ctx)
	},
}

func init() {
	netCmd.Flags().String("socket", "", "host-side unixgram socket vfkit will connect to")
	netCmd.Flags().String("capture", "", "optional pcap file for debugging")
	Cmd.AddCommand(netCmd)
}

// execNetRunner is the production envpkg.NetRunner. Start re-execs the
// running `forge` binary as `forge env _net` and detaches it so the
// netstack outlives the parent that spawned it. Stop reads the PID
// file and sends SIGTERM, escalating to SIGKILL after a grace period.
//
// Mirrors execProxyRunner in proxy.go — same shape, different child.
type execNetRunner struct {
	stopGrace time.Duration
}

func newExecNetRunner() *execNetRunner {
	return &execNetRunner{stopGrace: 3 * time.Second}
}

func (r *execNetRunner) Start(_ context.Context, envDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating forge binary: %w", err)
	}

	sockPath := envpkg.NetSocketPath(envDir)
	logPath := filepath.Join(envDir, "gvproxy.log")
	pidPath := envpkg.NetPIDPath(envDir)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(exe, "env", "_net", //nolint:gosec // re-execing our own binary
		"--socket", sockPath,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting gvproxy: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("writing %s: %w", pidPath, err)
	}
	_ = cmd.Process.Release()

	// gvproxy's unixgram bind is fast (no DHCP/DNS handshake to wait
	// for). Poll briefly until the socket file exists so vfkit's
	// virtio-net device doesn't race ahead and fail to dial. This
	// only matters on cold create/start; subsequent dials within the
	// VM's lifetime go through gvproxy's already-bound socket.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("gvproxy did not bind %s within deadline", sockPath)
}

func (r *execNetRunner) Stop(_ context.Context, envDir string) error {
	pidPath := envpkg.NetPIDPath(envDir)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidPath)
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(pidPath)
		return nil
	}
	_ = proc.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(r.stopGrace)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			_ = os.Remove(pidPath)
			_ = os.Remove(envpkg.NetSocketPath(envDir))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	_ = os.Remove(pidPath)
	_ = os.Remove(envpkg.NetSocketPath(envDir))
	return nil
}
