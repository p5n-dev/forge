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
	"github.com/p5n-dev/forge/internal/forgejoproxy"
)

// proxyCmd is the in-process side of the host-side Forgejo proxy:
// `forge env start` (and `create`) re-exec the running binary with this
// hidden subcommand to keep the unix-socket forwarder alive past their
// own exit. Same lifecycle pattern as vfkit.
//
// Hidden because there is no scenario where a human runs this directly
// — it's an implementation detail of the env start path. The flags are
// documented for the rare case someone is debugging a wedged proxy.
var proxyCmd = &cobra.Command{
	Use:    "_proxy",
	Short:  "(internal) run the per-env Forgejo unix-socket proxy",
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		sock, _ := cmd.Flags().GetString("socket")
		target, _ := cmd.Flags().GetString("target")
		if sock == "" || target == "" {
			return fmt.Errorf("--socket and --target are required")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()

		return forgejoproxy.Proxy{SocketPath: sock, Target: target}.Run(ctx)
	},
}

func init() {
	proxyCmd.Flags().String("socket", "", "host-side unix socket to bind")
	proxyCmd.Flags().String("target", "", "TCP target (host:port) to forward to")
	Cmd.AddCommand(proxyCmd)
}

// execProxyRunner is the production envpkg.ProxyRunner. Start re-execs
// the running `forge` binary as `forge env _proxy` and detaches it so
// the proxy outlives the parent that spawned it. Stop reads the PID
// file written at Start and sends SIGTERM, then SIGKILL after a grace
// period — same shape as internal/vm/vfkit.go's runner.
type execProxyRunner struct {
	// stopGrace overrides the default SIGTERM→SIGKILL window. Tests
	// keep it short; production uses a few seconds.
	stopGrace time.Duration
}

func newExecProxyRunner() *execProxyRunner {
	return &execProxyRunner{stopGrace: 3 * time.Second}
}

func (r *execProxyRunner) Start(_ context.Context, envDir, target string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating forge binary: %w", err)
	}

	sockPath := envpkg.ForgejoSocketPath(envDir)
	logPath := filepath.Join(envDir, "forgejo-proxy.log")
	pidPath := envpkg.ForgejoProxyPIDPath(envDir)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(exe, "env", "_proxy", //nolint:gosec // re-execing our own binary
		"--socket", sockPath,
		"--target", target,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting forgejoproxy: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("writing %s: %w", pidPath, err)
	}
	_ = cmd.Process.Release()
	return nil
}

func (r *execProxyRunner) Stop(_ context.Context, envDir string) error {
	pidPath := envpkg.ForgejoProxyPIDPath(envDir)
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
			// Best-effort: the proxy normally cleans up its own
			// socket, but if it died mid-shutdown the leftover
			// would block the next bind.
			_ = os.Remove(envpkg.ForgejoSocketPath(envDir))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	_ = os.Remove(pidPath)
	_ = os.Remove(envpkg.ForgejoSocketPath(envDir))
	return nil
}
