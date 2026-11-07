package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

// localChangeHintSession 直接在本地 vault 上构造单机 MCP 服务，并用真实
// in-memory MCP 会话调用工具。这里是单机变更提示的测试入口：测试只看工具结果，
// 不进入提示来源内部。
func localChangeHintSession(t *testing.T, st *store.Store, profileID string) *mcp.ClientSession {
	t.Helper()
	server, tunnels, tasks, err := newLocalVaultServer(st, profileID, "proj-local-change")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
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
	return clientSession
}

func localChangeHintTexts(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_servers", Arguments: map[string]any{},
	})
	if err != nil || result.IsError {
		t.Fatalf("list_servers 失败:error=%v,结果=%+v", err, result)
	}
	var texts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return texts
}

// TestLocalVaultChangeHint_ServerAddedAndShownOnce: 单机路径第一次工具调用
// 只记基线；owner 在两次调用之间新增并授权服务器后，下一次结果开头出现
// 与多机一致的变更提示，同一会话后续调用不再重复。
func TestLocalVaultChangeHint_ServerAddedAndShownOnce(t *testing.T) {
	st := newStore(t)
	firstID, err := st.AddServer(&models.Server{
		Name: "first", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{firstID}); err != nil {
		t.Fatal(err)
	}
	session := localChangeHintSession(t, st, profileID)

	first := localChangeHintTexts(t, session)
	if len(first) != 1 || !strings.HasPrefix(first[0], `{"servers":`) {
		t.Fatalf("第一次工具调用不应有提示，实际为 %#v", first)
	}
	before, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := st.AddServer(&models.Server{
		Name: "second", Host: "192.0.2.11", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{secondID}); err != nil {
		t.Fatal(err)
	}
	after, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 second", before, after)
	second := localChangeHintTexts(t, session)
	if len(second) != 2 || second[0] != want || !strings.HasPrefix(second[1], `{"servers":`) {
		t.Fatalf("新增服务器后的工具结果 = %#v，开头应为 %q 再接原结果", second, want)
	}
	third := localChangeHintTexts(t, session)
	if len(third) != 1 || !strings.HasPrefix(third[0], `{"servers":`) {
		t.Fatalf("同一会话里变更提示只应出现一次，实际为 %#v", third)
	}
}

// TestLocalVaultChangeHint_ServerDeleted: owner 删除当前 profile 可见的
// 服务器后，下一次工具调用开头报告删除的服务器名。
func TestLocalVaultChangeHint_ServerDeleted(t *testing.T) {
	st := newStore(t)
	keptID, err := st.AddServer(&models.Server{
		Name: "kept", Host: "192.0.2.20", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	removedID, err := st.AddServer(&models.Server{
		Name: "removed", Host: "192.0.2.21", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{keptID, removedID}); err != nil {
		t.Fatal(err)
	}
	session := localChangeHintSession(t, st, profileID)
	if texts := localChangeHintTexts(t, session); len(texts) != 1 {
		t.Fatalf("第一次工具调用不应有提示，实际为 %#v", texts)
	}

	before, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(removedID); err != nil {
		t.Fatal(err)
	}
	after, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除服务器 removed", before, after)
	texts := localChangeHintTexts(t, session)
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("删除服务器后的工具结果 = %#v，开头应为 %q 再接原结果", texts, want)
	}
}

// TestLocalVaultChangeHint_CommandPolicyChanged: owner 修改命令策略后，
// 下一次工具调用开头出现与多机路径相同的固定提示。
func TestLocalVaultChangeHint_CommandPolicyChanged(t *testing.T) {
	st := newStore(t)
	profileID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	session := localChangeHintSession(t, st, profileID)
	if texts := localChangeHintTexts(t, session); len(texts) != 1 {
		t.Fatalf("第一次工具调用不应有提示，实际为 %#v", texts)
	}

	before, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	err = st.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []models.ShellType{models.ShellPOSIX},
		Commands: []string{"reboot"}, Disposition: policy.DispositionBlock,
		Explanation: "重启由 owner 安排",
		Alternative: "向 owner 申请重启时间窗口",
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：命令策略已变化，此前被拒绝或允许的命令可能不同",
		before, after,
	)
	texts := localChangeHintTexts(t, session)
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("命令策略变化后的工具结果 = %#v，开头应为 %q 再接原结果", texts, want)
	}
}

// TestLocalVaultChangeHint_TopologyChanged: owner 修改网络拓扑后，单机部署
// 的下一次工具调用复用默认比较器，开头出现同一条拓扑变化提示。
func TestLocalVaultChangeHint_TopologyChanged(t *testing.T) {
	st := newStore(t)
	serverID, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("办公室", "原备注"); err != nil {
		t.Fatal(err)
	}
	session := localChangeHintSession(t, st, profileID)
	if texts := localChangeHintTexts(t, session); len(texts) != 1 {
		t.Fatalf("第一次工具调用不应有提示，实际为 %#v", texts)
	}

	before, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	note := "新备注"
	if _, err := st.EditSite("办公室", store.SiteChanges{Note: &note}); err != nil {
		t.Fatal(err)
	}
	after, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：网络拓扑已变化，list_servers 的 site、networks、device 字段可能不同",
		before, after,
	)
	texts := localChangeHintTexts(t, session)
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("拓扑变化后的工具结果 = %#v，开头应为 %q 再接原结果", texts, want)
	}
}

