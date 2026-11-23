package softwareinventory

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

// TestBuildPOSIXInventoryCommandIsFixedTemplate 覆盖所有 POSIX 服务器条目
// 共用的固定命令集：版本号是 posix-v3，并按协议输出系统识别、netstat、
// lsof、macOS 应用、Homebrew、群晖套件与第 3 版容器节。
func TestBuildPOSIXInventoryCommandIsFixedTemplate(t *testing.T) {
	command, err := BuildInventoryCommand(models.ShellPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, `echo "$M BEGIN posix-v3"`) {
		t.Fatalf("固定命令集版本应为 posix-v3，命令开头：\n%s", firstLines(command, 3))
	}
	want := "/bin/sh -c " + sshbroker.POSIXShellQuote(posixCommandSet)
	if command != want {
		t.Fatalf("POSIX 固定命令集必须由 /bin/sh 执行：\n得到前缀 %q\n期待前缀 %q",
			firstLines(command, 1), firstLines(want, 1))
	}
	for _, section := range []string{
		"os", "listen-netstat", "listen-lsof", "apps", "brew", "synology-packages",
		"containers-v3", "container-image-refs", "container-image-labels",
		"pending-updates-apt", "pending-updates-dnf",
	} {
		if !strings.Contains(command, `echo "$M SECTION `+section+`"`) {
			t.Fatalf("固定命令集缺少 %s 节", section)
		}
	}
	// #372：Docker 19.03 的 docker ps --format 没有 .State 字段，容器节只保留
	// 四列；运行状态改由 container-image-refs 节的 docker inspect 第三列提供。
	// docker inspect --format 不解析 \t，分隔符必须由模板里的 printf 输出。
	if strings.Contains(posixCommandSet, "{{.State}}") {
		t.Fatal("containers-v3 节不得使用 Docker 20.10 才加入的 {{.State}} 字段")
	}
	if !strings.Contains(posixCommandSet,
		`docker ps -a --no-trunc --format '{{.ID}}\t{{.Image}}\t{{.Names}}\t{{.Ports}}'`) {
		t.Fatalf("containers-v3 节应只输出容器标识、镜像、容器名与端口四列：\n%s",
			firstLines(command, 3))
	}
	if !strings.Contains(posixCommandSet, `{{printf "%s\t%s\t" .Id .Image}}{{.State.Status}}`) {
		t.Fatal("container-image-refs 节应从 docker inspect 的 .State.Status 取运行状态并用制表符分列")
	}
	// 经典镜像存储下 .Config 是结构体，containerd 镜像存储下模板的结构体路径
	// 也是结构体，字段访问对两种存储都成立；镜像编号用 .ID（Go 字段名）避免
	// docker CLI 退回缺键即报错的 JSON map 路径。
	if strings.Contains(posixCommandSet, `{{if .Config.Labels}}`) {
		t.Fatal("container-image-labels 节不得直接访问 .Config.Labels")
	}
	if !strings.Contains(posixCommandSet,
		`{{printf "%s\t" .ID}}{{with .Config}}{{with .Labels}}`+
			`{{index . "org.opencontainers.image.version"}}{{end}}{{end}}`) {
		t.Fatal("container-image-labels 节应安全读取镜像版本标签")
	}
	if strings.Contains(command, "Win32_Product") {
		t.Fatal("固定命令集不应包含有副作用的 Win32_Product 查询")
	}
	for _, forbiddenCommand := range []string{
		"docker exec", "apt update", "apt-get update", "dnf makecache", "yum makecache",
		"apt upgrade", "apt-get upgrade", "dnf upgrade", "dnf install", "yum install",
		"apt install", "apt-get install",
	} {
		if strings.Contains(command, forbiddenCommand) {
			t.Fatalf("固定命令集不应包含有副作用的命令 %q", forbiddenCommand)
		}
	}
	if !strings.Contains(command, "apt list --upgradable") {
		t.Fatal("固定命令集应只读 apt 本地缓存中的可升级列表")
	}
	for _, line := range strings.Split(command, "\n") {
		if strings.Contains(line, "check-update") && !strings.Contains(line, " -C ") {
			t.Fatalf("dnf/yum 检查待装安全更新时必须只用本地缓存：%s", line)
		}
	}
}

// TestPOSIXInventoryCommandWrapperRunsUnderLoginShells 模拟目标账户登录
// shell 为 bash、zsh 或 fish：远端命令仍必须先进入 /bin/sh，并且安全引
// 用不能改变脚本内容。
func TestPOSIXInventoryCommandWrapperRunsUnderLoginShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("目标登录 shell 包装只在 POSIX 系统上测试")
	}
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

	appsSection := scriptSection(t, posixCommandSet,
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
