package cloudinit_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kdomanski/iso9660"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/cloudinit"
)

func TestWriteISO_FileExists(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")

	err := cloudinit.WriteISO(out, []byte("user-data-content"), []byte("meta-data-content"), []byte("network-config-content"))
	require.NoError(t, err)

	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0), "ISO must not be empty")
}

func TestWriteISO_VolumeLabelAndContents(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")

	want := map[string][]byte{
		"user-data":      []byte("hello user data"),
		"meta-data":      []byte("hello meta data"),
		"network-config": []byte("hello network config"),
	}
	require.NoError(t, cloudinit.WriteISO(out, want["user-data"], want["meta-data"], want["network-config"]))

	f, err := os.Open(out)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	img, err := iso9660.OpenImage(f)
	require.NoError(t, err)

	// cloud-init NoCloud requires the volume label to be exactly "cidata".
	label, err := img.Label()
	require.NoError(t, err)
	assert.Equal(t, "cidata", label)

	root, err := img.RootDir()
	require.NoError(t, err)
	children, err := root.GetChildren()
	require.NoError(t, err)

	got := map[string][]byte{}
	for _, c := range children {
		body, err := io.ReadAll(c.Reader())
		require.NoError(t, err)
		got[c.Name()] = body
	}

	for name, expected := range want {
		body, ok := got[name]
		require.True(t, ok, "ISO missing file %q", name)
		assert.Equal(t, expected, body, "file %q content mismatch", name)
	}
}
