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
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// newPinnedTLSServer spins a TLS httptest server with a fresh self-signed
// ed25519 cert and returns its URL plus the SPKI pin a client must use
// (same construction as internal/cli's pinned-pull tests).
func newPinnedTLSServer(t *testing.T, h http.HandlerFunc) (string, string) {
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, mcpserver.SPKIFingerprint(cert)
}

// snapshotHandler serves a valid /snapshot body with a controllable Date.
// date==nil → suppress the Date header entirely; date!="" → emit verbatim.
func snapshotHandler(date *string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch {
		case date == nil:
			w.Header()["Date"] = nil // suppress auto-Date
		case *date != "":
			w.Header().Set("Date", *date)
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}
}

func TestCacheMaxOffline_Parsing(t *testing.T) {
	cases := []struct {
		env     string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		{env: "", set: false, want: 0},
		{env: "0", set: true, want: 0},
		{env: "0h", set: true, want: 0},
		{env: "168h", set: true, want: 168 * time.Hour},
		{env: "1h", set: true, want: time.Hour},
		{env: "abc", set: true, wantErr: true},
		{env: "-1h", set: true, wantErr: true},
		{env: "30m", set: true, wantErr: true},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", tc.env)
		} else {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
		}
		got, err := cacheMaxOffline()
		if tc.wantErr {
			if err == nil {
				t.Fatalf("env %q: want error, got %v", tc.env, got)
			}
			want := fmt.Sprintf("invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", tc.env)
			if err.Error() != want {
				t.Fatalf("env %q: error text mismatch:\n got %q\nwant %q", tc.env, err.Error(), want)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("env %q: got (%v, %v), want (%v, nil)", tc.env, got, err, tc.want)
		}
	}
}

func TestDoPull_InvalidEnv_RefusedBeforeHTTP(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "30m")
	hits := &atomic.Int32{}
	url, pin := newPinnedTLSServer(t, snapshotHandler(nil, hits))
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	err := DoPull(url, "code", pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "invalid SSHMGR_CACHE_MAX_OFFLINE") {
		t.Fatalf("want invalid-env refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("request must not be sent, got %d hits", hits.Load())
	}
}

func TestDoPull_PlaintextRefusedWhenMaxOffline(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	hits := &atomic.Int32{}
	url, _ := newPinnedTLSServer(t, snapshotHandler(nil, hits))
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	err := DoPull(url, "code", "", PullOpts{AllowPlain: true})
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext pull") {
		t.Fatalf("want plaintext refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("no HTTP expected, got %d hits", hits.Load())
	}
}

func TestDoPull_NoRedirect_Global(t *testing.T) {
	for _, b := range []string{"", "24h"} { // B off and B on — the ban is global
		t.Run("B="+b, func(t *testing.T) {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", b)
			var plainHits atomic.Int32
			plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				plainHits.Add(1)
				fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
			}))
			defer plain.Close()
			url, pin := newPinnedTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, plain.URL, http.StatusFound)
			})
			dir := t.TempDir()
			withDEK(t)
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
			err := DoPull(url, "code", pin, PullOpts{})
			if err == nil || !strings.Contains(err.Error(), "redirects are not followed") {
				t.Fatalf("want redirect refusal, got %v", err)
			}
			if plainHits.Load() != 0 {
				t.Fatalf("plaintext hop must not be reached, got %d hits", plainHits.Load())
			}
			if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
				t.Fatal("cache.bin must not be written from a redirect")
			}
		})
	}
}

func TestDoPull_DateTwoBadShapes_Refused(t *testing.T) {
	cases := []struct {
		name string
		date *string
	}{
		{name: "missing", date: nil},
		{name: "malformed", date: ptr("not-a-date")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
			url, pin := newPinnedTLSServer(t, snapshotHandler(tc.date, nil))
			dir := t.TempDir()
			withDEK(t)
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
			err := DoPull(url, "code", pin, PullOpts{})
			if err == nil || !strings.Contains(err.Error(), "no valid Date header") {
				t.Fatalf("want Date refusal, got %v", err)
			}
			if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
				t.Fatal("cache.bin must not be written without an anchor")
			}
		})
	}
	// Plan 40 §1 (P0): post fact/policy split, a PINNED pull is Date-gated
	// with OR without the env — the anchor exists on every pinned pull, so it
	// must be a valid one. (B-off tolerance was the old policy-gated behavior.)
	t.Run("B-off-pinned-refuses-too", func(t *testing.T) {
		t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
		url, pin := newPinnedTLSServer(t, snapshotHandler(nil, nil))
		dir := t.TempDir()
		withDEK(t)
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
		err := DoPull(url, "code", pin, PullOpts{})
		if err == nil || !strings.Contains(err.Error(), "no valid Date header") {
			t.Fatalf("want Date refusal without env (P0), got %v", err)
		}
		if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
			t.Fatal("cache.bin must not be written without an anchor")
		}
	})
}

func TestDoPull_Skew_Refused(t *testing.T) {
	skewed := time.Now().Add(2 * time.Hour).Format(http.TimeFormat)
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(skewed), nil))
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	err := DoPull(url, "code", pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "server clock skew too large") {
		t.Fatalf("want skew refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), time.Now().Format(time.RFC3339)[:11]) {
		t.Fatalf("error must carry RFC3339 stamps: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("cache.bin must not be written on skew")
	}
}

func TestDoPull_AnchorWritten_BothStates(t *testing.T) {
	serverNow := time.Now().Add(5 * time.Minute).UTC() // within 1h skew
	date := serverNow.Format(http.TimeFormat)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	// B on: PulledAt = server Date, ServerAnchored=true.
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("anchored pull: %v", err)
	}
	m := readMetaForTest(t, dir)
	if m.PulledAt != serverNow.Unix() {
		t.Fatalf("B-on PulledAt = %d, want server Date %d", m.PulledAt, serverNow.Unix())
	}
	if !m.ServerAnchored {
		t.Fatal("B-on must set ServerAnchored")
	}

	// B off, PINNED (Plan 40 §1 P0): the anchor no longer depends on the
	// pulling process's policy env — same server-clock anchor as B-on.
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	url2, pin2 := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	if err := DoPull(url2, "code", pin2, PullOpts{}); err != nil {
		t.Fatalf("B-off pinned pull: %v", err)
	}
	m = readMetaForTest(t, dir)
	if m.PulledAt != serverNow.Unix() {
		t.Fatalf("B-off pinned PulledAt = %d, want server Date %d", m.PulledAt, serverNow.Unix())
	}
	if !m.ServerAnchored {
		t.Fatal("B-off pinned must set ServerAnchored (P0: fact, not policy)")
	}
}

func ptr(s string) *string { return &s }

// readMetaForTest reads+decodes cache.meta.json from a cache dir.
func readMetaForTest(t *testing.T, dir string) cacheMeta {
	t.Helper()
	m, err := readCacheMeta(filepath.Join(dir, "cache.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
