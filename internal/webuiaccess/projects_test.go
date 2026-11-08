package webuiaccess

import (
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func seedProjectStore(t *testing.T) *store.Store {
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
	if _, err := st.AddProfile("运维"); err != nil {
		t.Fatal(err)
	}
	return st
}

func projectProfileID(t *testing.T, st *store.Store) string {
	t.Helper()
	profiles, err := st.ListProfiles()
	if err != nil || len(profiles) != 1 {
		t.Fatalf("准备 profile 失败：%v profiles=%+v", err, profiles)
	}
	return profiles[0].ID
}

func TestProjectAdapterUsesAccessManagerAndWebUIAuditSource(t *testing.T) {
	st := seedProjectStore(t)
	profileID := projectProfileID(t, st)
	source := webui.AccessWriteSource{RemoteAddress: "192.0.2.7"}
	adapter := FromManager(access.New(st))

	issued, err := adapter.AddProject("agent", profileID, source)
	if err != nil {
		t.Fatalf("签发项目令牌失败：%v", err)
	}
	if issued.Token == "" || issued.Snippet != access.ProjectTokenSnippet(issued.Token) {
		t.Fatalf("签发结果应带与命令行一致的片段：%+v", issued)
	}

	rotated, err := adapter.RotateProject(issued.ID, source)
	if err != nil {
		t.Fatalf("轮换项目令牌失败：%v", err)
	}
	if rotated.Token == issued.Token || rotated.Snippet != access.ProjectTokenSnippet(rotated.Token) {
		t.Fatalf("轮换结果应带新令牌与一致片段：%+v", rotated)
	}
	if err := adapter.DisableProject(issued.ID, source); err != nil {
		t.Fatalf("停用项目失败：%v", err)
	}
	if err := adapter.EnableProject(issued.ID, source); err != nil {
		t.Fatalf("重新启用项目失败：%v", err)
	}
	if err := adapter.RevokeProject(issued.ID, source); err != nil {
		t.Fatalf("吊销项目失败：%v", err)
	}

	visible, err := adapter.ListProjects(false)
	if err != nil || len(visible) != 0 {
		t.Fatalf("默认列表应隐藏已吊销项目，err=%v rows=%+v", err, visible)
	}
	all, err := adapter.ListProjects(true)
	if err != nil || len(all) != 1 {
		t.Fatalf("切换后应显示已吊销项目，err=%v rows=%+v", err, all)
	}
	row := all[0]
	if row.ID != issued.ID || row.Name != "agent" || row.TokenPrefix == "" ||
		row.Status != string(models.ProjectRevoked) || row.ProfileName != "运维" || row.CreatedAt.IsZero() {
		t.Fatalf("项目列表行不完整：%+v", row)
	}
	if err := adapter.DeleteProject(issued.ID, source); err != nil {
		t.Fatalf("删除已吊销项目失败：%v", err)
	}

	wantActions := []string{"project.add", "project.rotate", "project.disable", "project.enable", "project.revoke", "project.delete"}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: wantActions})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, auditRow := range rows {
		seen[auditRow.Action] = true
		if auditRow.Source != store.AuditSourceWebUI || auditRow.SourceIP != "192.0.2.7" {
			t.Fatalf("项目审计来源应带 Web UI 与来源地址，实际 %+v", auditRow)
		}
		if strings.Contains(auditRow.Command, issued.Token) || strings.Contains(auditRow.Command, rotated.Token) {
			t.Fatalf("项目审计摘要不得包含令牌：%+v", auditRow)
		}
	}
	for _, action := range wantActions {
		if !seen[action] {
			t.Fatalf("缺少 %s 审计行，实际 %+v", action, rows)
		}
	}
}

func TestProjectAdapterFromOpenerOpensStorePerCall(t *testing.T) {
	st := seedProjectStore(t)
	profileID := projectProfileID(t, st)
	var opens int
	adapter := FromOpener(func() (*access.Manager, func(), error) {
		opens++
		return access.New(st), func() {}, nil
	})

	if _, err := adapter.AddProject("agent", profileID, webui.AccessWriteSource{RemoteAddress: "192.0.2.7"}); err != nil {
		t.Fatalf("签发项目令牌失败：%v", err)
	}
	rows, err := adapter.ListProjects(false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("按需打开的适配器应能读写项目，err=%v rows=%+v", err, rows)
	}
	if opens != 2 {
		t.Fatalf("每次页面读写都应重新打开存储，实际打开 %d 次", opens)
	}
}
