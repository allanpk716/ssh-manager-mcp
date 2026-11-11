package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestUploadContentSecretFillFullFlow(t *testing.T) {
	const databasePassword = "database-password-7f31a9"
	const interfaceToken = "interface-token-c50d82"
	st := newStore(t)
	if err := st.AddSecret("DATABASE_PASSWORD", "数据库密码", []byte(databasePassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("INTERFACE_TOKEN", "接口令牌", []byte(interfaceToken), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, st, "real", addr, hostKey, "")
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"DATABASE_PASSWORD", "INTERFACE_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	target := toSlash(filepath.Join(t.TempDir(), "config", "app.yaml"))
	template := "driver: postgres\npassword: {{sshmgr-secret:DATABASE_PASSWORD}}\ntoken: {{sshmgr-secret:INTERFACE_TOKEN}}\nbackup_token: {{sshmgr-secret:INTERFACE_TOKEN}}\n"
	arguments := map[string]any{
		"server_id":   serverID,
		"content":     template,
		"remote_path": target,
	}

	// 先保持 profile 未授权，钉住既有拒绝行为，并确认拒绝结果不携带值。
	denied, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !denied.IsError {
		t.Fatal("out-of-profile upload unexpectedly succeeded")
	}
	assertUploadContentResultHasNoSecretValue(t, denied, databasePassword, interfaceToken)
	if _, statErr := os.Stat(filepath.FromSlash(target)); !os.IsNotExist(statErr) {
		t.Fatalf("out-of-profile upload must not create the target; stat error=%v", statErr)
	}

	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content", Arguments: arguments,
	})
	if err != nil || result.IsError {
		t.Fatal("upload_content failed")
	}
	var output struct {
		Bytes             int64    `json:"bytes"`
		FilledSecretNames []string `json:"filled_secret_names"`
	}
	unmarshalToolJSON(t, result, &output)
	wantNames := []string{"DATABASE_PASSWORD", "INTERFACE_TOKEN"}
	if fmt.Sprint(output.FilledSecretNames) != fmt.Sprint(wantNames) {
		t.Fatalf("filled secret names = %#v, want %#v", output.FilledSecretNames, wantNames)
	}
	want := "driver: postgres\npassword: " + databasePassword +
		"\ntoken: " + interfaceToken + "\nbackup_token: " + interfaceToken + "\n"
	if output.Bytes != int64(len(want)) {
		t.Fatalf("written bytes = %d, want %d", output.Bytes, len(want))
	}
	got, err := os.ReadFile(filepath.FromSlash(target))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("target content mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
	if info, err := os.Stat(filepath.FromSlash(target)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("new file containing secret values has mode %v, want 0600", info.Mode().Perm())
	}
	assertUploadContentResultHasNoSecretValue(t, result, databasePassword, interfaceToken)

	// 覆盖时保留既有权限；Base64 模式先解码再按原始字节填值。
	if err := os.Chmod(filepath.FromSlash(target), 0o644); err != nil {
		t.Fatal(err)
	}
	placeholder := []byte("{{sshmgr-secret:DATABASE_PASSWORD}}")
	binaryTemplate := bytes.Join([][]byte{
		{0xEF, 0xBB, 0xBF}, []byte("windows\r\n"), {0x00},
		placeholder, {0xFF, 0x81, 0x00}, placeholder, []byte("tail\r\n"),
	}, nil)
	binaryWant := bytes.Join([][]byte{
		{0xEF, 0xBB, 0xBF}, []byte("windows\r\n"), {0x00},
		[]byte(databasePassword), {0xFF, 0x81, 0x00}, []byte(databasePassword), []byte("tail\r\n"),
	}, nil)
	base64Result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     base64.StdEncoding.EncodeToString(binaryTemplate),
			"remote_path": target,
			"encoding":    "base64",
		},
	})
	if err != nil || base64Result.IsError {
		t.Fatal("base64 upload_content failed")
	}
	var base64Output struct {
		Bytes             int64    `json:"bytes"`
		FilledSecretNames []string `json:"filled_secret_names"`
	}
	unmarshalToolJSON(t, base64Result, &base64Output)
	if fmt.Sprint(base64Output.FilledSecretNames) != fmt.Sprint([]string{"DATABASE_PASSWORD"}) {
		t.Fatalf("base64 filled secret names = %#v", base64Output.FilledSecretNames)
	}
	if base64Output.Bytes != int64(len(binaryWant)) {
		t.Fatalf("base64 written bytes = %d, want %d", base64Output.Bytes, len(binaryWant))
	}
	binaryGot, err := os.ReadFile(filepath.FromSlash(target))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binaryGot, binaryWant) {
		t.Fatalf("base64 target bytes = %x, want %x", binaryGot, binaryWant)
	}
	if info, err := os.Stat(filepath.FromSlash(target)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("overwritten file mode = %v, want retained 0644", info.Mode().Perm())
	}
	assertUploadContentResultHasNoSecretValue(t, base64Result, databasePassword, interfaceToken)

	rows, err := st.AuditRows(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("audit rows = %+v", rows)
	}
	textRow := rows[1]
	base64Row := rows[0]
	deniedRow := rows[2]
	if base64Row.Action != "upload-content" || base64Row.Status != "ok" ||
		!strings.Contains(base64Row.Command, fmt.Sprintf("inline %d bytes -> %s", len(binaryWant), target)) ||
		!strings.Contains(base64Row.Command, "secrets=DATABASE_PASSWORD") {
		t.Fatalf("base64 success audit row = %+v", base64Row)
	}
	okRow := textRow
	if okRow.Action != "upload-content" || okRow.Status != "ok" ||
		!strings.Contains(okRow.Command, fmt.Sprintf("inline %d bytes -> %s", len(want), target)) ||
		!strings.Contains(okRow.Command, "secrets=DATABASE_PASSWORD,INTERFACE_TOKEN") {
		t.Fatalf("success audit row = %+v", okRow)
	}
	if deniedRow.Action != "upload-content" || deniedRow.Status != "denied" {
		t.Fatalf("denied audit row = %+v", deniedRow)
	}
	for _, row := range rows {
		auditText, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(auditText, []byte(databasePassword)) || bytes.Contains(auditText, []byte(interfaceToken)) {
			t.Fatal("audit leaks secret value")
		}
	}

	// 服务器条目标成 Windows shell 时不套用 POSIX 权限；这里用同一测试
	// SFTP 文件系统对比无机密项新建文件的默认模式，钉住“不额外处理”。
	controlTarget := toSlash(filepath.Join(t.TempDir(), "control.txt"))
	controlResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "upload_content",
		Arguments: map[string]any{"server_id": serverID, "content": "control\n", "remote_path": controlTarget},
	})
	if err != nil || controlResult.IsError {
		t.Fatal("control upload_content failed")
	}
	controlInfo, err := os.Stat(filepath.FromSlash(controlTarget))
	if err != nil {
		t.Fatal(err)
	}
	windowsServer, err := st.GetServer(serverID)
	if err != nil {
		t.Fatal(err)
	}
	windowsServer.ShellType = models.ShellPowerShell
	if err := st.UpdateServer(windowsServer); err != nil {
		t.Fatal(err)
	}
	windowsTarget := toSlash(filepath.Join(t.TempDir(), "windows.txt"))
	windowsResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     "token={{sshmgr-secret:DATABASE_PASSWORD}}\n",
			"remote_path": windowsTarget,
		},
	})
	if err != nil || windowsResult.IsError {
		t.Fatal("upload_content to Windows-shell target failed")
	}
	windowsInfo, err := os.Stat(filepath.FromSlash(windowsTarget))
	if err != nil {
		t.Fatal(err)
	}
	if windowsInfo.Mode().Perm() != controlInfo.Mode().Perm() {
		t.Fatalf("Windows-shell target mode = %v, want unchanged default %v", windowsInfo.Mode().Perm(), controlInfo.Mode().Perm())
	}
	assertUploadContentResultHasNoSecretValue(t, windowsResult, databasePassword, interfaceToken)
}

