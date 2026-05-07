package env

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/config"
)

// chdir temporarily switches CWD for the duration of the current test;
// resolveProject reads os.Getwd() to anchor the upward walk, so tests
// have to drive it through a real working directory.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func writeYAML(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(content), 0o644))
}

const validProjectYAML = `
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2
defaults:
  cpus: 7
`

func TestResolveProject_UsesExistingForgeYAML(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, validProjectYAML)
	chdir(t, dir)

	var out bytes.Buffer
	cfg, _, err := resolveProject(strings.NewReader(""), &out, false, true)
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Defaults.CPUs, "should have read forge.yaml, not defaults")
	assert.Empty(t, out.String(), "no prompt should be shown when forge.yaml exists")
}

func TestResolveProject_NoInitFlagUsesDefaultsSilently(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out bytes.Buffer
	cfg, _, err := resolveProject(strings.NewReader(""), &out, true, true)
	require.NoError(t, err)
	assert.Equal(t, 2, cfg.Defaults.CPUs, "should be embedded defaults")
	assert.Empty(t, out.String(), "no prompt with --no-init")
	_, statErr := os.Stat(filepath.Join(dir, "forge.yaml"))
	assert.True(t, os.IsNotExist(statErr), "should not write forge.yaml when --no-init")
}

func TestResolveProject_NonInteractiveErrorsWithHint(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out bytes.Buffer
	_, _, err := resolveProject(strings.NewReader(""), &out, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no forge.yaml found")
	assert.Contains(t, err.Error(), "forge init")
	assert.Contains(t, err.Error(), "--no-init")
}

func TestResolveProject_PromptYesWritesForgeYAML(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out bytes.Buffer
	cfg, _, err := resolveProject(strings.NewReader("y\n"), &out, false, true)
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Bootstrap.K3s, "should have loaded the freshly-written defaults")

	got, err := os.ReadFile(filepath.Join(dir, "forge.yaml"))
	require.NoError(t, err)
	assert.Equal(t, config.DefaultProjectYAML(), got)

	outStr := out.String()
	assert.Contains(t, outStr, "No forge.yaml found")
	assert.Contains(t, outStr, "Initialize one")
	assert.Contains(t, outStr, "Wrote ")
}

func TestResolveProject_PromptDefaultYesOnEnter(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out bytes.Buffer
	_, _, err := resolveProject(strings.NewReader("\n"), &out, false, true)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "forge.yaml"))
	assert.NoError(t, statErr, "default-yes (empty answer) should still write forge.yaml")
}

func TestResolveProject_PromptNoUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out bytes.Buffer
	cfg, _, err := resolveProject(strings.NewReader("n\n"), &out, false, true)
	require.NoError(t, err)
	assert.Equal(t, 2, cfg.Defaults.CPUs, "should be embedded defaults after declining init")

	_, statErr := os.Stat(filepath.Join(dir, "forge.yaml"))
	assert.True(t, os.IsNotExist(statErr), "should not write forge.yaml when user answers no")
	assert.Contains(t, out.String(), "Continuing with built-in defaults")
}

func TestResolveProject_PromptRetriesOnUnrecognisedInput(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out bytes.Buffer
	_, _, err := resolveProject(strings.NewReader("maybe\nyes\n"), &out, false, true)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "forge.yaml"))
	assert.NoError(t, statErr, "should accept the second answer")
}

func TestPromptYesNo_DefaultBehaviour(t *testing.T) {
	cases := []struct {
		input      string
		defaultYes bool
		want       bool
	}{
		{"\n", true, true},
		{"\n", false, false},
		{"y\n", false, true},
		{"yes\n", false, true},
		{"Y\n", false, true},
		{"YES\n", false, true},
		{"n\n", true, false},
		{"NO\n", true, false},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		got, err := promptYesNo(strings.NewReader(tc.input), &out, "?", tc.defaultYes)
		require.NoError(t, err, "input=%q default=%v", tc.input, tc.defaultYes)
		assert.Equal(t, tc.want, got, "input=%q default=%v", tc.input, tc.defaultYes)
	}
}
