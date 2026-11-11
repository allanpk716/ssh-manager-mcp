package mcpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestUploadContentSecretPlaceholderRejectsEveryFormatErrorBeforeConnect(t *testing.T) {
	st := newStore(t)
	var connections atomic.Int64
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Connections: &connections})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, st, "real", addr, hostKey, "")
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)

	target := toSlash(filepath.Join(t.TempDir(), "config", "app.yaml"))
	template := "first {{sshmgr-secret:BAD NAME}}\nsecond {{sshmgr-secret:MISSING_CLOSE\n"
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     template,
			"remote_path": target,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("upload_content unexpectedly succeeded: %+v", result.Content)
	}
	errorText := toolResultText(result)
	for _, want := range []string{
		"第1行字节偏移6：机密项占位符格式错误",
		`原文片段："{{sshmgr-secret:BAD NAME}}"`,
		"第2行字节偏移40：机密项占位符格式错误",
		`原文片段："{{sshmgr-secret:MISSING_CLOSE"`,
	} {
		if !strings.Contains(errorText, want) {
			t.Fatalf("error text misses %q: %q", want, errorText)
		}
	}
	if connections.Load() != 0 {
		t.Fatalf("rejected upload connected to target %d times", connections.Load())
	}
	if _, statErr := os.Stat(filepath.FromSlash(target)); !os.IsNotExist(statErr) {
		t.Fatalf("rejected upload must not create the target; stat error=%v", statErr)
	}
}

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

