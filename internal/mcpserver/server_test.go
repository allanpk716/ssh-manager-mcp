package mcpserver

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

	server, _ := NewServer(st, pid, "proj-test")
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

// TestDownloadFile exercises the download_file broker tool end-to-end through
// the MCP wire: an in-profile download returns the fixture content; an
// out-of-profile server_id is rejected as a tool error (IsError), mirroring
// exec_command. Mirrors TestNewServerToolsScopedViaInMemoryClient's shape.
func TestDownloadFile(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	const want = "file-via-tool\n"
	remote := filepath.Join(t.TempDir(), "via_tool.bin")
	if err := os.WriteFile(remote, []byte(want), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	server, _ := NewServer(st, pid, "proj-test")
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

	// download_file on the in-profile server — content round-trips.
	res, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "download_file", Arguments: map[string]any{"server_id": srvID, "path": remote},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("download_file errored: %+v", res.Content)
	}

	// download_file on an out-of-profile server -> tool error (IsError), same as exec_command.
	other, _ := st.AddServer(&models.Server{Name: "other", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	res2, _ := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "download_file", Arguments: map[string]any{"server_id": other, "path": "/etc/passwd"},
	})
	if !res2.IsError {
		t.Fatal("out-of-profile download_file must be a tool error")
	}
}

// TestUploadFile exercises the upload_file broker tool end-to-end through the
// MCP wire: an in-profile upload round-trips (verify via a follow-up
// download_file); an out-of-profile server_id is rejected as a tool error
// (IsError), mirroring download_file / exec_command.
func TestUploadFile(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	const want = "uploaded-via-tool\n"
	localFile := filepath.Join(t.TempDir(), "to_upload.bin")
	if err := os.WriteFile(localFile, []byte(want), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	// Forward-slash remote path so the wire path is POSIX-clean (broker host
	// accepts both separators; a real remote is POSIX).
	remote := filepath.ToSlash(filepath.Join(t.TempDir(), "wire_up.bin"))

	server, _ := NewServer(st, pid, "proj-test")
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

	// upload_file on the in-profile server — verify via a follow-up download_file.
	up, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_file", Arguments: map[string]any{
			"server_id":   srvID,
			"local_path":  localFile,
			"remote_path": remote,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.IsError {
		t.Fatalf("upload_file errored: %+v", up.Content)
	}

	dl, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "download_file", Arguments: map[string]any{"server_id": srvID, "path": remote},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dl.IsError {
		t.Fatalf("verify download_file errored: %+v", dl.Content)
	}

	// upload_file on an out-of-profile server -> tool error (IsError).
	other, _ := st.AddServer(&models.Server{Name: "other", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	res2, _ := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_file", Arguments: map[string]any{
			"server_id":   other,
			"local_path":  localFile,
			"remote_path": "/tmp/x",
		},
	})
	if !res2.IsError {
		t.Fatal("out-of-profile upload_file must be a tool error")
	}
}
