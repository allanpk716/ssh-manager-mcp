package secretmanage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个全新的临时加密 vault，并返回挂在它上面的机密项
// 管理模块与存储句柄。存储句柄只用于准备服务器条目和读回审计行。
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st), st
}

// mustServer 新增一台带密码凭据的服务器条目，返回标识符与名字。
func mustServer(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestWriteOperationsRecordSource 验证每个机密项写操作的审计来源：命令行来
// 源不带地址，Web UI 来源带来源地址；所有摘要都不包含值。
func TestWriteOperationsRecordSource(t *testing.T) {
	m, st := newTestManager(t)
	web1 := mustServer(t, st, "web1")
	web2 := mustServer(t, st, "web2")

	if err := m.AddSecret("DEPLOY_TOKEN", "部署令牌", "HF_TOKEN", []byte("web-add-secret-value"), WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSecretValue("DEPLOY_TOKEN", []byte("cli-set-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	note := "生产部署令牌"
	environmentName := "MODEL_TOKEN"
	if err := m.EditMetadata("DEPLOY_TOKEN", MetadataEdit{Note: &note, EnvironmentName: &environmentName}, WebUISource("192.0.2.8")); err != nil {
		t.Fatal(err)
	}
	if err := m.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web1", "web2"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.DisallowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web1"}, WebUISource("192.0.2.9")); err != nil {
		t.Fatal(err)
	}
	if err := m.AddSecret("OLD_TOKEN", "待删除", "", []byte("web-add-old-secret-value"), WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSecret("OLD_TOKEN", CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{
		"secret-add", "secret-set", "secret-edit", "secret-env",
		"secret-allow", "secret-disallow", "secret-rm",
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantSource := map[string]struct {
		source  string
		address string
	}{
		"secret-add":      {store.AuditSourceWebUI, "192.0.2.7"},
		"secret-env":      {store.AuditSourceWebUI, "192.0.2.8"},
		"secret-set":      {store.AuditSourceCLI, ""},
		"secret-edit":     {store.AuditSourceWebUI, "192.0.2.8"},
		"secret-allow":    {store.AuditSourceCLI, ""},
		"secret-disallow": {store.AuditSourceWebUI, "192.0.2.9"},
		"secret-rm":       {store.AuditSourceCLI, ""},
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		expected, exists := wantSource[row.Action]
		if row.Action == "secret-env" && strings.Contains(row.Command, `"after":"HF_TOKEN"`) {
			expected = struct {
				source  string
				address string
			}{store.AuditSourceWebUI, "192.0.2.7"}
		}
		if !exists {
			t.Fatalf("未预期的机密项审计动作 %q：%+v", row.Action, row)
		}
		if row.Status != "ok" {
			t.Fatalf("动作 %s 的审计状态 = %q，应为 ok", row.Action, row.Status)
		}
		if row.Source != expected.source || row.SourceIP != expected.address {
			t.Fatalf("动作 %s 的来源 = %q/%q，应为 %q/%q", row.Action, row.Source, row.SourceIP, expected.source, expected.address)
		}
		if strings.Contains(row.Command, "secret-value") {
			t.Fatalf("动作 %s 的审计摘要包含值：%s", row.Action, row.Command)
		}
		seen[row.Action] = true
	}
	for action := range wantSource {
		if !seen[action] {
			t.Fatalf("缺少动作 %q 的审计行，实际行：%+v", action, rows)
		}
	}

	views, err := m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "DEPLOY_TOKEN" || views[0].ID == "" {
		t.Fatalf("列表结果 = %+v，应只剩带标识符的 DEPLOY_TOKEN", views)
	}
	if len(views[0].AllowedServers) != 1 || views[0].AllowedServers[0] != "web2" {
		t.Fatalf("可写入服务器 = %v，应只剩 web2", views[0].AllowedServers)
	}
	if views[0].EnvironmentName != "MODEL_TOKEN" || views[0].Note != "生产部署令牌" {
		t.Fatalf("元数据 = %+v", views[0])
	}
	if strings.Join(views[0].AllowedServers, ",") == web1 || views[0].ID == web2 {
		t.Fatal("列表不应把服务器标识符当成机密项标识符")
	}
}

// TestAuthorizationPreviewAndApply 覆盖授权矩阵的差异计算、确定性摘要与确
// 认执行：预览按机密项分组；摘要与目标集合的输入顺序无关；执行后每个发
// 生变化的机密项各有一条审计，摘要列出新增与移除的服务器。
func TestAuthorizationPreviewAndApply(t *testing.T) {
	m, st := newTestManager(t)
	web1 := mustServer(t, st, "web1")
	web2 := mustServer(t, st, "web2")
	if err := m.AddSecret("DEPLOY_TOKEN", "部署令牌", "", []byte("deploy-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.AddSecret("DATABASE_PASSWORD", "数据库口令", "", []byte("database-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.AllowSecrets([]string{"DATABASE_PASSWORD"}, []string{"web1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	secretID := func(name string) string {
		for _, view := range views {
			if view.Name == name {
				return view.ID
			}
		}
		t.Fatalf("列表里找不到机密项 %s：%+v", name, views)
		return ""
	}
	deployID, databaseID := secretID("DEPLOY_TOKEN"), secretID("DATABASE_PASSWORD")

	target := []AuthorizationCell{
		{SecretID: deployID, ServerID: web2, Allow: false},
		{SecretID: deployID, ServerID: web1, Allow: true},
		{SecretID: databaseID, ServerID: web2, Allow: true},
		{SecretID: databaseID, ServerID: web1, Allow: false},
	}
	preview, err := m.PreviewAuthorizationChanges(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 2 {
		t.Fatalf("预览分组数量 = %d，应为 2：%+v", len(preview.Changes), preview)
	}
	if preview.Changes[0].SecretName != "DATABASE_PASSWORD" ||
		len(preview.Changes[0].Removed) != 1 || preview.Changes[0].Removed[0].Name != "web1" ||
		len(preview.Changes[0].Added) != 1 || preview.Changes[0].Added[0].Name != "web2" {
		t.Fatalf("DATABASE_PASSWORD 的预览 = %+v", preview.Changes[0])
	}
	if preview.Changes[1].SecretName != "DEPLOY_TOKEN" ||
		len(preview.Changes[1].Added) != 1 || preview.Changes[1].Added[0].Name != "web1" ||
		len(preview.Changes[1].Removed) != 0 {
		t.Fatalf("DEPLOY_TOKEN 的预览 = %+v", preview.Changes[1])
	}
	if preview.TotalAdded != 2 || preview.TotalRemoved != 1 || preview.Summary == "" {
		t.Fatalf("预览总数与摘要 = %+v", preview)
	}

	reordered := []AuthorizationCell{target[3], target[1], target[0], target[2]}
	sameState, err := m.PreviewAuthorizationChanges(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if sameState.Summary != preview.Summary {
		t.Fatalf("摘要随目标集合顺序改变：%q 与 %q", preview.Summary, sameState.Summary)
	}

	result, err := m.ApplyAuthorizationChanges(target, preview.Summary, WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied {
		t.Fatalf("有差异的确认未执行：%+v", result)
	}
	views, err = m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		switch view.Name {
		case "DEPLOY_TOKEN":
			if names := view.AllowedServers; len(names) != 1 || names[0] != "web1" {
				t.Fatalf("DEPLOY_TOKEN 执行后的可写入服务器 = %v", names)
			}
		case "DATABASE_PASSWORD":
			if names := view.AllowedServers; len(names) != 1 || names[0] != "web2" {
				t.Fatalf("DATABASE_PASSWORD 执行后的可写入服务器 = %v", names)
			}
		}
	}
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	wantSummaries := []string{
		`{"added":"web1","name":"DEPLOY_TOKEN","removed":""}`,
		`{"added":"web2","name":"DATABASE_PASSWORD","removed":"web1"}`,
	}
	if len(rows) != len(wantSummaries) {
		t.Fatalf("授权变更审计行 = %+v", rows)
	}
	for i, want := range wantSummaries {
		if rows[i].Command != want || rows[i].Source != store.AuditSourceWebUI || rows[i].SourceIP != "192.0.2.7" {
			t.Fatalf("授权变更审计行 %d = %+v，摘要应为 %s", i, rows[i], want)
		}
	}
	if strings.Contains(rows[0].Command+rows[1].Command, "secret-value") {
		t.Fatalf("授权变更审计泄露值：%+v", rows)
	}
}

// TestAuthorizationApplyRejectsStaleSummary 钉住确认时的并发保护：预览后被
// 其他入口改动时，摘要不一致整体拒绝，不写数据也不写审计，并返回最新
// 预览。空差异同样不写数据与审计。
func TestAuthorizationApplyRejectsStaleSummary(t *testing.T) {
	m, st := newTestManager(t)
	web1 := mustServer(t, st, "web1")
	web2 := mustServer(t, st, "web2")
	if err := m.AddSecret("DEPLOY_TOKEN", "", "", []byte("deploy-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	deployID := views[0].ID
	target := []AuthorizationCell{
		{SecretID: deployID, ServerID: web1, Allow: true},
		{SecretID: deployID, ServerID: web2, Allow: true},
	}
	preview, err := m.PreviewAuthorizationChanges(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 1 || preview.TotalAdded != 2 {
		t.Fatalf("初始预览 = %+v", preview)
	}
	// 预览后另一个入口先授权 web2：确认请求仍按旧摘要要求全部两格，
	// 必须整体拒绝而不是补写 web1。
	if err := m.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web2"}, CLISource()); err != nil {
		t.Fatal(err)
	}

	var conflict *AuthorizationConflictError
	_, err = m.ApplyAuthorizationChanges(target, preview.Summary, WebUISource("192.0.2.7"))
	if !errors.As(err, &conflict) {
		t.Fatalf("过期摘要应返回冲突错误，实际 %v", err)
	}
	if len(conflict.Latest.Changes) != 1 || conflict.Latest.TotalAdded != 1 ||
		conflict.Latest.Changes[0].Added[0].Name != "web1" {
		t.Fatalf("冲突携带的最新预览 = %+v", conflict.Latest)
	}
	views, err = m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if names := views[0].AllowedServers; len(names) != 1 || names[0] != "web2" {
		t.Fatalf("冲突后的数据应保持现状，实际 %v", names)
	}
	if rows, _ := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-allow-change"}}); len(rows) != 0 {
		t.Fatalf("冲突不应写授权变更审计：%+v", rows)
	}

	current := []AuthorizationCell{
		{SecretID: deployID, ServerID: web1, Allow: false},
		{SecretID: deployID, ServerID: web2, Allow: true},
	}
	empty, err := m.PreviewAuthorizationChanges(current)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Changes) != 0 || empty.TotalAdded != 0 || empty.TotalRemoved != 0 {
		t.Fatalf("空差异预览 = %+v", empty)
	}
	result, err := m.ApplyAuthorizationChanges(current, empty.Summary, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatalf("空差异不应执行：%+v", result)
	}
	if rows, _ := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-allow-change"}}); len(rows) != 0 {
		t.Fatalf("空差异不应写审计：%+v", rows)
	}
}

// TestAllowAndDisallowKeepMergedMissingErrors 保持命令行两个方向命令的存
// 储语义：名字缺失时一次合并报出全部问题，整体不生效。
func TestAllowAndDisallowKeepMergedMissingErrors(t *testing.T) {
	m, st := newTestManager(t)
	web1 := mustServer(t, st, "web1")
	if err := m.AddSecret("DEPLOY_TOKEN", "", "", []byte("deploy-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	err := m.AllowSecrets([]string{"DEPLOY_TOKEN", "GHOST_TOKEN"}, []string{"web1", "no-such"}, CLISource())
	if err == nil || !strings.Contains(err.Error(), `机密项 "GHOST_TOKEN" 不存在`) ||
		!strings.Contains(err.Error(), `服务器条目 "no-such" 不存在`) {
		t.Fatalf("缺失名单应合并报错，实际 %v", err)
	}
	views, listErr := m.List(ListFilter{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(views[0].AllowedServers) != 0 {
		t.Fatalf("失败的整体授权不应生效：%+v", views[0])
	}
	if err := m.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	err = m.DisallowSecrets([]string{"DEPLOY_TOKEN", "GHOST_TOKEN"}, []string{"web1", "no-such"}, CLISource())
	if err == nil || !strings.Contains(err.Error(), `机密项 "GHOST_TOKEN" 不存在`) {
		t.Fatalf("取消授权的缺失名单应合并报错，实际 %v", err)
	}
	views, err = m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if names := views[0].AllowedServers; len(names) != 1 || names[0] != "web1" {
		t.Fatalf("失败的整体取消不应生效，实际 %v", names)
	}
	if views[0].ID == web1 {
		t.Fatal("测试误把服务器标识符当作机密项标识符")
	}
}

// TestApplyAuthorizationTreatsDeletedCellEntriesAsConflict 覆盖审查第 2 项：
// 预览之后机密项或服务器条目被删除，确认必须按冲突拒绝，并附按剩余格子重
// 新计算的最新预览，而不是把内部错误暴露给界面。
func TestApplyAuthorizationTreatsDeletedCellEntriesAsConflict(t *testing.T) {
	m, st := newTestManager(t)
	web1 := mustServer(t, st, "web1")
	db1 := mustServer(t, st, "db1")
	for _, name := range []string{"DEPLOY_TOKEN", "BACKUP_TOKEN"} {
		if err := m.AddSecret(name, "", "", []byte(name+"-secret-value"), CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	views, err := m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]string, len(views))
	for _, view := range views {
		ids[view.Name] = view.ID
	}
	target := []AuthorizationCell{
		{SecretID: ids["DEPLOY_TOKEN"], ServerID: web1, Allow: true},
		{SecretID: ids["BACKUP_TOKEN"], ServerID: db1, Allow: true},
	}
	preview, err := m.PreviewAuthorizationChanges(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSecret("DEPLOY_TOKEN", CLISource()); err != nil {
		t.Fatal(err)
	}

	var conflict *AuthorizationConflictError
	_, err = m.ApplyAuthorizationChanges(target, preview.Summary, CLISource())
	if !errors.As(err, &conflict) {
		t.Fatalf("机密项被删除后确认应返回冲突错误，实际 %v", err)
	}
	if len(conflict.Latest.Changes) != 1 || conflict.Latest.Changes[0].SecretName != "BACKUP_TOKEN" ||
		conflict.Latest.TotalAdded != 1 {
		t.Fatalf("冲突应附剩余格子的最新预览，实际 %+v", conflict.Latest)
	}

	remaining := []AuthorizationCell{{SecretID: ids["BACKUP_TOKEN"], ServerID: db1, Allow: true}}
	latest, err := m.PreviewAuthorizationChanges(remaining)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(db1); err != nil {
		t.Fatal(err)
	}
	_, err = m.ApplyAuthorizationChanges(remaining, latest.Summary, CLISource())
	if !errors.As(err, &conflict) {
		t.Fatalf("服务器条目被删除后确认应返回冲突错误，实际 %v", err)
	}
	if len(conflict.Latest.Changes) != 0 {
		t.Fatalf("剩余格子为空时最新预览应为空差异，实际 %+v", conflict.Latest)
	}
	after, err := m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || len(after[0].AllowedServers) != 0 {
		t.Fatalf("冲突后的数据应保持现状，实际 %+v", after)
	}
	if rows, _ := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-allow-change"}}); len(rows) != 0 {
		t.Fatalf("冲突不应写授权变更审计：%+v", rows)
	}
}
