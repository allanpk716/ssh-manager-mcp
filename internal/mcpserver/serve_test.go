package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
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

func TestHTTPHandler_AuthGate(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	token, _, _ := seedActiveProjectToken(t, st, "project-A")

	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()
	h := r.HTTPHandler()

	// Minimal JSON-RPC initialize body the streamable handler accepts.
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

	cases := []struct {
		name string
		auth string
		want int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"bad token", "Bearer not-a-real-token", http.StatusUnauthorized},
		{"malformed header", "Token " + token, http.StatusUnauthorized},
		{"valid token", "Bearer " + token, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(initBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Fatalf("%s: status = %d, want %d (body=%q)", c.name, rr.Code, c.want, rr.Body.String())
			}
		})
	}
}

// TestHTTPHandler_SessionBinding_RejectsCrossProjectReplay is the load-bearing
// proof that the SDK's session-hijack defense is engaged: a token from project A
// that initialized a session must NOT be replayable onto that session by a
// different project B's token. The SDK captures UserID at session creation
// (streamable.go:425-435) and re-checks it per request (streamable.go:250-258);
// our auth.TokenVerifier (verifyToken) sets UserID = project ID, so a mismatched
// project → HTTP 403 "session user mismatch".
//
// Uses httptest.NewServer (not NewRecorder) so the SDK's session map persists
// across the two requests against one handler instance.
func TestHTTPHandler_SessionBinding_RejectsCrossProjectReplay(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()

	// Two active projects, each with its own profile + token.
	tokenA, _, _ := seedActiveProjectToken(t, st, "project-A")
	tokenB, _, _ := seedActiveProjectToken(t, st, "project-B")

	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()
	ts := httptest.NewServer(r.HTTPHandler())
	defer ts.Close()

	doPost := func(t *testing.T, body, token, sessionID string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	// 1) Initialize a session as project A. SDK captures userID = A.
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	resp := doPost(t, initBody, tokenA, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize: status=%d want 200 (body=%q)", resp.StatusCode, b)
	}
	mcpSessionID := resp.Header.Get("Mcp-Session-Id")
	if mcpSessionID == "" {
		t.Fatal("initialize did not return Mcp-Session-Id — session was not created; SDK defense cannot be exercised")
	}

	// 2) Cross-project replay: project B's token + A's session → MUST be 403.
	//    The SDK check fires at streamable.go:250-258 BEFORE method dispatch.
	pingBody := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`
	resp2 := doPost(t, pingBody, tokenB, mcpSessionID)
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project replay: status=%d want 403 (body=%q) — SDK session-binding defense is NOT engaged", resp2.StatusCode, b2)
	}
	if !bytes.Contains(b2, []byte("session user mismatch")) {
		t.Fatalf("cross-project replay: body=%q want substring \"session user mismatch\"", b2)
	}

	// 3) Sanity: same project (A) on A's session → 200 (allowed).
	resp3 := doPost(t, pingBody, tokenA, mcpSessionID)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		b3, _ := io.ReadAll(resp3.Body)
		t.Fatalf("same-project ping: status=%d want 200 (body=%q)", resp3.StatusCode, b3)
	}
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

// ---- Plan 33 T5: serve body-limit middleware (spec rev3 §3.2) ----

// ucServeSetup: testsshd + store + profile + a seeded REAL server usable by the
// upload_content tool over serve; returns (st, token, srvID, remoteRootSlash).
// The runner is built AFTER any t.Setenv so the env seam resolves per-test.
// seedActiveProjectToken creates its OWN profile — the grant must go to THAT
// profile (the token's project resolves to it at serve time), or the iron-rule
// gate rejects the call ("server is not in your profile").
func ucServeSetup(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	st := newTestStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	token, _, profID := seedActiveProjectToken(t, st, "project-uc")
	_ = st.GrantServers(profID, []string{srvID})
	return st, token, srvID, toSlash(t.TempDir())
}

func TestNewServeRunnerFailClosedOnBadEnv(t *testing.T) {
	t.Setenv("SSHMGR_UPLOAD_CONTENT_MAX", "not-a-number")
	if _, err := NewServeRunner(newTestStore(t)); err == nil {
		t.Fatal("NewServeRunner must refuse to start on an invalid SSHMGR_UPLOAD_CONTENT_MAX (fail-closed, spec rev3 §3.1)")
	}
}

func TestServeBodyLimit(t *testing.T) {
	// Small seam → small body limit: cap 4096 → limit = 4096 + 1365 + 65536.
	t.Setenv("SSHMGR_UPLOAD_CONTENT_MAX", "4096")
	st, token, srvID, root := ucServeSetup(t)
	defer st.Close()
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()
	ts := httptest.NewServer(r.HTTPHandler())
	defer ts.Close()

	post := func(body string, cl bool) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if !cl {
			req.ContentLength = -1 // strip Content-Length → chunked path (fallback tier)
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("Do: %v", derr)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	if got := post(initBody, true); got != http.StatusOK {
		t.Fatalf("small initialize must pass: %d", got)
	}

	// honest Content-Length over the limit → 413 (the real-client path).
	big := initBody + strings.Repeat(" ", 80*1024)
	if got := post(big, true); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit with Content-Length: %d, want 413", got)
	}

	// chunked over the limit → the MaxBytesReader fallback: an ERROR response,
	// not 413 (the SDK owns the response) — asserted as non-OK per spec §3.2.
	if got := post(big, false); got == http.StatusOK {
		t.Fatalf("over-limit chunked: 200, want an error status (fallback tier)")
	}

	// at-cap base64 tool call passes the limit end-to-end: cap=4096 decoded
	// bytes → 5464 encoded chars — under cap+cap/3+64KiB, over a naive cap.
	payload := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA5}, 4096))
	callBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"upload_content","arguments":{"server_id":%q,"content":%q,"remote_path":%q,"encoding":"base64"}}}`, srvID, payload, root+"/atcap.bin")
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(callBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	// session dance: initialize first to obtain Mcp-Session-Id.
	ireq, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(initBody))
	ireq.Header.Set("Content-Type", "application/json")
	ireq.Header.Set("Accept", "application/json, text/event-stream")
	ireq.Header.Set("Authorization", "Bearer "+token)
	iresp, derr := http.DefaultClient.Do(ireq)
	if derr != nil || iresp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: err=%v status=%d", derr, iresp.StatusCode)
	}
	sid := iresp.Header.Get("Mcp-Session-Id")
	iresp.Body.Close()
	req.Header.Set("Mcp-Session-Id", sid)
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		t.Fatalf("tools/call at-cap: %v", derr)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Success is pinned by the structured "bytes":4096 output; a tool error
	// (IsError) carries only text, so the substring can never appear in it.
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"bytes":4096`)) {
		t.Fatalf("at-cap tool call: status=%d body=%q", resp.StatusCode, body)
	}
	if got, _ := os.ReadFile(filepath.FromSlash(root + "/atcap.bin")); len(got) != 4096 {
		t.Fatalf("at-cap file = %d bytes, want 4096", len(got))
	}
}

// TestServeUploadContentUFFFDFullChain pins the text-mode contract at the
// TRANSPORT layer (spec rev3 §1.1/§7): raw invalid-UTF-8 bytes inside a JSON
// string are replaced with U+FFFD by JSON DECODING (Go encoding/json public
// behavior) before the tool sees them — an SDK-client test can never exercise
// this (client-side Marshal replaces first), so this drives raw HTTP bytes.
func TestServeUploadContentUFFFDFullChain(t *testing.T) {
	st, token, srvID, root := ucServeSetup(t)
	defer st.Close()
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()
	ts := httptest.NewServer(r.HTTPHandler())
	defer ts.Close()

	doPost := func(body string, sid string) (int, string, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("Do: %v", derr)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), string(b)
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	code, sid, _ := doPost(initBody, "")
	if code != http.StatusOK || sid == "" {
		t.Fatalf("initialize: code=%d sid=%q", code, sid)
	}
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	doPost(notif, sid)

	// RAW invalid UTF-8 byte 0xFF inside the content string: JSON decoding
	// replaces it with U+FFFD (EF BF BD) before the tool runs.
	target := root + "/ufffd.txt"
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"upload_content","arguments":{"server_id":"` + srvID + `","content":"pre-` + "\xFF" + `-post","remote_path":"` + target + `","encoding":"text"}}}`
	code, _, body := doPost(call, sid)
	if code != http.StatusOK {
		t.Fatalf("tools/call raw-UTF8: code=%d body=%q", code, body)
	}
	got, _ := os.ReadFile(filepath.FromSlash(target))
	want := "pre-\xEF\xBF\xBD-post"
	if string(got) != want {
		t.Fatalf("U+FFFD full chain: file=%q want %q", got, want)
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
	// NB: AddCacheToken returns (id, plaintextToken, err) — bind the SECOND
	// value (the brief's snippet bound the first, sending the ID as the bearer
	// code, which misclassifies as "unknown"; caught in Step-2 red analysis).
	_, tok, err := st.AddCacheToken("laptop")
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
