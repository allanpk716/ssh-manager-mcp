package machineinit_test

import (
	"os"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func TestSetAdminPassword(t *testing.T) {
	withMachineEnv(t)
	if _, err := machineinit.Initialize(roles.RoleServer); err != nil {
		t.Fatal(err)
	}

	_, err := machineinit.SetAdminPassword(nil, store.AuditRow{
		Action: "admin.password_set", Status: "ok",
		Source: store.AuditSourceCLI, Command: "{}",
	})
	if err == nil || !strings.Contains(err.Error(), "管理员口令不能为空") {
		t.Fatalf("空口令应被拒绝：%v", err)
	}

	generation, err := machineinit.SetAdminPassword([]byte("admin-password"), store.AuditRow{
		Action: "admin.password_set", Status: "ok",
		Source: store.AuditSourceCLI, Command: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("第一次设置后会话代 = %d，应为 1", generation)
	}

	masterKey, err := os.ReadFile(os.Getenv("SSHMGR_FILEKEY_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, err := st.WebAdminPasswordHash()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("vault 中应保存 argon2id 哈希：%q", hash)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "admin.password_set" || rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("应写入管理员口令审计行：%+v", rows)
	}

	generation, err = machineinit.SetAdminPassword([]byte("new-password"), store.AuditRow{
		Action: "admin.password_set", Status: "ok",
		Source: store.AuditSourceWebUI, Command: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("重设口令后会话代 = %d，应为 2", generation)
	}
}
