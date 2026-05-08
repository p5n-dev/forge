package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/config"
)

func TestLoadGlobal_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `
forgejo:
  url: "https://git.example.com"
  token: "abc123"
image:
  cache_dir: "/tmp/forge-images"
ssh:
  inject_user_key: true
  user_key_path: "/home/user/.ssh/id_rsa.pub"
`)
	cfg, err := config.LoadGlobal(dir + "/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, "https://git.example.com", cfg.Forgejo.URL)
	assert.Equal(t, "abc123", cfg.Forgejo.Token)
	assert.Equal(t, "/tmp/forge-images", cfg.Image.CacheDir)
	assert.True(t, cfg.SSH.InjectUserKey)
	assert.Equal(t, "/home/user/.ssh/id_rsa.pub", cfg.SSH.UserKeyPath)
}

func TestLoadGlobal_MissingFile(t *testing.T) {
	cfg, err := config.LoadGlobal("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, config.DefaultGlobal(), cfg)
}

func TestLoadGlobal_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", "")
	cfg, err := config.LoadGlobal(dir + "/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, config.DefaultGlobal(), cfg)
}

func TestLoadGlobal_Defaults(t *testing.T) {
	defaults := config.DefaultGlobal()
	assert.Equal(t, "", defaults.Forgejo.URL)
	assert.Equal(t, "", defaults.Forgejo.Token)
	assert.Equal(t, "~/.forge/images", defaults.Image.CacheDir)
	assert.False(t, defaults.SSH.InjectUserKey)
	assert.Equal(t, "~/.ssh/id_ed25519.pub", defaults.SSH.UserKeyPath)
}

func TestLoadGlobal_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", `
forgejo:
  url: "https://git.example.com"
`)
	cfg, err := config.LoadGlobal(dir + "/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, "https://git.example.com", cfg.Forgejo.URL)
	assert.Equal(t, "~/.forge/images", cfg.Image.CacheDir)
	assert.False(t, cfg.SSH.InjectUserKey)
}

func TestSaveGlobal_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"

	original := config.DefaultGlobal()
	original.Forgejo.URL = "https://git.example.com"
	original.Forgejo.Token = "secret"
	original.Forgejo.Port = 3001
	original.Forgejo.AdminUser = "forge"
	original.Forgejo.AdminToken = "generated-token"

	require.NoError(t, config.SaveGlobal(path, original))

	loaded, err := config.LoadGlobal(path)
	require.NoError(t, err)
	assert.Equal(t, "https://git.example.com", loaded.Forgejo.URL)
	assert.Equal(t, "secret", loaded.Forgejo.Token)
	assert.Equal(t, 3001, loaded.Forgejo.Port)
	assert.Equal(t, "forge", loaded.Forgejo.AdminUser)
	assert.Equal(t, "generated-token", loaded.Forgejo.AdminToken)
}

func TestSaveGlobal_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nested/dir/config.yaml"

	cfg := config.DefaultGlobal()
	cfg.Forgejo.AdminUser = "forge"

	require.NoError(t, config.SaveGlobal(path, cfg))

	loaded, err := config.LoadGlobal(path)
	require.NoError(t, err)
	assert.Equal(t, "forge", loaded.Forgejo.AdminUser)
}
