package image_test

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/image"
)

func makeGzip(t *testing.T, path string, payload []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(payload)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func TestImport_VersionFromFilename(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v0.1.0-arm64.img.gz")
	makeGzip(t, src, []byte("disk bytes"))

	res, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.NoError(t, err)
	assert.Equal(t, "v0.1.0", res.Version)

	dest := filepath.Join(cacheDir, "forge-base-v0.1.0-arm64.img.gz")
	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0))
	assert.Equal(t, dest, res.ImagePath)
}

func TestImport_ExplicitVersion(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "anything.img.gz")
	makeGzip(t, src, []byte("disk"))

	res, err := image.Import(src, image.ImportOptions{
		CacheDir: cacheDir,
		Version:  "dev",
	})
	require.NoError(t, err)
	assert.Equal(t, "dev", res.Version)
	assert.FileExists(t, filepath.Join(cacheDir, "forge-base-dev-arm64.img.gz"))
}

func TestImport_MissingSource(t *testing.T) {
	cacheDir := t.TempDir()
	_, err := image.Import("/nope/image.img.gz", image.ImportOptions{
		CacheDir: cacheDir,
		Version:  "dev",
	})
	require.Error(t, err)
}

func TestImport_NotGzip(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v1-arm64.img.gz")
	require.NoError(t, os.WriteFile(src, []byte("not gzip"), 0o644))

	_, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gzip")
}

func TestImport_VersionRequiredWhenNotInferable(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "weirdname.bin")
	makeGzip(t, src, []byte("disk"))

	_, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

func TestImport_AlreadyCached_ErrorsByDefault(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v1-arm64.img.gz")
	makeGzip(t, src, []byte("first"))

	_, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.NoError(t, err)

	makeGzip(t, src, []byte("second"))
	_, err = image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already")
}

func TestImport_AlreadyCached_ForceOverwrites(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v1-arm64.img.gz")
	makeGzip(t, src, []byte("first"))

	_, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.NoError(t, err)

	makeGzip(t, src, []byte("second-overwritten"))
	res, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir, Force: true})
	require.NoError(t, err)
	assert.Equal(t, "v1", res.Version)
}

func TestImport_VersionedSBOMSibling(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v0.1.0-arm64.img.gz")
	makeGzip(t, src, []byte("disk"))

	sbomSrc := filepath.Join(srcDir, "forge-base-v0.1.0-arm64.sbom.cdx.json")
	require.NoError(t, os.WriteFile(sbomSrc, []byte(`{"sbom":1}`), 0o644))

	res, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(cacheDir, "forge-base-v0.1.0-arm64.sbom.cdx.json"), res.SBOMPath)
	got, err := os.ReadFile(res.SBOMPath)
	require.NoError(t, err)
	assert.Equal(t, `{"sbom":1}`, string(got))
}

func TestImport_UnversionedSBOMSibling(t *testing.T) {
	// build.sh writes a generic sbom.cdx.json next to the image. The import
	// must rename it to the versioned form so multiple cached images don't clash.
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v0.1.0-arm64.img.gz")
	makeGzip(t, src, []byte("disk"))

	sbomSrc := filepath.Join(srcDir, "sbom.cdx.json")
	require.NoError(t, os.WriteFile(sbomSrc, []byte(`{"sbom":2}`), 0o644))

	res, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(cacheDir, "forge-base-v0.1.0-arm64.sbom.cdx.json"), res.SBOMPath)
}

func TestImport_NoSBOMSibling(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v0.1.0-arm64.img.gz")
	makeGzip(t, src, []byte("disk"))

	res, err := image.Import(src, image.ImportOptions{CacheDir: cacheDir})
	require.NoError(t, err)
	assert.Empty(t, res.SBOMPath, "no SBOM expected when none alongside source")
}

func TestImport_ExplicitSBOMPath(t *testing.T) {
	srcDir := t.TempDir()
	cacheDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v0.1.0-arm64.img.gz")
	makeGzip(t, src, []byte("disk"))

	otherDir := t.TempDir()
	customSBOM := filepath.Join(otherDir, "my-sbom.json")
	require.NoError(t, os.WriteFile(customSBOM, []byte(`{"sbom":3}`), 0o644))

	res, err := image.Import(src, image.ImportOptions{
		CacheDir: cacheDir,
		SBOMPath: customSBOM,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SBOMPath)
	got, err := os.ReadFile(res.SBOMPath)
	require.NoError(t, err)
	assert.Equal(t, `{"sbom":3}`, string(got))
}