func TestUploadContentSecretAuthorizationRejectsBeforeConnectAndAuditsNames(t *testing.T) {
	const emptyListValue = "empty-list-secret-1e541a"
	const visibleValue = "visible-server-secret-53d20b"
	const hiddenValue = "hidden-profile-secret-91c372"
	st := newStore(t)
	var connections atomic.Int64
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Connections: &connections})
	t.Cleanup(cleanup)
	targetID := seedRealServer(t, st, "real", addr, hostKey, "")
	visibleServerID, err := st.AddServer(&models.Server{Name: "visible-server", Host: "192.0.2.20", Port: 22, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.AddServer(&models.Server{Name: "outside-profile", Host: "192.0.2.30", Port: 22, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"EMPTY_LIST":    emptyListValue,
		"OUTSIDE_LIST":  visibleValue,
		"HIDDEN_SECRET": hiddenValue,
	} {
		if err := st.AddSecret(name, "", []byte(value), store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AllowSecrets([]string{"OUTSIDE_LIST"}, []string{"visible-server", "outside-profile"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"HIDDEN_SECRET"}, []string{"outside-profile"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{targetID, visibleServerID}); err != nil {
		t.Fatal(err)
	}

	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)
	target := toSlash(filepath.Join(t.TempDir(), "config", "app.yaml"))
	rejectTwo := "empty: {{sshmgr-secret:EMPTY_LIST}}\noutside: {{sshmgr-secret:OUTSIDE_LIST}}\n"
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "upload_content",
		Arguments: map[string]any{"server_id": targetID, "content": rejectTwo, "remote_path": target},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("authorization upload unexpectedly succeeded: %+v", result.Content)
	}
	errorText := toolResultText(result)
	for _, want := range []string{
		"机密项 EMPTY_LIST 不存在，或不允许写入服务器 real",
		"机密项 OUTSIDE_LIST 不存在，或不允许写入服务器 real",
		"它当前没有可写入服务器。请 owner 到权威端把 real 加入该机密项的可写入服务器列表",
		"它当前可写入服务器：visible-server。请 owner 到权威端把 real 加入该机密项的可写入服务器列表",
	} {
		if !strings.Contains(errorText, want) {
			t.Fatalf("authorization error misses %q: %q", want, errorText)
		}
	}
	assertUploadContentResultHasNoSecretValue(t, result, emptyListValue, visibleValue)

	snapshot, err := st.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	clientCache := newStore(t)
	if err := clientCache.ImportSnapshotForClientCache(snapshot); err != nil {
		t.Fatal(err)
	}
	cacheServer, cacheTunnels, cacheTasks, err := NewServer(clientCache, profileID, "project-cache")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cacheTunnels.CloseAll)
	t.Cleanup(cacheTasks.CloseAll)
	cacheClient := connectInMemoryMCPClient(t, cacheServer)
	hiddenTarget := toSlash(filepath.Join(t.TempDir(), "config", "hidden.yaml"))
	hiddenResult, err := cacheClient.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   targetID,
			"content":     "hidden: {{sshmgr-secret:HIDDEN_SECRET}}\n",
			"remote_path": hiddenTarget,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hiddenResult.IsError {
		t.Fatalf("snapshot-trimmed secret unexpectedly succeeded: %+v", hiddenResult.Content)
	}
	hiddenError := toolResultText(hiddenResult)
	if !strings.Contains(hiddenError, "机密项 HIDDEN_SECRET 不存在，或不允许写入服务器 real") {
		t.Fatalf("snapshot-trimmed error = %q", hiddenError)
	}
	if strings.Contains(hiddenError, "outside-profile") || strings.Contains(hiddenError, hiddenValue) {
		t.Fatalf("snapshot-trimmed error leaks profile scope or value: %q", hiddenError)
	}
	if strings.Contains(errorText, "outside-profile") {
		t.Fatalf("local profile rejection leaks server outside the profile: %q", errorText)
	}
	assertUploadContentResultHasNoSecretValue(t, hiddenResult, hiddenValue)

	if connections.Load() != 0 {
		t.Fatalf("rejected uploads connected to target %d times", connections.Load())
	}
	for _, path := range []string{target, hiddenTarget} {
		if _, statErr := os.Stat(filepath.FromSlash(path)); !os.IsNotExist(statErr) {
			t.Fatalf("rejected upload must not create %s; stat error=%v", path, statErr)
		}
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "upload-content" || rows[0].Status != "denied" ||
		!strings.Contains(rows[0].Command, "secrets=EMPTY_LIST,OUTSIDE_LIST") {
		t.Fatalf("local denied audit row = %+v", rows)
	}
	cacheRows, err := clientCache.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cacheRows) != 1 || cacheRows[0].Action != "upload-content" || cacheRows[0].Status != "denied" ||
		!strings.Contains(cacheRows[0].Command, "secrets=HIDDEN_SECRET") {
		t.Fatalf("cache denied audit row = %+v", cacheRows)
	}
	for _, row := range append(rows, cacheRows...) {
		auditText, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(auditText, []byte(emptyListValue)) || bytes.Contains(auditText, []byte(visibleValue)) ||
			bytes.Contains(auditText, []byte(hiddenValue)) || bytes.Contains(auditText, []byte("empty: {{sshmgr-secret:")) {
			t.Fatalf("denied audit leaks value or template: %s", auditText)
		}
	}
}

func TestUploadContentSecretFillSizeChecksAfterSubstitution(t *testing.T) {
	const longValue = "12345678901234"
	st := newStore(t)
	var connections atomic.Int64
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Connections: &connections})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, st, "real", addr, hostKey, "")
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("V", "", []byte(longValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"V"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SSHMGR_UPLOAD_CONTENT_MAX", "67")
	cappedServer, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	cappedClient := connectInMemoryMCPClient(t, cappedServer)
	overTarget := toSlash(filepath.Join(t.TempDir(), "config", "over.yaml"))
	overResult, err := cappedClient.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     "x: {{sshmgr-secret:V}}{{sshmgr-secret:BAD NAME}}{{sshmgr-secret:MISSING}}",
			"remote_path": overTarget,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !overResult.IsError {
		t.Fatalf("over-cap upload unexpectedly succeeded: %+v", overResult.Content)
	}
	overError := toolResultText(overResult)
	if !strings.Contains(overError, "机密项占位符格式错误") ||
		!strings.Contains(overError, "机密项 MISSING 不存在，或不允许写入服务器 real") ||
		!strings.Contains(overError, "content (68 bytes) exceeds upload-content cap 67") {
		t.Fatalf("over-cap error must list authorization, format, and size together: %q", overError)
	}
	assertUploadContentResultHasNoSecretValue(t, overResult, longValue)
	if connections.Load() != 0 {
		t.Fatalf("over-cap upload connected to target %d times", connections.Load())
	}
	if _, statErr := os.Stat(filepath.FromSlash(overTarget)); !os.IsNotExist(statErr) {
		t.Fatalf("over-cap upload must not create the target; stat error=%v", statErr)
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "upload-content" || rows[0].Status != "denied" ||
		!strings.Contains(rows[0].Command, "secrets=V,MISSING") {
		t.Fatalf("over-cap denied audit row = %+v", rows)
	}
	overAuditText, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(overAuditText, []byte(longValue)) || bytes.Contains(overAuditText, []byte("{{sshmgr-secret:")) {
		t.Fatalf("over-cap denied audit leaks value or template: %s", overAuditText)
	}

	t.Setenv("SSHMGR_UPLOAD_CONTENT_MAX", "17")
	exactServer, exactTunnels, exactTasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exactTunnels.CloseAll)
	t.Cleanup(exactTasks.CloseAll)
	exactClient := connectInMemoryMCPClient(t, exactServer)
	exactTarget := toSlash(filepath.Join(t.TempDir(), "config", "exact.yaml"))
	exactResult, err := exactClient.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     "x: {{sshmgr-secret:V}}",
			"remote_path": exactTarget,
		},
	})
	if err != nil || exactResult.IsError {
		t.Fatalf("exact-cap upload failed: err=%v result=%q", err, toolResultText(exactResult))
	}
	var output struct {
		Bytes             int64    `json:"bytes"`
		FilledSecretNames []string `json:"filled_secret_names"`
	}
	unmarshalToolJSON(t, exactResult, &output)
	if output.Bytes != 17 || fmt.Sprint(output.FilledSecretNames) != fmt.Sprint([]string{"V"}) {
		t.Fatalf("exact-cap output = %+v", output)
	}
	got, err := os.ReadFile(filepath.FromSlash(exactTarget))
	if err != nil || string(got) != "x: "+longValue {
		t.Fatalf("exact-cap target = %q, %v", got, err)
	}
	assertUploadContentResultHasNoSecretValue(t, exactResult, longValue)
	if connections.Load() != 1 {
		t.Fatalf("target connections after exact-cap write = %d, want 1", connections.Load())
	}
}

