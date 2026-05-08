package vm_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/vm"
)

func TestEnvDir_ExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	dir := vm.EnvDir("my-env")
	assert.Equal(t, filepath.Join(home, ".forge", "envs", "my-env"), dir)
}

func TestState_SaveAndLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	created := time.Now().UTC().Truncate(time.Second)
	original := &vm.State{
		Name:         "demo",
		Status:       vm.StatusRunning,
		IP:           "192.168.127.10",
		MAC:          "52:54:00:aa:bb:cc",
		PID:          12345,
		CreatedAt:    created,
		ImageVersion: "v0.1.0",
		CPUs:         2,
		Memory:       4096,
		Disk:         20480,
	}

	require.NoError(t, original.Save(dir))

	loaded, err := vm.LoadState(dir)
	require.NoError(t, err)
	assert.Equal(t, original.Name, loaded.Name)
	assert.Equal(t, original.Status, loaded.Status)
	assert.Equal(t, original.IP, loaded.IP)
	assert.Equal(t, original.MAC, loaded.MAC)
	assert.Equal(t, original.PID, loaded.PID)
	assert.True(t, original.CreatedAt.Equal(loaded.CreatedAt))
	assert.Equal(t, original.ImageVersion, loaded.ImageVersion)
	assert.Equal(t, original.CPUs, loaded.CPUs)
	assert.Equal(t, original.Memory, loaded.Memory)
	assert.Equal(t, original.Disk, loaded.Disk)
}

func TestState_Save_AtomicWritesNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	s := &vm.State{Name: "demo", Status: vm.StatusCreating, CreatedAt: time.Now()}
	require.NoError(t, s.Save(dir))

	// Final file exists, temp file is gone.
	_, err := os.Stat(filepath.Join(dir, "state.json"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "state.json.tmp"))
	require.True(t, os.IsNotExist(err), "temp file should not exist after save")
}

func TestState_Save_CreatesEnvDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "envdir")
	s := &vm.State{Name: "demo", Status: vm.StatusCreating, CreatedAt: time.Now()}
	require.NoError(t, s.Save(dir))

	_, err := os.Stat(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
}

func TestLoadState_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := vm.LoadState(dir)
	require.Error(t, err)
}

func TestLoadState_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o644))
	_, err := vm.LoadState(dir)
	require.Error(t, err)
}

func TestState_JSONFieldNames(t *testing.T) {
	s := &vm.State{
		Name:         "demo",
		Status:       vm.StatusRunning,
		IP:           "1.2.3.4",
		MAC:          "52:54:00:aa:bb:cc",
		PID:          1,
		CreatedAt:    time.Now(),
		ImageVersion: "v1",
		CPUs:         2,
		Memory:       4096,
		Disk:         20480,
	}
	b, err := json.Marshal(s)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(b, &raw))

	for _, key := range []string{"name", "status", "ip", "mac", "pid", "created_at", "image_version", "cpus", "memory", "disk"} {
		_, ok := raw[key]
		assert.Truef(t, ok, "expected JSON key %q in state.json", key)
	}
}

func TestStateMachine_ValidTransitions(t *testing.T) {
	cases := []struct {
		from vm.Status
		to   vm.Status
	}{
		{vm.StatusCreating, vm.StatusStarting},
		{vm.StatusCreating, vm.StatusCrashed},
		{vm.StatusCreating, vm.StatusDestroyed},
		{vm.StatusStarting, vm.StatusRunning},
		{vm.StatusStarting, vm.StatusStopped}, // forge env stop --force on a stuck start
		{vm.StatusStarting, vm.StatusCrashed},
		{vm.StatusStarting, vm.StatusDestroyed},
		{vm.StatusRunning, vm.StatusStopping},
		{vm.StatusRunning, vm.StatusCrashed},
		{vm.StatusStopping, vm.StatusStopped},
		{vm.StatusStopping, vm.StatusCrashed},
		{vm.StatusStopped, vm.StatusStarting},
		{vm.StatusStopped, vm.StatusDestroyed},
		{vm.StatusCrashed, vm.StatusStarting},
		{vm.StatusCrashed, vm.StatusStopped}, // forge env stop --force on a crashed env
		{vm.StatusCrashed, vm.StatusDestroyed},
	}

	for _, c := range cases {
		c := c
		t.Run(string(c.from)+"_to_"+string(c.to), func(t *testing.T) {
			s := &vm.State{Status: c.from}
			require.NoError(t, s.Transition(c.to))
			assert.Equal(t, c.to, s.Status)
		})
	}
}

func TestStateMachine_InvalidTransitions(t *testing.T) {
	cases := []struct {
		from vm.Status
		to   vm.Status
	}{
		{vm.StatusCreating, vm.StatusRunning},   // must go through starting
		{vm.StatusCreating, vm.StatusStopped},   // can't stop while creating
		{vm.StatusRunning, vm.StatusStopped},    // must go through stopping
		{vm.StatusRunning, vm.StatusStarting},   // already running
		{vm.StatusStopped, vm.StatusRunning},    // must go through starting
		{vm.StatusStopped, vm.StatusCrashed},    // already stopped is terminal w.r.t. crash
		{vm.StatusDestroyed, vm.StatusCreating}, // terminal
		{vm.StatusDestroyed, vm.StatusStarting},
		{vm.StatusDestroyed, vm.StatusRunning},
		{vm.StatusCrashed, vm.StatusRunning}, // must restart through starting

		// starting->stopped and crashed->stopped are NOW allowed (the
		// `forge env stop --force` recovery path), so they're no longer
		// in the "invalid" list. See state_test's positive coverage.
	}

	for _, c := range cases {
		c := c
		t.Run(string(c.from)+"_to_"+string(c.to), func(t *testing.T) {
			s := &vm.State{Status: c.from}
			err := s.Transition(c.to)
			require.Error(t, err)
			// Status should be unchanged on error.
			assert.Equal(t, c.from, s.Status)
		})
	}
}

func TestStateMachine_UnknownTargetStatus(t *testing.T) {
	s := &vm.State{Status: vm.StatusRunning}
	err := s.Transition(vm.Status("bogus"))
	require.Error(t, err)
}
