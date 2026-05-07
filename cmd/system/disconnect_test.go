package system

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/config"
)

func TestDisconnect_ClearsExternalConnection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	configPath := filepath.Join(dir, ".forge", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`
forgejo:
  url: "https://git.example.com"
  token: "abc"
  admin_user: "admin"
  admin_token: "tok"
image:
  cache_dir: "~/.forge/images"
`), 0o644))

	var buf bytes.Buffer
	require.NoError(t, runDisconnect(&buf))

	assert.Contains(t, buf.String(), "https://git.example.com")

	cfg, err := config.LoadGlobal(configPath)
	require.NoError(t, err)
	assert.Equal(t, config.ForgejoConfig{}, cfg.Forgejo, "forgejo block must be cleared")
	// Unrelated config should survive.
	assert.Equal(t, "~/.forge/images", cfg.Image.CacheDir, "image config must not be touched")
}

func TestDisconnect_ClearsManagedConnection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	configPath := filepath.Join(dir, ".forge", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`
forgejo:
  port: 3000
  admin_user: "forge"
  admin_token: "tok"
`), 0o644))

	var buf bytes.Buffer
	require.NoError(t, runDisconnect(&buf))

	assert.Contains(t, buf.String(), "3000")

	cfg, err := config.LoadGlobal(configPath)
	require.NoError(t, err)
	assert.Equal(t, config.ForgejoConfig{}, cfg.Forgejo)
}

func TestDisconnect_AlreadyDisconnected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	var buf bytes.Buffer
	require.NoError(t, runDisconnect(&buf))

	assert.Contains(t, strings.ToLower(buf.String()), "no forgejo connection")
}

// TestRunStart_ForceBypassesExternalGate verifies --force lets start
// re-enter the setup flow even when an external URL is already in
// config. We don't go all the way through the prompt; reaching the
// admin-user prompt (rather than the early "Nothing to start" return)
// is enough to prove the gate was bypassed.
func TestRunStart_ForceBypassesExternalGate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	configPath := filepath.Join(dir, ".forge", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`
forgejo:
  url: "https://git.example.com"
  token: "abc"
`), 0o644))

	prevForce := startFlagForce
	prevMode := startFlagMode
	t.Cleanup(func() {
		startFlagForce = prevForce
		startFlagMode = prevMode
	})
	startFlagForce = true
	startFlagMode = "existing"

	var buf bytes.Buffer
	// Empty stdin → not a terminal → resolveExistingURL/resolveAdminUser
	// will return an error well after the external-URL gate. That error
	// is expected; the assertion is that the gate was bypassed (we
	// printed the "Reconfiguring" line) instead of "Nothing to start".
	err := runStart(context.Background(), strings.NewReader(""), &buf)
	require.Error(t, err, "expected non-interactive flow to fail before completion")

	out := buf.String()
	assert.Contains(t, out, "Reconfiguring", "force should print reconfigure banner")
	assert.NotContains(t, out, "Nothing to start", "force must skip the external-URL no-op gate")
}
