package cli

import (
	"bytes"
	"context"
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
