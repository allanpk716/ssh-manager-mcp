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
	"runtime"
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

// TestConnectErrorSinglePrefix structurally locks the owner ruling that Connect
// returns redactAddr's error AS-IS (no outer wrap): the rendered text carries
// exactly ONE "ssh dial: " prefix end-to-end — a re-introduced
// fmt.Errorf("ssh dial: %w", …) wrap would double it and fail this test.
//
// End-to-end through a REAL refused dial (port 1, no auth needed — the TCP
// dial fails before any SSH handshake). Two empirical facts pin the inputs
// (probed on this codebase's CI matrix, ubuntu + windows):
//   - host must be "localhost", NOT "127.0.0.1": a host-name dial puts the
//     RESOLVED IP in the error text, which survives step-1 scrubbing and
//     forces the degradation path → one of the FROZEN phrases. With host
//     "127.0.0.1" the host string equals the dialed address, scrubbing
//     succeeds, and the raw OS text (worded differently per platform) passes
//     through — never a frozen phrase.
//   - the frozen phrase differs per OS: linux's syscall.ECONNREFUSED is the
//     real errno 111 in the dial chain, so the ECONNREFUSED phrase fires;
//     windows's syscall.ECONNREFUSED is an invented APPLICATION_ERROR errno
//     (0x20000016) that a real connectex/WSAECONNREFUSED(10061) chain never
//     errors.Is-matches, so classification falls to the default phrase.
//
// Both phrases are frozen literals of degradedText (redact.go), so this pins
// redactAddr's output shape on both CI lanes.
func TestConnectErrorSinglePrefix(t *testing.T) {
	want := "ssh dial: connect failed: connection refused"
	if runtime.GOOS == "windows" {
		want = "ssh dial: connect failed"
	}
	_, err := Connect(context.Background(), "localhost", 1, "u", nil, nil)
	if err == nil {
		t.Fatal("dial localhost:1 must fail")
	}
	if got := err.Error(); got != want {
		t.Fatalf("Connect error text:\n got %q\nwant %q (single \"ssh dial: \" prefix, frozen phrase)", got, want)
	}
}
