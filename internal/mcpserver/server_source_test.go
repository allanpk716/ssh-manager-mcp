package mcpserver

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestNewServerFromSource_ResolvesStorePerCall: the tool closures must resolve
// the store via storeFn() AT CALL TIME (not capture it). A counting sourceFn
// proves per-call resolution, and swapping the returned store mid-session
// proves the next call serves the new store — the hot-reload contract.
func TestNewServerFromSource_ResolvesStorePerCall(t *testing.T) {
	// Seed store A: one in-profile server. ExportSnapshot → hydrate store B,
	// then add a SECOND server + grant to the SAME profile id (snapshot round-trip
	// preserves ids, so both stores share profile/project identity).
	stA := newStore(t)
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	id1, err := stA.AddServer(&models.Server{Name: "one", Host: "192.0.2.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := stA.AddProfile("p")
	_ = stA.GrantServers(pid, []string{id1})
	snap, err := stA.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	stB := newStore(t)
	if err := stB.ImportSnapshot(snap); err != nil { // same profile/project ids
		t.Fatal(err)
	}
	id2, err := stB.AddServer(&models.Server{Name: "two", Host: "192.0.2.2", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	_ = stB.GrantServers(pid, []string{id2})
	t.Cleanup(func() { stB.Close() })

	var calls int32
	cur := stA
	server, mgr, tasks, err := NewServerFromSource(func() *store.Store {
		atomic.AddInt32(&calls, 1)
		return cur
	}, pid, "proj-src")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()
	defer tasks.CloseAll()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSession, err := server.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSession.Close()
	cliSession, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cliSession.Close()

	listIDs := func() (n int) {
		t.Helper()
		res, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("list_servers: err=%v isError=%v", err, res.IsError)
		}
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				return containsCount(tc.Text, `"one"`) + containsCount(tc.Text, `"two"`)
			}
		}
		return 0
	}

	if got := listIDs(); got != 1 {
		t.Fatalf("store A should serve 1 server, got %d", got)
	}
	n1 := atomic.LoadInt32(&calls)

	cur = stB // swap the source — the running session must see it next call
	if got := listIDs(); got != 2 {
		t.Fatalf("store B should serve 2 servers after swap, got %d", got)
	}
	if n2 := atomic.LoadInt32(&calls); n2 <= n1 {
		t.Fatalf("storeFn not called per tool invocation: %d -> %d", n1, n2)
	}
}

func containsCount(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
