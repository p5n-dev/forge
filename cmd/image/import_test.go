package image

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTestGzip(t *testing.T, path string, payload []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(payload)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

// runImportFor invokes runImport with a one-shot cobra command bound to a
// fresh writer; the package-level flag vars are reset before each call.
func runImportFor(t *testing.T, args []string, version, sbom string, force bool) string {
	t.Helper()
	importFlagVersion = version
	importFlagSBOM = sbom
	importFlagForce = force
	t.Cleanup(func() {
		importFlagVersion = ""
		importFlagSBOM = ""
		importFlagForce = false
	})

	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	require.NoError(t, runImport(c, args))
	return out.String()
}

func TestImportCmd_HappyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v0.9.9-arm64.img.gz")
	writeTestGzip(t, src, []byte("disk"))

	output := runImportFor(t, []string{src}, "", "", false)

	assert.Contains(t, output, "imported v0.9.9")
	assert.FileExists(t, filepath.Join(home, ".forge", "images", "forge-base-v0.9.9-arm64.img.gz"))
}

func TestImportCmd_VersionFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "weirdname.bin")
	writeTestGzip(t, src, []byte("disk"))

	output := runImportFor(t, []string{src}, "dev", "", false)

	assert.Contains(t, output, "imported dev")
	assert.FileExists(t, filepath.Join(home, ".forge", "images", "forge-base-dev-arm64.img.gz"))
}

func TestImportCmd_PrintsSBOMPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "forge-base-v1-arm64.img.gz")
	writeTestGzip(t, src, []byte("disk"))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "sbom.cdx.json"), []byte("{}"), 0o644))

	output := runImportFor(t, []string{src}, "", "", false)

	assert.Contains(t, output, "imported v1")
	assert.Contains(t, output, "sbom.cdx.json")
}
