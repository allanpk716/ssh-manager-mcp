package access

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// mustProfile 直接经存储层新增一个 profile（准备数据用，被测行为不经过
// 这里），返回它的标识符。
func mustProfile(t *testing.T, m *Manager, name string) string {
	t.Helper()
	id, err := m.st.AddProfile(name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// projectAuditRows 读回指定项目动作的全部审计行（最新在前）。项目生命
// 周期行挂项目标识符，不走 owner-only 过滤（owner-only 面板不收它们）。
func projectAuditRows(t *testing.T, st *store.Store, actions ...string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestAddProjectIssuesTokenAndWritesAudit 签发项目令牌返回一次性令牌与
// 已代入令牌的 `.mcp.json` 片段；成功与失败各写一条 `project.add` 审计
// 行，摘要只有名字和标识符，绝不含令牌。
func TestAddProjectIssuesTokenAndWritesAudit(t *testing.T) {
	m, st := newTestManager(t)
	pid := mustProfile(t, m, "team-a")

	added, err := m.AddProject("proj-web", "team-a", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if added.ID == "" || added.Name != "proj-web" {
		t.Fatalf("签发结果 = %+v，应带项目标识符与名字", added)
	}
	if added.Token == "" || strings.Contains(added.Snippet, "%") {
		t.Fatalf("签发结果 = %+v，应带一次性令牌与片段", added)
	}
	if !strings.Contains(added.Snippet, added.Token) {
		t.Fatalf("片段 %q 应代入令牌", added.Snippet)
	}
	wantSnippet := `{"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"` + added.Token + `"}}}}`
	if added.Snippet != wantSnippet {
		t.Fatalf("片段 = %q，应为 %q", added.Snippet, wantSnippet)
	}
	proj, err := st.GetProjectByName("proj-web")
	if err != nil || proj == nil {
		t.Fatalf("签发后读回项目失败：%v（%v）", proj, err)
	}
	if proj.ProfileID != pid || proj.Status != models.ProjectActive {
		t.Fatalf("签发后的项目 = %+v，应绑定 profile 且为 active", proj)
	}

	// 名称重复：写失败审计行（Web UI 来源带来源地址）。
	if _, err := m.AddProject("proj-web", "team-a", WebUISource("192.0.2.7")); err == nil {
		t.Fatal("名称重复的签发应报错")
	}

	rows := projectAuditRows(t, st, "project.add")
	if len(rows) != 2 {
		t.Fatalf("project.add 审计行数量 = %d，应为 2：%+v", len(rows), rows)
	}
	assertAuditShape(t, rows[0], "project.add", "error", `{"name":"proj-web"}`, store.AuditSourceWebUI, "192.0.2.7")
	assertAuditShape(t, rows[1], "project.add", "ok", `{"id":"`+added.ID+`","name":"proj-web"}`, store.AuditSourceCLI, "")
	if rows[1].ProjectID != added.ID {
		t.Fatalf("成功的签发审计行应挂项目标识符：%+v", rows[1])
	}
	for _, r := range rows {
		if strings.Contains(r.Command, added.Token) {
			t.Fatalf("审计摘要含令牌：%+v", r)
		}
	}
}

// TestRotateProjectReplacesToken 轮换返回新的一次性令牌与片段，旧令牌
// 立刻失效；轮换已吊销项目被拒绝并写失败审计行，摘要不含令牌。
func TestRotateProjectReplacesToken(t *testing.T) {
	m, st := newTestManager(t)
	mustProfile(t, m, "team-a")
	added, err := m.AddProject("proj-web", "team-a", CLISource())
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := m.RotateProject("proj-web", WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID != added.ID || rotated.Name != "proj-web" {
		t.Fatalf("轮换结果 = %+v，应保持项目标识符与名字", rotated)
	}
	if rotated.Token == "" || rotated.Snippet != ProjectTokenSnippet(rotated.Token) {
		t.Fatalf("轮换结果 = %+v，应带新令牌与片段", rotated)
	}
	if old, err := st.VerifyToken(added.Token); err != nil || old != nil {
		t.Fatalf("旧令牌轮换后仍有效：%v（%v）", old, err)
	}
	if fresh, err := st.VerifyToken(rotated.Token); err != nil || fresh == nil {
		t.Fatalf("新令牌应能认证：%v（%v）", fresh, err)
	}

	if _, err := m.RotateProject("no-such", CLISource()); err == nil {
		t.Fatal("轮换不存在的项目应报错")
	}
	if err := st.SetProjectStatus(added.ID, models.ProjectRevoked); err != nil {
		t.Fatalf("准备已吊销项目失败：%v", err)
	}
	if _, err := m.RotateProject("proj-web", CLISource()); err == nil {
		t.Fatal("轮换已吊销项目应报错")
	}

	rows := projectAuditRows(t, st, "project.rotate")
	if len(rows) != 3 {
		t.Fatalf("project.rotate 审计行数量 = %d，应为 3：%+v", len(rows), rows)
	}
	if rows[0].ProjectID != added.ID || rows[2].ProjectID != added.ID {
		t.Fatalf("成功与已吊销失败的轮换审计行应挂项目标识符：%+v", rows)
	}
	assertAuditShape(t, rows[0], "project.rotate", "error", `{"id":"`+added.ID+`","name":"proj-web"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[1], "project.rotate", "error", `{"name":"no-such"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[2], "project.rotate", "ok", `{"id":"`+added.ID+`","name":"proj-web"}`, store.AuditSourceWebUI, "192.0.2.7")
	for _, r := range rows {
		if strings.Contains(r.Command, added.Token) || strings.Contains(r.Command, rotated.Token) {
			t.Fatalf("审计摘要含令牌：%+v", r)
		}
	}
}

// TestProjectLifecycleStatusAndDelete 停用与启用可逆，吊销后不能再启用；
// 只有已吊销的项目能删除；每个动作各写一条审计行，摘要不含令牌。
func TestProjectLifecycleStatusAndDelete(t *testing.T) {
	m, st := newTestManager(t)
	mustProfile(t, m, "team-a")
	added, err := m.AddProject("proj-web", "team-a", CLISource())
	if err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		name   string
		action string
		call   func() error
	}{
		{"disable", "project.disable", func() error { return m.DisableProject("proj-web", CLISource()) }},
		{"enable", "project.enable", func() error { return m.EnableProject("proj-web", WebUISource("192.0.2.7")) }},
		{"revoke", "project.revoke", func() error { return m.RevokeProject("proj-web", CLISource()) }},
		{"delete", "project.delete", func() error { return m.DeleteProject("proj-web", CLISource()) }},
	}
	for _, step := range steps {
		if err := step.call(); err != nil {
			t.Fatalf("%s 应成功：%v", step.name, err)
		}
	}

	// 删除后项目不存在：再次删除报错并写失败审计行。
	if err := m.DeleteProject("proj-web", CLISource()); err == nil {
		t.Fatal("删除不存在的项目应报错")
	}

	proj, err := st.GetProjectByName("proj-web")
	if err != nil || proj != nil {
		t.Fatalf("删除后项目仍在：%v（%v）", proj, err)
	}

	rows := projectAuditRows(t, st, "project.disable", "project.enable", "project.revoke", "project.delete")
	if len(rows) != 5 {
		t.Fatalf("生命周期审计行数量 = %d，应为 5：%+v", len(rows), rows)
	}
	want := []struct {
		action string
		status string
		audit  string
		source string
		ip     string
	}{
		{"project.delete", "error", `{"name":"proj-web"}`, store.AuditSourceCLI, ""},
		{"project.delete", "ok", `{"id":"` + added.ID + `","name":"proj-web"}`, store.AuditSourceCLI, ""},
		{"project.revoke", "ok", `{"id":"` + added.ID + `","name":"proj-web"}`, store.AuditSourceCLI, ""},
		{"project.enable", "ok", `{"id":"` + added.ID + `","name":"proj-web"}`, store.AuditSourceWebUI, "192.0.2.7"},
		{"project.disable", "ok", `{"id":"` + added.ID + `","name":"proj-web"}`, store.AuditSourceCLI, ""},
	}
	for i, w := range want {
		assertAuditShape(t, rows[i], w.action, w.status, w.audit, w.source, w.ip)
		if strings.Contains(w.audit, `"id"`) && rows[i].ProjectID != added.ID {
			t.Fatalf("%s 审计行应挂项目标识符：%+v", w.action, rows[i])
		}
		if strings.Contains(rows[i].Command, added.Token) {
			t.Fatalf("审计摘要含令牌：%+v", rows[i])
		}
	}
}

// TestRevokedProjectIsTerminal 吊销不可逆：不能再启用、停用或轮换；未
// 吊销的项目不能直接删除。
func TestRevokedProjectIsTerminal(t *testing.T) {
	m, st := newTestManager(t)
	mustProfile(t, m, "team-a")
	if _, err := m.AddProject("proj-web", "team-a", CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddProject("proj-keep", "team-a", CLISource()); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteProject("proj-keep", CLISource()); err == nil {
		t.Fatal("删除未吊销项目应报错")
	}
	if err := m.RevokeProject("proj-web", CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.EnableProject("proj-web", CLISource()); err == nil {
		t.Fatal("启用已吊销项目应报错")
	}
	if err := m.DisableProject("proj-web", CLISource()); err == nil {
		t.Fatal("停用已吊销项目应报错")
	}
	if _, err := m.RotateProject("proj-web", CLISource()); err == nil {
		t.Fatal("轮换已吊销项目应报错")
	}

	proj, err := st.GetProjectByName("proj-web")
	if err != nil || proj == nil || proj.Status != models.ProjectRevoked {
		t.Fatalf("反复拒绝后项目应保持已吊销：%+v（%v）", proj, err)
	}
	rows := projectAuditRows(t, st, "project.enable", "project.disable", "project.rotate")
	if len(rows) != 3 {
		t.Fatalf("被拒绝的动作应各写一条失败审计行，实际 %d 条：%+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Status != "error" {
			t.Fatalf("被拒绝的动作应为失败审计行：%+v", r)
		}
	}
}

// TestListProjectsFiltersRevoked 列表默认不显示已吊销的项目，打开开关后
// 显示；每行带标识符、令牌前缀、状态与 profile 的名字和标识符，供命令
// 行与 Web UI 共用。
func TestListProjectsFiltersRevoked(t *testing.T) {
	m, _ := newTestManager(t)
	pid := mustProfile(t, m, "team-a")
	kept, err := m.AddProject("proj-web", "team-a", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	gone, err := m.AddProject("proj-old", pid, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RevokeProject(gone.ID, CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := m.ListProjects(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("默认列表应只有 1 个未吊销项目，实际 %+v", rows)
	}
	got := rows[0]
	if got.ID != kept.ID || got.Name != "proj-web" || got.Status != models.ProjectActive {
		t.Fatalf("列表行 = %+v，应保持项目标识符、名字与状态", got)
	}
	if got.TokenPrefix != kept.Token[:8] {
		t.Fatalf("列表行令牌前缀 = %q，应只显示令牌前 8 位", got.TokenPrefix)
	}
	if got.ProfileID != pid || got.ProfileName != "team-a" {
		t.Fatalf("列表行 profile = %s/%s，应为 team-a 的标识符与名字", got.ProfileID, got.ProfileName)
	}

	all, err := m.ListProjects(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "proj-old" || all[1].Name != "proj-web" {
		t.Fatalf("含已吊销的列表应按名字排序为 proj-old、proj-web，实际 %+v", all)
	}
	if all[0].Status != models.ProjectRevoked {
		t.Fatalf("已吊销项目状态 = %s，应为 revoked", all[0].Status)
	}
}
