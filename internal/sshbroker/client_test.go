package sshbroker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"testing"
	"time"

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
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", auth, ssh.FixedHostKey(hostKey))
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
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", auth, ssh.FixedHostKey(hostKey))
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

// TestConnectCancelContext proves a cancelled ctx aborts an in-flight Connect
// promptly. ssh.Dial cannot be interrupted, so Connect abandons it and returns
// ctx.Err(); the dial goroutine closes the connection it eventually gets (no
// *ssh.Client leak). We deterministically hold the dial open with a local
// listener that Accepts but NEVER sends the SSH banner — ssh.Dial then blocks on
// the banner wait (no black-hole IP dependency, no OS TCP-timeout minutes).
func TestConnectCancelContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			_ = conn // intentionally do NOT send the SSH banner — hold the dial open
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = Connect(ctx, hostOf(ln.Addr().String()), portOf(ln.Addr().String()), "u", PasswordAuth("pw"), ssh.InsecureIgnoreHostKey())
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Connect took %v on cancel, want < 2s (dial should have been abandoned)", elapsed)
	}
}
