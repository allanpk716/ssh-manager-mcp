package mcpserver

import (
	"context"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

func TestNewServerToolsScopedViaInMemoryClient(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	server, _ := NewServer(st, pid)
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

	// list_servers
	res, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("list_servers errored: %+v", res.Content)
	}

	// exec_command on the in-profile server
	res2, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command", Arguments: map[string]any{"server_id": srvID, "command": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.IsError {
		t.Fatalf("exec_command errored: %+v", res2.Content)
	}

	// exec_command on an out-of-profile server -> tool error (IsError)
	other, _ := st.AddServer(&models.Server{Name: "other", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	res3, _ := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command", Arguments: map[string]any{"server_id": other, "command": "nope"},
	})
	if !res3.IsError {
		t.Fatal("out-of-profile exec_command must be a tool error")
	}
}
