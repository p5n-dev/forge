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

// TestRunStart_ExternalForgejo_NoOp verifies that `forge system start` is a
// no-op (no docker invocation) when an external Forgejo URL is configured.
func TestRunStart_ExternalForgejo_NoOp(t *testing.T) {
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
	require.NoError(t, runStart(context.Background(), strings.NewReader(""), &buf))

	out := buf.String()
	assert.True(t, strings.Contains(out, "https://git.example.com"), "expected message to mention external URL: %q", out)
	assert.True(t, strings.Contains(strings.ToLower(out), "external"), "expected message to call out external Forgejo: %q", out)
}

// TestRunStop_ExternalForgejo_NoOp verifies that `forge system stop` is a
// no-op (no docker invocation) when an external Forgejo URL is configured.
func TestRunStop_ExternalForgejo_NoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	configPath := filepath.Join(dir, ".forge", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`
forgejo:
  url: "https://git.example.com"
`), 0o644))

	var buf bytes.Buffer
	require.NoError(t, runStop(context.Background(), &buf))

	out := buf.String()
	assert.True(t, strings.Contains(out, "https://git.example.com"), "expected message to mention external URL: %q", out)
}
