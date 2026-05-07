package env_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

func defaultStartInput(t *testing.T, root, name string) env.StartInput {
	t.Helper()
	// Place a dummy disk.img so callers don't have to.
	envDir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "disk.img"), []byte("disk"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAA forge-test\n"), 0o644))
	return env.StartInput{
		Name:          name,
		EnvBaseDir:    root,
		K3sVersion:    "v1.32.0+k3s1",
		RageVersion:   "v0.4.2",
		ClaudeVersion: "latest",
		HelmVersion:   "v3.20.2",
	}
}

// stubWaitForSSH always succeeds. Tests that need to exercise the
// WaitForSSH error path build their own.
func stubWaitForSSH(_ context.Context, _ string) error { return nil }

func defaultStartDeps(runner vm.Runner) env.StartDeps {
	return env.StartDeps{
		Runner: runner,
		WriteISO: func(out string, _, _, _ []byte) error {
			return os.WriteFile(out, []byte("ISO"), 0o644)
		},
		WaitForSSH: stubWaitForSSH,
	}
}

func TestStart_HappyPathFromStopped(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)

	in := defaultStartInput(t, root, "demo")
	runner := &recordingRunner{}

	res, err := env.Start(context.Background(), in, defaultStartDeps(runner))
	require.NoError(t, err)

	// Runner.Start was invoked with the right env dir + options.
	assert.Equal(t, 1, runner.startCalls)
	assert.Equal(t, envDir, runner.startEnv)
	assert.Equal(t, filepath.Join(envDir, "disk.img"), runner.startOpts.DiskPath)
	assert.Equal(t, filepath.Join(envDir, "cloud-init.iso"), runner.startOpts.CloudInitISO)
	assert.Equal(t, filepath.Join(envDir, "ssh.sock"), runner.startOpts.SSHSocketPath)
	assert.Equal(t, filepath.Join(envDir, "efi-vars"), runner.startOpts.EFIVarStorePath)
	assert.Equal(t, "52:54:00:aa:bb:cc", runner.startOpts.MAC)
	assert.Equal(t, 2, runner.startOpts.CPUs)
	assert.Equal(t, 4096, runner.startOpts.MemoryMB)
	// The boot-ready vsock listener is intentionally not set up on
	// start: it's first-boot-only, so it would just be a wasted socket.
	assert.Empty(t, runner.startOpts.VsockSocketPath, "start path must not request the boot-ready vsock listener")

	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusRunning, state.Status)
	assert.Equal(t, "192.168.127.42", state.IP)
	assert.Equal(t, "0.1.0", state.ImageVersion)
	assert.Equal(t, vm.StatusRunning, res.State.Status)

	// Cloud-init ISO was rewritten.
	_, statErr := os.Stat(filepath.Join(envDir, "cloud-init.iso"))
	require.NoError(t, statErr)
}

func TestStart_FromCrashedClearsStalePIDFile(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusCrashed)

	// A stale pid file from the previous run.
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "vfkit.pid"), []byte("99999\n"), 0o644))

	in := defaultStartInput(t, root, "demo")
	runner := &recordingRunner{}

	_, err := env.Start(context.Background(), in, defaultStartDeps(runner))
	require.NoError(t, err)

	// The stale pid file is gone (Start removed it before booting).
	_, statErr := os.Stat(filepath.Join(envDir, "vfkit.pid"))
	assert.True(t, os.IsNotExist(statErr), "stale vfkit.pid should be removed before boot")

	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusRunning, state.Status)
}

func TestStart_RemovesStaleSSHSocket(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)
	// Pretend a previous boot left an ssh.sock behind. vfkit refuses to
	// bind a socketURL whose path already exists, so Start must nuke it.
	sshSock := filepath.Join(envDir, "ssh.sock")
	require.NoError(t, os.WriteFile(sshSock, []byte("stale"), 0o644))

	in := defaultStartInput(t, root, "demo")
	runner := &recordingRunner{}

	_, err := env.Start(context.Background(), in, defaultStartDeps(runner))
	require.NoError(t, err)

	// Start cleared the file before vfkit got it. (Our recordingRunner
	// doesn't actually run vfkit, so we can't check that vfkit
	// re-created it — only that the stale file was removed.)
	_, statErr := os.Stat(sshSock)
	assert.True(t, os.IsNotExist(statErr), "stale ssh.sock should be removed before vfkit binds it")
}

func TestStart_MissingEnv(t *testing.T) {
	root := t.TempDir()
	in := defaultStartInput(t, root, "ghost")
	// Remove the env dir we just created so the env truly doesn't exist.
	require.NoError(t, os.RemoveAll(filepath.Join(root, "ghost")))

	_, err := env.Start(context.Background(), in, defaultStartDeps(&recordingRunner{}))
	require.Error(t, err)
}

func TestStart_RejectsRunningEnv(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusRunning)

	in := defaultStartInput(t, root, "demo")

	_, err := env.Start(context.Background(), in, defaultStartDeps(&recordingRunner{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "running")
}

func TestStart_RejectsStartingEnv(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStarting)

	in := defaultStartInput(t, root, "demo")
	_, err := env.Start(context.Background(), in, defaultStartDeps(&recordingRunner{}))
	require.Error(t, err)
}

func TestStart_WaitForSSHTimeoutPropagates(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	in := defaultStartInput(t, root, "demo")
	runner := &recordingRunner{}
	deps := defaultStartDeps(runner)
	deps.WaitForSSH = func(_ context.Context, _ string) error {
		return context.DeadlineExceeded
	}

	_, err := env.Start(context.Background(), in, deps)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestStart_WaitForSSHFailureLeavesEnvCrashed(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)

	in := defaultStartInput(t, root, "demo")
	runner := &recordingRunner{}
	deps := defaultStartDeps(runner)
	deps.WaitForSSH = func(_ context.Context, _ string) error {
		return context.DeadlineExceeded
	}

	_, err := env.Start(context.Background(), in, deps)
	require.Error(t, err)

	// Runner.Stop must have run so the half-booted vfkit doesn't
	// linger. Otherwise the user is stuck and needs --force.
	assert.GreaterOrEqual(t, runner.stopCalls, 1, "Start must reap vfkit on WaitForSSH failure")

	// State must reflect failure, not be stuck at "starting".
	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusCrashed, state.Status,
		"a failed Start must leave the env in 'crashed' so `forge env list` is honest")
}

func TestStart_RunnerError(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	in := defaultStartInput(t, root, "demo")
	runner := &recordingRunner{startErr: errors.New("vfkit boom")}

	_, err := env.Start(context.Background(), in, defaultStartDeps(runner))
	require.Error(t, err)
}
