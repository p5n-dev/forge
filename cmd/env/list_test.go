package env_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	envcmd "github.com/p5n-dev/forge/cmd/env"
	"github.com/p5n-dev/forge/internal/vm"
)

// ansiPattern matches ANSI escape sequences so test assertions can ignore
// whatever terminal styling lipgloss emitted.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// writeStateJSON drops a state.json into baseDir/<name>/state.json with the
// given fields. Tests use it to fabricate ~/.forge/envs/* layouts.
func writeStateJSON(t *testing.T, baseDir, name string, state vm.State) {
	t.Helper()
	envDir := filepath.Join(baseDir, name)
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	data, err := json.MarshalIndent(state, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "state.json"), data, 0o644))
}

// writePIDFile writes vfkit.pid for a given env. Used to exercise
// stale-PID detection through Manager.Status -> VfkitRunner.IsAlive.
func writePIDFile(t *testing.T, baseDir, name string, pid int) {
	t.Helper()
	envDir := filepath.Join(baseDir, name)
	require.NoError(t, os.MkdirAll(envDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(envDir, "vfkit.pid"),
		[]byte(fmt.Sprintf("%d\n", pid)),
		0o644,
	))
}

// runListWith executes `forge env list` with envsBaseDir overridden via the
// exported test hook.
func runListWith(t *testing.T, baseDir string) string {
	t.Helper()

	restore := envcmd.SetEnvsBaseDirForTest(baseDir)
	t.Cleanup(restore)

	out := &bytes.Buffer{}
	envcmd.Cmd.SetOut(out)
	envcmd.Cmd.SetErr(out)
	envcmd.Cmd.SetArgs([]string{"list"})
	require.NoError(t, envcmd.Cmd.Execute())
	return out.String()
}

func TestListCommand_EmptyMissingDir(t *testing.T) {
	// Point at a directory that doesn't exist at all — listing must still
	// succeed and produce the friendly "no envs" message.
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	got := runListWith(t, missing)
	assert.Contains(t, got, "No environments found")
}

func TestListCommand_EmptyExistingDir(t *testing.T) {
	// An existing but empty envs/ directory should also produce the
	// friendly message (no header, no error).
	base := t.TempDir()

	got := runListWith(t, base)
	assert.Contains(t, got, "No environments found")
	assert.NotContains(t, got, "NAME")
}

func TestListCommand_RendersMixedStatuses(t *testing.T) {
	base := t.TempDir()

	now := time.Now()
	writeStateJSON(t, base, "alpha", vm.State{
		Name:      "alpha",
		Status:    vm.StatusStopped,
		IP:        "192.168.127.10",
		CPUs:      2,
		Memory:    4096,
		CreatedAt: now.Add(-3 * 24 * time.Hour),
	})
	writeStateJSON(t, base, "beta", vm.State{
		Name:      "beta",
		Status:    vm.StatusCrashed,
		IP:        "192.168.127.11",
		CPUs:      4,
		Memory:    8192,
		CreatedAt: now.Add(-2 * time.Hour),
	})

	out := stripANSI(runListWith(t, base))

	// Header present.
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "STATUS")
	assert.Contains(t, out, "IP")
	assert.Contains(t, out, "CPUS")
	assert.Contains(t, out, "MEM")
	assert.Contains(t, out, "CREATED")

	// Each env's name and status must appear.
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "stopped")
	assert.Contains(t, out, "192.168.127.10")

	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "crashed")
	assert.Contains(t, out, "192.168.127.11")
}

func TestListCommand_StalePIDDetection(t *testing.T) {
	base := t.TempDir()

	// Persisted state says "running" but the PID file points at a process
	// that won't exist (PID 99999 is conventionally unused on a fresh
	// system; even if taken, the env's vfkit.pid is written-here so we
	// know it's not actually a running vfkit). Manager.Status must
	// detect this and rewrite the status to "crashed" on disk + return
	// "crashed" to the renderer.
	writeStateJSON(t, base, "ghost", vm.State{
		Name:      "ghost",
		Status:    vm.StatusRunning,
		IP:        "192.168.127.99",
		PID:       99999,
		CPUs:      2,
		Memory:    4096,
		CreatedAt: time.Now().Add(-5 * time.Minute),
	})
	writePIDFile(t, base, "ghost", 99999)

	out := stripANSI(runListWith(t, base))

	assert.Contains(t, out, "ghost")
	assert.Contains(t, out, "crashed",
		"running state with a dead PID must surface as crashed in the table")

	// And the on-disk state should now be persisted as crashed.
	loaded, err := vm.LoadState(filepath.Join(base, "ghost"))
	require.NoError(t, err)
	assert.Equal(t, vm.StatusCrashed, loaded.Status)
}

func TestListCommand_SkipsInvalidEnvs(t *testing.T) {
	base := t.TempDir()

	// One valid env and one directory with no state.json — the latter
	// should be silently ignored, not reported as an error.
	writeStateJSON(t, base, "real", vm.State{
		Name:      "real",
		Status:    vm.StatusStopped,
		IP:        "192.168.127.20",
		CPUs:      2,
		Memory:    4096,
		CreatedAt: time.Now(),
	})
	require.NoError(t, os.MkdirAll(filepath.Join(base, "junk"), 0o755))
	// And one with a malformed state.json.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "broken"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(base, "broken", "state.json"),
		[]byte("not json"),
		0o644,
	))

	got := stripANSI(runListWith(t, base))
	assert.Contains(t, got, "real")
	assert.NotContains(t, got, "junk")
}

func TestHumanRelative(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "just now"},
		{"sub-minute", 30 * time.Second, "just now"},
		{"minutes", 5 * time.Minute, "5m ago"},
		{"one hour", time.Hour, "1h ago"},
		{"hours", 4 * time.Hour, "4h ago"},
		{"days", 3 * 24 * time.Hour, "3d ago"},
		{"months", 60 * 24 * time.Hour, "2mo ago"},
		{"negative clamped", -5 * time.Second, "just now"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, envcmd.HumanRelativeForTest(tc.d))
		})
	}
}
