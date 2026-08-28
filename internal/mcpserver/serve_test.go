package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// newTestStore + seedActiveProjectToken follow the existing pattern in
// internal/store/projects_test.go — read it for the exact seeding signature.
// Contract: seedActiveProjectToken creates an active project bound to a profile
// and returns the plaintext token + that project's ID + profile ID.
func TestServeRunner_CachesByProject(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()

	token, projID, profileID := seedActiveProjectToken(t, st, "project-A")
	project, err := st.VerifyToken(token)
	if err != nil || project == nil {
		t.Fatalf("VerifyToken: err=%v project=%v (contract: active project resolves)", err, project)
	}
	if project.ID != projID || project.ProfileID != profileID {
		t.Fatalf("resolved project mismatch: got id=%s profile=%s", project.ID, project.ProfileID)
	}

	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()

	s1, err := r.ServerForProject(project)
	if err != nil || s1 == nil {
		t.Fatalf("ServerForProject: err=%v srv=%v", err, s1)
	}
	s2, err := r.ServerForProject(project)
	if err != nil {
		t.Fatalf("second ServerForProject: %v", err)
	}
	if s1 != s2 {
		t.Fatal("cache miss: same project must yield the same *mcp.Server pointer")
	}
}

// newTestStore returns an open encrypted store in a temp dir. The mcpserver
// package already has newStore (core_test.go) with this exact shape; this is a
// thin alias so the brief's test code reads as written. Test-only.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	return newStore(t)
}

// seedActiveProjectToken mirrors internal/store/projects_test.go: create an
// active project named `name` bound to a fresh profile and return (token,
// projectID, profileID). AddProject's default status is active, so the token
// verifies. Test-only.
func seedActiveProjectToken(t *testing.T, st *store.Store, name string) (token, projectID, profileID string) {
	t.Helper()
	pid, err := st.AddProfile(name + "-profile")
	if err != nil {
		t.Fatalf("AddProfile: %v", err)
	}
	projID, tok, err := st.AddProject(name, pid)
	if err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if tok == "" || projID == "" {
		t.Fatalf("AddProject returned empty id or token: projID=%q token=%q", projID, tok)
	}
	return tok, projID, pid
}

func TestRunServe_ShutdownOnCancel(t *testing.T) {
	// Hermetic auto-TLS: empty cert/key forces the auto-TLS path, which
	// resolves paths.ServeCertPath → the program-fixed vault dir (/var/lib/...)
	// when SSHMGR_SERVE_CERT is unset — a non-root linux runner cannot mkdir
	// there (CI first-run red, run 31978817807). Same seam as
	// TestRunServe_AutoTLSCreatesCert; the init marker follows the cert dir
	// automatically (paths.ServeCertMarkerPath).
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(dir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(dir, "serve-key.pem"))

	st := newTestStore(t)
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunServe(ctx, st, "127.0.0.1:0", "", "") }()

	cancel() // RunServe must exit cleanly regardless of timing
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe returned %v; want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunServe did not shut down within 2s")
	}
}

// TestRunServe_HeartbeatWritesLog verifies that serve periodically writes a
// heartbeat to its log sink (stderr, captured by each platform's service
// manager — Windows EventLog / systemd journald / launchd syslog). Plan 16 T7
// dropped the serve.log marker-scan (vaultUnlockedFromLog was Windows-only);
// the heartbeat is now a liveness marker for operators inspecting logs.
// Heartbeat is a runtime behavior; this test is skipped in favor of CI
// integration verification (hard to test a real serve's log writes in a unit
// test without starting a full serve process).
func TestRunServe_HeartbeatWritesLog(t *testing.T) {
	t.Skip("heartbeat verification in T8 CI integration test (unit test cannot drive a real serve's stderr)")
}

// TestRunServe_AutoTLSCreatesCert proves RunServe forces TLS even when no
// explicit cert/key are supplied: with tlsCert="" + tlsKey="", RunServe must
// auto-generate a self-signed cert at SSHMGR_SERVE_CERT/SSHMGR_SERVE_KEY (via
// LoadOrCreateServeCert) and ServeTLS with it. The cert+key files must appear
// on disk. It must NEVER silently downgrade to plaintext.
//
// The fingerprint returned by LoadOrCreateServeCert is covered by cert_test.go
// (TestLoadOrCreateServeCert); here we assert only the serve-integration
// contract: RunServe with empty tlsCert materializes cert files.
func TestRunServe_AutoTLSCreatesCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "serve-cert.pem")
	keyPath := filepath.Join(dir, "serve-key.pem")
	t.Setenv("SSHMGR_SERVE_CERT", certPath)
	t.Setenv("SSHMGR_SERVE_KEY", keyPath)

	st := newTestStore(t)
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run in goroutine; cancel after a moment to let bind + cert-gen run.
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunServe(ctx, st, "127.0.0.1:0", "", "") // empty cert/key → auto-TLS path
	}()

	// Give it a moment to bind + generate, then cancel.
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("RunServe did not return after cancel")
	}

	// Cert files MUST exist — this is the load-bearing assertion: RunServe
	// generated them via LoadOrCreateServeCert instead of falling back to plaintext.
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("cert not auto-generated at %s: %v", certPath, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key not auto-generated at %s: %v", keyPath, err)
	}
}

