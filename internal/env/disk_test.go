package env_test

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/env"
)

func writeGzip(t *testing.T, path string, content []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(content)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func TestPrepareDisk_DecompressesAndExtends(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img.gz")
	dst := filepath.Join(dir, "disk.img")

	original := []byte("forge base image bytes")
	writeGzip(t, src, original)

	wantSize := int64(8 * 1024 * 1024) // 8 MiB
	require.NoError(t, env.PrepareDisk(src, dst, wantSize))

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, wantSize, info.Size(), "destination should be extended to requested size")

	// First bytes should match the decompressed payload.
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, original, got[:len(original)])
}

func TestPrepareDisk_NoShrink(t *testing.T) {
	// If the decompressed image is already larger than requested, leave it.
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img.gz")
	dst := filepath.Join(dir, "disk.img")

	big := make([]byte, 4096)
	writeGzip(t, src, big)

	require.NoError(t, env.PrepareDisk(src, dst, 1024)) // smaller than payload

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, int64(len(big)), info.Size(), "should not shrink existing payload")
}

func TestPrepareDisk_MissingSource(t *testing.T) {
	dir := t.TempDir()
	err := env.PrepareDisk(filepath.Join(dir, "nope.gz"), filepath.Join(dir, "out.img"), 1024)
	require.Error(t, err)
}

func TestPrepareDisk_BadGzip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img.gz")
	dst := filepath.Join(dir, "disk.img")

	require.NoError(t, os.WriteFile(src, []byte("not a gzip"), 0o644))

	err := env.PrepareDisk(src, dst, 1024)
	require.Error(t, err)
}
