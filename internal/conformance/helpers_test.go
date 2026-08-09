package conformance

import (
	"os"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// mustPrivAuth reads a private key file and returns an ssh.AuthMethod via the broker.
// Shared across conformance tests (Tasks 3/4 reuse it).
func mustPrivAuth(t *testing.T, privPath, passphrase string) ssh.AuthMethod {
	t.Helper()
	keyPEM, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := sshbroker.PrivateKeyAuth(keyPEM, []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	return auth
}
