package env_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

func writeEnvWithIP(t *testing.T, base, name, ip string) {
	t.Helper()
	dir := filepath.Join(base, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	state := &vm.State{
		Name:      name,
		Status:    vm.StatusRunning,
		IP:        ip,
		MAC:       "52:54:00:00:00:01",
		CreatedAt: time.Now(),
	}
	require.NoError(t, state.Save(dir))
}

func TestAllocateIP_FirstEnvGetsLowestAvailable(t *testing.T) {
	base := t.TempDir()
	ip, err := env.AllocateIP(base)
	require.NoError(t, err)
	assert.Equal(t, "192.168.127.42", ip)
}

func TestAllocateIP_BaseDirMissingIsFine(t *testing.T) {
	// First-ever invocation: ~/.forge/envs doesn't exist yet.
	ip, err := env.AllocateIP(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.127.42", ip)
}

func TestAllocateIP_SkipsAlreadyUsed(t *testing.T) {
	base := t.TempDir()
	writeEnvWithIP(t, base, "alpha", "192.168.127.42")
	writeEnvWithIP(t, base, "beta", "192.168.127.43")

	ip, err := env.AllocateIP(base)
	require.NoError(t, err)
	assert.Equal(t, "192.168.127.44", ip)
}

func TestAllocateIP_RecyclesGaps(t *testing.T) {
	// Bottom-up: a gap left by a destroyed env should be filled before
	// the high-water mark grows.
	base := t.TempDir()
	writeEnvWithIP(t, base, "alpha", "192.168.127.42")
	writeEnvWithIP(t, base, "gamma", "192.168.127.44") // .43 is free

	ip, err := env.AllocateIP(base)
	require.NoError(t, err)
	assert.Equal(t, "192.168.127.43", ip)
}

func TestAllocateIP_IgnoresNonEnvDirsAndFiles(t *testing.T) {
	base := t.TempDir()
	// A loose file at the env base — not an env dir.
	require.NoError(t, os.WriteFile(filepath.Join(base, "README.md"), []byte("hi"), 0o644))
	// A dir without a state.json.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "scratch"), 0o755))

	ip, err := env.AllocateIP(base)
	require.NoError(t, err)
	assert.Equal(t, "192.168.127.42", ip)
}
