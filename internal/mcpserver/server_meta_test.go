package mcpserver

// Plan 51 T7 (spec §9): the update_server_metadata tool face — cache-face
// registration (MetadataEditor injected), the absent/empty pointer semantics,
// the local mirror after a forwarded 200 (verified through list_servers, the
// same read face the agent uses), the mirror-skip warning, and the
// authority-face ABSENCE (Q10-A).

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// fakeMetaEditor stands in for clientops.MetadataForwarder (the import
// direction forbids mcpserver's tests from touching the real one through
// clientops — the interface seam is exactly what is under test here).
type fakeMetaEditor struct {
	gotServerID string
	gotRev      int64
	gotFields   map[string]*string
	outcome     MetadataEditOutcome
	err         error
}

func (f *fakeMetaEditor) EditMeta(serverID string, expectedRevision int64, fields map[string]*string) (MetadataEditOutcome, error) {
	f.gotServerID, f.gotRev, f.gotFields = serverID, expectedRevision, fields
	return f.outcome, f.err
}

// seedMetaFaceSnap builds an authority store + its snapshot with one granted
// server; returns (snapshot, project token, server id).
func seedMetaFaceSnap(t *testing.T) (*store.Snapshot, string, string) {
	t.Helper()
	st := newStoreAt(t, filepath.Join(t.TempDir(), "face.db"))
	id, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, Hardware: "old-hw", Role: "edge",
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(pid, []string{id}); err != nil {
		t.Fatal(err)
	}
	_, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap, token, id
}

// metaFaceSession wires a cache broker with the given editor and returns a
// connected client session.
func metaFaceSession(t *testing.T, editor MetadataEditor) *mcp.ClientSession {
	t.Helper()
	snap, token, _ := seedMetaFaceSnap(t)
	srv, tunnels, tasks, cleanup, err := NewCacheBroker(token, snap, filepath.Join(t.TempDir(), "audit.log"), nil, nil, "laptop", editor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srvSess.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cliSess.Close() })
	return cliSess
}

func metaCallTool(t *testing.T, sess *mcp.ClientSession, args map[string]any) (string, bool) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: BrokerTools[12], Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError
}

func metaListServers(t *testing.T, sess *mcp.ClientSession) ListServersOutput {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: BrokerTools[0]})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	var out ListServersOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("list_servers output: %v (%s)", err, text)
	}
	return out
}

func TestUpdateServerMetadata_CacheFaceHappyPath(t *testing.T) {
	editor := &fakeMetaEditor{outcome: MetadataEditOutcome{ServerName: "gpu", Revision: 1, UpdatedAt: 12345}}
	sess := metaFaceSession(t, editor)
	sid := metaFirstServerID(t, sess) // the id the session's own cache holds

	text, isErr := metaCallTool(t, sess, map[string]any{
		"server_id": sid, "expected_revision": 0, "hardware": "H200 x1",
	})
	if isErr {
		t.Fatalf("happy path errored: %s", text)
	}
	var out UpdateServerMetadataOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("output parse: %v (%s)", err, text)
	}
	if out.ServerName != "gpu" || out.Revision != 1 || out.Mirrored != true {
		t.Fatalf("output = %+v", out)
	}
	// The forwarder received EXACTLY the present fields (absent = keep).
	if editor.gotServerID != sid || editor.gotRev != 0 || len(editor.gotFields) != 1 ||
		editor.gotFields["hardware"] == nil || *editor.gotFields["hardware"] != "H200 x1" {
		t.Fatalf("editor received: sid=%q rev=%d fields=%+v", editor.gotServerID, editor.gotRev, editor.gotFields)
	}
	// The local mirror landed: list_servers (the agent's read face) sees it.
	ls := metaListServers(t, sess)
	if len(ls.Servers) != 1 || ls.Servers[0].Hardware != "H200 x1" || ls.Servers[0].Revision != 1 {
		t.Fatalf("local mirror via list_servers: %+v", ls.Servers)
	}
}

func TestUpdateServerMetadata_ClearSemantics(t *testing.T) {
	// An explicit EMPTY STRING clears; another field set in the same call.
	editor := &fakeMetaEditor{outcome: MetadataEditOutcome{ServerName: "gpu", Revision: 2, UpdatedAt: 1}}
	sess := metaFaceSession(t, editor)
	sid := metaFirstServerID(t, sess)
	text, isErr := metaCallTool(t, sess, map[string]any{
		"server_id": sid, "expected_revision": 0, "caveats": "", "role": "edge-2",
	})
	if isErr {
		t.Fatalf("clear-semantics call errored: %s", text)
	}
	if len(editor.gotFields) != 2 {
		t.Fatalf("both present fields must carry: %+v", editor.gotFields)
	}
	if editor.gotFields["caveats"] == nil || *editor.gotFields["caveats"] != "" {
		t.Fatalf("empty string must arrive as a present-empty pointer (CLEAR): %+v", editor.gotFields)
	}
	// Mirror applied the clear too.
	ls := metaListServers(t, sess)
	if ls.Servers[0].Caveats != "" || ls.Servers[0].Role != "edge-2" {
		t.Fatalf("mirror after clear: %+v", ls.Servers[0])
	}
}

