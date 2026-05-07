package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runner is the abstraction over the hypervisor subprocess. Higher-level
// code (the Manager) only knows about Runner; the concrete vfkit-backed
// implementation lives below.
//
// Runner implementations must be safe to call from any goroutine but
// are not expected to support concurrent Start/Stop on the same envDir.
type Runner interface {
	Start(ctx context.Context, envDir string, opts StartOptions) error
	Stop(ctx context.Context, envDir string) error
	IsAlive(envDir string) (bool, error)
}

// StartOptions is the set of knobs the runtime layer needs to boot a VM.
// Higher layers (env create) build this from project config and the
// resolved image.
type StartOptions struct {
	DiskPath     string // path to the VM disk image
	CloudInitISO string // path to the generated cloud-init ISO
	CPUs         int    // virtual CPU count
	MemoryMB     int    // RAM in MB
	MAC          string // MAC address for the virtio-net device
	// NetSocketPath is the unixgram (SOCK_DGRAM) socket vfkit's
	// virtio-net device connects to. Required: this is the boundary
	// between vfkit and the gvproxy userspace netstack. With it set
	// we use `--device virtio-net,unixSocketPath=<path>,mac=…`. The
	// vmnet-NAT mode (`virtio-net,nat`) is intentionally not exposed
	// — see internal/gvproxy for why FORGE doesn't use it.
	NetSocketPath   string
	EFIVarStorePath string // EFI variable store path; created on first boot
	VsockSocketPath string // host Unix socket vfkit bridges to guest vsock
	VsockPort       int    // guest vsock port FORGE listens on (default: 1234)
	// SSHSocketPath, when set, asks vfkit to expose vsock port 22 inside
	// the guest at this Unix socket on the host. Combined with the
	// in-guest socat unit (see internal/cloudinit), this lets the host
	// ssh into the VM without any IP routing — the connection rides the
	// vsock channel end-to-end. Required for VPN-immune host→VM SSH.
	SSHSocketPath string
	// RageShareDir, when set, is shared with the guest as a virtio-fs
	// mount with tag `rage-share`. Cloud-init's forge-bootstrap picks
	// the platform-specific rage binary out of this share and installs
	// it as /usr/local/bin/rage in the guest (see internal/cloudinit).
	// Empty → no share is attached; the guest skips rage install.
	RageShareDir string
	// WorkspaceShareDir, when set, is shared with the guest as a
	// virtio-fs mount with tag `workspace-share`. The guest mounts it
	// at /home/forge/workspace via /etc/fstab. Holds the env's clone
	// of the Forgejo repo so host and in-VM agent see the same tree.
	WorkspaceShareDir string
	// ForgejoSocketPath / ForgejoVsockPort, when both set, attach a
	// third virtio-vsock device in `listen` mode at the given vsock
	// port, with vfkit dialing ForgejoSocketPath on the host whenever
	// the guest opens vsock(host, ForgejoVsockPort). The guest-side
	// socat unit (see internal/cloudinit) listens on
	// 127.0.0.1:ForgejoVsockPort inside the VM and forwards into
	// vsock, which gives `git push` an IP-routing-free path to a
	// host-side Forgejo. Layered defense alongside gvproxy: even if
	// the userspace netstack hits an issue, the vsock channel keeps
	// working because it never touches the IP stack.
	ForgejoSocketPath string
	ForgejoVsockPort  int
}

// vfkitDefaultBinary is the executable name we look up on PATH when a
// caller hasn't overridden it. Real users on macOS get this via
// `brew install vfkit`.
const vfkitDefaultBinary = "vfkit"

// defaultStopGracePeriod is how long we wait for vfkit to exit after
// SIGTERM before escalating to SIGKILL.
const defaultStopGracePeriod = 10 * time.Second

// VfkitRunner is the concrete Runner backed by the vfkit binary.
//
// The Binary and ArgsBuilder fields are exported so tests can swap in a
// stand-in process (e.g. /bin/sleep) without depending on a real vfkit
// install. Production callers should use NewVfkitRunner and not touch
// these fields.
type VfkitRunner struct {
	// Binary is the executable path or PATH-resolvable name. Defaults
	// to "vfkit".
	Binary string
	// ArgsBuilder converts a StartOptions into the vfkit argv. Tests
	// can override this to invoke a stand-in process; production
	// callers can leave it nil to use the default builder.
	ArgsBuilder func(StartOptions) []string
	// StopGracePeriod overrides defaultStopGracePeriod. Used by tests
	// to keep the suite fast; zero means use the default.
	StopGracePeriod time.Duration
}

// NewVfkitRunner returns a VfkitRunner with production defaults.
func NewVfkitRunner() *VfkitRunner {
	return &VfkitRunner{Binary: vfkitDefaultBinary}
}

func (r *VfkitRunner) gracePeriod() time.Duration {
	if r.StopGracePeriod > 0 {
		return r.StopGracePeriod
	}
	return defaultStopGracePeriod
}

// vfkitFiles collects the per-env paths the runner manages.
type vfkitFiles struct {
	pidPath string
	logPath string
}

