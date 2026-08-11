package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// newTestStore + seedActiveProjectToken follow the existing pattern in
// internal/store/projects_test.go — read it for the exact seeding signature.
// Contract: seedActiveProjectToken creates an active project bound to a profile
// and returns the plaintext token + that project's ID + profile ID.
func TestServeRunner_CachesByProject(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()

	token, projID, profileID := seedActiveProjectToken(t, st)
	project, err := st.VerifyToken(token)
	if err != nil || project == nil {
		t.Fatalf("VerifyToken: err=%v project=%v (contract: active project resolves)", err, project)
	}
	if project.ID != projID || project.ProfileID != profileID {
		t.Fatalf("resolved project mismatch: got id=%s profile=%s", project.ID, project.ProfileID)
	}

	r := NewServeRunner(st)
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
// active project bound to a fresh profile and return (token, projectID,
// profileID). AddProject's default status is active, so the token verifies.
// Test-only.
func seedActiveProjectToken(t *testing.T, st *store.Store) (token, projectID, profileID string) {
	t.Helper()
	pid, err := st.AddProfile("dev")
	if err != nil {
		t.Fatalf("AddProfile: %v", err)
	}
	projID, tok, err := st.AddProject("project-A", pid)
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
	token, _, _ := seedActiveProjectToken(t, st)

	r := NewServeRunner(st)
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
