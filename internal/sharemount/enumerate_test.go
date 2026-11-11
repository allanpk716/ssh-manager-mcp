package sharemount

import (
	"os/exec"
	"strings"
	"testing"
)

// TestEnumerateCommandsAreValidShell 只验证固定枚举命令的 shell 语法。真实
// 的行为断言在共享存储管理模块测试里经进程内 SSH 服务完成。
func TestEnumerateCommandsAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("本机没有 sh，跳过语法检查")
	}
	commands := map[string]string{
		"Linux":  buildLinuxEnumerateCommand(),
		"Darwin": buildDarwinEnumerateCommand(),
	}
	for system, command := range commands {
		t.Run(system, func(t *testing.T) {
			process := exec.Command("sh", "-n")
			process.Stdin = strings.NewReader(command)
			output, err := process.CombinedOutput()
			if err != nil {
				t.Fatalf("固定枚举命令语法不合法：%v\n%s\n%s", err, output, command)
			}
		})
	}
}
