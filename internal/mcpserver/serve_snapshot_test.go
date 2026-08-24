package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newSnapshotRunner stands up a ServeRunner over a seeded store + a live httptest server.
// Returns the server (close it via t.Cleanup) + a valid cache token + a valid PROJECT token
// (the latter for the cross-auth-isolation assertions) + the underlying *store.Store (so the
// revoked-token test can call st.RevokeCacheToken then re-GET /snapshot).
func newSnapshotRunner(t *testing.T) (*httptest.Server, string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	// seed one server + credential so ExportSnapshot has content.
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name:         "gpu",
		Host:         "192.0.2.10",
		Port:         22,
		User:         "u",
		AuthMethod:   models.AuthPassword,
		CredentialID: cid,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, cacheToken, err := st.AddCacheToken("laptop")
	if err != nil {
		t.Fatal(err)
	}
	_, projToken, _ := seedActiveProjectToken(t, st, "proj-x")
	return srv, cacheToken, projToken, st
}

func TestSnapshot_ValidCacheTokenReturnsFullSnapshot(t *testing.T) {
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
	body, _ := io.ReadAll(res.Body)
	var snap store.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("not a Snapshot: %v\nbody=%s", err, body)
	}
	if snap.Version != 1 || len(snap.Servers) != 1 {
		t.Fatalf("snapshot mismatch: version=%d servers=%d", snap.Version, len(snap.Servers))
	}
}

// THE KEYSTONE: a project token must NOT authenticate /snapshot (else any agent token
// dumps the whole vault). And a cache token must NOT authenticate the MCP endpoint.
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
