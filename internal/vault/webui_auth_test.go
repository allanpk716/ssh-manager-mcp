package vault

import (
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestWebUIAuthStoreAdaptsVaultStateAndAudit(t *testing.T) {
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHMGR_VAULT_ROOT", t.TempDir())
	t.Setenv("SSHMGR_STORE", t.TempDir()+"/test.db")
	st, err := OpenStore(&fakeKeyProvider{key: masterKey})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var auth webui.AuthStore = NewWebUIAuthStore(st)
	set, err := auth.AdminPasswordIsSet()
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Fatal("新 vault 不应已经设置管理员口令")
	}
	generation, err := auth.SetAdminPassword([]byte("correct-password"))
	if err != nil || generation != 1 {
		t.Fatalf("设置管理员口令返回会话代 = %d，错误 = %v", generation, err)
	}
	valid, err := auth.VerifyAdminPassword([]byte("correct-password"))
	if err != nil || !valid {
		t.Fatalf("适配器应校验口令：valid=%v err=%v", valid, err)
	}
	now := time.Unix(1800000000, 0)
	for i := 0; i < 5; i++ {
		if _, err := auth.RecordLoginFailure("192.0.2.10", now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	limit, err := auth.LoginLimit("192.0.2.10", now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !limit.Locked || limit.Delay != 5*time.Second {
		t.Fatalf("适配器应返回锁定与延迟：%+v", limit)
	}
	if err := auth.WriteAudit(webui.AuditEvent{
		Action: "web.login.success", Status: "ok", SourceIP: "192.0.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI ||
		rows[0].SourceIP != "192.0.2.10" {
		t.Fatalf("适配器应写入 Web UI 审计：%+v", rows)
	}
}
