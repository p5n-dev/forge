package env

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initLocalBareRepo creates an empty bare repo at path with default
// branch main, suitable as a push target. Returns a file:// URL so the
// production seedWorkspaceFromProject can exercise its real `git push
// <url>` path without needing a Forgejo container running.
func initLocalBareRepo(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", path).CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", string(out))
	return "file://" + path
}

func TestSeedWorkspaceFromProject_CopiesAndPushes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	root := t.TempDir()

	// Source project root with a .pre-commit-config.yaml.
	projectRoot := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(projectRoot, 0o755))
	preCommit := []byte("repos:\n  - repo: example\n    rev: abc\n    hooks:\n      - id: trailing-whitespace\n")
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, ".pre-commit-config.yaml"), preCommit, 0o644))

	// Bare repo + workspace clone — mirrors what the create flow gives
	// seedWorkspaceFromProject after gitCloneWithToken finishes.
	bare := filepath.Join(root, "bare.git")
	cloneURL := initLocalBareRepo(t, bare)

	workspace := filepath.Join(root, "workspace")
	out, err := exec.Command("git", "clone", cloneURL, workspace).CombinedOutput()
	require.NoError(t, err, "git clone: %s", string(out))

	err = seedWorkspaceFromProject(context.Background(), projectRoot, workspace, cloneURL, "", "")
	require.NoError(t, err)

	// File landed in the workspace clone.
	got, err := os.ReadFile(filepath.Join(workspace, ".pre-commit-config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, preCommit, got)

	// The push made the bare repo's main branch real — clone it
	// fresh and verify the seed file is present.
	verify := filepath.Join(root, "verify")
	out, err = exec.Command("git", "clone", cloneURL, verify).CombinedOutput()
	require.NoError(t, err, "verify clone: %s", string(out))
	got, err = os.ReadFile(filepath.Join(verify, ".pre-commit-config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, preCommit, got)
}

func TestSeedWorkspaceFromProject_NoOpWithoutSeedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(projectRoot, 0o755))
	// No .pre-commit-config.yaml in projectRoot — the function must
	// be a clean no-op (no spurious commit, no push).

	bare := filepath.Join(root, "bare.git")
	cloneURL := initLocalBareRepo(t, bare)
	workspace := filepath.Join(root, "workspace")
	out, err := exec.Command("git", "clone", cloneURL, workspace).CombinedOutput()
	require.NoError(t, err, "git clone: %s", string(out))

	require.NoError(t, seedWorkspaceFromProject(context.Background(), projectRoot, workspace, cloneURL, "", ""))

	// No commits made — `git log` returns non-zero on an unborn HEAD.
	cmd := exec.Command("git", "-C", workspace, "rev-parse", "HEAD")
	cmd.Stderr = nil
	if err := cmd.Run(); err == nil {
		t.Fatal("expected no commits in workspace after no-op seed, but HEAD resolves")
	}
}
