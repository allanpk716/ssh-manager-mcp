package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// runTUICommand 以给定参数驱动根命令的 `tui` 子命令，返回标准输出、标准
// 错误、命令错误与剩余标准输入字节数，供表格测试逐项断言。
func runTUICommand(t *testing.T, args ...string) (string, string, error, int) {
	t.Helper()
	pinTUIMachineEnv(t)
	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	stdin := strings.NewReader("标准输入不应被读取")
	root.SetIn(stdin)
	root.SetArgs(append([]string{"tui"}, args...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err, stdin.Len()
}

// pinTUIMachineEnv 把命令收尾会读到的本机状态固定在空临时目录，避免测试
// 依赖开发机的真实角色文件。
func pinTUIMachineEnv(t *testing.T) {
	t.Helper()
	withEnv(t, map[string]string{
		"SSHMGR_CONFIG_ROOT": t.TempDir(),
		"SSHMGR_VAULT_ROOT":  t.TempDir(),
		"SSHMGR_CACHE_DIR":   "",
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})
}

// TestTUICmdPrintsMigrationNotice 锁定旧命令的全部行为：无论带什么参数
// （含旧的 --mode 取值、多余位置参数、未知参数），都只向标准错误打印迁移
// 提示并以退出码 1 结束；不向标准输出写内容，也不读标准输入。
func TestTUICmdPrintsMigrationNotice(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "无参数"},
		{name: "客户端旧取值", args: []string{"--mode", "client"}},
		{name: "权威端旧取值", args: []string{"--mode", "broker"}},
		{name: "任意取值", args: []string{"--mode", "anything"}},
		{name: "多余位置参数", args: []string{"extra", "arguments"}},
		{name: "未知参数", args: []string{"--unknown-flag", "value"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err, stdinLeft := runTUICommand(t, tc.args...)
			if err == nil {
				t.Fatalf("sshmgr tui %v 应返回错误以产生退出码 1", tc.args)
			}
			if code := ExitCodeFor(err); code != 1 {
				t.Fatalf("退出码 = %d，应为 1（错误：%v）", code, err)
			}
			// 提示文字来自规格 #168「隐藏命令 `sshmgr tui`」一节的逐字要求。
			const want = "TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`"
			if got := strings.TrimSpace(stderr); got != want {
				t.Fatalf("标准错误 = %q，应为迁移提示 %q", got, want)
			}
			if stdout != "" {
				t.Fatalf("标准输出应为空，实际为 %q", stdout)
			}
			if stdinLeft != len("标准输入不应被读取") {
				t.Fatalf("命令不应读标准输入，剩余字节数 = %d", stdinLeft)
			}
		})
	}
}

// TestTUICmdHiddenFromHelp 锁定 `tui` 是隐藏命令：根命令帮助里不再出现它。
func TestTUICmdHiddenFromHelp(t *testing.T) {
	pinTUIMachineEnv(t)
	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sshmgr --help 不应失败：%v", err)
	}
	if strings.Contains(strings.ToLower(stdout.String()+stderr.String()), "tui") {
		t.Fatalf("根命令帮助不应再出现 tui：\n%s%s", stdout, stderr)
	}
}
