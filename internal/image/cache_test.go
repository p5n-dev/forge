package image_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/image"
)

func TestExpandPath_Tilde(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	got, err := image.ExpandPath("~/.forge/images")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".forge/images"), got)
}

func TestExpandPath_Absolute(t *testing.T) {
	got, err := image.ExpandPath("/tmp/forge-images")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/forge-images", got)
}

func TestExpandPath_Empty(t *testing.T) {
	got, err := image.ExpandPath("")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestListCached_Empty(t *testing.T) {
	dir := t.TempDir()
	got, err := image.ListCached(dir)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListCached_NonexistentDir(t *testing.T) {
	got, err := image.ListCached(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListCached_Populated(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "forge-base-v0.1.0-arm64.img.gz", make([]byte, 128))
	writeBytes(t, dir, "forge-base-v0.2.0-arm64.img.gz", make([]byte, 256))
	// non-image files should be ignored
	writeBytes(t, dir, "SHA256SUMS", []byte("noise"))
	writeBytes(t, dir, "forge-base-v0.1.0-arm64.sbom.cdx.json", []byte("{}"))
	writeBytes(t, dir, "unrelated.txt", []byte("noise"))

	got, err := image.ListCached(dir)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Results are sorted by version, newest first.
	assert.Equal(t, "v0.2.0", got[0].Version)
	assert.Equal(t, int64(256), got[0].Size)
	assert.Equal(t, "v0.1.0", got[1].Version)
	assert.Equal(t, int64(128), got[1].Size)
	assert.NotZero(t, got[0].PulledAt)
}

func TestIsCached(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "forge-base-v0.1.0-arm64.img.gz", []byte("data"))

	assert.True(t, image.IsCached(dir, "v0.1.0"))
	assert.False(t, image.IsCached(dir, "v0.2.0"))
	assert.False(t, image.IsCached(filepath.Join(t.TempDir(), "nope"), "v0.1.0"))
}

func TestCachePath(t *testing.T) {
	got := image.CachePath("/tmp/cache", "v0.1.0")
	assert.Equal(t, "/tmp/cache/forge-base-v0.1.0-arm64.img.gz", got)
}

func writeBytes(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}
