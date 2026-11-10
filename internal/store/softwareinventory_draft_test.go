package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// newDraftTestStore 造一个带一台受管设备的测试库。
func newDraftTestStore(t *testing.T) (*Store, *models.Server) {
	t.Helper()
	st := newCollectTestStore(t)
	server := addCollectServer(t, st, "工作站", "工作站入口")
	return st, server
}

// TestSoftwareDeclarationDraftLifecycle 覆盖草稿的一次多条提交、确认前
// 不影响查询、列出、确认、修改后确认与驳回，以及各写操作的审计。
func TestSoftwareDeclarationDraftLifecycle(t *testing.T) {
	st, server := newDraftTestStore(t)
	_ = server

	ids, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{
		{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "工作站", Category: models.SoftwareCategoryServiceProgram,
				Name: "nginx", Version: "1.25.3",
				ListeningPorts: []models.SoftwareListeningPort{{
					Address: "0.0.0.0", Port: 443, Protocol: "tcp",
					Exposure: models.SoftwareExposurePublic,
				}},
			},
		},
		{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "工作站", Category: models.SoftwareCategorySystemPackage,
				Name: "libc6", Version: "2.39-0ubuntu8",
			},
		},
	}, AllSoftwareDevices(), CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("一次提交两条草稿应返回两个不同的草稿编号：%v", ids)
	}

	// 草稿确认前不影响任何查询结果。
	inventory, err := st.SoftwareDeclarationsForDevice("工作站", AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Declarations) != 0 {
		t.Fatalf("草稿确认前不应出现在按设备查询里：%+v", inventory.Declarations)
	}
	search, err := st.SearchSoftwareDeclarations("nginx", AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(search) != 0 {
		t.Fatalf("草稿确认前不应出现在搜索里：%+v", search)
	}

	drafts, err := st.SoftwareDeclarationDrafts(AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 {
		t.Fatalf("列出草稿应看到两条：%+v", drafts)
	}
	var nginxDraft SoftwareDeclarationView
	for _, draft := range drafts {
		if draft.ID == ids[0] {
			nginxDraft = draft
		}
	}
	if nginxDraft.ID == "" || nginxDraft.Status != "draft" ||
		nginxDraft.Name != "nginx" || len(nginxDraft.ListeningPorts) != 1 ||
		nginxDraft.ListeningPorts[0].Exposure != models.SoftwareExposurePublic {
		t.Fatalf("草稿列表字段不完整：%+v", nginxDraft)
	}

	// 确认第一条：立即生效。
	confirmed, err := st.ConfirmSoftwareDeclarationDraft(ids[0], nil, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != "active" {
		t.Fatalf("确认后的声明应为生效状态：%+v", confirmed)
	}
	inventory, err = st.SoftwareDeclarationsForDevice("工作站", AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Declarations) != 1 || inventory.Declarations[0].Name != "nginx" {
		t.Fatalf("确认后声明应出现在查询里：%+v", inventory.Declarations)
	}

	// 修改后确认第二条：修改生效且仍为一条声明。
	version := "2.40-0ubuntu8"
	edited, err := st.ConfirmSoftwareDeclarationDraft(ids[1], &models.SoftwareDeclarationEdit{
		Version: &version,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if edited.Version != version || edited.Status != "active" {
		t.Fatalf("修改后确认应保存修改后的字段：%+v", edited)
	}
	drafts, err = st.SoftwareDeclarationDrafts(AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("全部确认后草稿列表应为空：%+v", drafts)
	}

	// 驳回：草稿消失，也不进入生效声明。
	rejectIDs, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "工作站", Category: models.SoftwareCategoryServiceProgram,
			Name: "redis", Version: "7.2",
		},
	}}, AllSoftwareDevices(), CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RejectSoftwareDeclarationDraft(rejectIDs[0], CLISource()); err != nil {
		t.Fatal(err)
	}
	search, err = st.SearchSoftwareDeclarations("redis", AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(search) != 0 {
		t.Fatalf("驳回后的草稿不应成为生效声明：%+v", search)
	}
	drafts, err = st.SoftwareDeclarationDrafts(AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("驳回后的草稿应从列表消失：%+v", drafts)
	}
	if _, err := st.ConfirmSoftwareDeclarationDraft(rejectIDs[0], nil, CLISource()); err == nil {
		t.Fatal("驳回后的草稿不能再确认")
	}

	// 各写操作的审计：提交、确认、修改后确认、驳回，各记录来源。
	rows, err := st.QueryAudit(AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]AuditRow{}
	for _, row := range rows {
		actions[row.Action] = row
	}
	for _, action := range []string{
		"software-declaration-draft", "software-declaration-confirm",
		"software-declaration-confirm-edit", "software-declaration-reject",
	} {
		row, ok := actions[action]
		if !ok {
			t.Fatalf("缺少审计动作 %s（已有：%v）", action, actions)
		}
		if row.Source != AuditSourceCLI {
			t.Fatalf("审计动作 %s 的来源应为命令行，实际 %q", action, row.Source)
		}
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(actions["software-declaration-confirm-edit"].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["name"] != "libc6" {
		t.Fatalf("确认审计摘要缺少软件名：%v", summary)
	}
}

// TestSoftwareDeclarationDraftVisibilityAndClaim 覆盖可见范围核对、认领
// 实测条目的草稿、受审计转发写的设备码编号，以及设备删除级联。
func TestSoftwareDeclarationDraftVisibilityAndClaim(t *testing.T) {
	st := newCollectTestStore(t)
	visibleServer := addCollectServer(t, st, "可见设备", "可见入口")
	addCollectServer(t, st, "授权外设备", "授权外入口")
	if _, err := st.AddDevice("不受管设备", "", ""); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("草稿 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{visibleServer.ID}); err != nil {
		t.Fatal(err)
	}
	scope := SoftwareDevicesForServers(visibleServer.ID)

	// 越界设备被拒，整批不落库。
	_, err = st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{
		{SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "可见设备", Category: models.SoftwareCategoryServiceProgram,
			Name: "nginx", Version: "1.25.3",
		}},
		{SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "授权外设备", Category: models.SoftwareCategoryServiceProgram,
			Name: "redis", Version: "7.2",
		}},
	}, scope, ForwardedWriteSource("mini6"))
	if !errors.Is(err, ErrSoftwareDeviceNotVisible) {
		t.Fatalf("越界设备应返回可见范围错误：%v", err)
	}
	drafts, err := st.SoftwareDeclarationDrafts(scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("整批被拒后不应留下任何草稿：%+v", drafts)
	}

	// 认领的实测条目必须已经存在；不存在的稳定键在提交时就拒绝。
	if _, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "可见设备", Category: models.SoftwareCategoryServiceProgram,
			Name: "不存在的认领", Version: "1.0",
		},
		ClaimedObservedKey: "observed-key-missing",
	}}, scope, ForwardedWriteSource("mini6")); err == nil {
		t.Fatal("认领不存在的实测条目应在提交时被拒")
	}
	if err := st.ReplaceSoftwareObservedItems("可见设备", SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			StableKey: "observed-key-1", Category: models.SoftwareCategoryServiceProgram,
			Name: "gateway-bin", Version: "2.0.9",
		}},
		ServerID: visibleServer.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}

	// 受审计转发写提交：审计记录设备码编号。
	ids, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{
		{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "可见设备", Category: models.SoftwareCategoryServiceProgram,
				Name: "业务网关", Version: "2.1",
			},
			ClaimedObservedKey: "observed-key-1",
		},
	}, scope, ForwardedWriteSource("mini6"))
	if err != nil {
		t.Fatal(err)
	}

	confirmed, err := st.ConfirmSoftwareDeclarationDraft(ids[0], nil, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.ClaimedObservedKey != "observed-key-1" {
		t.Fatalf("确认认领草稿应保留认领关系：%+v", confirmed)
	}

	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"software-declaration-draft"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 {
		t.Fatalf("成功的整批提交应留审计行：%+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["device_code"] != "mini6" {
		t.Fatalf("草稿提交审计应记录设备码编号：%v", summary)
	}
	if rows[0].Source != AuditSourceForward {
		t.Fatalf("转发写审计来源应为 forward，实际 %q", rows[0].Source)
	}

	// 设备删除级联：草稿随设备一起删除。
	if _, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "不受管设备", Category: models.SoftwareCategoryOperatingSystem,
			Name: "NAS 系统", Version: "7.2.1",
		},
	}}, AllSoftwareDevices(), CLISource()); err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeviceDeletePreview("不受管设备")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteDeviceCascading("不受管设备", preview); err != nil {
		t.Fatal(err)
	}
	drafts, err = st.SoftwareDeclarationDrafts(AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("设备删除后草稿应级联删除：%+v", drafts)
	}
}

