package softwareinventory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

// TestBuildPOSIXInventoryCommandIsFixedTemplate 覆盖所有 POSIX 服务器条目
// 共用的固定命令集：版本号是 posix-v2，并按协议输出新增的系统识别、
// netstat、lsof、macOS 应用与 Homebrew、群晖套件节。
func TestBuildPOSIXInventoryCommandIsFixedTemplate(t *testing.T) {
	command, err := BuildInventoryCommand(models.ShellPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, `echo "$M BEGIN posix-v2"`) {
		t.Fatalf("固定命令集版本应为 posix-v2，命令开头：\n%s", firstLines(command, 3))
	}
	want := "/bin/sh -c " + sshbroker.POSIXShellQuote(posixCommandSetV2)
	if command != want {
		t.Fatalf("POSIX 固定命令集必须由 /bin/sh 执行：\n得到前缀 %q\n期待前缀 %q",
			firstLines(command, 1), firstLines(want, 1))
	}
	for _, section := range []string{
		"os", "listen-netstat", "listen-lsof", "apps", "brew", "synology-packages",
	} {
		if !strings.Contains(command, `echo "$M SECTION `+section+`"`) {
			t.Fatalf("固定命令集缺少 %s 节", section)
		}
	}
	if strings.Contains(command, "Win32_Product") {
		t.Fatal("固定命令集不应包含有副作用的 Win32_Product 查询")
	}
}

// TestPOSIXInventoryCommandWrapperRunsUnderLoginShells 模拟目标账户登录
// shell 为 bash、zsh 或 fish：远端命令仍必须先进入 /bin/sh，并且安全引
// 用不能改变脚本内容。
func TestPOSIXInventoryCommandWrapperRunsUnderLoginShells(t *testing.T) {
	script := `printf '%s\n' "SSHMGR-INVENTORY END"`
	remoteCommand := "/bin/sh -c " + sshbroker.POSIXShellQuote(script)
	shells := []string{"/bin/bash"}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, zsh)
	} else {
		t.Log("本机没有 zsh，包装层测试只覆盖 bash")
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		shells = append(shells, fish)
	}
	for _, shell := range shells {
		output, err := exec.Command(shell, "-c", remoteCommand).CombinedOutput()
		if err != nil {
			t.Fatalf("%s 执行生成的远端命令失败：%v\n输出：%s", shell, err, output)
		}
		if strings.TrimSpace(string(output)) != "SSHMGR-INVENTORY END" {
			t.Fatalf("%s 执行命令时没有输出结尾标记，得到：%q", shell, string(output))
		}
	}
}

// TestPOSIXInventoryAppsSectionSurvivesEmptyUtilitiesInZsh 只执行嵌入脚本
// 的 apps 节，并把 /Applications 替换成测试临时目录，避免读写真实系统位
// 置。Utilities 为空时 zsh 也不能因通配符无匹配而中断。
func TestPOSIXInventoryAppsSectionSurvivesEmptyUtilitiesInZsh(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("本机没有 zsh，跳过登录 shell 兼容测试")
	}
	applications := filepath.Join(t.TempDir(), "Applications")
	if err := os.MkdirAll(filepath.Join(applications, "Example.app"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(applications, "Utilities"), 0o700); err != nil {
		t.Fatal(err)
	}

	appsSection := scriptSection(t, posixCommandSetV2,
		`echo "$M SECTION apps"`, `echo "$M SECTION brew"`)
	appsSection = strings.ReplaceAll(appsSection, "/Applications", applications)
	script := "M=SSHMGR-INVENTORY\n" + appsSection
	output, err := exec.Command(zsh, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("zsh 执行 apps 节失败：%v\n输出：%s", err, output)
	}
	if !strings.Contains(string(output), "SSHMGR-INVENTORY SECTION apps") ||
		!strings.Contains(string(output), "SSHMGR-INVENTORY STATUS apps 0") {
		t.Fatalf("apps 节没有完整结束，输出：%s", output)
	}
	if strings.Contains(string(output), "no matches found") {
		t.Fatalf("空的 Utilities 目录不应让 zsh 中断：%s", output)
	}
}

// TestBuildInventoryCommandRejectsUnsupportedShells 保持命令生成的显式边
// 界：没有固定命令集的 shell 不能静默退回 POSIX 命令。
func TestBuildInventoryCommandRejectsUnsupportedShells(t *testing.T) {
	if _, err := BuildInventoryCommand(models.ShellType("unknown")); err == nil {
		t.Fatal("未知 shell 类型应报错")
	}
}

func firstLines(value string, count int) string {
	lines := strings.Split(value, "\n")
	if len(lines) < count {
		count = len(lines)
	}
	return strings.Join(lines[:count], "\n")
}

func scriptSection(t *testing.T, script, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(script, startMarker)
	if start < 0 {
		t.Fatalf("脚本缺少开始标记 %q", startMarker)
	}
	relativeEnd := strings.Index(script[start:], endMarker)
	if relativeEnd < 0 {
		t.Fatalf("脚本缺少结束标记 %q", endMarker)
	}
	return script[start : start+relativeEnd]
}
