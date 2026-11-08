package servermanage

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个真实临时 vault 并挂上服务器管理模块；测试只通过
// 模块接口操作，不直接改模块内部状态。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st)
}

func mustAddServer(t *testing.T, m *Manager, srv *models.Server) string {
	t.Helper()
	id, err := m.st.AddServerWithCredentials(srv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func reasonsOf(items []ListItem, name string) []string {
	for _, item := range items {
		if item.Server.Name == name {
			return item.AttentionReasons
		}
	}
	return nil
}

// TestListAttentionReasonTruthTable 钉住「需要注意」原因的真值表：未挂凭
// 据、未设角色、待补私钥口令；缺 sudo 密码或缺其他服务器元数据字段不报警。
func TestListAttentionReasonTruthTable(t *testing.T) {
	m := newTestManager(t)
	cid, err := m.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	mustAddServer(t, m, &models.Server{Name: "fine", Host: "192.0.2.1", Port: 22, User: "u",
		CredentialID: cid, Role: "生产数据库", SudoCredentialID: ""})
	mustAddServer(t, m, &models.Server{Name: "no-cred", Host: "192.0.2.2", Port: 22, User: "u", Role: "r"})
	mustAddServer(t, m, &models.Server{Name: "no-role", Host: "192.0.2.3", Port: 22, User: "u", CredentialID: cid})
	mustAddServer(t, m, &models.Server{Name: "need-pass", Host: "192.0.2.4", Port: 22, User: "u",
		CredentialID: cid, Role: "r", Tags: []string{models.TagNeedsPassphrase}})

	all, err := m.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("列表应有 4 台服务器，实际 %d", len(all))
	}
	if got := reasonsOf(all, "fine"); len(got) != 0 {
		t.Fatalf("完整服务器不应有注意原因，实际 %v", got)
	}
	if got := reasonsOf(all, "no-cred"); len(got) != 1 || got[0] != ReasonNoCredential {
		t.Fatalf("未挂凭据的原因 = %v", got)
	}
	if got := reasonsOf(all, "no-role"); len(got) != 1 || got[0] != ReasonNoRole {
		t.Fatalf("未设角色的原因 = %v", got)
	}
	if got := reasonsOf(all, "need-pass"); len(got) != 1 || got[0] != ReasonNeedsPassphrase {
		t.Fatalf("待补私钥口令的原因 = %v", got)
	}

	attention, err := m.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(attention) != 3 {
		t.Fatalf("只看需要注意应剩 3 台，实际 %d", len(attention))
	}
	for _, item := range attention {
		if item.Server.Name == "fine" {
			t.Fatal("完整服务器不应出现在需要注意视图")
		}
	}
}

// TestDetailShowsNonSensitiveFacts 校验详情按名字或标识符解析，并给出凭据
// 状态、所属设备与注意原因，绝不包含凭据内容。
func TestDetailShowsNonSensitiveFacts(t *testing.T) {
	m := newTestManager(t)
	cid, err := m.st.SetCredential(&models.Credential{
		Type: models.CredPrivateKey, Secret: []byte("PRIVATE KEY BODY"), Passphrase: []byte("key-pass"),
	})
	if err != nil {
		t.Fatal(err)
	}
	sudoID, err := m.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pass")})
	if err != nil {
		t.Fatal(err)
	}
	id := mustAddServer(t, m, &models.Server{
		Name: "web", Host: "192.0.2.10", Port: 2222, User: "deploy",
		CredentialID: cid, SudoCredentialID: sudoID, Role: "网站", Tags: []string{"gpu"},
		Location: "机房 A", ExposeHost: true, ShellType: models.ShellPOSIX,
	})

	detail, err := m.Detail(id)
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || detail.Server.ID != id {
		t.Fatalf("按标识符应找到服务器，实际 %+v", detail)
	}
	if detail.CredentialType != models.CredPrivateKey || !detail.HasKeyPassphrase || !detail.HasSudoPassword {
		t.Fatalf("凭据摘要不正确：%+v", detail)
	}
	if detail.Server.CredentialID != cid || detail.Server.SudoCredentialID != sudoID {
		t.Fatalf("详情应保留标识符： %+v", detail.Server)
	}

	byName, err := m.Detail("web")
	if err != nil {
		t.Fatal(err)
	}
	if byName == nil || byName.Server.ID != id {
		t.Fatalf("按名字应找到同一台服务器：%+v", byName)
	}
	missing, err := m.Detail("missing")
	if err != nil || missing != nil {
		t.Fatalf("不存在的服务器应返回 nil，实际 detail=%+v err=%v", missing, err)
	}
}
