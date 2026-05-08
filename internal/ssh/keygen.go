// Package ssh generates per-environment SSH key material.
package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"

	xssh "golang.org/x/crypto/ssh"
)

// GenerateKeyPair creates an ed25519 SSH key pair, writing the OpenSSH-format
// private key to privPath (mode 0600) and the authorized_keys-format public
// key to pubPath (mode 0644). The comment is embedded in both files for
// identification.
func GenerateKeyPair(privPath, pubPath, comment string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating ed25519 key: %w", err)
	}

	privBlock, err := xssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return fmt.Errorf("marshalling private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(privBlock)

	sshPub, err := xssh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("wrapping public key: %w", err)
	}
	pubLine := xssh.MarshalAuthorizedKey(sshPub)
	// MarshalAuthorizedKey returns "<algo> <key>\n"; insert the comment so
	// it lands in authorized_keys as "<algo> <key> <comment>\n".
	pubLine = appendComment(pubLine, comment)

	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}
	if err := os.WriteFile(pubPath, pubLine, 0o644); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}
	return nil
}

func appendComment(pubLine []byte, comment string) []byte {
	if comment == "" {
		return pubLine
	}
	// pubLine ends with '\n'; replace it with " <comment>\n".
	if len(pubLine) > 0 && pubLine[len(pubLine)-1] == '\n' {
		pubLine = pubLine[:len(pubLine)-1]
	}
	out := make([]byte, 0, len(pubLine)+len(comment)+2)
	out = append(out, pubLine...)
	out = append(out, ' ')
	out = append(out, comment...)
	out = append(out, '\n')
	return out
}