func TestUpdateServerMetadata_NoFieldsIsToolError(t *testing.T) {
	editor := &fakeMetaEditor{}
	sess := metaFaceSession(t, editor)
	sid := metaFirstServerID(t, sess)
	text, isErr := metaCallTool(t, sess, map[string]any{
		"server_id": sid, "expected_revision": 0,
	})
	if !isErr || !strings.Contains(text, "at least one") {
		t.Fatalf("all-nil fields must be a tool error naming the six fields: isErr=%v text=%s", isErr, text)
	}
	if editor.gotServerID != "" {
		t.Fatal("a no-field call must never reach the forwarder")
	}
}

func TestUpdateServerMetadata_StaleErrorSurfacesMergeInput(t *testing.T) {
	stale := &staleForTest{sid: "srv", cur: 4}
	editor := &fakeMetaEditor{err: stale}
	sess := metaFaceSession(t, editor)
	sid := metaFirstServerID(t, sess)
	text, isErr := metaCallTool(t, sess, map[string]any{
		"server_id": sid, "expected_revision": 0, "hardware": "H200",
	})
	if !isErr {
		t.Fatal("409 must surface as a tool error")
	}
	if !strings.Contains(text, "current revision 4") || !strings.Contains(text, "retry with expected_revision=4") {
		t.Fatalf("error must carry the merge input verbatim: %s", text)
	}
	// The local cache is untouched (nothing was mirrored).
	ls := metaListServers(t, sess)
	if ls.Servers[0].Hardware != "old-hw" {
		t.Fatalf("stale path must not mirror: %+v", ls.Servers[0])
	}
}

func TestUpdateServerMetadata_MirrorSkipWarns(t *testing.T) {
	// The broker accepted (fake succeeds) but the row is absent locally —
	// success with mirrored=false + the cache-pull warning.
	editor := &fakeMetaEditor{outcome: MetadataEditOutcome{ServerName: "ghost", Revision: 1, UpdatedAt: 1}}
	sess := metaFaceSession(t, editor)
	text, isErr := metaCallTool(t, sess, map[string]any{
		"server_id": "srv-not-in-cache", "expected_revision": 0, "hardware": "x",
	})
	if isErr {
		t.Fatalf("a broker-accepted edit is a SUCCESS even when the mirror skips: %s", text)
	}
	var out UpdateServerMetadataOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mirrored || out.Warning == "" || !strings.Contains(out.Warning, "cache pull") {
		t.Fatalf("mirror-skip output = %+v", out)
	}
}

func TestUpdateServerMetadata_AbsentOnAuthorityFace(t *testing.T) {
	_, _, _ = seedMetaFaceSnap(t) // throwaway: we only need the store shape below
	st := newStoreAt(t, filepath.Join(t.TempDir(), "auth.db"))
	srv, tunnels, tasks, err := NewServerFromSource(func() *store.Store { return st }, "p", "proj", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srvSess.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cliSess.Close() })

	lt, err := cliSess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.Tools) != len(authorityTools) {
		t.Fatalf("authority face = %d tools (authorityTools has %d)", len(lt.Tools), len(authorityTools))
	}
	for _, tl := range lt.Tools {
		if tl.Name == BrokerTools[12] {
			t.Fatal("update_server_metadata must NOT exist on the authority face (Q10-A)")
		}
	}
}

// ---- small helpers ----

// staleForTest renders like clientops.MetaStaleError without importing it
// (same-package seam test — the tool only ever calls Error()).
type staleForTest struct {
	sid string
	cur int64
}

func (e *staleForTest) Error() string {
	return "stale revision for " + e.sid + " — the entry changed since your listing (current revision 4); merge your intent and retry with expected_revision=4"
}

// metaFirstServerID reads the id from the session's own list_servers.
func metaFirstServerID(t *testing.T, sess *mcp.ClientSession) string {
	t.Helper()
	ls := metaListServers(t, sess)
	if len(ls.Servers) != 1 {
		t.Fatal("session cache must hold exactly one server")
	}
	return ls.Servers[0].ID
}
