package image_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	imgcmd "github.com/p5n-dev/forge/cmd/image"
)

// TestListCommand_Empty exercises `forge image list` with an empty cache,
// using an isolated HOME so we don't read the developer's real cache.
func TestListCommand_Empty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out := &bytes.Buffer{}
	imgcmd.Cmd.SetOut(out)
	imgcmd.Cmd.SetErr(out)
	imgcmd.Cmd.SetArgs([]string{"list"})

	require.NoError(t, imgcmd.Cmd.Execute())
	assert.Contains(t, out.String(), "No images cached")
}

// TestListCommand_Populated checks that a cached image surfaces in the output.
func TestListCommand_Populated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cacheDir := filepath.Join(home, ".forge", "images")
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cacheDir, "forge-base-v0.1.0-arm64.img.gz"),
		make([]byte, 4096), 0o644))

	out := &bytes.Buffer{}
	imgcmd.Cmd.SetOut(out)
	imgcmd.Cmd.SetErr(out)
	imgcmd.Cmd.SetArgs([]string{"list"})

	require.NoError(t, imgcmd.Cmd.Execute())
	assert.Contains(t, out.String(), "v0.1.0")
	assert.Contains(t, out.String(), "VERSION")
}