func TestUploadContentSecretReadFailureRejectsBeforeConnectAndAuditsError(t *testing.T) {
	const corruptedValue = "corrupted-secret-a41d68"
	dbPath := filepath.Join(t.TempDir(), "vault.db")
	st := newStoreAt(t, dbPath)
	var connections atomic.Int64
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Connections: &connections})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, st, "real", addr, hostKey, "")
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("BROKEN_SECRET", "", []byte(corruptedValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"BROKEN_SECRET"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	if _, err := rawDB.Exec(`UPDATE secrets SET value_blob=? WHERE name=?`, []byte("not-encrypted"), "BROKEN_SECRET"); err != nil {
		t.Fatal(err)
	}

	target := toSlash(filepath.Join(t.TempDir(), "config", "broken.yaml"))
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     "token: {{sshmgr-secret:BROKEN_SECRET}}\n",
			"remote_path": target,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("broken secret unexpectedly succeeded: %+v", result.Content)
	}
	errorText := toolResultText(result)
	if !strings.Contains(errorText, "机密项 BROKEN_SECRET 读取失败") {
		t.Fatalf("broken secret error = %q", errorText)
	}
	assertUploadContentResultHasNoSecretValue(t, result, corruptedValue)
	if connections.Load() != 0 {
		t.Fatalf("broken secret upload connected to target %d times", connections.Load())
	}
	if _, statErr := os.Stat(filepath.FromSlash(target)); !os.IsNotExist(statErr) {
		t.Fatalf("broken secret upload must not create the target; stat error=%v", statErr)
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "upload-content" || rows[0].Status != "error" ||
		!strings.Contains(rows[0].Command, "secrets=BROKEN_SECRET") {
		t.Fatalf("broken secret audit row = %+v", rows)
	}
	auditText, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(auditText, []byte(corruptedValue)) || bytes.Contains(auditText, []byte("token: {{sshmgr-secret:")) {
		t.Fatalf("broken secret audit leaks value or template: %s", auditText)
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
