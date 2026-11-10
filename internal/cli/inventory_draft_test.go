package cli

// 票 #188：声明草稿的 owner 命令行——列出、确认、修改后确认与驳回。草稿
// 由 agent 工具提交；这里的命令只消费草稿，不新增提交入口。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// addInventoryDraft 直接向存储层放一条草稿，模拟 agent 工具的产物。
func addInventoryDraft(t *testing.T, device, name, version, observedKey string) string {
	t.Helper()
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: device, Category: models.SoftwareCategoryServiceProgram,
			Name: name, Version: version,
		},
		ClaimedObservedKey: observedKey,
	}}, store.AllSoftwareDevices(), store.AgentSource())
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

// TestInventoryDraftCLILifecycle 覆盖草稿的列出、确认、修改后确认与驳回，
// 以及各命令的审计来源。
func TestInventoryDraftCLILifecycle(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "工作站")
	mustInventoryCli(t, "", "devices", "add", "网络存储")
	firstID := addInventoryDraft(t, "工作站", "nginx", "1.25.3", "")
	secondID := addInventoryDraft(t, "网络存储", "NAS 系统", "7.2.1", "")
	_ = secondID

	listing := mustInventoryCli(t, "", "inventory", "drafts")
	for _, want := range []string{firstID, "工作站", "nginx", "1.25.3", "网络存储", "NAS 系统"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("草稿列表缺少 %q：\n%s", want, listing)
		}
	}
	if !strings.Contains(listing, "草稿") {
		t.Fatalf("草稿列表应有标题：\n%s", listing)
	}

	jsonLines := mustInventoryCli(t, "", "inventory", "drafts", "--json")
	lines := strings.Split(strings.TrimSpace(jsonLines), "\n")
	if len(lines) != 2 {
		t.Fatalf("drafts --json 应每个草稿一行：%q", jsonLines)
	}
	var firstRow map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &firstRow); err != nil {
		t.Fatalf("解析草稿 JSON 失败：%v\n%s", err, lines[0])
	}
	if firstRow["id"] != firstID || firstRow["status"] != "draft" {
		t.Fatalf("草稿 JSON 字段不完整：%v", firstRow)
	}

	// 修改后确认：版本与备注改完才生效。
	output := mustInventoryCli(t, "", "inventory", "draft-confirm", firstID,
		"--version", "1.27.0", "--note", "owner 修订过版本")
	if !strings.Contains(output, firstID) || !strings.Contains(output, "确认") {
		t.Fatalf("确认输出 = %q", output)
	}
	view := mustInventoryCli(t, "", "inventory", "view", "工作站")
	if !strings.Contains(view, "nginx") || !strings.Contains(view, "1.27.0") ||
		!strings.Contains(view, "owner 修订过版本") {
		t.Fatalf("修改后确认的声明应生效：\n%s", view)
	}

	// 驳回：草稿消失，不进入清单。
	rejectOutput := mustInventoryCli(t, "", "inventory", "draft-reject", secondID)
	if !strings.Contains(rejectOutput, secondID) || !strings.Contains(rejectOutput, "已驳回") {
		t.Fatalf("驳回输出 = %q", rejectOutput)
	}
	if view := mustInventoryCli(t, "", "inventory", "view", "网络存储"); strings.Contains(view, "NAS 系统") {
		t.Fatalf("驳回的草稿不应进入清单：\n%s", view)
	}
	remaining := mustInventoryCli(t, "", "inventory", "drafts")
	if strings.Contains(remaining, firstID) || strings.Contains(remaining, secondID) {
		t.Fatalf("处理完的草稿应从列表消失：\n%s", remaining)
	}

	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{
		"software-declaration-confirm", "software-declaration-confirm-edit",
		"software-declaration-reject",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("确认与驳回都应写审计：%+v", rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceCLI {
			t.Fatalf("命令行确认与驳回的审计来源应为命令行：%+v", row)
		}
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[1].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["name"] != "nginx" || summary["fields"] == nil {
		t.Fatalf("修改后确认的审计应记录修改字段：%v", summary)
	}
}

// TestInventoryDraftConfirmUnknownID 未知的草稿编号应报错，退出码为一般错
// 误。
func TestInventoryDraftConfirmUnknownID(t *testing.T) {
	newSecretsEnv(t)
	if _, _, err := runInventoryRoot(t, "", "inventory", "draft-confirm", "no-such-draft"); err == nil {
		t.Fatal("未知草稿编号应报错")
	}
	if _, _, err := runInventoryRoot(t, "", "inventory", "draft-reject", "no-such-draft"); err == nil {
		t.Fatal("未知草稿编号驳回应报错")
	}
	_ = softwareinventory.CLISource()
}
