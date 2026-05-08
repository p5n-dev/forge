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
)

func TestStatus_FormatsForgejoReachable(t *testing.T) {
	report := statusReport{
		ForgejoURL:       "http://localhost:3000",
		ForgejoReachable: true,
		ForgejoReason:    "200 OK",
		VfkitInstalled:   true,
		VfkitVersion:     "vfkit version 0.5.0",
		ImageDir:         "/home/u/.forge/images",
		LatestImage:      "v0.1.0",
	}
	var buf bytes.Buffer
	require.NoError(t, renderStatus(&buf, report))

	out := buf.String()
	assert.Contains(t, out, "Forgejo")
	assert.Contains(t, out, "http://localhost:3000")
	assert.Contains(t, out, "vfkit")
	assert.Contains(t, out, "0.5.0")
	assert.Contains(t, out, "v0.1.0")
}

func TestStatus_FormatsForgejoUnreachable(t *testing.T) {
	report := statusReport{
		ForgejoURL:       "http://localhost:3000",
		ForgejoReachable: false,
		ForgejoReason:    "connection refused",
		VfkitInstalled:   false,
		VfkitVersion:     "",
		ImageDir:         "/home/u/.forge/images",
		LatestImage:      "",
	}

	var buf bytes.Buffer
	require.NoError(t, renderStatus(&buf, report))

	out := buf.String()
	assert.Contains(t, out, "connection refused")
	assert.Contains(t, out, "not installed")
	assert.Contains(t, out, "no images cached")
}

func TestLatestImage_FindsImage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "forge-base-v0.1.0-arm64.img.gz"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "forge-base-v0.2.0-arm64.img.gz"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("a"), 0o644))

	got, err := latestImageInDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "v0.2.0", got)
}

func TestLatestImage_NoImages(t *testing.T) {
	dir := t.TempDir()
	got, err := latestImageInDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestLatestImage_MissingDir(t *testing.T) {
	got, err := latestImageInDir("/nonexistent/path/forge")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestVfkitVersion_NotInstalled(t *testing.T) {
	// PATH lookup with a binary name that should not exist
	version, ok := lookupVfkitVersion(context.Background(), "definitely-not-vfkit-zzz")
	assert.False(t, ok)
	assert.Equal(t, "", version)
}

func TestRunStatus_ExternalForgejo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	configPath := filepath.Join(dir, ".forge", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`
forgejo:
  url: "https://git.example.com"
  token: "abc"
`), 0o644))

	var buf bytes.Buffer
	require.NoError(t, runStatus(context.Background(), &buf))
	out := buf.String()
	assert.True(t, strings.Contains(out, "https://git.example.com"), "expected external URL in output: %q", out)
}
