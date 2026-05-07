package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/config"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestLoadProject_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
defaults:
  cpus: 4
  memory: 8192
  disk: 40960
`)
	cfg, err := config.LoadProject(filepath.Join(dir, "forge.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "v1.32.0+k3s1", cfg.Bootstrap.K3s)
	assert.Equal(t, "v0.4.2", cfg.Bootstrap.Rage)
	assert.Equal(t, "latest", cfg.Bootstrap.ClaudeCode)
	assert.Equal(t, 4, cfg.Defaults.CPUs)
	assert.Equal(t, 8192, cfg.Defaults.Memory)
	assert.Equal(t, 40960, cfg.Defaults.Disk)
}

func TestLoadProject_DefaultResources(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
`)
	cfg, err := config.LoadProject(filepath.Join(dir, "forge.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 2, cfg.Defaults.CPUs)
	assert.Equal(t, 4096, cfg.Defaults.Memory)
	assert.Equal(t, 20480, cfg.Defaults.Disk)
}

func TestLoadProject_MissingK3s(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
`)
	_, err := config.LoadProject(filepath.Join(dir, "forge.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap.k3s")
}

func TestLoadProject_MissingRage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  claude_code: latest
  helm: v3.20.2
`)
	_, err := config.LoadProject(filepath.Join(dir, "forge.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap.rage")
}

func TestLoadProject_MissingClaudeCode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
`)
	_, err := config.LoadProject(filepath.Join(dir, "forge.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap.claude_code")
}

func TestLoadProject_FileNotFound(t *testing.T) {
	_, err := config.LoadProject("/nonexistent/forge.yaml")
	require.Error(t, err)
}

func TestLoadProject_InvalidCPUs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
defaults:
  cpus: -1
`)
	_, err := config.LoadProject(filepath.Join(dir, "forge.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "defaults.cpus")
}

// chdir changes into dir for the duration of the current test, restoring
// the original CWD on cleanup. Discover() reads os.Getwd, so tests have
// to drive it through the real CWD.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func TestDiscover_FindsInCWD(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
defaults:
  cpus: 8
`)
	chdir(t, dir)

	cfg, source, err := config.Discover()
	require.NoError(t, err)
	assert.Equal(t, 8, cfg.Defaults.CPUs)
	// Source should be an absolute path resolving to the file we wrote.
	assert.NotEmpty(t, source)
	resolvedSource, err := filepath.EvalSymlinks(source)
	require.NoError(t, err)
	resolvedExpected, err := filepath.EvalSymlinks(filepath.Join(dir, "forge.yaml"))
	require.NoError(t, err)
	assert.Equal(t, resolvedExpected, resolvedSource)
}

func TestDiscover_WalksUp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "forge.yaml", `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
defaults:
  cpus: 16
`)
	deep := filepath.Join(root, "src", "feature", "nested")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	chdir(t, deep)

	cfg, source, err := config.Discover()
	require.NoError(t, err)
	assert.Equal(t, 16, cfg.Defaults.CPUs)
	resolvedSource, err := filepath.EvalSymlinks(source)
	require.NoError(t, err)
	resolvedExpected, err := filepath.EvalSymlinks(filepath.Join(root, "forge.yaml"))
	require.NoError(t, err)
	assert.Equal(t, resolvedExpected, resolvedSource)
}

func TestDiscover_FallsBackToDefaultsWhenNotFound(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	cfg, source, err := config.Discover()
	require.NoError(t, err)
	assert.Empty(t, source, "source should be empty when defaults are used")
	// Embedded defaults must be valid and produce known values.
	assert.NotEmpty(t, cfg.Bootstrap.K3s)
	assert.NotEmpty(t, cfg.Bootstrap.Rage)
	assert.NotEmpty(t, cfg.Bootstrap.ClaudeCode)
	assert.Equal(t, 2, cfg.Defaults.CPUs)
	assert.Equal(t, 4096, cfg.Defaults.Memory)
	assert.Equal(t, 20480, cfg.Defaults.Disk)
}

func TestDefaultProjectYAML_ParsesAndValidates(t *testing.T) {
	// The embedded default must round-trip through the loader without
	// error — this guards against typos and against drift between the
	// template and the schema.
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(path, config.DefaultProjectYAML(), 0o644))
	cfg, err := config.LoadProject(path)
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Bootstrap.K3s)
	assert.NotEmpty(t, cfg.Bootstrap.Rage)
	assert.NotEmpty(t, cfg.Bootstrap.ClaudeCode)
}