func filesFor(envDir string) vfkitFiles {
	return vfkitFiles{
		pidPath: filepath.Join(envDir, "vfkit.pid"),
		logPath: filepath.Join(envDir, "vfkit.log"),
	}
}

// DefaultArgs builds the vfkit command line for the given StartOptions.
// It is exported so callers (and tests) can inspect the argv that would
// be passed to vfkit.
func DefaultArgs(opts StartOptions) []string {
	// --log-level debug surfaces vfkit's tcpproxy / vsock-listener
	// internals into vfkit.log. Worth the noise: a missing line here
	// is precisely how we'd diagnose a silent vsock-bind failure.
	args := []string{"--log-level", "debug"}

	if opts.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(opts.CPUs))
	}
	if opts.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(opts.MemoryMB))
	}
	if opts.EFIVarStorePath != "" {
		args = append(args, "--bootloader",
			"efi,variable-store="+opts.EFIVarStorePath+",create")
	}
	if opts.DiskPath != "" {
		args = append(args, "--device", "virtio-blk,path="+opts.DiskPath)
	}
	if opts.CloudInitISO != "" {
		args = append(args, "--device", "virtio-blk,path="+opts.CloudInitISO)
	}

	// virtio-net via the unixSocketPath transport: vfkit talks to a
	// SOCK_DGRAM unix socket on the host; internal/gvproxy is what
	// listens on the other end and provides DHCP/DNS/NAT in
	// userspace. This replaces vmnet shared mode (`virtio-net,nat`)
	// and is what gives the VM OrbStack-grade connectivity through
	// tunnel-all corp VPNs — every guest connection becomes a
	// host-side socket call (see internal/gvproxy package docs).
	if opts.NetSocketPath != "" {
		netDev := "virtio-net,unixSocketPath=" + opts.NetSocketPath
		if opts.MAC != "" {
			netDev += ",mac=" + opts.MAC
		}
		args = append(args, "--device", netDev)
	}

	// Per vfkit's doc/usage.md (virtio-vsock section), the device
	// supports two directions, controlled by bare flags:
	//
	//   listen  (default): host listens for guest-initiated vsock
	//                      connections; vfkit dials socketURL on the
	//                      host to deliver them.
	//   connect          : host connects to a guest that is itself
	//                      vsock-listening; vfkit binds socketURL on
	//                      the host and forwards inbound to the guest.
	//
	// Both flags are BARE — `connect=true` / `listen=true` are NOT
	// the canonical syntax; vfkit silently falls back to the default
	// `listen` mode when it sees them, which is how we ended up with
	// ssh.sock never appearing on disk for several iterations.
	// `socketURL` takes a plain filesystem path (no `unix://` URL).
	if opts.VsockSocketPath != "" {
		port := opts.VsockPort
		if port == 0 {
			port = 1234
		}
		// Boot-ready: guest dials, host receives. FORGE binds
		// VsockSocketPath via Go's net.Listen and vfkit dials it
		// whenever the guest opens vsock(host, port). This is the
		// `listen` direction — explicit so the intent is obvious in
		// the argv even though it's the default.
		args = append(args, "--device",
			fmt.Sprintf("virtio-vsock,port=%d,socketURL=%s,listen",
				port, opts.VsockSocketPath))
	}

	// SSH bridge: host dials, guest receives. vfkit binds
	// SSHSocketPath on the host; an inbound unix-socket connection is
	// forwarded to vsock(guest, 22), where the in-VM
	// forge-ssh-vsock.service is doing `socat VSOCK-LISTEN:22 ...`.
	// Full path: host-unix → vfkit → guest-vsock → in-VM socat → sshd.
	if opts.SSHSocketPath != "" {
		args = append(args, "--device",
			fmt.Sprintf("virtio-vsock,port=22,socketURL=%s,connect",
				opts.SSHSocketPath))
	}

	// virtio-fs share for the host's rage/ directory. Cloud-init's
	// forge-bootstrap mounts this read-only as `rage-share` and copies
	// the right rage binary (by `uname -m`) and rage.toml into the
	// guest. We deliberately don't fetch rage from the network: it's
	// user-provided per the CAGE convention.
	if opts.RageShareDir != "" {
		args = append(args, "--device",
			fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=rage-share",
				opts.RageShareDir))
	}

	// virtio-fs share for the env's workspace directory. The guest
	// mounts this read-write at /home/forge/workspace via fstab so the
	// in-VM agent and the host's git client edit the same tree.
	if opts.WorkspaceShareDir != "" {
		args = append(args, "--device",
			fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=workspace-share",
				opts.WorkspaceShareDir))
	}

	// Forgejo bridge: guest dials, host receives. Mirrors the SSH
	// path's flag shape (see above) — bare `listen`, plain socketURL.
	// The host-side proxy (internal/forgejoproxy, spawned by env
	// create/start) binds ForgejoSocketPath and forwards each
	// accepted connection to the configured Forgejo TCP endpoint.
	if opts.ForgejoSocketPath != "" && opts.ForgejoVsockPort != 0 {
		args = append(args, "--device",
			fmt.Sprintf("virtio-vsock,port=%d,socketURL=%s,listen",
				opts.ForgejoVsockPort, opts.ForgejoSocketPath))
	}

	return args
}

