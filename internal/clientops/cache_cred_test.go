package clientops

import (
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

func TestCacheCred_RoundTripAndMissing(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	if cred, err := ReadCacheCred(); err != nil || cred != nil {
		t.Fatalf("missing cred file: got (%v, %v), want (nil, nil)", cred, err)
	}

	in := &CacheCred{URL: "https://192.0.2.5:7878", Token: "devcode-abc", Pin: "sha256:" + strings.Repeat("a", 64)}
	if err := WriteCacheCred(in); err != nil {
		t.Fatalf("WriteCacheCred: %v", err)
	}
	got, err := ReadCacheCred()
	if err != nil {
		t.Fatalf("ReadCacheCred: %v", err)
	}
	if *got != *in {
		t.Fatalf("round trip mismatch: %+v want %+v", *got, *in)
	}
}

func TestCacheCred_CorruptFileErrors(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCacheCred(); err == nil {
		t.Fatal("corrupt cred must error")
	}
	// Empty fields must also error
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"),
		[]byte(`{"url":"","token":"","pin":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCacheCred(); err == nil {
		t.Fatal("cred missing url/token must error")
	}
}

type tlsSnapshotServer struct{ url, fp string }

// newTLSSnapshotServer spins a TLS httptest server serving /snapshot (any bearer
// accepted) and returns its URL + SPKI pin. Mirrors standUpServeTLS in cli's
// cache_test.go but also returns the fingerprint. Local copy for the moved
// clientops tests (the cobra-driving twin lives in internal/cli/cache_cred_test.go).
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
