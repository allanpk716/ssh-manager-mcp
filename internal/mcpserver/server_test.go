package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

// TestServerInfoCarriesBuildinfoVersion pins the initialize handshake's
// serverInfo to buildinfo.Version — the same ldflags-injected source the CLI
// `version` command reads, so a release build's MCP handshake can never drift
// from its binary version (a "dev" serverInfo on a tagged release is the
// regression this pins). Path note (the brief's either/or): the go-sdk v1.2.0
// exposes (*ClientSession).InitializeResult() — "only set synchronously during
// Client.Connect" per client.go — so the accessor path was chosen over
// extracting a serverImplementation() helper: the assertion runs through the
// REAL in-memory client handshake (the server_test.go:30 pattern), pinning the
// wiring end-to-end rather than a helper's return value.
func TestServerInfoCarriesBuildinfoVersion(t *testing.T) {
	buildinfo.Version = "test-9.9.9"
	defer func() { buildinfo.Version = "dev" }()

	st := newStore(t)
	server, mgr, _ := NewServer(st, "p", "proj-test")
	defer mgr.CloseAll()
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

	res := cliSession.InitializeResult()
	if res == nil {
		t.Fatal("InitializeResult() = nil after Connect — handshake did not complete")
	}
	if res.ServerInfo == nil {
		t.Fatalf("InitializeResult.ServerInfo = nil: %+v", res)
	}
	if res.ServerInfo.Name != "ssh-manager" {
		t.Fatalf("serverInfo.name = %q, want %q", res.ServerInfo.Name, "ssh-manager")
	}
	if res.ServerInfo.Version != "test-9.9.9" {
		t.Fatalf("serverInfo.version = %q, want %q (buildinfo.Version at handshake time)", res.ServerInfo.Version, "test-9.9.9")
	}
}

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

	server, mgr, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
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

	server, mgr, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
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

	server, mgr, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
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

// TestForwardPortClosePort exercises the forward_port + close_port broker tools
// end-to-end through the MCP wire: an in-profile forward_port returns a
// ForwardOutput {tunnel_id, local_port}; a follow-up close_port on that id
// returns ok; an out-of-profile forward_port is rejected as a tool error
// (IsError), mirroring upload_file / exec_command. This is the first STATEFUL
// broker op — the server holds the long-lived ssh.Client + tunnel in its
// TunnelManager across the two tool calls (deferred mgr.CloseAll is the safety
// net; the explicit close_port is the agent-driven teardown).
func TestForwardPortClosePort(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	echoPort := startEchoListener(t)

	server, mgr, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
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

	// forward_port on the in-profile server — returns {tunnel_id, local_port}.
	fwd, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "forward_port", Arguments: map[string]any{
			"server_id":   srvID,
			"remote_host": "127.0.0.1",
			"remote_port": echoPort,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fwd.IsError {
		t.Fatalf("forward_port errored: %+v", fwd.Content)
	}
	tunnelID := jsonStringField(t, fwd, "tunnel_id")
	if tunnelID == "" {
		t.Fatal("forward_port returned empty tunnel_id")
	}
	localPort := jsonIntField(t, fwd, "local_port")
	if localPort <= 0 {
		t.Fatalf("forward_port local_port=%d, want > 0", localPort)
	}

	// close_port on the returned id — must succeed (the TunnelManager held it).
	cls, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "close_port", Arguments: map[string]any{"tunnel_id": tunnelID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cls.IsError {
		t.Fatalf("close_port errored: %+v", cls.Content)
	}

	// A second close_port on the same id -> tool error (already torn down).
	cls2, _ := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "close_port", Arguments: map[string]any{"tunnel_id": tunnelID},
	})
	if !cls2.IsError {
		t.Fatal("second close_port on the same tunnel must be a tool error (already closed)")
	}

	// forward_port on an out-of-profile server -> tool error (IsError).
	other, _ := st.AddServer(&models.Server{Name: "other", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	res2, _ := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "forward_port", Arguments: map[string]any{
			"server_id":   other,
			"remote_host": "127.0.0.1",
			"remote_port": echoPort,
		},
	})
	if !res2.IsError {
		t.Fatal("out-of-profile forward_port must be a tool error")
	}
}

// jsonStringField extracts a string field from the JSON-encoded TextContent of
// a CallToolResult (the SDK serializes the typed Out into Content[0].Text as
// JSON when the handler returns a nil *CallToolResult).
func jsonStringField(t *testing.T, res *mcp.CallToolResult, field string) string {
	t.Helper()
	m := decodeToolJSON(t, res)
	v, ok := m[field]
	if !ok {
		t.Fatalf("field %q not in tool json: %+v", field, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field %q = %v, want string", field, v)
	}
	return s
}

// jsonIntField extracts a numeric field (as int) from the JSON-encoded
// TextContent of a CallToolResult.
func jsonIntField(t *testing.T, res *mcp.CallToolResult, field string) int {
	t.Helper()
	m := decodeToolJSON(t, res)
	v, ok := m[field]
	if !ok {
		t.Fatalf("field %q not in tool json: %+v", field, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("field %q = %v, want number", field, v)
	}
	return int(f)
}

func decodeToolJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
				return m
			}
		}
	}
	t.Fatalf("no JSON TextContent in result: %+v", res.Content)
	return nil
}
