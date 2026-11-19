package mcpserver

// query_topology 输出多级地点完整路径与机器地点不一致提示的测试。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func newQueryTopologyLocationFixture(t *testing.T) *queryTopologyFixture {
	t.Helper()
	vaultDirectory := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDirectory, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDirectory, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	company, err := st.AddSiteWithParent("公司", "", "")
	if err != nil {
		t.Fatal(err)
	}
	factory, err := st.AddSiteWithParent("一厂区", "", company.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("3 号楼", "", factory.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("二厂区", "", company.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "办公网", Type: models.NetworkPhysical, SiteName: "公司 / 一厂区",
	}); err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDeviceAtSite("笔记本", "", "", "公司 / 二厂区")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("机房机器", "", "", "公司 / 一厂区 / 3 号楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("走错楼机器", "", "", "公司 / 二厂区"); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"笔记本", "办公网", "192.0.2.1"},
		{"机房机器", "办公网", "192.0.2.2"},
		{"走错楼机器", "办公网", "192.0.2.3"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	profileID, err := st.AddProfile("query-location")
	if err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(st, profileID, "project-query-location")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
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
	return &queryTopologyFixture{store: st, profileID: profileID, session: clientSession}
}

// TestQueryTopologyOutputsLocationPathsAndMismatch 覆盖分组视图里的完整地
// 点路径与「机器地点不在网络覆盖范围内」的提示。
func TestQueryTopologyOutputsLocationPathsAndMismatch(t *testing.T) {
	fixture := newQueryTopologyLocationFixture(t)

	text, isError := fixture.callQuery(t, map[string]any{"from": "self"})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	for _, want := range []string{
		"出发点：笔记本（地点：公司 / 二厂区）",
		"办公网（物理局域网，覆盖地点：公司 / 一厂区）",
		"地点不一致：机器地点为公司 / 二厂区；办公网覆盖公司 / 一厂区",
		"走错楼机器（地点不一致：机器在公司 / 二厂区；办公网覆盖公司 / 一厂区）",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("query_topology 输出缺少 %q：\n%s", want, text)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "机房机器") && strings.Contains(line, "地点不一致") {
			t.Fatalf("机器地点被网络覆盖时不应提示不一致：%s", line)
		}
	}
}