// Start launches the vfkit subprocess in its own session and writes
// the PID to `<envDir>/vfkit.pid`. The child's stdout and stderr are
// merged into `<envDir>/vfkit.log`.
//
// The child does NOT inherit the parent's controlling terminal, which
// means the VM will outlive a `forge env create` invocation that
// closes its terminal. This is the explicit behaviour required by the
// spec (decision #5).
func (r *VfkitRunner) Start(ctx context.Context, envDir string, opts StartOptions) error {
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return fmt.Errorf("creating env dir %s: %w", envDir, err)
	}

	files := filesFor(envDir)

	bin := r.Binary
	if bin == "" {
		bin = vfkitDefaultBinary
	}
	build := r.ArgsBuilder
	if build == nil {
		build = DefaultArgs
	}
	args := build(opts)

	logFile, err := os.OpenFile(files.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", files.logPath, err)
	}
	// Closed once the child has been started; the kernel keeps the
	// underlying fd alive for the child via dup() in fork+exec.
	defer func() { _ = logFile.Close() }()

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // Binary path is trusted; tests inject a known stand-in.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", bin, err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(files.pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		// Best-effort: kill the child we just started since the caller
		// won't have a way to find it.
		_ = cmd.Process.Kill()
		return fmt.Errorf("writing %s: %w", files.pidPath, err)
	}

	// Release the child so the Go runtime doesn't reap it on our behalf
	// — the supervising `forge env` process is short-lived and the VM
	// must outlive it. Ignoring the error: Release on a freshly started
	// process is documented to never fail in practice.
	_ = cmd.Process.Release()

	return nil
}

// Stop reads the PID file, sends SIGTERM, waits up to r.gracePeriod()
// for the process to exit, and then escalates to SIGKILL. It removes
// the PID file once the process is gone.
//
// It is a no-op (returns nil) if the PID file does not exist.
func (r *VfkitRunner) Stop(_ context.Context, envDir string) error {
	files := filesFor(envDir)

	pid, err := readPID(files.pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Unix os.FindProcess never errors; defensive nonetheless.
		return fmt.Errorf("finding process %d: %w", pid, err)
	}

	// SIGTERM first.
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// If the process is already gone, treat as success.
		if !isProcessGone(err) {
			return fmt.Errorf("sending SIGTERM to %d: %w", pid, err)
		}
	}

	if waitForExit(pid, r.gracePeriod()) {
		_ = os.Remove(files.pidPath)
		return nil
	}

	// Escalate.
	if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessGone(err) {
		return fmt.Errorf("sending SIGKILL to %d: %w", pid, err)
	}
	// Give the kernel a moment to actually reap.
	waitForExit(pid, r.gracePeriod())

	_ = os.Remove(files.pidPath)
	return nil
}

// IsAlive reports whether the recorded PID is still running.
//
// Returns (false, nil) when:
//   - the PID file is missing, or
//   - the recorded PID does not correspond to a live process (the
//     "stale PID" case from spec section 6).
//
// Returns (false, err) when the PID file exists but is unreadable or
// contains a non-numeric PID.
func (r *VfkitRunner) IsAlive(envDir string) (bool, error) {
	files := filesFor(envDir)

	pid, err := readPID(files.pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	return processAlive(pid), nil
}

// readPID parses an integer PID out of a pid file. Returns errors that
// callers can compare against os.ErrNotExist for the missing-file case.
func readPID(pidPath string) (int, error) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parsing pid file %s: %w", pidPath, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d in %s", pid, pidPath)
	}
	return pid, nil
}

// processAlive reports whether pid corresponds to a live (i.e. not
// exited, not a zombie) process.
//
// We first try a non-blocking wait4: if pid is our child and has
// exited, this reaps it and returns "not alive". This matters in
// tests where the supervising process is also the parent — without
// reaping, kill(pid, 0) keeps succeeding on the zombie until somebody
// waits for it. In production `forge env create` exits before Stop
// would be called, the VM is reparented to launchd/init, and wait4
// returns ECHILD; the kill(0) probe is then authoritative.
//
// If wait4 doesn't conclude things, we fall back to kill(pid, 0).
func processAlive(pid int) bool {
	var status syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	switch {
	case err == nil && wpid == pid:
		// Child has exited and we just reaped it.
		return false
	case err == nil && wpid == 0:
		// Child exists and is still running.
		return true
	}
	// err != nil typically means ECHILD (not our child) — fall through
	// to the signal-0 probe.

	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		return false
	}
	sigErr := proc.Signal(syscall.Signal(0))
	if sigErr == nil {
		return true
	}
	// EPERM means the process exists but we can't signal it: still alive.
	if errors.Is(sigErr, syscall.EPERM) {
		return true
	}
	return false
}

// isProcessGone returns true when err signals that the target process
// has already exited (and hence the signal call is a no-op).
func isProcessGone(err error) bool {
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	return false
}

// waitForExit polls processAlive until the pid is gone or the deadline
// expires. Returns true if the process exited within the deadline.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
