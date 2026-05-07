package env_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

func TestDestroy_HappyPathWithForce(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "disk.img"), []byte("disk"), 0o644))

	runner := &recordingRunner{}
	out := &bytes.Buffer{}

	res, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        out,
	}, env.DestroyDeps{Runner: runner})
	require.NoError(t, err)
	assert.True(t, res.Removed)

	// Env dir is gone.
	_, statErr := os.Stat(envDir)
	assert.True(t, os.IsNotExist(statErr))

	// Stop wasn't called — env was already stopped.
	assert.Equal(t, 0, runner.stopCalls)
}

func TestDestroy_StopsRunningVMFirst(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusRunning)
	require.NoError(t, os.WriteFile(filepath.Join(envDir, "disk.img"), []byte("disk"), 0o644))

	runner := &recordingRunner{}
	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: runner})
	require.NoError(t, err)

	assert.Equal(t, 1, runner.stopCalls, "running env should be stopped before destruction")
	assert.Equal(t, envDir, runner.stopEnv)

	// Env dir is gone.
	_, statErr := os.Stat(envDir)
	assert.True(t, os.IsNotExist(statErr))
}

func TestDestroy_StopsCrashedVM(t *testing.T) {
	// Crashed envs may still have a leftover process (rare, but possible
	// during the race between liveness probe and reality). Best-effort:
	// call runner.Stop anyway — it's a no-op if the pid file is missing.
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusCrashed)

	runner := &recordingRunner{}
	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: runner})
	require.NoError(t, err)

	_, statErr := os.Stat(envDir)
	assert.True(t, os.IsNotExist(statErr))
}

func TestDestroy_PromptAcceptsYes(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)

	runner := &recordingRunner{}
	out := &bytes.Buffer{}
	res, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      false,
		In:         strings.NewReader("y\n"),
		Out:        out,
	}, env.DestroyDeps{Runner: runner})
	require.NoError(t, err)
	assert.True(t, res.Removed)

	_, statErr := os.Stat(envDir)
	assert.True(t, os.IsNotExist(statErr))

	// Prompt was printed.
	assert.Contains(t, out.String(), "demo")
}

func TestDestroy_PromptAcceptsYesUppercase(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)

	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      false,
		In:         strings.NewReader("YES\n"),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}})
	require.NoError(t, err)

	_, statErr := os.Stat(envDir)
	assert.True(t, os.IsNotExist(statErr))
}

func TestDestroy_PromptDeclined(t *testing.T) {
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)

	out := &bytes.Buffer{}
	res, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      false,
		In:         strings.NewReader("n\n"),
		Out:        out,
	}, env.DestroyDeps{Runner: &recordingRunner{}})
	require.NoError(t, err)
	assert.False(t, res.Removed)

	// Env dir still exists.
	_, statErr := os.Stat(envDir)
	require.NoError(t, statErr)

	assert.Contains(t, out.String(), "Aborted")
}

func TestDestroy_MissingEnv(t *testing.T) {
	root := t.TempDir()

	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "ghost",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}})
	require.Error(t, err)
}

func TestDestroy_StopErrorPropagates(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusRunning)

	runner := &recordingRunner{stopErr: errors.New("boom")}
	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: runner})
	require.Error(t, err)
}

// fakeForgejoPurger records PurgeEnvUser calls for assertion.
type fakeForgejoPurger struct {
	calls    []string
	purgeErr error
}

func (f *fakeForgejoPurger) PurgeEnvUser(_ context.Context, name string) error {
	f.calls = append(f.calls, name)
	return f.purgeErr
}

func TestDestroy_PurgeForgejo_OffByDefault(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	purger := &fakeForgejoPurger{}
	res, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}, Forgejo: purger})
	require.NoError(t, err)
	assert.True(t, res.Removed)
	assert.False(t, res.PurgedForgejo, "default destroy must NOT touch forgejo")
	assert.Empty(t, purger.calls, "default destroy must NOT call forgejo")
}

func TestDestroy_PurgeForgejo_WhenRequested(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	purger := &fakeForgejoPurger{}
	res, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:         "demo",
		EnvBaseDir:   root,
		Force:        true,
		PurgeForgejo: true,
		In:           strings.NewReader(""),
		Out:          &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}, Forgejo: purger})
	require.NoError(t, err)
	assert.True(t, res.Removed)
	assert.True(t, res.PurgedForgejo)
	require.Equal(t, []string{"demo"}, purger.calls)
}

func TestDestroy_PurgeForgejo_RequiresClient(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:         "demo",
		EnvBaseDir:   root,
		Force:        true,
		PurgeForgejo: true,
		In:           strings.NewReader(""),
		Out:          &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}}) // Forgejo is nil
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forgejo")
}

func TestDestroy_PurgeForgejo_PropagatesError(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, "demo", vm.StatusStopped)

	purger := &fakeForgejoPurger{purgeErr: errors.New("403 forbidden")}
	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:         "demo",
		EnvBaseDir:   root,
		Force:        true,
		PurgeForgejo: true,
		In:           strings.NewReader(""),
		Out:          &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}, Forgejo: purger})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestDestroy_ToleratesMissingKnownHosts(t *testing.T) {
	// known_hosts may or may not exist depending on whether `forge env
	// connect` was ever called for this env. RemoveAll handles both cases
	// — this test pins the requirement.
	root := t.TempDir()
	envDir := writeState(t, root, "demo", vm.StatusStopped)
	// No known_hosts file written.

	_, err := env.Destroy(context.Background(), env.DestroyInput{
		Name:       "demo",
		EnvBaseDir: root,
		Force:      true,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
	}, env.DestroyDeps{Runner: &recordingRunner{}})
	require.NoError(t, err)

	_, statErr := os.Stat(envDir)
	assert.True(t, os.IsNotExist(statErr))
}
