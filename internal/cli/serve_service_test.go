package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestVaultStatusString_CorruptKeyReportsLocked covers the regression that
// motivated Plan 16 T7 review Important finding 1: the old vaultStatusString
// only did os.ReadFile and reported "ok" for ANY master.key file that existed
// on disk — including corrupt / truncated / wrong-length / garbage-byte files
// the running serve would crash-loop on at boot.
//
// This is a NON-gated unit test (no SSHMGR_SERVE_INSTALL env required) so it
// runs in the normal `go test ./...` hermetic loop. It seeds a temp vault dir
// via SSHMGR_FILEKEY_PATH, writes a corrupt master.key, and asserts the probe
// returns a LOCKED signal — not "ok".
func TestVaultStatusString_CorruptKeyReportsLocked(t *testing.T) {
	// Pin the master.key path into a per-test temp dir so we never touch a
	// real vault. restoreFileKeyPathEnv restores the prior env on exit.
	mkPath := filepath.Join(t.TempDir(), "master.key.plain")
	t.Setenv("SSHMGR_FILEKEY_PATH", mkPath)

	cases := []struct {
		name       string
		keyBytes   []byte
		wantSubstr string // must appear in the LOCKED message
		wantNotOK  bool   // true → must NOT be "ok"
	}{
		{
			name:       "missing file",
			keyBytes:   nil, // do not write
			wantSubstr: "not found",
			wantNotOK:  true,
		},
		{
			name:       "zero-byte file",
			keyBytes:   []byte{},
			wantSubstr: "0 bytes",
			wantNotOK:  true,
		},
		{
			name:       "truncated 4-byte file",
			keyBytes:   []byte{0x00, 0x01, 0x02, 0x03},
			wantSubstr: "4 bytes",
			wantNotOK:  true,
		},
		{
			name:       "garbage 31-byte file (one short of 32)",
			keyBytes:   []byte(strings.Repeat("x", 31)),
			wantSubstr: "31 bytes",
			wantNotOK:  true,
		},
		{
			name:       "over-length 64-byte file",
			keyBytes:   []byte(strings.Repeat("y", 64)),
			wantSubstr: "64 bytes",
			wantNotOK:  true,
		},
		{
			name:       "valid 32-byte key (control: must be ok)",
			keyBytes:   make([]byte, 32),
			wantSubstr: "",
			wantNotOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// (Re)create the master.key file per case. Remove first so the
			// missing-file case is actually missing, not stale from a prior subtest.
			if err := os.Remove(mkPath); err != nil && !os.IsNotExist(err) {
				t.Fatalf("setup remove: %v", err)
			}
			if tc.keyBytes != nil {
				if err := os.WriteFile(mkPath, tc.keyBytes, 0o600); err != nil {
					t.Fatalf("setup write: %v", err)
				}
			}

			got := vaultStatusString()
			if tc.wantNotOK {
				if got == "ok" {
					t.Fatalf("vaultStatusString returned %q; want a LOCKED signal containing %q", got, tc.wantSubstr)
				}
				if !strings.Contains(got, "LOCKED") {
					t.Fatalf("vaultStatusString returned %q; want a LOCKED signal", got)
				}
				if tc.wantSubstr != "" && !strings.Contains(got, tc.wantSubstr) {
					t.Fatalf("vaultStatusString returned %q; want it to contain %q", got, tc.wantSubstr)
				}
			} else {
				if got != "ok" {
					t.Fatalf("vaultStatusString returned %q; want \"ok\" for a valid 32-byte key", got)
				}
			}
		})
	}
}

// TestProbeServeHTTPOverTLS covers the Plan 22 T1 production bug: since
// auto-TLS, serve is TLS-ONLY, but probeServeHTTP probed plain http:// — the
// TLS handshake against a plaintext request never succeeds, so `serve status`
// reported http "not responding" forever on a perfectly healthy serve.
//
// The probe contract (mirroring what serve actually serves):
//
//   - https 401 → alive (the auth gate answered — the correct unauthenticated
//     probe result, Plan 10 bearer-token gate),
//   - https 500 → not alive (only 200/401 count),
//   - PLAINTEXT http server → not alive. We no longer accept a plaintext
//     response as an alive signal: serve never speaks plaintext post-auto-TLS,
//     so anything answering plaintext on that port is not our serve.
//
// The target path is pinned too (Plan 42 批1 T1, spec F2): the probe must GET
// /snapshot — the only authenticated route left after the ②a removal (the root
// answers 404 on a real serve, so a root probe would false-negative a healthy
// serve). The handler records the request path and the alive leg asserts it.
//
// The httptest TLS server uses a self-signed cert — which is exactly the
// production shape (auto-TLS self-signed on first start), so this test also
// proves the probe does not fail the TLS handshake on an untrusted cert
// (liveness ≠ identity verification; identity is pinned via the cert
// fingerprint, see `serve cert-info`).
func TestProbeServeHTTPOverTLS(t *testing.T) {
	// One TLS server; the handler's status code flips between the two alive /
	// not-alive cases via an atomic (handler runs on the server goroutine).
	var mode atomic.Int32 // 0 → 401, 1 → 500
	var gotPath atomic.Pointer[string]
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		gotPath.Store(&p)
		if mode.Load() == 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tlsAddr := strings.TrimPrefix(srv.URL, "https://")

	// TLS 401 = alive + auth gate wired.
	mode.Store(0)
	if !probeServeHTTP(tlsAddr) {
		t.Errorf("probeServeHTTP(%q) = false for TLS 401; want true (401 = auth gate responded over https)", tlsAddr)
	}
	if p := gotPath.Load(); p == nil || *p != "/snapshot" {
		t.Errorf("probe target path = %v, want /snapshot (root 404s on a real serve since ②a removal)", p)
	}

	// TLS 500 = not alive per the probe contract (only 200/401 count).
	mode.Store(1)
	if probeServeHTTP(tlsAddr) {
		t.Errorf("probeServeHTTP(%q) = true for TLS 500; want false (only 200/401 are alive signals)", tlsAddr)
	}

	// PLAINTEXT server (even one answering 200!) = not alive: the https probe
	// gets a plaintext response where it expects a TLS handshake — connection
	// error → false. Post-auto-TLS serve never speaks plaintext, so plaintext
	// is not (and must not become) an alive signal.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()
	plainAddr := strings.TrimPrefix(plain.URL, "http://")
	if probeServeHTTP(plainAddr) {
		t.Errorf("probeServeHTTP(%q) = true for PLAINTEXT 200; want false (serve is TLS-only; plaintext is not an alive signal)", plainAddr)
	}
}