// TestLocalVaultChangeHint_OtherProfileOnlyChangeDoesNotHint: 快照修订号
// 因其他 profile 的配置变化而前进时，如果当前 profile 可见的服务器与
// 命令策略都没变，不产生变更提示。
func TestLocalVaultChangeHint_OtherProfileOnlyChangeDoesNotHint(t *testing.T) {
	st := newStore(t)
	currentServerID, err := st.AddServer(&models.Server{
		Name: "current", Host: "192.0.2.30", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	currentProfileID, err := st.AddProfile("current")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(currentProfileID, []string{currentServerID}); err != nil {
		t.Fatal(err)
	}
	otherProfileID, err := st.AddProfile("other")
	if err != nil {
		t.Fatal(err)
	}
	session := localChangeHintSession(t, st, currentProfileID)
	if texts := localChangeHintTexts(t, session); len(texts) != 1 {
		t.Fatalf("第一次工具调用不应有提示，实际为 %#v", texts)
	}

	otherServerID, err := st.AddServer(&models.Server{
		Name: "other-only", Host: "192.0.2.31", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(otherProfileID, []string{otherServerID}); err != nil {
		t.Fatal(err)
	}
	if texts := localChangeHintTexts(t, session); len(texts) != 1 || !strings.HasPrefix(texts[0], `{"servers":`) {
		t.Fatalf("其他 profile 独有变化不应产生提示，实际为 %#v", texts)
	}
}

// TestLocalVaultChangeHint_AttachedToFailedToolResult: 工具本身失败时，
// 变更提示仍附在失败结果最前面，不会被失败路径吞掉。
func TestLocalVaultChangeHint_AttachedToFailedToolResult(t *testing.T) {
	st := newStore(t)
	serverID, err := st.AddServer(&models.Server{
		Name: "server", Host: "192.0.2.40", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	session := localChangeHintSession(t, st, profileID)
	if texts := localChangeHintTexts(t, session); len(texts) != 1 {
		t.Fatalf("第一次工具调用不应有提示，实际为 %#v", texts)
	}

	before, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	addedID, err := st.AddServer(&models.Server{
		Name: "new-server", Host: "192.0.2.41", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{addedID}); err != nil {
		t.Fatal(err)
	}
	after, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": "does-not-exist", "command": "uptime"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("无效服务器的 exec_command 应为失败工具结果，实际为 %+v", result)
	}
	var texts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 new-server", before, after)
	if len(texts) != 2 || texts[0] != want {
		t.Fatalf("失败工具结果最前应附变更提示 %q，实际为 %#v", want, texts)
	}
}