func TestUploadFileAndRelayFileKeepSecretPlaceholdersLiteral(t *testing.T) {
	const secretValue = "literal-transfer-secret-2c81e6"
	t.Setenv("SSHMGR_TRANSFER_CHUNK", "16777216")
	st := newStore(t)
	if err := st.AddSecret("LITERAL_TOKEN", "接口令牌", []byte(secretValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, st, "real", addr, hostKey, "")
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"LITERAL_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	content := []byte("token: {{sshmgr-secret:LITERAL_TOKEN}}\r\n\x00\xff")
	localDirectory := t.TempDir()
	uploadSource := filepath.Join(localDirectory, "upload-template")
	if err := os.WriteFile(uploadSource, content, 0o644); err != nil {
		t.Fatal(err)
	}
	uploadTarget := toSlash(filepath.Join(localDirectory, "uploaded-template"))
	uploadResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_file",
		Arguments: map[string]any{
			"server_id": serverID, "local_path": uploadSource, "remote_path": uploadTarget,
		},
	})
	if err != nil || uploadResult.IsError {
		t.Fatal("upload_file failed")
	}
	if got, err := os.ReadFile(filepath.FromSlash(uploadTarget)); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, content) {
		t.Fatalf("upload_file target = %x, want %x", got, content)
	}
	assertUploadContentResultHasNoSecretValue(t, uploadResult, secretValue)

	relaySource := filepath.Join(localDirectory, "relay-template")
	if err := os.WriteFile(relaySource, content, 0o644); err != nil {
		t.Fatal(err)
	}
	relayTarget := toSlash(filepath.Join(localDirectory, "relayed-template"))
	relayResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_path": relaySource, "to_server_id": serverID, "to_path": relayTarget,
		},
	})
	if err != nil || relayResult.IsError {
		t.Fatal("relay_file failed")
	}
	var relayStart RelayOutput
	unmarshalToolJSON(t, relayResult, &relayStart)
	relayFinal := waitBackgroundTerminal(t, clientSession, relayStart.TaskID)
	if relayFinal.Status != bgStatusDone {
		t.Fatalf("relay task status = %q, error=%q", relayFinal.Status, relayFinal.Error)
	}
	if got, err := os.ReadFile(filepath.FromSlash(relayTarget)); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, content) {
		t.Fatalf("relay_file target = %x, want %x", got, content)
	}
	assertUploadContentResultHasNoSecretValue(t, relayResult, secretValue)
	if strings.Contains(relayFinal.Stdout, secretValue) || strings.Contains(relayFinal.Stderr, secretValue) {
		t.Fatal("relay task output leaks secret value")
	}
	auditRows, err := st.AuditRows(30)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range auditRows {
		auditText, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(auditText, []byte(secretValue)) {
			t.Fatal("audit leaks secret value")
		}
	}
}

func assertUploadContentResultHasNoSecretValue(t *testing.T, result *mcp.CallToolResult, values ...string) {
	t.Helper()
	var visible bytes.Buffer
	for _, content := range result.Content {
		encoded, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		visible.Write(encoded)
	}
	for _, value := range values {
		if bytes.Contains(visible.Bytes(), []byte(value)) {
			t.Fatal("tool result leaks secret value")
		}
	}
}
