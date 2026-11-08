package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func prepareServerRoleVault(t *testing.T) (string, []byte) {
	t.Helper()
	path, masterKey := withCliStoreEnv(t)
	createVaultStoreAt(t, path, masterKey)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return path, masterKey
}

func TestServeAdminRejectsNonServerRoles(t *testing.T) {
	path, masterKey := withCliStoreEnv(t)
	createVaultStoreAt(t, path, masterKey)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	got := runCliErr(t, "serve", "admin", "status")
	if !strings.Contains(got, "权威端") {
		t.Fatalf("非权威端执行 serve admin 应明确拒绝：%s", got)
	}
}

func createVaultStoreAt(t *testing.T, path string, masterKey []byte) {
	t.Helper()
	st, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeAdminSetWritesArgon2idAndAudit(t *testing.T) {
	path, masterKey := prepareServerRoleVault(t)
	previous := serveAdminPasswordPrompt
	values := []string{"new-password", "new-password"}
	serveAdminPasswordPrompt = func() ([]byte, error) {
		value := values[0]
		values = values[1:]
		return []byte(value), nil
	}
	t.Cleanup(func() { serveAdminPasswordPrompt = previous })

	out := runCli(t, "serve", "admin", "set")
	if !strings.Contains(out, "已设置") {
		t.Fatalf("设置口令输出应确认成功：%s", out)
	}
	st, err := store.Open(path, masterKey)
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
	generation, err := st.WebAdminSessionGeneration()
	if err != nil || generation != 1 {
		t.Fatalf("会话代 = %d，应为 1，错误 = %v", generation, err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "cli.admin.password.set" ||
		rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("设置口令应写命令行审计：%+v", rows)
	}
}

func TestServeAdminUnlockListsAndClearsLock(t *testing.T) {
	path, masterKey := prepareServerRoleVault(t)
	st, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1800000000, 0)
	for i := 0; i < 5; i++ {
		if _, err := st.RecordWebAdminLoginFailure("192.0.2.10", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "serve", "admin", "unlock")
	if !strings.Contains(out, "192.0.2.10") || !strings.Contains(out, "剩余") {
		t.Fatalf("unlock 不带参数应列出锁定与剩余时间：%s", out)
	}
	out = runCli(t, "serve", "admin", "unlock", "192.0.2.10")
	if !strings.Contains(out, "已解除") {
		t.Fatalf("unlock 带 IP 应确认解除：%s", out)
	}
	st, err = store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state, err := st.WebAdminLoginState("192.0.2.10", base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if state.Locked {
		t.Fatalf("命令执行后仍处于锁定状态：%+v", state)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "cli.admin.unlock" || rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("解除锁定应写命令行审计：%+v", rows)
	}
}

func TestServeAdminLogoutAllAndStatus(t *testing.T) {
	path, masterKey := prepareServerRoleVault(t)
	st, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWebAdminPassword([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "serve", "admin", "logout-all")
	if !strings.Contains(out, "已注销") {
		t.Fatalf("logout-all 输出应确认成功：%s", out)
	}
	st, err = store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	generation, err := st.WebAdminSessionGeneration()
	if err != nil || generation != 2 {
		t.Fatalf("logout-all 后会话代 = %d，应为 2，错误 = %v", generation, err)
	}

	var statusOutput bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&statusOutput)
	root.SetArgs([]string{"serve", "admin", "status"})
	if err := root.Execute(); err != nil {
		t.Fatalf("serve admin status: %v", err)
	}
	output := statusOutput.String()
	for _, want := range []string{"已设置", "https://127.0.0.1:7879/ui/", "sha256:", "serve.ui_listen"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status 输出应包含 %q：%s", want, output)
		}
	}
}
