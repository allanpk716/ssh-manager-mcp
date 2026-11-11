package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSharesReselectRequiresYesOnNonInteractiveTerminal(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	oldIsInteractive := sharesStdinIsInteractive
	sharesStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { sharesStdinIsInteractive = oldIsInteractive })

	errText := runCliErr(t, "shares", "reselect", "datasets", "linux-web")
	for _, want := range []string{"非交互终端", "--yes", "共享存储重选地址"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("错误应包含 %q：%s", errText, want)
		}
	}
}

func TestSharesReselectShowsPreviewAndAcceptsInteractiveCancellation(t *testing.T) {
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
	root.SetArgs([]string{"shares", "reselect", "datasets", "linux-web"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"将重选共享存储 datasets", "服务器 linux-web", "已取消，未重选共享存储地址",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("输出应包含 %q：%s", want, out.String())
		}
	}
}

func TestSharesReselectRejectsCacheClient(t *testing.T) {
	seedSharesClientCache(t, true)

	errText := runCliErr(t, "shares", "reselect", "datasets", "linux-web", "--yes")
	if !strings.Contains(errText, "本机是缓存客户端") ||
		!strings.Contains(errText, "权威端执行") {
		t.Fatalf("缓存客户端应被拒绝：%s", errText)
	}
}

func TestSharesReselectRejectsWindowsServer(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")

	errText := runCliErr(t, "shares", "reselect", "datasets", "windows-web", "--yes")
	for _, want := range []string{
		"Windows 服务器不能重选共享存储地址", "每条命令执行前本来就会重新选址",
	} {
		if !strings.Contains(errText, want) {
			t.Fatalf("错误应包含 %q：%s", errText, want)
		}
	}
}
