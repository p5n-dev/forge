package ssh_test

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/ssh"
)

func TestGenerateKeyPair_FilesAndPermissions(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub := filepath.Join(dir, "id_ed25519.pub")

	require.NoError(t, ssh.GenerateKeyPair(priv, pub, "forge-env-test"))

	privInfo, err := os.Stat(priv)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), privInfo.Mode().Perm(), "private key should be 0600")

	pubInfo, err := os.Stat(pub)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), pubInfo.Mode().Perm(), "public key should be 0644")
}

func TestGenerateKeyPair_PublicKeyParseable(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub := filepath.Join(dir, "id_ed25519.pub")

	require.NoError(t, ssh.GenerateKeyPair(priv, pub, "forge-env-test"))

	pubBytes, err := os.ReadFile(pub)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(pubBytes), "ssh-ed25519 "),
		"public key should start with ssh-ed25519, got: %s", pubBytes)
	assert.Contains(t, string(pubBytes), "forge-env-test", "comment should be embedded")

	// authorized_keys format must parse cleanly via golang.org/x/crypto/ssh.
	parsed, _, _, _, err := xssh.ParseAuthorizedKey(pubBytes)
	require.NoError(t, err)
	assert.Equal(t, "ssh-ed25519", parsed.Type())
}

func TestGenerateKeyPair_PrivateKeyParseable(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub := filepath.Join(dir, "id_ed25519.pub")

	require.NoError(t, ssh.GenerateKeyPair(priv, pub, "forge-env-test"))

	privBytes, err := os.ReadFile(priv)
	require.NoError(t, err)

	signer, err := xssh.ParsePrivateKey(privBytes)
	require.NoError(t, err)
	assert.Equal(t, "ssh-ed25519", signer.PublicKey().Type())
}

func TestGenerateKeyPair_MatchingKeys(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub := filepath.Join(dir, "id_ed25519.pub")

	require.NoError(t, ssh.GenerateKeyPair(priv, pub, "forge-env-test"))

	privBytes, err := os.ReadFile(priv)
	require.NoError(t, err)
	pubBytes, err := os.ReadFile(pub)
	require.NoError(t, err)

	signer, err := xssh.ParsePrivateKey(privBytes)
	require.NoError(t, err)

	parsedPub, _, _, _, err := xssh.ParseAuthorizedKey(pubBytes)
	require.NoError(t, err)

	assert.Equal(t, parsedPub.Marshal(), signer.PublicKey().Marshal(),
		"public key on disk must match private key's public half")
}

func TestGenerateKeyPair_KeyTypeIsEd25519(t *testing.T) {
	// Sanity check that we're not accidentally generating RSA.
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub := filepath.Join(dir, "id_ed25519.pub")

	require.NoError(t, ssh.GenerateKeyPair(priv, pub, "forge-env-test"))

	privBytes, err := os.ReadFile(priv)
	require.NoError(t, err)

	rawKey, err := xssh.ParseRawPrivateKey(privBytes)
	require.NoError(t, err)
	_, ok := rawKey.(*ed25519.PrivateKey)
	assert.True(t, ok, "expected *ed25519.PrivateKey, got %T", rawKey)
}
