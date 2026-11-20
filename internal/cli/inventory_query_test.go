package cli

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// TestInventoryViewAndSearchIncludeObservedOnlyItems 命令行的查看与搜索也
// 走实测加声明的统一查询；没有任何声明的软件能被查到。
func TestInventoryViewAndSearchIncludeObservedOnlyItems(t *testing.T) {
	dbPath, masterKey := newSecretsEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	item := models.SoftwareObservedItem{
		Category: softwareinventory.CategoryServiceProgram,
		Name:     "observed-only", Version: "2.0",
		CollectedAt: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items:             []models.SoftwareObservedItem{item},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	view := mustInventoryCli(t, "", "inventory", "view", "工作站")
	if !strings.Contains(view, "observed-only 2.0（来源：实测）") {
		t.Fatalf("按设备查看应包含只有实测的条目：\n%s", view)
	}
	search := mustInventoryCli(t, "", "inventory", "search", "OBSERVED")
	if !strings.Contains(search, "工作站") || !strings.Contains(search, "来源：实测") {
		t.Fatalf("跨设备搜索应包含只有实测的条目：\n%s", search)
	}
}

// TestInventorySearchAcceptsExactVersionSiteAndNetwork 命令行搜索与领域查
// 询使用同一组条件；结果只来自实测条目。
func TestInventorySearchAcceptsExactVersionSiteAndNetwork(t *testing.T) {
	dbPath, masterKey := newSecretsEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("总部", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("机房", "", "总部"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "总部物理局域网", Type: models.NetworkPhysical, SiteName: "总部",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "跨地点虚拟局域网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	site := "机房"
	if _, err := st.EditDevice("工作站", store.DeviceChanges{Site: &site}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "总部物理局域网", "192.0.2.20"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: softwareinventory.CategorySystemPackage, Name: "openssl", Version: "3.0.2-0ubuntu1.18"},
			{Category: softwareinventory.CategorySystemPackage, Name: "openssl-tools", Version: "3.0.13-0ubuntu3.4"},
		},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := softwareinventory.New(st).AddDeclaration(softwareinventory.Declaration{
		Device: "工作站", Category: softwareinventory.CategorySystemPackage,
		Name: "openssl", Version: "3.0.2-0ubuntu1.18",
	}, softwareinventory.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	output := mustInventoryCli(t, "",
		"inventory", "search", "OPENSSL", "--version", " 3.0.2-0ubuntu1.18 ",
		"--site", "总部", "--network", "总部物理局域网")
	if strings.Count(output, "openssl") != 1 || !strings.Contains(output, "3.0.2-0ubuntu1.18") ||
		strings.Contains(output, "3.0.13-0ubuntu3.4") {
		t.Fatalf("命令行精确版本与地点网络筛选失败：\n%s", output)
	}
	virtual := mustInventoryCli(t, "",
		"inventory", "search", "openssl", "--version", "3.0.2-0ubuntu1.18",
		"--site", "总部", "--network", "跨地点虚拟局域网")
	if !strings.Contains(virtual, "没有匹配的软件条目。") {
		t.Fatalf("设备不是虚拟局域网成员时不应有结果：\n%s", virtual)
	}
}
