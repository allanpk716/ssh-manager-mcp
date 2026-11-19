package cli

// `sshmgr sites` / `devices` / `networks` 命令里多级地点相关的行为测试。

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func seedSiteTree(t *testing.T) {
	t.Helper()
	runCli(t, "sites", "add", "公司", "--note", "总部")
	runCli(t, "sites", "add", "一厂区", "--parent", "公司")
	runCli(t, "sites", "add", "3 号楼", "--parent", "公司 / 一厂区")
	runCli(t, "sites", "add", "家")
}

// TestSitesTreeCLI 覆盖 --parent 的登记、同名不同上级、移动、清空上级、
// 成环拒绝与树形列表。
func TestSitesTreeCLI(t *testing.T) {
	withCliStoreEnv(t)
	seedSiteTree(t)

	out := runCli(t, "sites", "add", "3 号楼", "--parent", "家")
	if !strings.Contains(out, "已登记地点 家 / 3 号楼") {
		t.Fatalf("add must confirm the full location path:\n%s", out)
	}

	errText := runCliErr(t, "sites", "add", "一厂区", "--parent", "公司")
	if !strings.Contains(errText, "地点名字已存在") {
		t.Fatalf("duplicate name under one parent must be refused, got:\n%s", errText)
	}

	errText = runCliErr(t, "sites", "edit", "公司", "--parent", "公司 / 一厂区 / 3 号楼")
	if !strings.Contains(errText, "地点循环") && !strings.Contains(errText, "形成地点循环") {
		t.Fatalf("making a parent loop must be refused, got:\n%s", errText)
	}

	runCli(t, "networks", "add", "办公网", "--type", "physical", "--site", "公司 / 一厂区 / 3 号楼",
		"--cidr", "10.0.0.0/24")
	out = runCli(t, "sites", "list")
	company := strings.Index(out, "地点 公司")
	factory := strings.Index(out, "地点 一厂区")
	building := strings.Index(out, "地点 3 号楼")
	if company < 0 || factory < 0 || building < 0 || !(company < factory && factory < building) {
		t.Fatalf("list must render the location tree in order:\n%s", out)
	}
	if !strings.Contains(out, "    物理局域网：办公网（网段 10.0.0.0/24）") {
		t.Fatalf("networks must be listed under their covering location:\n%s", out)
	}

	out = runCli(t, "sites", "edit", "公司 / 一厂区 / 3 号楼", "--parent", "")
	if !strings.Contains(out, "已修改地点") {
		t.Fatalf("clearing the parent must be accepted, got:\n%s", out)
	}
	out = runCli(t, "sites", "list")
	if !strings.HasPrefix(out, "地点 3 号楼 — ") && !strings.Contains(out, "\n地点 3 号楼 — ") {
		t.Fatalf("cleared parent must move the site to the root level:\n%s", out)
	}

	runCli(t, "sites", "add", "独立楼", "--parent", "公司 / 一厂区")
	runCli(t, "sites", "edit", "独立楼", "--parent", "")
	out = runCli(t, "sites", "edit", "独立楼", "--parent", "公司 / 一厂区", "--note", "重新挂回")
	if !strings.Contains(out, "已修改地点") {
		t.Fatalf("moving a site by full path must be accepted, got:\n%s", out)
	}
}

// TestDevicesSiteCLI 覆盖机器直接登记地点、修改、清空与详情里的完整路
// 径显示。
func TestDevicesSiteCLI(t *testing.T) {
	withCliStoreEnv(t)
	seedSiteTree(t)
	runCli(t, "networks", "add", "办公网", "--type", "physical", "--site", "公司 / 一厂区 / 3 号楼")

	out := runCli(t, "devices", "add", "工作站", "--site", "公司 / 一厂区 / 3 号楼")
	if !strings.Contains(out, "已登记设备 工作站") {
		t.Fatalf("device add must succeed:\n%s", out)
	}
	errText := runCliErr(t, "devices", "add", "另一台", "--site", "不存在")
	if !strings.Contains(errText, "地点不存在") {
		t.Fatalf("unknown device location must be refused, got:\n%s", errText)
	}

	runCli(t, "devices", "member", "add", "工作站", "--network", "办公网", "--address", "10.0.0.5")
	out = runCli(t, "devices", "show", "工作站")
	if !strings.Contains(out, "地点：公司 / 一厂区 / 3 号楼") {
		t.Fatalf("device show must render the full location path:\n%s", out)
	}
	if !strings.Contains(out, "办公网（物理局域网，地点 公司 / 一厂区 / 3 号楼）") {
		t.Fatalf("membership line must render the covering location path:\n%s", out)
	}

	runCli(t, "devices", "edit", "工作站", "--site", "公司")
	out = runCli(t, "devices", "show", "工作站")
	if !strings.Contains(out, "地点：公司") {
		t.Fatalf("device edit must change the machine location:\n%s", out)
	}
	runCli(t, "devices", "edit", "工作站", "--site", "")
	out = runCli(t, "devices", "show", "工作站")
	if !strings.Contains(out, "地点：未登记") {
		t.Fatalf("empty --site must clear the machine location:\n%s", out)
	}
}

// TestNetworksCoveringSiteCLI 覆盖物理局域网按完整路径登记覆盖地点与列
// 表显示。
func TestNetworksCoveringSiteCLI(t *testing.T) {
	withCliStoreEnv(t)
	seedSiteTree(t)

	runCli(t, "networks", "add", "办公网", "--type", "physical", "--site", "公司 / 一厂区 / 3 号楼")
	out := runCli(t, "networks", "list")
	if !strings.Contains(out, "覆盖地点：公司 / 一厂区 / 3 号楼") {
		t.Fatalf("network list must render the covering location path:\n%s", out)
	}

	runCli(t, "networks", "edit", "办公网", "--site", "公司 / 一厂区")
	out = runCli(t, "networks", "list", "--site", "公司")
	if !strings.Contains(out, "覆盖地点：公司 / 一厂区") {
		t.Fatalf("network edit must accept a full covering location path:\n%s", out)
	}
}

// TestSiteTreeWritesAdvanceSnapshotRevision 钉住地点树的写入照常推进快照修
// 订号（供客户端重新拉取）。
func TestSiteTreeWritesAdvanceSnapshotRevision(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	s, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	runCli(t, "sites", "add", "公司", "--parent", "")
	runCli(t, "sites", "add", "3 号楼", "--parent", "公司")
	runCli(t, "devices", "add", "工作站", "--site", "公司 / 3 号楼")
	runCli(t, "networks", "add", "办公网", "--type", "physical", "--site", "公司 / 3 号楼")

	s, err = store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("site tree writes must advance the snapshot revision: before=%d after=%d", before, after)
	}
	tree := models.NewSiteTree(mustListSites(t, s))
	if got := tree.PathString(mustDeviceSiteID(t, s, "工作站")); got != "公司 / 3 号楼" {
		t.Fatalf("machine location path = %q", got)
	}
}

func mustListSites(t *testing.T, s *store.Store) []*models.Site {
	t.Helper()
	sites, err := s.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

func mustDeviceSiteID(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	device, err := s.GetDevice(name)
	if err != nil {
		t.Fatal(err)
	}
	return device.SiteID
}
