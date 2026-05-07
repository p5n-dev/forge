package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/config"
)

func TestInit_WritesDefaultYAML(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	require.NoError(t, runInit(&out, dir, false))

	target := filepath.Join(dir, "forge.yaml")
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, config.DefaultProjectYAML(), got)

	// The file we just wrote must parse cleanly through the loader.
	cfg, err := config.LoadProject(target)
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Bootstrap.K3s)

	assert.Contains(t, out.String(), "Wrote")
}

func TestInit_RefusesWhenExists(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(target, []byte("existing: true\n"), 0o644))

	err := runInit(&bytes.Buffer{}, dir, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Original content is preserved.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "existing: true\n", string(got))
}

func TestInit_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(target, []byte("existing: true\n"), 0o644))

	require.NoError(t, runInit(&bytes.Buffer{}, dir, true))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, config.DefaultProjectYAML(), got)
}

func TestInit_ErrorsWhenDirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := runInit(&bytes.Buffer{}, missing, false)
	require.Error(t, err)
}

func TestInit_ErrorsWhenTargetIsFile(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o644))

	err := runInit(&bytes.Buffer{}, notADir, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// --- copyRageDir ---

func TestCopyRageDir_NoSrcIsHarmless(t *testing.T) {
	var buf bytes.Buffer
	src := filepath.Join(t.TempDir(), "rage")
	dst := filepath.Join(t.TempDir(), "rage-dst")

	require.NoError(t, copyRageDir(&buf, src, dst, false))

	_, err := os.Stat(dst)
	assert.True(t, os.IsNotExist(err), "dst must not be created when src is missing")
	assert.Contains(t, buf.String(), "No rage/")
}

func TestCopyRageDir_FreshCopyPreservesPerms(t *testing.T) {
	src := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(src, 0o755))

	bin := filepath.Join(src, "rage-aarch64-linux")
	require.NoError(t, os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F'}, 0o755))

	cfg := filepath.Join(src, "rage.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("# rage\n"), 0o644))

	dst := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, copyRageDir(&bytes.Buffer{}, src, dst, false))

	binStat, err := os.Stat(filepath.Join(dst, "rage-aarch64-linux"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), binStat.Mode().Perm(),
		"binary must keep its +x bit so the guest can exec it")

	cfgStat, err := os.Stat(filepath.Join(dst, "rage.toml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), cfgStat.Mode().Perm())

	cfgBytes, err := os.ReadFile(filepath.Join(dst, "rage.toml"))
	require.NoError(t, err)
	assert.Equal(t, "# rage\n", string(cfgBytes))
}

func TestCopyRageDir_PreservesPlatformBinaryName(t *testing.T) {
	// The rage release binaries are named `rage-<arch>-<os>` (e.g.
	// rage-aarch64-linux). We must NOT rename them on copy — the
	// downstream consumer (env create) is responsible for picking
	// the right one based on the guest architecture.
	src := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(src, 0o755))
	for _, name := range []string{"rage-x86_64-linux", "rage-aarch64-linux", "rage.toml"} {
		require.NoError(t, os.WriteFile(filepath.Join(src, name), []byte(name), 0o644))
	}

	dst := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, copyRageDir(&bytes.Buffer{}, src, dst, false))

	for _, name := range []string{"rage-x86_64-linux", "rage-aarch64-linux", "rage.toml"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		require.NoError(t, err, "expected %s to be copied verbatim", name)
		assert.Equal(t, name, string(got))
	}
}

func TestCopyRageDir_ExistingDstWithoutForce_Warns(t *testing.T) {
	src := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "rage-aarch64-linux"), []byte("new"), 0o755))

	dst := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dst, "rage-aarch64-linux"), []byte("OLD"), 0o755))

	var buf bytes.Buffer
	require.NoError(t, copyRageDir(&buf, src, dst, false))

	contents, err := os.ReadFile(filepath.Join(dst, "rage-aarch64-linux"))
	require.NoError(t, err)
	assert.Equal(t, "OLD", string(contents),
		"without --force, an existing dst must be left untouched")
	assert.Contains(t, buf.String(), "--force")
}

func TestCopyRageDir_ForceReplacesEntireDst(t *testing.T) {
	src := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "rage-aarch64-linux"), []byte("new"), 0o755))

	dst := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dst, "stale-file"), []byte("stale"), 0o644))

	require.NoError(t, copyRageDir(&bytes.Buffer{}, src, dst, true))

	// stale-file is gone (rm -rf semantics, not merge)
	_, err := os.Stat(filepath.Join(dst, "stale-file"))
	assert.True(t, os.IsNotExist(err),
		"--force must rm -rf the existing dst before the copy, not merge into it")

	contents, err := os.ReadFile(filepath.Join(dst, "rage-aarch64-linux"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(contents))
}

func TestCopyRageDir_HandlesNestedDirs(t *testing.T) {
	src := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "subdir", "nested"), []byte("hi"), 0o644))

	dst := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, copyRageDir(&bytes.Buffer{}, src, dst, false))

	contents, err := os.ReadFile(filepath.Join(dst, "subdir", "nested"))
	require.NoError(t, err)
	assert.Equal(t, "hi", string(contents))
}

func TestCopyRageDir_RejectsRegularFileAsSrc(t *testing.T) {
	// If the user accidentally points us at a file (not a directory),
	// fail loudly rather than silently doing nothing.
	srcFile := filepath.Join(t.TempDir(), "rage")
	require.NoError(t, os.WriteFile(srcFile, []byte("not a dir"), 0o644))

	dst := filepath.Join(t.TempDir(), "rage")
	err := copyRageDir(&bytes.Buffer{}, srcFile, dst, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}
