package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestSharesRevokeWindowsServerDoesNotConnectAndExplainsInjection(t *testing.T) {
	databasePath, masterKey := seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")

	output := runCli(t, "shares", "revoke", "datasets", "windows-web", "--yes")
	for _, want := range []string{
		"已撤销共享存储 datasets 在服务器 windows-web 上的可挂载范围",
		"版本号：2",
		"未连接 Windows 服务器",
		"Windows 服务器不留共享存储配置",
		"在线客户端默认 5 分钟内生效",
		"正在执行的命令保留盘符直到结束",
		"长期离线的客户端同步前仍会注入",
		"真正的撤销手段是更换共享密码",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("输出应包含 %q：\n%s", want, output)
		}
	}

	s, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 || len(share.MountableServers) != 1 {
		t.Fatalf("撤销 Windows 服务器后登记 = %+v", share)
	}
	for _, server := range share.MountableServers {
		if server.Name == "windows-web" {
			t.Fatalf("Windows 服务器仍在可挂载范围内：%+v", share.MountableServers)
		}
	}
	if strings.Contains(output, "远端持久配置已删除") {
		t.Fatalf("Windows 服务器没有远端持久配置，不应输出已删除：%s", output)
	}
}

func TestSharesRevokeNewPasswordFromStdinListsRemainingServers(t *testing.T) {
	databasePath, masterKey := seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetIn(strings.NewReader("second-mount-password"))
	root.SetArgs([]string{
		"shares", "revoke", "datasets", "windows-web",
		"--yes", "--new-password-stdin",
	})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("shares revoke: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"已更换共享存储挂载密码",
		"仍可挂载的 Linux 与 macOS 服务器：linux-web",
		"这些服务器需要 agent 按新版本号调用 mount_share 更新挂载",
		"桌面上手动添加的共享连接也需要更换密码",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("输出应包含 %q：\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "second-mount-password") {
		t.Fatalf("输出泄露新挂载密码：\n%s", out.String())
	}

	s, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 || len(share.MountableServers) != 1 ||
		share.MountableServers[0].Name != "linux-web" {
		t.Fatalf("撤销加换密码后的共享存储登记 = %+v", share)
	}
	value, err := s.SecretValueForSharedStorageMount(share.PasswordSecretID)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Value) != "second-mount-password" {
		t.Fatal("命令应保存标准输入里的新挂载密码")
	}
	rows, err := s.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || strings.Contains(rows[0].Command, "second-mount-password") {
		t.Fatalf("共享撤销审计应只记录是否换密码：%+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["password_changed"] != true {
		t.Fatalf("共享撤销审计摘要应记录换密码：%#v", summary)
	}
}

func TestSharesRevokeNewPasswordFromFileChangesStoredPassword(t *testing.T) {
	databasePath, masterKey := seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	passwordPath := filepath.Join(t.TempDir(), "new-password")
	if err := os.WriteFile(passwordPath, []byte("file-mount-password"), 0600); err != nil {
		t.Fatal(err)
	}

	output := runCli(t, "shares", "revoke", "datasets", "windows-web", "--yes",
		"--new-password-from-file", passwordPath)
	for _, want := range []string{"已更换共享存储挂载密码", "仍可挂载的 Linux 与 macOS 服务器：linux-web"} {
		if !strings.Contains(output, want) {
			t.Fatalf("输出应包含 %q：\n%s", want, output)
		}
	}
	if strings.Contains(output, "file-mount-password") {
		t.Fatalf("输出泄露新挂载密码：\n%s", output)
	}

	s, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	value, err := s.SecretValueForSharedStorageMount(share.PasswordSecretID)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Value) != "file-mount-password" {
		t.Fatal("命令应按字节保存文件里的新挂载密码")
	}
}

func TestSharesRevokeDoesNotAcceptPasswordAsArgument(t *testing.T) {
	databasePath, masterKey := seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")

	errText := runCliErr(t, "shares", "revoke", "datasets", "windows-web",
		"argument-mount-password", "--yes")
	if !strings.Contains(errText, "accepts 2 arg(s), received 3") ||
		strings.Contains(errText, "argument-mount-password") {
		t.Fatalf("命令行应拒绝第三个位置参数且不回显它：%s", errText)
	}
	if errText := runCliErr(t, "shares", "revoke", "datasets", "windows-web",
		"--new-password", "argument-mount-password", "--yes"); errText == "" {
		t.Fatal("命令行不应提供值形式的换密码参数")
	}

	s, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 1 || len(share.MountableServers) != 2 {
		t.Fatalf("被拒绝的命令不应改动登记：%+v", share)
	}
}

func TestSharesRevokeNewPasswordValidationErrorDoesNotEchoPassword(t *testing.T) {
	databasePath, masterKey := seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	passwordPath := filepath.Join(t.TempDir(), "short-password")
	if err := os.WriteFile(passwordPath, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}

	errText := runCliErr(t, "shares", "revoke", "datasets", "windows-web", "--yes",
		"--new-password-from-file", passwordPath)
	if errText == "" || strings.Contains(errText, "short") {
		t.Fatalf("换密码错误不应回显新密码：%s", errText)
	}
	s, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 1 || len(share.MountableServers) != 2 {
		t.Fatalf("新密码校验失败时不应撤销共享存储：%+v", share)
	}
}

func TestSharesRevokeRequiresYesOnNonInteractiveTerminal(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	oldIsInteractive := sharesStdinIsInteractive
	sharesStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { sharesStdinIsInteractive = oldIsInteractive })

	errText := runCliErr(t, "shares", "revoke", "datasets", "linux-web")
	for _, want := range []string{"非交互终端", "--yes", "共享存储撤销"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("错误应包含 %q：%s", want, errText)
		}
	}
}

func TestSharesRevokeShowsPreviewAndAcceptsInteractiveCancellation(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	oldIsInteractive := sharesStdinIsInteractive
	sharesStdinIsInteractive = func() bool { return true }
	t.Cleanup(func() { sharesStdinIsInteractive = oldIsInteractive })

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"shares", "revoke", "datasets", "linux-web"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"将撤销共享存储 datasets", "服务器 linux-web", "现有挂载保留", "已取消，未撤销共享存储",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("输出应包含 %q：\n%s", want, out.String())
		}
	}
}

func TestSharesRevokeRejectsCacheClient(t *testing.T) {
	seedSharesClientCache(t, true)

	errText := runCliErr(t, "shares", "revoke", "datasets", "linux-web", "--yes")
	if !strings.Contains(errText, "本机是缓存客户端") || !strings.Contains(errText, "权威端执行") {
		t.Fatalf("缓存客户端应被拒绝：%s", errText)
	}
}
