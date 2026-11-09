package ops

import (
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个全新的临时加密库，并返回挂在它上面的运维模块与
// 存储句柄；存储句柄只用于准备审计数据。
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
	t.Cleanup(func() { st.Close() })
	return New(st), st
}

// TestQueryAuditFiltersByNameSourceAndCursor 钉住运维模块的第一条完整路径：
// 审计查询支持名称或标识符过滤、来源与来源地址过滤、编号翻页，并把项目与
// 服务器的显示名填进返回行。
func TestQueryAuditFiltersByNameSourceAndCursor(t *testing.T) {
	m, st := newTestManager(t)
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.AddServer(&models.Server{
		Name: "db-1", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := st.AddProject("alpha", profileID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rows := []store.AuditRow{
		{TS: now.Add(-3 * time.Hour), Action: "exec", Command: "uptime", Status: "ok", ServerID: server, ProjectID: project, Source: store.AuditSourceAgent},
		{TS: now.Add(-2 * time.Hour), Action: "admin.login", Command: "登录成功", Status: "ok", Source: store.AuditSourceWebUI, SourceIP: "192.0.2.23"},
		{TS: now.Add(-time.Hour), Action: "exec", Command: "whoami", Status: "error", ServerID: server, ProjectID: project, Source: store.AuditSourceWebUI, SourceIP: "192.0.2.23", InjectedEnvironmentNames: []string{"HF_TOKEN"}},
		{TS: now, Action: "exec", Command: "date", Status: "ok", ServerID: server, ProjectID: project, Source: store.AuditSourceCLI},
	}
	for _, row := range rows {
		if err := st.WriteAudit(row); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.QueryAudit(AuditQuery{
		Servers:  []string{"db-1"},
		Projects: []string{"alpha"},
		Actions:  []string{"exec"},
		Statuses: []string{"error"},
		Source:   store.AuditSourceWebUI,
		SourceIP: "192.0.2.23",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("组合过滤后的审计行数量 = %d，应为 1：%+v", len(got), got)
	}
	row := got[0]
	if row.Command != "whoami" || row.ServerName != "db-1" || row.ProjectName != "alpha" {
		t.Fatalf("组合过滤与显示名解析结果不符：%+v", row)
	}
	if row.ID <= 0 || row.TS.IsZero() || row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.23" {
		t.Fatalf("审计行缺少编号、时间或来源信息：%+v", row)
	}
	if len(row.InjectedEnvironmentNames) != 1 || row.InjectedEnvironmentNames[0] != "HF_TOKEN" {
		t.Fatalf("审计行缺少注入的环境变量名：%+v", row.InjectedEnvironmentNames)
	}
	if names, ok := row.JSONMap()["injected_environment_names"].([]string); !ok || len(names) != 1 || names[0] != "HF_TOKEN" {
		t.Fatalf("审计 JSON 输出缺少注入的环境变量名：%+v", row.JSONMap())
	}

	cursor := row.ID
	older, err := m.QueryAudit(AuditQuery{
		Servers:  []string{server},
		Actions:  []string{"exec"},
		BeforeID: cursor,
		Limit:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].Command != "uptime" || older[0].ID >= cursor {
		t.Fatalf("按名称或标识符与编号游标翻页的结果不符：%+v", older)
	}
}

func TestAuditSources(t *testing.T) {
	if got := CLISource(); got.kind != store.AuditSourceCLI || got.addr != "" {
		t.Fatalf("命令行来源 = %+v", got)
	}
	if got := WebUISource("192.0.2.23"); got.kind != store.AuditSourceWebUI || got.addr != "192.0.2.23" {
		t.Fatalf("Web UI 来源 = %+v", got)
	}
}