// TestSoftwareDeclarationDraftBackupBoundaries 钉住草稿行的备份边界：全
// 量导出包含并能恢复；profile 导出一律不含。
func TestSoftwareDeclarationDraftBackupBoundaries(t *testing.T) {
	st, server := newDraftTestStore(t)
	ids, err := st.AddSoftwareDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "工作站", Category: models.SoftwareCategoryServiceProgram,
			Name: "nginx", Version: "1.25.3",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 443, Protocol: "tcp",
				Exposure: models.SoftwareExposurePublic,
			}},
		},
	}}, AllSoftwareDevices(), ForwardedWriteSource("laptop"))
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("备份 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	profileSnapshot, err := st.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(profileSnapshot.SoftwareDeclarations) != 0 || len(profileSnapshot.SoftwareDeclarationPorts) != 0 {
		t.Fatalf("profile 导出不得携带声明草稿：%+v %+v",
			profileSnapshot.SoftwareDeclarations, profileSnapshot.SoftwareDeclarationPorts)
	}

	fullSnapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(fullSnapshot.SoftwareDeclarations) != 1 || len(fullSnapshot.SoftwareDeclarationPorts) != 1 {
		t.Fatalf("全量导出应包含声明草稿与端口：%+v %+v",
			fullSnapshot.SoftwareDeclarations, fullSnapshot.SoftwareDeclarationPorts)
	}
	if fullSnapshot.SoftwareDeclarations[0].Status != "draft" ||
		fullSnapshot.SoftwareDeclarations[0].ID != ids[0] {
		t.Fatalf("全量导出的草稿行字段不完整：%+v", fullSnapshot.SoftwareDeclarations[0])
	}

	targetKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(filepath.Join(t.TempDir(), "restored.db"), targetKey)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(fullSnapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := target.SoftwareDeclarationDrafts(AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].ID != ids[0] ||
		len(restored[0].ListeningPorts) != 1 {
		t.Fatalf("恢复后的草稿应保持原样：%+v", restored)
	}
}
