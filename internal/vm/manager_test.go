package vm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/vm"
)

// fakeRunner is a Runner stub used to drive Manager tests deterministically.
type fakeRunner struct {
	alive    bool
	aliveErr error
	startErr error
	stopErr  error
	started  int
	stopped  int
}

func (f *fakeRunner) Start(_ context.Context, _ string, _ vm.StartOptions) error {
	f.started++
	return f.startErr
}

func (f *fakeRunner) Stop(_ context.Context, _ string) error {
	f.stopped++
	return f.stopErr
}

func (f *fakeRunner) IsAlive(_ string) (bool, error) {
	return f.alive, f.aliveErr
}

func writeRunningState(t *testing.T, baseDir, name string, pid int) {
	t.Helper()
	envDir := filepath.Join(baseDir, name)
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	s := &vm.State{
		Name:      name,
		Status:    vm.StatusRunning,
		PID:       pid,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.Save(envDir))
}

func TestManager_Status_HealthyRunning(t *testing.T) {
	base := t.TempDir()
	writeRunningState(t, base, "demo", 1234)

	runner := &fakeRunner{alive: true}
	m := vm.NewManager(runner, base)

	st, err := m.Status("demo")
	require.NoError(t, err)
	assert.Equal(t, vm.StatusRunning, st)
}

func TestManager_Status_StalePIDDetectedAsCrashed(t *testing.T) {
	base := t.TempDir()
	writeRunningState(t, base, "demo", 99999) // PID is irrelevant; fakeRunner decides liveness.

	runner := &fakeRunner{alive: false}
	m := vm.NewManager(runner, base)

	st, err := m.Status("demo")
	require.NoError(t, err)
	assert.Equal(t, vm.StatusCrashed, st, "running state with dead PID must be reported as crashed")

	// Crashed status must be persisted.
	loaded, err := vm.LoadState(filepath.Join(base, "demo"))
	require.NoError(t, err)
	assert.Equal(t, vm.StatusCrashed, loaded.Status, "crashed state should be persisted to disk")
}

func TestManager_Status_StalePIDFromStartingState(t *testing.T) {
	base := t.TempDir()
	envDir := filepath.Join(base, "demo")
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	s := &vm.State{Name: "demo", Status: vm.StatusStarting, PID: 99999, CreatedAt: time.Now()}
	require.NoError(t, s.Save(envDir))

	runner := &fakeRunner{alive: false}
	m := vm.NewManager(runner, base)

	st, err := m.Status("demo")
	require.NoError(t, err)
	assert.Equal(t, vm.StatusCrashed, st)
}

func TestManager_Status_StoppedDoesNotCheckLiveness(t *testing.T) {
	base := t.TempDir()
	envDir := filepath.Join(base, "demo")
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	s := &vm.State{Name: "demo", Status: vm.StatusStopped, CreatedAt: time.Now()}
	require.NoError(t, s.Save(envDir))

	runner := &fakeRunner{aliveErr: errors.New("should not be called")}
	m := vm.NewManager(runner, base)

	st, err := m.Status("demo")
	require.NoError(t, err)
	assert.Equal(t, vm.StatusStopped, st)
}

func TestManager_Status_LivenessCheckErrors(t *testing.T) {
	base := t.TempDir()
	writeRunningState(t, base, "demo", 1234)

	runner := &fakeRunner{aliveErr: errors.New("boom")}
	m := vm.NewManager(runner, base)

	_, err := m.Status("demo")
	require.Error(t, err)
}

func TestManager_Status_MissingEnv(t *testing.T) {
	base := t.TempDir()
	m := vm.NewManager(&fakeRunner{}, base)

	_, err := m.Status("does-not-exist")
	require.Error(t, err)
}

func TestManager_MarkStopped(t *testing.T) {
	base := t.TempDir()
	writeRunningState(t, base, "demo", 1234)

	m := vm.NewManager(&fakeRunner{alive: false}, base)
	// Running -> Stopping -> Stopped
	require.NoError(t, m.MarkStopping("demo"))
	require.NoError(t, m.MarkStopped("demo"))

	loaded, err := vm.LoadState(filepath.Join(base, "demo"))
	require.NoError(t, err)
	assert.Equal(t, vm.StatusStopped, loaded.Status)
}

func TestManager_MarkCrashed(t *testing.T) {
	base := t.TempDir()
	writeRunningState(t, base, "demo", 1234)

	m := vm.NewManager(&fakeRunner{}, base)
	require.NoError(t, m.MarkCrashed("demo"))

	loaded, err := vm.LoadState(filepath.Join(base, "demo"))
	require.NoError(t, err)
	assert.Equal(t, vm.StatusCrashed, loaded.Status)
}

func TestManager_MarkCrashed_FromStopped(t *testing.T) {
	base := t.TempDir()
	envDir := filepath.Join(base, "demo")
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	// stopped -> crashed is not a valid state-machine transition.
	s := &vm.State{Name: "demo", Status: vm.StatusStopped, CreatedAt: time.Now()}
	require.NoError(t, s.Save(envDir))

	m := vm.NewManager(&fakeRunner{}, base)
	err := m.MarkCrashed("demo")
	require.Error(t, err, "stopped -> crashed should be rejected by the state machine")
}

func TestNewManager_ExpandsTilde(t *testing.T) {
	// We pass "~/.forge/envs" and verify the manager resolves env paths
	// against the actual home directory. We don't actually read from the
	// home dir — just confirm Status fails-on-missing rather than
	// fails-with-permission-or-bogus-tilde-path.
	m := vm.NewManager(&fakeRunner{}, "~/.forge/envs")
	_, err := m.Status("definitely-does-not-exist-1234567890")
	require.Error(t, err)
	// The error should reference the resolved path, not the raw tilde.
	home, _ := os.UserHomeDir()
	assert.Contains(t, err.Error(), home)
}