// ---- Plan 34 T2: /snapshot 401 reason (spec rev4 §1) ----

// TestSnapshot401Reason pins the Plan 34 rev4 §1 observability contract: a
// revoked device code 401s with "invalid cache token: revoked" (serve stderr
// logs the device name), an unknown code with "invalid cache token: unknown".
// The client NEVER branches on the reason — this test exists so the
// owner-facing signal cannot silently regress.
//
// Step-1实测 (SDK v1.2.0 auth.go:99-107): RequireBearerToken maps an
// ErrInvalidToken-wrapped verifier error to http.Error(w, err.Error(), 401) —
// the 401 body DOES carry the verifier's error text, so the reason is asserted
// at the HTTP layer (no degradation to unit-layer-only text checks needed).
// The stderr log lines are verified by code review, not captured here
// (swapping os.Stderr in an integration test is racy for marginal value).
func TestSnapshot401Reason(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	profID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	// NB: AddCacheToken returns (id, plaintextToken, err) — bind the SECOND
	// value (the brief's snippet bound the first, sending the ID as the bearer
	// code, which misclassifies as "unknown"; caught in Step-2 red analysis).
	_, tok, err := st.AddCacheToken("laptop", profID)
	if err != nil {
		t.Fatalf("AddCacheToken: %v", err)
	}
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()
	ts := httptest.NewServer(r.HTTPHandler())
	defer ts.Close()

	get := func(auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/snapshot", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("Do: %v", derr)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(tok); code != http.StatusUnauthorized || !strings.Contains(body, "invalid cache token: revoked") {
		t.Fatalf("revoked token: status=%d body=%q, want 401 with %q", code, body, "invalid cache token: revoked")
	}
	if code, body := get("definitely-not-a-real-code-123456"); code != http.StatusUnauthorized || !strings.Contains(body, "invalid cache token: unknown") {
		t.Fatalf("unknown token: status=%d body=%q, want 401 with %q", code, body, "invalid cache token: unknown")
	}
}

// TestVerifyCacheTokenReason (Plan 34 T5 watch item ①): unit-layer pin of the
// reason classification branch. The stderr line and the error text are minted
// in the SAME branch of verifyCacheToken, so asserting the error text pins the
// stderr reason too — no need to capture os.Stderr (same rationale as the
// comment above TestSnapshot401Reason). Direct call, no HTTP stack.
func TestVerifyCacheTokenReason(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	profID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	// AddCacheToken returns (id, plaintextToken, err) — bind the SECOND value.
	_, tok, err := st.AddCacheToken("laptop", profID)
	if err != nil {
		t.Fatalf("AddCacheToken: %v", err)
	}
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()

	// Revoked: the 8-char prefix lookup hits the revoked cache-token row.
	if _, err := r.verifyCacheToken(context.Background(), tok, nil); err == nil ||
		!strings.Contains(err.Error(), "invalid cache token: revoked") {
		t.Fatalf("revoked: err = %v, want %q", err, "invalid cache token: revoked")
	}
	// Unknown: no revoked-row prefix hit.
	if _, err := r.verifyCacheToken(context.Background(), "definitely-not-a-real-code-123456", nil); err == nil ||
		!strings.Contains(err.Error(), "invalid cache token: unknown") {
		t.Fatalf("unknown: err = %v, want %q", err, "invalid cache token: unknown")
	}
}

// ---- Plan 42 批1 T1: ②a 移除契约 (spec §3.1-1) ----

// TestServe_MCPOverHTTPRemoved pins the ②a removal contract: the MCP-over-HTTP
// route is GONE — every path except /snapshot answers 404, and the 404 comes
// BEFORE any auth verdict (a valid project token included: a project token is
// no longer a REMOTE MCP credential at all; it survives only as the client-side
// spawn gate validated by `mcp --cache` against the snapshot's projects).
//
// Drives the live server (newSnapshotRunner's httptest.Server) so the real
// mux — not just a handler value — is exercised.
func TestServe_MCPOverHTTPRemoved(t *testing.T) {
	srv, _, projTok, _ := newSnapshotRunner(t) // projTok 仍由 helper 铸出

	do := func(method, path string) int {
		t.Helper()
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{}`)
		}
		req, err := http.NewRequest(method, srv.URL+path, body)
		if err != nil {
			t.Fatalf("NewRequest %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+projTok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do %s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	// Root: the historical MCP streamable endpoint → 404, even WITH a valid token.
	if code := do(http.MethodPost, "/"); code != http.StatusNotFound {
		t.Fatalf("root = %d, want 404", code)
	}
	// Legacy/alternate MCP paths + an arbitrary path: all 404.
	for _, p := range []string{"/mcp", "/messages", "/anything"} {
		if code := do(http.MethodGet, p); code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", p, code)
		}
	}
}

// TestServe_SnapshotGateUnchanged is the existing-behavior anchor across the
// ②a removal: an unauthenticated GET /snapshot still 401s (the device-code
// gate is untouched).
func TestServe_SnapshotGateUnchanged(t *testing.T) {
	srv, _, _, _ := newSnapshotRunner(t)
	res, err := http.Get(srv.URL + "/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", res.StatusCode)
	}
}
