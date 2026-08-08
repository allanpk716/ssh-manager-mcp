package mcpserver

import (
	"context"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestE2EIronRule is the capstone: a Profile-scoped MCP client can use its servers
// and is blocked from others, with credentials never crossing the tool boundary.
func TestE2EIronRule(t *testing.T) {
	st := newStore(t)

	// Two real sshd backends: one the agent may use, one it may not.
	allowedAddr, allowedHK, allowedCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "ALLOWED:" + cmd + "\n", "", 0 },
	})
	defer allowedCleanup()
	forbiddenAddr, forbiddenHK, forbiddenCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "FORBIDDEN\n", "", 0 },
	})
	defer forbiddenCleanup()

	allowedID := seedRealServer(t, st, "allowed", allowedAddr, allowedHK, "")
	// forbidden is routed through the "localhost" loopback alias (same listener as
	// 127.0.0.1) so its host key is stored under a distinct host string. The TOFU
	// host-key store is keyed by host only, so two loopback sshd instances seeded
	// under "127.0.0.1" would collide and clobber allowed's trusted key. Using
	// "localhost" keeps the entries separate AND keeps forbidden genuinely
	// dialable — so if the iron rule ever failed, forbidden would actually
	// connect and return "FORBIDDEN" (failing the test), rather than failing
	// coincidentally on a host-key mismatch.
	forbiddenID := seedServerOnHost(t, st, "forbidden", "localhost", forbiddenAddr, forbiddenHK, "")

	pid, _ := st.AddProfile("agent-profile")
	_ = st.GrantServers(pid, []string{allowedID}) // only allowed in profile

	server, _ := NewServer(st, pid)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, _ := server.Connect(context.Background(), t1, nil)
	defer srvSess.Close()
	cliSess, _ := client.Connect(context.Background(), t2, nil)
	defer cliSess.Close()
	ctx := context.Background()

	// 1. list_servers -> only "allowed"
	res, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if res.IsError {
		t.Fatal("list_servers should succeed")
	}
	// (Content is JSON; assert it contains "allowed" and not "forbidden" via the text.)
	if !textContains(res, "allowed") || textContains(res, "forbidden") {
		t.Fatalf("list_servers leaked a forbidden server: %+v", res.Content)
	}

	// 2. exec on allowed -> works
	res2, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"server_id": allowedID, "command": "hi"}})
	if res2.IsError {
		t.Fatalf("allowed exec should succeed: %+v", res2.Content)
	}

	// 3. exec on forbidden -> tool error (iron rule)
	res3, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"server_id": forbiddenID, "command": "hi"}})
	if !res3.IsError {
		t.Fatal("forbidden exec must be rejected (iron rule)")
	}
}

func textContains(res *mcp.CallToolResult, want string) bool {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if containsStr(tc.Text, want) {
				return true
			}
		}
	}
	return false
}
func containsStr(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// seedServerOnHost is seedRealServer with an explicit Host string (used to keep
// two same-loopback test sshd backends distinct in the host-key store).
func seedServerOnHost(t *testing.T, st *store.Store, name, host, addr string, hk ssh.PublicKey, sudoPw string) string {
	t.Helper()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srv := &models.Server{
		Name: name, Host: host, Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: cid,
	}
	if sudoPw != "" {
		sid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(sudoPw)})
		srv.SudoCredentialID = sid
	}
	id, _ := st.AddServer(srv)
	_ = st.SaveHostKey(host, hk.Marshal()) // pre-trust the testsshd host key under this host alias
	return id
}
