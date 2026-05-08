package vm_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/vm"
)

// newSleepRunner returns a Runner backed by `/bin/sleep` rather than
// vfkit. This lets the subprocess-management code be exercised on any
// machine, regardless of whether vfkit is installed.
func newSleepRunner() *vm.VfkitRunner {
	r := vm.NewVfkitRunner()
	r.Binary = "/bin/sleep"
	r.ArgsBuilder = func(_ vm.StartOptions) []string {
		return []string{"60"}
	}
	// Keep the suite fast: production grace period is 10s, but the
	// test stand-in (`sleep`) responds to SIGTERM immediately.
	r.StopGracePeriod = 500 * time.Millisecond
	return r
}

func TestVfkitRunner_StartWritesPidFileAndIsAlive(t *testing.T) {
	envDir := t.TempDir()
	r := newSleepRunner()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, r.Start(ctx, envDir, vm.StartOptions{
		DiskPath:     filepath.Join(envDir, "disk.img"),
		CloudInitISO: filepath.Join(envDir, "cloud-init.iso"),
		CPUs:         2,
		MemoryMB:     4096,
		MAC:          "52:54:00:aa:bb:cc",
	}))

	t.Cleanup(func() { _ = r.Stop(context.Background(), envDir) })

	// PID file written.
	pidPath := filepath.Join(envDir, "vfkit.pid")
	pidData, err := os.ReadFile(pidPath)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	require.NoError(t, err)
	assert.Greater(t, pid, 0)

	// Log file exists.
	_, err = os.Stat(filepath.Join(envDir, "vfkit.log"))
	require.NoError(t, err)

	alive, err := r.IsAlive(envDir)
	require.NoError(t, err)
	assert.True(t, alive, "freshly-started subprocess should be alive")
}

func TestVfkitRunner_StopTerminatesProcess(t *testing.T) {
	envDir := t.TempDir()
	r := newSleepRunner()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, r.Start(ctx, envDir, vm.StartOptions{}))

	require.NoError(t, r.Stop(context.Background(), envDir))

	alive, err := r.IsAlive(envDir)
	require.NoError(t, err)
	assert.False(t, alive, "subprocess should be gone after Stop")
}

func TestVfkitRunner_IsAlive_NoPIDFile(t *testing.T) {
	envDir := t.TempDir()
	r := vm.NewVfkitRunner()

	alive, err := r.IsAlive(envDir)
	require.NoError(t, err)
	assert.False(t, alive)
}

func TestVfkitRunner_IsAlive_StalePID(t *testing.T) {
	envDir := t.TempDir()
	// Write an obviously-dead PID into the pid file.
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "vfkit.pid"), []byte("99999\n"), 0o644))

	r := vm.NewVfkitRunner()
	alive, err := r.IsAlive(envDir)
	require.NoError(t, err)
	assert.False(t, alive, "stale PID must be reported as not alive")
}

func TestVfkitRunner_IsAlive_GarbagePIDFile(t *testing.T) {
	envDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "vfkit.pid"), []byte("not-a-number"), 0o644))

	r := vm.NewVfkitRunner()
	_, err := r.IsAlive(envDir)
	require.Error(t, err, "non-numeric PID file should be reported as a parsing error")
}

func TestVfkitRunner_StartCreatesEnvDir(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "nested", "env")
	r := newSleepRunner()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, r.Start(ctx, envDir, vm.StartOptions{}))
	t.Cleanup(func() { _ = r.Stop(context.Background(), envDir) })

	_, err := os.Stat(filepath.Join(envDir, "vfkit.pid"))
	require.NoError(t, err)
}

func TestVfkitRunner_StopWithNoPIDFile(t *testing.T) {
	envDir := t.TempDir()
	r := vm.NewVfkitRunner()
	// Stop with no pid file should be a no-op (or at least not error).
	require.NoError(t, r.Stop(context.Background(), envDir))
}

func TestDefaultArgs_IncludesResourcesAndDevices(t *testing.T) {
	opts := vm.StartOptions{
		DiskPath:      "/tmp/disk.img",
		CloudInitISO:  "/tmp/ci.iso",
		CPUs:          4,
		MemoryMB:      8192,
		MAC:           "52:54:00:aa:bb:cc",
		NetSocketPath: "/tmp/net.sock",
	}
	args := vm.DefaultArgs(opts)
	joined := strings.Join(args, " ")

	assert.Contains(t, joined, "--cpus 4")
	assert.Contains(t, joined, "--memory 8192")
	assert.Contains(t, joined, "virtio-blk,path=/tmp/disk.img")
	assert.Contains(t, joined, "virtio-blk,path=/tmp/ci.iso")
	// virtio-net,unixSocketPath replaces virtio-net,nat. The MAC is
	// appended after the socket path; both must be present.
	assert.Contains(t, joined, "virtio-net,unixSocketPath=/tmp/net.sock,mac=52:54:00:aa:bb:cc")
	assert.NotContains(t, joined, "virtio-net,nat",
		"vmnet shared mode is gone — gvproxy userspace netstack replaces it")
}

func TestDefaultArgs_OmitsZeroValues(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{})
	joined := strings.Join(args, " ")

	assert.NotContains(t, joined, "--cpus")
	assert.NotContains(t, joined, "--memory")
	assert.NotContains(t, joined, "virtio-blk")
	// Without NetSocketPath there is no NIC. Production callers
	// (env create/start) always set it; tests that don't are
	// exercising other knobs.
	assert.NotContains(t, joined, "virtio-net")
}

