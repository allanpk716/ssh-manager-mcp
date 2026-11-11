package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSharesUnmountRequiresYesOnNonInteractiveTerminal(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	oldIsInteractive := sharesStdinIsInteractive
	sharesStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { sharesStdinIsInteractive = oldIsInteractive })

	errText := runCliErr(t, "shares", "unmount", "datasets", "linux-web")
	for _, want := range []string{"非交互终端", "--yes", "共享存储卸载"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("错误应包含 %q：%s", want, errText)
		}
	}
}

func TestSharesUnmountRejectsWindowsServer(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")

	errText := runCliErr(t, "shares", "unmount", "datasets", "windows-web", "--yes")
	for _, want := range []string{"Windows 服务器不能执行共享存储卸载", "sshmgr 不在其上留下任何持久改动"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("错误应包含 %q：%s", want, errText)
		}
	}
}

func TestSharesUnmountShowsPreviewAndAcceptsInteractiveCancellation(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	oldIsInteractive := sharesStdinIsInteractive
	sharesStdinIsInteractive = func() bool { return true }
	t.Cleanup(func() { sharesStdinIsInteractive = oldIsInteractive })

	root := NewRootCmd()
	out := &bytes.Buffer{}
	in := strings.NewReader("n\n")
	root.SetOut(out)
	root.SetIn(in)
	root.SetArgs([]string{"shares", "unmount", "datasets", "linux-web"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"将卸载共享存储 datasets", "服务器 linux-web", "已取消，未卸载共享存储"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("输出应包含 %q：%s", want, out.String())
		}
	}
}

func TestSharesUnmountRejectsCacheClient(t *testing.T) {
	seedSharesClientCache(t, true)

	errText := runCliErr(t, "shares", "unmount", "datasets", "linux-web", "--yes")
	if !strings.Contains(errText, "本机是缓存客户端") || !strings.Contains(errText, "权威端执行") {
		t.Fatalf("缓存客户端应被拒绝：%s", errText)
	}
}
