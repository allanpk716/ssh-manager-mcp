package mcpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
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

func TestUploadContentClientCacheFillsOfflineAndUsesRefreshedValue(t *testing.T) {
	const oldValue = "offline-old-secret-25df70"
	const newValue = "offline-new-secret-97ba31"
	authority := newStore(t)
	var connections atomic.Int64
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Connections: &connections})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, authority, "real", addr, hostKey, "")
	if err := authority.AddSecret("OFFLINE_TOKEN", "", []byte(oldValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := authority.AllowSecrets([]string{"OFFLINE_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := authority.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	_, projectToken, err := authority.AddProject("客户端项目", profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := authority.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	currentRevision := snapshot.SnapshotRevision
	reload := func() (*store.Snapshot, bool, error) {
		fresh, err := authority.ExportSnapshotForProfile(profileID, true, "")
		if err != nil {
			return nil, false, err
		}
		if fresh.SnapshotRevision == currentRevision {
			return nil, false, nil
		}
		currentRevision = fresh.SnapshotRevision
		return fresh, true, nil
	}
	auditPath := filepath.Join(t.TempDir(), "upload-content-audit.jsonl")
	broker, tunnels, tasks, _, brokerCleanup, err := NewCacheBrokerWithRefresher(
		projectToken, snapshot, auditPath, reload, nil, "laptop", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(brokerCleanup)
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, broker)

	firstTarget := toSlash(filepath.Join(t.TempDir(), "first.yaml"))
	firstResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     "token: {{sshmgr-secret:OFFLINE_TOKEN}}\n",
			"remote_path": firstTarget,
		},
	})
	if err != nil || firstResult.IsError {
		t.Fatalf("离线客户端第一次填值上传失败：err=%v result=%q", err, toolResultText(firstResult))
	}
	firstBytes, err := os.ReadFile(filepath.FromSlash(firstTarget))
	if err != nil || string(firstBytes) != "token: "+oldValue+"\n" {
		t.Fatalf("第一次客户端缓存写入 = %q, %v", firstBytes, err)
	}
	assertUploadContentResultHasNoSecretValue(t, firstResult, oldValue, newValue)

	if err := authority.SetSecretValue("OFFLINE_TOKEN", []byte(newValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	secondTarget := toSlash(filepath.Join(t.TempDir(), "second.yaml"))
	secondResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     "token: {{sshmgr-secret:OFFLINE_TOKEN}}\n",
			"remote_path": secondTarget,
		},
	})
	if err != nil || secondResult.IsError {
		t.Fatalf("快照刷新后的客户端填值上传失败：err=%v result=%q", err, toolResultText(secondResult))
	}
	secondBytes, err := os.ReadFile(filepath.FromSlash(secondTarget))
	if err != nil || string(secondBytes) != "token: "+newValue+"\n" {
		t.Fatalf("快照刷新后的客户端缓存写入 = %q, %v", secondBytes, err)
	}
	assertUploadContentResultHasNoSecretValue(t, secondResult, oldValue, newValue)
	if connections.Load() != 2 {
		t.Fatalf("目标服务器连接次数 = %d，应为 2", connections.Load())
	}

	auditText, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(auditText, []byte(oldValue)) || bytes.Contains(auditText, []byte(newValue)) {
		t.Fatalf("客户端离线审计泄露机密值：%s", auditText)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(auditText)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("解析客户端审计行失败：%v（%s）", err, line)
		}
		if row["action"] != "upload-content" || row["status"] != "ok" ||
			!strings.Contains(fmt.Sprint(row["command"]), "secrets=OFFLINE_TOKEN") {
			t.Fatalf("客户端填值上传审计行 = %#v", row)
		}
	}
}

