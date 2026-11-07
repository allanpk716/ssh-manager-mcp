package mcpserver

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/testsshd"
)

// TestCacheExecSelectsPhysicalNetworkAddress: 客户端缓存模式经权威端裁剪快照
// 拿到拓扑与本机设备标识后，exec_command 必须先选中共同物理局域网里的真实
// 地址，而不是条目的兜底测试地址。
func TestCacheExecSelectsPhysicalNetworkAddress(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "absent-store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	var connections atomic.Int64
	fixtureAddr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", Connections: &connections,
		Exec: func(command string, _ io.Reader) (string, string, int) {
			return "SELECTED:" + command + "\n", "", 0
		},
	})
	defer cleanup()

	st := newStore(t)
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	laptopTokenID, _, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("笔记本", "", "laptop"); err != nil {
		t.Fatal(err)
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	serverResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站 SSH", Host: "192.0.2.10", Port: portOfAddr(fixtureAddr), User: "u",
	}, loginCredential, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer(serverResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(srv, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{srv.ID}); err != nil {
		t.Fatal(err)
	}
	// 先登记虚拟局域网，再登记物理局域网：排序必须按网络类型优先，而不是按
	// 登记顺序把虚拟地址放在前面。
	for _, membership := range []struct {
		device, network, address string
	}{
		{"工作站", "虚拟局域网", "198.51.100.5"},
		{"工作站", "家A局域网", hostOfAddr(fixtureAddr)},
		{"笔记本", "虚拟局域网", "198.51.100.6"},
		{"笔记本", "家A局域网", "127.0.0.2"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	_, projectToken, err := st.AddProject("proj", profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshotForProfile(profileID, false, laptopTokenID)
	if err != nil {
		t.Fatal(err)
	}

	broker, tunnels, tasks, closeBroker, err := NewCacheBroker(
		projectToken, snapshot, filepath.Join(t.TempDir(), "audit.log"), nil, nil, "laptop", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeBroker()
		tunnels.CloseAll()
		tasks.CloseAll()
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := broker.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: BrokerTools[1], Arguments: map[string]any{"server_id": srv.ID, "command": "topology"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output ExecOutput
	unmarshalToolJSON(t, result, &output)
	if result.IsError || output.Stdout != "SELECTED:topology\n" {
		t.Fatalf("exec result is error=%v stdout=%q, want success through the physical address", result.IsError, output.Stdout)
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok && strings.Contains(text.Text, `"connection_notes"`) {
			t.Fatalf("connection notes must be absent when no address was skipped: %s", text.Text)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("fixture connections = %d, want exactly the physical address attempt", connections.Load())
	}
}

// TestCacheExecReportsSkippedHostKeyMismatch: 第一个候选地址的主机密钥与条
// 目锚不一致时，工具继续用兜底地址执行，并把不泄露地址的连接说明放进结构
// 化输出；被跳过的地址没有收到登录认证请求。
func TestCacheExecReportsSkippedHostKeyMismatch(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "absent-store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	var skippedAuthRequests atomic.Int64
	correctAddr, correctHostKey, correctCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(command string, _ io.Reader) (string, string, int) {
			return "EXECUTED:" + command + "\n", "", 0
		},
	})
	defer correctCleanup()
	wrongAddr, _, wrongCleanup := testsshd.Start(t, testsshd.Options{
		ListenHost: "::1", ListenPort: portOfAddr(correctAddr), Password: "pw",
		AuthRequests: &skippedAuthRequests,
	})
	defer wrongCleanup()

	st := newStore(t)
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	laptopTokenID, _, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("笔记本", "", "laptop"); err != nil {
		t.Fatal(err)
	}
	serverResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站 SSH", Host: hostOfAddr(correctAddr), Port: portOfAddr(correctAddr), User: "u",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer(serverResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(srv, correctHostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{srv.ID}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"工作站", "家A局域网", hostOfAddr(wrongAddr)},
		{"笔记本", "家A局域网", "127.0.0.2"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	_, projectToken, err := st.AddProject("proj", profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshotForProfile(profileID, false, laptopTokenID)
	if err != nil {
		t.Fatal(err)
	}

	broker, tunnels, tasks, closeBroker, err := NewCacheBroker(
		projectToken, snapshot, filepath.Join(t.TempDir(), "audit.log"), nil, nil, "laptop", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeBroker()
		tunnels.CloseAll()
		tasks.CloseAll()
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := broker.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: BrokerTools[1], Arguments: map[string]any{"server_id": srv.ID, "command": "topology"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output ExecOutput
	unmarshalToolJSON(t, result, &output)
	if result.IsError || output.Stdout != "EXECUTED:topology\n" {
		t.Fatalf("exec result is error=%v stdout=%q, want success through the fallback address", result.IsError, output.Stdout)
	}
	wantNotes := []string{"家A局域网 第1个地址呈现了不同的主机密钥，已跳过"}
	if strings.Join(output.ConnectionNotes, "\n") != strings.Join(wantNotes, "\n") {
		t.Fatalf("connection notes = %q, want %q", output.ConnectionNotes, wantNotes)
	}
	if n := skippedAuthRequests.Load(); n != 0 {
		t.Fatalf("skipped fixture authentication requests = %d, want 0", n)
	}
}
