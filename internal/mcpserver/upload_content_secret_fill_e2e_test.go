package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	assertUploadContentResultHasNoSecretValue(t, result, databasePassword, interfaceToken)

	rows, err := st.AuditRows(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v", rows)
	}
	okRow := rows[0]
	deniedRow := rows[1]
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