func TestDefaultArgs_IncludesEFIBootloader(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{EFIVarStorePath: "/tmp/efi-vars"})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "--bootloader")
	assert.Contains(t, joined, "efi,variable-store=/tmp/efi-vars,create")
}

func TestDefaultArgs_IncludesVsockDevice(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{
		VsockSocketPath: "/tmp/vsock.sock",
		VsockPort:       1234,
	})
	joined := strings.Join(args, " ")
	// Bare `listen` flag (the canonical form per vfkit's docs); a
	// `listen=true` k=v form is silently mistreated as the default.
	assert.Contains(t, joined, "virtio-vsock,port=1234,socketURL=/tmp/vsock.sock,listen")
}

func TestDefaultArgs_VsockUsesDefaultPort(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{VsockSocketPath: "/tmp/vsock.sock"})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "port=1234")
}

func TestDefaultArgs_SSHVsockDevice(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{
		VsockSocketPath: "/tmp/vsock.sock",
		SSHSocketPath:   "/tmp/ssh.sock",
	})
	joined := strings.Join(args, " ")
	// Boot-ready: bare `listen` flag (vfkit doc default). vfkit dials
	// FORGE's pre-bound unix socket whenever the guest connects out.
	assert.Contains(t, joined, "virtio-vsock,port=1234,socketURL=/tmp/vsock.sock,listen")
	// SSH bridge: bare `connect` flag. vfkit binds the unix socket on
	// the host and forwards inbound to vsock 22 in the guest. Using
	// `listen=true` / `listen=false` here was the bug — vfkit silently
	// fell back to the default listen mode and never bound ssh.sock.
	assert.Contains(t, joined, "virtio-vsock,port=22,socketURL=/tmp/ssh.sock,connect")
}

func TestDefaultArgs_SSHVsockOmittedWhenUnset(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{VsockSocketPath: "/tmp/vsock.sock"})
	joined := strings.Join(args, " ")
	assert.NotContains(t, joined, "port=22")
}

func TestDefaultArgs_ForgejoVsockDevice(t *testing.T) {
	// Third virtio-vsock device, listen direction. vfkit dials the
	// host-side unix socket whenever the guest connects to the
	// matching vsock port — internal/forgejoproxy is what's listening
	// on the other end. Bare `listen` flag (canonical syntax); same
	// trap as the SSH and boot-ready devices.
	args := vm.DefaultArgs(vm.StartOptions{
		ForgejoSocketPath: "/tmp/forgejo.sock",
		ForgejoVsockPort:  4000,
	})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "virtio-vsock,port=4000,socketURL=/tmp/forgejo.sock,listen")
}

func TestDefaultArgs_ForgejoVsockOmittedWhenIncomplete(t *testing.T) {
	// Both fields are required — emitting `port=0` or `socketURL=`
	// alone would just confuse vfkit.
	cases := []vm.StartOptions{
		{ForgejoSocketPath: "/tmp/forgejo.sock"},
		{ForgejoVsockPort: 4000},
	}
	for _, opts := range cases {
		joined := strings.Join(vm.DefaultArgs(opts), " ")
		assert.NotContains(t, joined, "forgejo.sock")
		assert.NotContains(t, joined, "port=4000")
	}
}

func TestDefaultArgs_RageShareDirAddsVirtioFS(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{
		RageShareDir: "/host/rage",
	})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "virtio-fs,sharedDir=/host/rage,mountTag=rage-share")
}

func TestDefaultArgs_RageShareOmittedWhenUnset(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{VsockSocketPath: "/tmp/vsock.sock"})
	joined := strings.Join(args, " ")
	assert.NotContains(t, joined, "virtio-fs")
	assert.NotContains(t, joined, "rage-share")
}

func TestDefaultArgs_WorkspaceShareDirAddsVirtioFS(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{
		WorkspaceShareDir: "/host/envs/test1/workspace",
	})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined,
		"virtio-fs,sharedDir=/host/envs/test1/workspace,mountTag=workspace-share")
}

func TestDefaultArgs_BothSharesCanCoexist(t *testing.T) {
	args := vm.DefaultArgs(vm.StartOptions{
		RageShareDir:      "/host/rage",
		WorkspaceShareDir: "/host/workspace",
	})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "mountTag=rage-share")
	assert.Contains(t, joined, "mountTag=workspace-share")
}

func TestVfkitRunner_ProcessOutlivesParentSession(t *testing.T) {
	// Verifies the child process gets its own session (Setsid).
	// We can confirm this by checking the child's session id differs
	// from the parent (test process) session id via /proc on Linux,
	// or by inspecting the child PGID == child PID.
	envDir := t.TempDir()
	r := newSleepRunner()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, r.Start(ctx, envDir, vm.StartOptions{}))
	t.Cleanup(func() { _ = r.Stop(context.Background(), envDir) })

	pidData, err := os.ReadFile(filepath.Join(envDir, "vfkit.pid"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	require.NoError(t, err)

	// On Setsid, the child's process group ID equals its PID.
	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err)
	assert.Equal(t, pid, pgid, "Setsid should put child in its own process group; PGID == PID")
}
