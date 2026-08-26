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

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
)

// TestCacheCred_RoundTripAndMissing and TestCacheCred_CorruptFileErrors moved
// to internal/clientops/cache_cred_test.go with the credential implementation.

type tlsSnapshotServer struct{ url, fp string }

// newTLSSnapshotServer spins a TLS httptest server serving /snapshot (any bearer
// accepted) and returns its URL + SPKI pin. Mirrors standUpServeTLS below but
// also returns the fingerprint. (clientops holds its own copy for the moved
// lazy/reload tests.)
func newTLSSnapshotServer(t *testing.T) *tlsSnapshotServer {
	t.Helper()
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
	cert, _ := x509.ParseCertificate(der)
	fp := mcpserver.SPKIFingerprint(cert)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &tlsSnapshotServer{url: srv.URL, fp: fp}
}

func TestCachePull_PersistsCred_PinPathOnly(t *testing.T) {
	// pin path: reuse the TLS test-server builder to get a server + its SPKI pin.
	srv := newTLSSnapshotServer(t) // helper defined below
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.url,
		"SSHMGR_CACHE_TOKEN": "code-123:" + srv.fp, // embedded-pin form, like enroll prints
		"SSHMGR_SERVE_PIN":   "",
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})
	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("pinned pull: %v", err)
	}
	cred, err := clientops.ReadCacheCred()
	if err != nil || cred == nil {
		t.Fatalf("cred not persisted after pinned pull: %v %v", cred, err)
	}
	if cred.Token != "code-123" {
		t.Fatalf("cred.Token = %q, want bare code %q", cred.Token, "code-123")
	}
	if cred.Pin != srv.fp {
		t.Fatalf("cred.Pin = %q, want resolved pin %q", cred.Pin, srv.fp)
	}

	// plaintext path: fresh dir, --allow-plaintext pull must NOT write cred
	cacheDir2 := t.TempDir()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer plain.Close()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   plain.URL,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   "",
		"SSHMGR_CACHE_DIR":   cacheDir2,
	})
	root2 := NewRootCmd()
	root2.SetArgs([]string{"cache", "pull", "--allow-plaintext"})
	root2.SetOut(&bytes.Buffer{})
	root2.SetErr(&bytes.Buffer{})
	if err := root2.Execute(); err != nil {
		t.Fatalf("plaintext pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir2, "cache.auth.json")); err == nil {
		t.Fatal("plaintext pull must not persist a credential")
	}
}

// TestCachePull_PlaintextMaxOfflineNotPersisted (fix round 1 [Important]):
// `cache pull --allow-plaintext --max-offline 24h` — the pull succeeds, the
// flag was validated, but the cap is NOT persisted (a plaintext pull records
// no server-anchored time; with the cap on, this cache would be unloadable).
// The drop must be AUDIBLE: a WARNING on stderr, and no cache.config.json.
func TestCachePull_PlaintextMaxOfflineNotPersisted(t *testing.T) {
	withDEK(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer plain.Close()
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":         plain.URL,
		"SSHMGR_CACHE_TOKEN":       "code-123",
		"SSHMGR_SERVE_PIN":         "",
		"SSHMGR_CACHE_DIR":         cacheDir,
		"SSHMGR_STORE":             filepath.Join(t.TempDir(), "store.db"),
		"SSHMGR_CACHE_MAX_OFFLINE": "",
	})
	var errBuf bytes.Buffer
	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--allow-plaintext", "--max-offline", "24h"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&errBuf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plaintext pull: %v", err)
	}
	if !strings.Contains(errBuf.String(), "--max-offline not persisted") {
		t.Fatalf("want the not-persisted WARNING on stderr, got %q", errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.config.json")); err == nil {
		t.Fatal("plaintext pull must not persist cache.config.json")
	}
}
