package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// TestCachePull_TokenEmbeddedPin_Succeeds is the Task-8 integration capstone.
//
// WHY THIS TEST EXISTS — the gap not covered by Task 5's three e2e tests:
//   - TestCachePull_PinnedTLS_Succeeds      → env pin (SSHMGR_SERVE_PIN) path
//   - TestCachePull_PinMismatch_Fails       → env pin mismatch
//   - TestCachePull_PlaintextFallback_Warns → no pin, PLAINTEXT server
//
// None of those drive the DEFAULT enrollment flow: `cache-tokens add` emits the
// fingerprint as a token-embedded "<code>:<pin>" (spec §3.3 形态 A). At runtime
// cachePullCmd must split that token so that
//   - the PIN half drives the pinning TLS transport (handshake succeeds), AND
//   - the CODE half goes alone to the Authorization header (handshake auth OK).
//
// `clientops.SplitTokenPin` does this split; it had ONLY logic-level
// coverage (TestResolvePin). If it regressed, the default enroll path would
// 401 silently and no existing test would catch it. This test closes that gap
// end-to-end against a real httptest TLS server with a self-signed cert.
//
// It also documents the migration contract from spec §4.1: a client that has
// the embedded pin connects over TLS on the very first pull — there is no
// "first blind connect" window, which is the security property the design
// claims (§2.2 "比 StrictHostKeyChecking=accept-new 更强").
func TestCachePull_TokenEmbeddedPin_Succeeds(t *testing.T) {
	// Fresh self-signed ed25519 cert — same shape `serve` generates.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	fp := mcpserver.SPKIFingerprint(cert)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	// The server asserts BOTH load-bearing invariants:
	//   - TLS handshake reached us (a plaintext client would never get here)
	//   - Authorization header carries ONLY the bare device code "code-xyz",
	//     never "<code>:<pin>" (SplitTokenPin must have split the token).
	// If the split regressed (whole token sent as Bearer), this 401s and the
	// test fails loudly — exactly the silent-failure mode we're guarding.
	const deviceCode = "code-xyz"
	gotAuth := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		if r.Header.Get("Authorization") != "Bearer "+deviceCode {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	// DEFAULT-enrollment shape: pin travels INSIDE the token as "<code>:<pin>".
	// SSHMGR_SERVE_PIN is intentionally unset to prove the embedded pin alone
	// is enough to engage TLS+pinning (env > flag > embedded priority).
	embeddedToken := deviceCode + ":" + fp
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.URL,
		"SSHMGR_CACHE_TOKEN": embeddedToken,
		"SSHMGR_SERVE_PIN":   "", // ← embedded-pin path under test
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("embedded-pin pull failed: %v (out=%s)", err, out.String())
	}

	// cache.bin landed as an SSHMGRV1 envelope → pull reached the body,
	// meaning TLS handshake PASSED on the embedded pin AND auth was accepted.
	bin := filepath.Join(cacheDir, "cache.bin")
	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("cache.bin not written: %v", err)
	}
	if len(blob) == 0 || string(blob[:8]) != "SSHMGRV1" {
		t.Fatalf("cache.bin not an SSHMGRV1 envelope: %x", blob[:min(8, len(blob))])
	}

	// Belt-and-suspenders: assert the server actually saw the BARE device code,
	// not the whole "<code>:<pin>" token. This is the SplitTokenPin
	// contract — if it ever sends the pin up as part of the Bearer header the
	// server's auth check 401s and the pull fails above; asserting here too
	// makes the failure mode legible in test output.
	select {
	case a := <-gotAuth:
		if a != "Bearer "+deviceCode {
			t.Fatalf("Authorization header = %q, want %q (SplitTokenPin leak?)", a, "Bearer "+deviceCode)
		}
		if strings.Contains(a, fp) {
			t.Fatalf("Authorization header leaked the fingerprint: %q", a)
		}
	case <-time.After(time.Second):
		t.Fatal("server never received the request (TLS handshake likely failed on the embedded pin)")
	}
}
