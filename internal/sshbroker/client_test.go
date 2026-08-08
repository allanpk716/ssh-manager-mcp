package sshbroker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"testing"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func mustRSAPEM(t *testing.T, passphrase string) ([]byte, ssh.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(k, "", []byte(passphrase))
		// fall back to x509 if MarshalPrivateKeyWithPassphrase unavailable
		if err != nil {
			der, _ := x509.MarshalPKCS8PrivateKey(k)
			block, _ = x509.EncryptPEMBlock(rand.Reader, "ENCRYPTED PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES128)
		}
	} else {
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), pub
}

func TestConnectPrivateKeyPlain(t *testing.T) {
	keyPEM, pub := mustRSAPEM(t, "")
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		AuthorizedKey: pub,
		Exec:          func(string, io.Reader) (string, string, int) { return "ok\n", "", 0 },
	})
	defer cleanup()
	auth, err := PrivateKeyAuth(keyPEM, nil)
	if err != nil {
		t.Fatalf("PrivateKeyAuth: %v", err)
	}
	cli, err := Connect(hostOf(addr), portOf(addr), "u", auth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cli.Close()
}

func TestConnectPrivateKeyEncrypted(t *testing.T) {
	keyPEM, pub := mustRSAPEM(t, "keypass")
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{AuthorizedKey: pub})
	defer cleanup()
	auth, err := PrivateKeyAuth(keyPEM, []byte("keypass"))
	if err != nil {
		t.Fatalf("PrivateKeyAuth: %v", err)
	}
	cli, err := Connect(hostOf(addr), portOf(addr), "u", auth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect encrypted key: %v", err)
	}
	cli.Close()
}

func TestPrivateKeyAuthWrongPassphraseFails(t *testing.T) {
	keyPEM, _ := mustRSAPEM(t, "keypass")
	if _, err := PrivateKeyAuth(keyPEM, []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
}
