package mcpserver

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newSnapshotRunner stands up a ServeRunner over a seeded store + a live httptest server.
// Plan-39 seeding: TWO servers (gpu granted to profile team-a; secret NOT granted), the
// bound device code "laptop" (AddCacheToken(name, profileID)), a project token on the
// same profile, and one audit row — so the scoped-snapshot assertions have teeth.
// Returns the server (close via t.Cleanup) + a valid BOUND cache token + a valid PROJECT
// token (the latter for the cross-auth-isolation assertions) + the underlying *store.Store
// (so the revoked-token test can call st.RevokeCacheToken then re-GET /snapshot).
func newSnapshotRunner(t *testing.T) (*httptest.Server, string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	cid2, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("topsecret")})
	if err != nil {
		t.Fatal(err)
	}
	gpuID, err := st.AddServer(&models.Server{
		Name:         "gpu",
		Host:         "192.0.2.10",
		Port:         22,
		User:         "u",
		AuthMethod:   models.AuthPassword,
		CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	secretID, err := st.AddServer(&models.Server{
		Name:         "secret",
		Host:         "192.0.2.99",
		Port:         22,
		User:         "u",
		AuthMethod:   models.AuthPassword,
		CredentialID: cid2,
	})
	if err != nil {
		t.Fatal(err)
	}
	profID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	// Grant ONLY gpu — "secret" stays out of profile (the authorization gap the fix closes).
	if err := st.GrantServers(profID, []string{gpuID}); err != nil {
		t.Fatal(err)
	}
	// An audit row on the ungranted server: must never ride a scoped snapshot.
	if err := st.WriteAudit(store.AuditRow{Action: "exec", ServerID: secretID, Status: "ok", Command: "cat /etc/shadow"}); err != nil {
		t.Fatal(err)
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, cacheToken, err := st.AddCacheToken("laptop", profID)
	if err != nil {
		t.Fatal(err)
	}
	_, projToken, err := st.AddProject("proj-x", profID)
	if err != nil {
		t.Fatal(err)
	}
	return srv, cacheToken, projToken, st
}

// TestSnapshot_ScopedToBoundProfile (Plan 39, replacing the old whole-vault contract):
// a bound device code receives ONLY its profile's authorization set — the granted
// server + its credential, the profile + grants, the same-profile projects, that
// server's host keys — and NO audit rows. The ungranted server AND its credential
// must not appear anywhere in the body.
func TestSnapshot_ScopedToBoundProfile(t *testing.T) {
	srv, cacheToken, _, _ := newSnapshotRunner(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+cacheToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// Plan 39 provenance header: the client records it in cache.meta (scoped)
	// so `cache status` can tell a cropped cache from a legacy whole-vault one.
	if got := res.Header.Get("X-Sshmgr-Snapshot-Scope"); got != "profile" {
		t.Fatalf("scoped snapshot must carry X-Sshmgr-Snapshot-Scope: profile, got %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	var snap store.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("not a Snapshot: %v\nbody=%s", err, body)
	}
	if snap.Version != 1 {
		t.Fatalf("snapshot version = %d, want 1 (same envelope, subset rows)", snap.Version)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "gpu" {
		t.Fatalf("servers must be exactly [gpu], got %+v", snap.Servers)
	}
	if len(snap.Credentials) != 1 || string(snap.Credentials[0].Secret) != "pw" {
		t.Fatalf("credentials must be exactly gpu's, got %+v", snap.Credentials)
	}
	if len(snap.Profiles) != 1 || snap.Profiles[0].Name != "team-a" {
		t.Fatalf("profiles must be exactly [team-a], got %+v", snap.Profiles)
	}
	if len(snap.Grants) != 1 || snap.Grants[0].ServerID != snap.Servers[0].ID {
		t.Fatalf("grants must be exactly team-a→gpu, got %+v", snap.Grants)
	}
	if len(snap.Projects) != 1 || snap.Projects[0].Name != "proj-x" {
		t.Fatalf("projects must be exactly [proj-x] (same profile), got %+v", snap.Projects)
	}
	if len(snap.HostKeys) != 0 {
		t.Fatalf("no host keys were seeded for gpu; snapshot must carry none, got %+v", snap.HostKeys)
	}
	if len(snap.Audit) != 0 {
		t.Fatalf("scoped snapshot must carry NO audit rows, got %d", len(snap.Audit))
	}
	// Defense: the ungranted server's host and its credential (base64-wrapped in
	// JSON) must not ride the body. (The server NAME "secret" collides with the
	// credentials' JSON field name, so assert host + secret material instead —
	// the structured len==1 check above already pins the name.)
	topB64 := base64.StdEncoding.EncodeToString([]byte("topsecret"))
	if strings.Contains(string(body), "192.0.2.99") || strings.Contains(string(body), topB64) {
		t.Fatalf("ungranted server or its credential leaked into the scoped snapshot body: %s", body)
	}
}

// TestSnapshot_UnboundToken403Not401 pins the Plan-39 upgrade path: a device code that
// migrated UNBOUND (legacy pre-Plan-39 row, profile_id NULL) is REFUSED — but with 403,
// never 401. 401 on a pinned connection is the Plan-34 quarantine trigger (client
// destroys its cache); 403 keeps the cache intact and tells the owner to bind.
func TestSnapshot_UnboundToken403Not401(t *testing.T) {
	// Build a legacy-shape DB whose only cache_tokens row is unbound.
	path := filepath.Join(t.TempDir(), "legacy-unbound.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cache_tokens (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, token_hash BLOB NOT NULL,
		token_salt BLOB NOT NULL, token_prefix TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
		last_pull_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	tok, err := store.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	hash := store.HashToken([]byte(tok), salt)
	if _, err := db.Exec(`INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,created_at,updated_at)
		VALUES ('ct1','laptop-legacy',?,?,?,'active',1,1)`, hash, salt, tok[:8]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	st, err := store.Open(path, mk) // migrate() adds profile_id (NULL = unbound)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unbound device code: status = %d, want 403 (NEVER 401 — Plan 34 would destroy the client cache); body=%s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "bind") {
		t.Fatalf("403 body must tell the owner how to fix it (cache-tokens bind), got: %s", body)
	}
}

// THE KEYSTONE (unchanged by Plan 39): a project token must NOT authenticate /snapshot
// (else any agent token dumps the vault). And a cache token must NOT authenticate the
// MCP endpoint.
func TestSnapshot_ProjectTokenRejected(t *testing.T) {
	srv, _, projToken, _ := newSnapshotRunner(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+projToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatalf("project token must NOT reach /snapshot (status=200 is a vault-dump breach); got %d", res.StatusCode)
	}
}

func TestSnapshot_CacheTokenRejectedOnMCPPath(t *testing.T) {
	srv, cacheToken, _, _ := newSnapshotRunner(t)
	// The MCP endpoint expects a streamable-HTTP MCP initialize. Send a real initialize
	// body so this exercises the same shape an agent's MCP handshake would — and assert a
	// cache token is still rejected at the auth layer (401/403), not admitted.
	initBody := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", initBody)
	req.Header.Set("Authorization", "Bearer "+cacheToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatalf("cache token must NOT authenticate the MCP endpoint; got 200")
	}
}

func TestSnapshot_RevokedCacheTokenRejected(t *testing.T) {
	srv, cacheToken, _, st := newSnapshotRunner(t)
	// Revoke the device code on the underlying store, then re-GET /snapshot. The status filter
	// in VerifyCacheToken (status='active') rejects revoked codes — the HTTP verifier plumbs
	// that through to a non-200. (The status-filter logic itself is proven by T1's
	// TestVerifyCacheToken_RejectsAfterRevoke; this test only proves the HTTP plumbing.)
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+cacheToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatalf("revoked cache token must NOT reach /snapshot; got %d", res.StatusCode)
	}
}