func TestUploadContentOldAuthoritySnapshotRefusesPlaceholdersBeforeConnect(t *testing.T) {
	const cachedValue = "old-authority-secret-61c94b"
	authority := newStore(t)
	var connections atomic.Int64
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Connections: &connections})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, authority, "real", addr, hostKey, "")
	if err := authority.AddSecret("CACHED_TOKEN", "", []byte(cachedValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := authority.AllowSecrets([]string{"CACHED_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := authority.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	_, projectToken, err := authority.AddProject("客户端项目", profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := authority.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.RecommendedVersion = ""
	auditPath := filepath.Join(t.TempDir(), "upload-content-audit.jsonl")
	broker, tunnels, tasks, _, brokerCleanup, err := NewCacheBrokerWithRefresher(
		projectToken, snapshot, auditPath, nil, nil, "laptop", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(brokerCleanup)
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, broker)

	upload := func(content, target string) (string, bool) {
		t.Helper()
		result, callErr := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "upload_content",
			Arguments: map[string]any{
				"server_id": serverID, "content": content, "remote_path": target,
			},
		})
		if callErr != nil {
			t.Fatal(callErr)
		}
		assertUploadContentResultHasNoSecretValue(t, result, cachedValue)
		return toolResultText(result), result.IsError
	}
	want := "权威端版本过旧，不支持机密项，请先升级权威端"
	rejectedDirectory := t.TempDir()
	legalTarget := toSlash(filepath.Join(rejectedDirectory, "legal.yaml"))
	legalText, legalFailed := upload("token: {{sshmgr-secret:CACHED_TOKEN}}\n", legalTarget)
	if !legalFailed || legalText != want {
		t.Fatalf("合法占位符在旧权威端快照下的结果 = %q, failed=%v", legalText, legalFailed)
	}
	invalidTarget := toSlash(filepath.Join(rejectedDirectory, "invalid.yaml"))
	invalidText, invalidFailed := upload("broken {{sshmgr-secret:BAD NAME}}\n", invalidTarget)
	if !invalidFailed || invalidText != want {
		t.Fatalf("非法占位符前缀在旧权威端快照下的结果 = %q, failed=%v", invalidText, invalidFailed)
	}
	if _, statErr := os.Stat(filepath.FromSlash(legalTarget)); !os.IsNotExist(statErr) {
		t.Fatalf("旧权威端拒绝不应创建目标文件；stat error=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.FromSlash(invalidTarget)); !os.IsNotExist(statErr) {
		t.Fatalf("旧权威端拒绝不应创建非法占位符目标文件；stat error=%v", statErr)
	}

	plainTarget := toSlash(filepath.Join(t.TempDir(), "plain.txt"))
	plainText, plainFailed := upload("plain content\n", plainTarget)
	if plainFailed {
		t.Fatalf("不含占位符的内容在旧权威端快照下应照常上传，实际报错：%q", plainText)
	}
	plainBytes, err := os.ReadFile(filepath.FromSlash(plainTarget))
	if err != nil || string(plainBytes) != "plain content\n" {
		t.Fatalf("普通上传目标内容 = %q, %v", plainBytes, err)
	}
	if connections.Load() != 1 {
		t.Fatalf("目标服务器连接次数 = %d，两次拒绝后应只有普通上传连接 1 次", connections.Load())
	}

	auditText, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(auditText, []byte(cachedValue)) {
		t.Fatalf("客户端离线审计泄露机密值：%s", auditText)
	}
	auditLines := strings.Split(strings.TrimSpace(string(auditText)), "\n")
	if len(auditLines) != 3 {
		t.Fatalf("客户端离线审计行数 = %d，应为 3：%q", len(auditLines), auditText)
	}
	wantStatuses := []string{"denied", "denied", "ok"}
	for index, line := range auditLines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("解析客户端审计第 %d 行失败：%v（%s）", index+1, err, line)
		}
		if row["action"] != "upload-content" || row["status"] != wantStatuses[index] {
			t.Fatalf("客户端审计第 %d 行 = %#v，期待 action=upload-content 且 status=%s", index+1, row, wantStatuses[index])
		}
		if index == 0 && !strings.Contains(fmt.Sprint(row["command"]), "secrets=CACHED_TOKEN") {
			t.Fatalf("合法占位符的拒绝审计应记录名字：%#v", row)
		}
	}
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
	cacheAuditPath := filepath.Join(t.TempDir(), "upload-content-audit.jsonl")
	cacheAuditFile, err := os.OpenFile(cacheAuditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cacheAuditFile.Close() })
	clientCache.SetReadOnly(cacheAuditFile)
	clientCache.SetCacheRecommendedVersion(snapshot.RecommendedVersion)
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
	for _, row := range rows {
		localAuditText, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(localAuditText, []byte(emptyListValue)) || bytes.Contains(localAuditText, []byte(visibleValue)) ||
			bytes.Contains(localAuditText, []byte(hiddenValue)) || bytes.Contains(localAuditText, []byte("empty: {{sshmgr-secret:")) {
			t.Fatalf("本地拒绝审计泄露值或模板：%s", localAuditText)
		}
	}
	cacheAuditText, err := os.ReadFile(cacheAuditPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cacheAuditText, []byte(emptyListValue)) || bytes.Contains(cacheAuditText, []byte(visibleValue)) ||
		bytes.Contains(cacheAuditText, []byte(hiddenValue)) || bytes.Contains(cacheAuditText, []byte("empty: {{sshmgr-secret:")) {
		t.Fatalf("客户端离线拒绝审计泄露值或模板：%s", cacheAuditText)
	}
	cacheAuditLines := strings.Split(strings.TrimSpace(string(cacheAuditText)), "\n")
	if len(cacheAuditLines) != 1 {
		t.Fatalf("客户端离线拒绝审计行数 = %d，应为 1：%q", len(cacheAuditLines), cacheAuditText)
	}
	var cacheAuditRow map[string]any
	if err := json.Unmarshal([]byte(cacheAuditLines[0]), &cacheAuditRow); err != nil {
		t.Fatalf("解析客户端离线拒绝审计失败：%v（%s）", err, cacheAuditLines[0])
	}
	if cacheAuditRow["action"] != "upload-content" || cacheAuditRow["status"] != "denied" ||
		!strings.Contains(fmt.Sprint(cacheAuditRow["command"]), "secrets=HIDDEN_SECRET") {
		t.Fatalf("客户端离线拒绝审计行 = %#v", cacheAuditRow)
	}
	if cacheRows, err := clientCache.AuditRows(10); err != nil || len(cacheRows) != 0 {
		t.Fatalf("客户端缓存数据库审计行 = %d (err=%v)，应为 0", len(cacheRows), err)
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
		!strings.Contains(rows[0].Command, "inline 73 bytes -> "+overTarget) ||
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

func TestUploadContentBase64SizeChecksFilledContent(t *testing.T) {
	const secretValue = "12345678"
	st := newStore(t)
	if err := st.AddSecret("LONG_NAME", "", []byte(secretValue), store.CLISource()); err != nil {
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
	if err := st.AllowSecrets([]string{"LONG_NAME"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHMGR_UPLOAD_CONTENT_MAX", "20")
	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)

	shortTemplate := []byte("x: {{sshmgr-secret:LONG_NAME}}")
	target := toSlash(filepath.Join(t.TempDir(), "config", "filled.yaml"))
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     base64.StdEncoding.EncodeToString(shortTemplate),
			"remote_path": target,
			"encoding":    "base64",
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("filled Base64 content under cap failed: err=%v result=%q", err, toolResultText(result))
	}
	got, err := os.ReadFile(filepath.FromSlash(target))
	if err != nil || string(got) != "x: "+secretValue {
		t.Fatalf("filled Base64 target = %q, %v", got, err)
	}

	invalidTemplate := []byte("{{sshmgr-secret:LONG_NAME}}{{sshmgr-secret:BAD NAME}}")
	rejected, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     base64.StdEncoding.EncodeToString(invalidTemplate),
			"remote_path": toSlash(filepath.Join(t.TempDir(), "config", "rejected.yaml")),
			"encoding":    "base64",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rejected.IsError {
		t.Fatalf("over-cap Base64 content unexpectedly succeeded: %+v", rejected.Content)
	}
	errorText := toolResultText(rejected)
	if !strings.Contains(errorText, "机密项占位符格式错误") ||
		!strings.Contains(errorText, "content (34 bytes) exceeds upload-content cap 20") {
		t.Fatalf("Base64 rejection must list format and filled-size errors together: %q", errorText)
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
