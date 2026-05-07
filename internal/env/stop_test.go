package env_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

// recordingRunner is a Runner stub that records invocations and exposes
// configurable error returns. It is shared by stop_test, start_test, and
// destroy_test in this package.
type recordingRunner struct {
	mu sync.Mutex

	startCalls int
	startEnv   string
	startOpts  vm.StartOptions
	startErr   error

	stopCalls int
	stopEnv   string
	stopErr   error

	alive    bool
	aliveErr error
}

func (r *recordingRunner) Start(_ context.Context, envDir string, opts vm.StartOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCalls++
	r.startEnv = envDir
	r.startOpts = opts
	return r.startErr
}

func (r *recordingRunner) Stop(_ context.Context, envDir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopCalls++
	r.stopEnv = envDir
	return r.stopErr
}

func (r *recordingRunner) IsAlive(_ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alive, r.aliveErr
}

// writeState fabricates a `state.json` for the stop/start/destroy tests.
func writeState(t *testing.T, baseDir, name string, status vm.Status) string {
	t.Helper()
	envDir := filepath.Join(baseDir, name)
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	s := &vm.State{
		Name:         name,
		Status:       status,
		MAC:          "52:54:00:aa:bb:cc",
		CreatedAt:    time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
		ImageVersion: "0.1.0",
		CPUs:         2,
		Memory:       4096,
		Disk:         128,
		IP:           "192.168.127.42",
	}
	require.NoError(t, s.Save(envDir))
	return envDir
}

func TestStop_HappyPath(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusRunning)

	runner := &recordingRunner{}
	out := &bytes.Buffer{}

	res, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Out:        out,
	}, env.StopDeps{Runner: runner})
	require.NoError(t, err)

	// Runner.Stop was invoked with the right env dir.
	assert.Equal(t, 1, runner.stopCalls)
	assert.Equal(t, envDir, runner.stopEnv)

	// State persisted as stopped.
	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusStopped, state.Status)
	assert.Equal(t, vm.StatusStopped, res.State.Status)

	// Disk and other artefacts preserved.
	_, statErr := os.Stat(envDir)
	require.NoError(t, statErr)

	assert.Contains(t, out.String(), "demo")
}

func TestStop_AlreadyStopped(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	runner := &recordingRunner{}
	out := &bytes.Buffer{}

	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Out:        out,
	}, env.StopDeps{Runner: runner})
	require.NoError(t, err)

	assert.Equal(t, 0, runner.stopCalls)
	assert.Contains(t, out.String(), "already")
}

func TestStop_MissingEnv(t *testing.T) {
	root := t.TempDir()

	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "missing",
		EnvBaseDir: root,
		Out:        &bytes.Buffer{},
	}, env.StopDeps{Runner: &recordingRunner{}})
	require.Error(t, err)
}

func TestStop_RejectsCrashedWithoutForce(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusCrashed)

	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Out:        &bytes.Buffer{},
	}, env.StopDeps{Runner: &recordingRunner{}})
	require.Error(t, err, "stopping a crashed env without --force should error")
	assert.Contains(t, err.Error(), "--force")
}

func TestStop_RejectsStartingWithoutForce(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStarting)

	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Out:        &bytes.Buffer{},
	}, env.StopDeps{Runner: &recordingRunner{}})
	require.Error(t, err, "stopping a starting env without --force should error")
	assert.Contains(t, err.Error(), "--force")
}

func TestStop_ForceFromStarting(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStarting)

	runner := &recordingRunner{}
	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		Out:        &bytes.Buffer{},
	}, env.StopDeps{Runner: runner})
	require.NoError(t, err)

	// Runner.Stop was called so any wedged vfkit gets reaped.
	assert.GreaterOrEqual(t, runner.stopCalls, 1, "runner.Stop must run so a stuck vfkit is killed")

	// State flipped straight to stopped — the user can `forge env start` again.
	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusStopped, state.Status)
}

func TestStop_ForceFromCrashed(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusCrashed)

	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		Out:        &bytes.Buffer{},
	}, env.StopDeps{Runner: &recordingRunner{}})
	require.NoError(t, err)

	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusStopped, state.Status)
}

func TestStop_RunnerError(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusRunning)

	runner := &recordingRunner{stopErr: errors.New("boom")}
	_, err := env.Stop(context.Background(), env.StopInput{
		Name:       "demo",
		EnvBaseDir: root,
		Out:        &bytes.Buffer{},
	}, env.StopDeps{Runner: runner})
	require.Error(t, err)
}
