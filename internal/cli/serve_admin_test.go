package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
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

func TestAuthorityWebUIStatusWithoutCertDoesNotCreateCert(t *testing.T) {
	prepareServerRoleVault(t)
	st, err := openUnlockedStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var output bytes.Buffer
	if err := writeAuthorityWebUIStatus(&output, st); err != nil {
		t.Fatalf("证书不存在时状态命令不应失败：%v", err)
	}
	got := output.String()
	for _, want := range []string{"尚未生成证书", "先启动一次 `sshmgr serve`", "默认证书路径", "--tls-cert"} {
		if !strings.Contains(got, want) {
			t.Fatalf("状态输出应包含 %q：%s", want, got)
		}
	}
	for _, path := range []string{os.Getenv("SSHMGR_SERVE_CERT"), os.Getenv("SSHMGR_SERVE_KEY"), os.Getenv("SSHMGR_SERVE_MARKER")} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("状态命令不应创建 %s：%v", path, err)
		}
	}
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

func TestServeCommandDescriptionsUseEnglish(t *testing.T) {
	root := NewRootCmd()
	serve := findCommand(t, root.Command, "serve")
	if serve.Short != "Run the authoritative vault server (/snapshot + /pair) for the multi-machine bridge posture" {
		t.Fatalf("serve 命令 Short 应保持英文原文本：%q", serve.Short)
	}
	if !strings.Contains(serve.Long, "It serves exactly: the authenticated /snapshot endpoint") ||
		!strings.Contains(serve.Long, "in a later release — the web admin UI") {
		t.Fatalf("serve 命令 Long 应保持英文原文本：\n%s", serve.Long)
	}

	admin := newServeAdminCmd()
	if admin.Short != "Manage the authoritative Web UI administrator password and sessions" {
		t.Fatalf("serve admin 的 Short = %q", admin.Short)
	}
	wantShort := map[string]string{
		"set":        "Set or reset the authoritative Web UI administrator password",
		"unlock":     "Unlock a Web UI login lock; without an IP, list current locks",
		"logout-all": "Log out all authoritative Web UI sessions",
		"status":     "Show authoritative Web UI addresses and certificate fingerprint",
	}
	for name, want := range wantShort {
		got := findCommand(t, admin, name).Short
		if got != want {
			t.Fatalf("serve admin %s 的 Short = %q，应为 %q", name, got, want)
		}
	}
}

func findCommand(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, command := range parent.Commands() {
		if command.Name() == name {
			return command
		}
	}
	t.Fatalf("找不到命令 %s", name)
	return nil
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
	if len(rows) != 1 || rows[0].Action != "admin.password_set" ||
		rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("设置口令应写命令行审计：%+v", rows)
	}
}

func TestServeAdminSetAllowsIncompleteSetupFlag(t *testing.T) {
	path, masterKey := withCliStoreEnv(t)
	createVaultStoreAt(t, path, masterKey)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: false}); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("setup_complete 为假时也应能设置管理员口令：%s", out)
	}
	st, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	passwordSet, err := st.WebAdminPasswordIsSet()
	if err != nil {
		t.Fatal(err)
	}
	if !passwordSet {
		t.Fatal("管理员口令应已写入 vault")
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
	if len(rows) != 1 || rows[0].Action != "admin.unlock" || rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("解除锁定应写命令行审计：%+v", rows)
	}
}

func TestServeAdminLogoutAllAndStatus(t *testing.T) {
	path, masterKey := prepareServerRoleVault(t)
	if _, _, _, err := mcpserver.LoadOrCreateServeCert(); err != nil {
		t.Fatal(err)
	}
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
