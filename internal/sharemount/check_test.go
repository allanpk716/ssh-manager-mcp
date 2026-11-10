package sharemount

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func checkTestShares() []*models.SharedStorage {
	return []*models.SharedStorage{
		{ID: "share-001", Name: "datasets"},
		{ID: "share-002", Name: "archive"},
	}
}

// TestShareCheckCommandsAreReadOnly 检查现场检测固定命令的只读约束：不得
// 触碰文件锁，也不得包含任何改写远端文件或刷新挂载的动作。
func TestShareCheckCommandsAreReadOnly(t *testing.T) {
	commands := map[string]string{
		"linux 全量":  buildLinuxShareCheckCommand(checkTestShares()),
		"darwin 全量": buildDarwinShareCheckCommand(checkTestShares()),
		"linux 受限":  buildLimitedShareCheckCommand(linuxCheckMarker, checkTestShares()),
		"darwin 受限": buildLimitedShareCheckCommand(darwinCheckMarker, checkTestShares()),
	}
	for name, command := range commands {
		for _, forbidden := range []string{
			linuxLockPath, darwinLockPath, "install ", "mv ", "rm ", "touch ",
			"mkdir", "systemctl", "automount -cv", ">>",
		} {
			if strings.Contains(command, forbidden) {
				t.Fatalf("%s 检测命令包含改写动作 %q：\n%s", name, forbidden, command)
			}
		}
		if !strings.Contains(command, "/mnt/sshmgr/datasets") ||
			!strings.Contains(command, "/mnt/sshmgr/archive") {
			t.Fatalf("%s 检测命令应包含每个共享的挂载点：\n%s", name, command)
		}
	}
}

// TestShareCheckCommandMatchesEncodedMarkerName 检查命令按跨操作系统标记
// 的共享名编码匹配远端条目，特殊字符共享名不会被误配。
func TestShareCheckCommandMatchesEncodedMarkerName(t *testing.T) {
	shares := []*models.SharedStorage{{ID: "share-001", Name: "data set/2026"}}
	for _, command := range []string{
		buildLinuxShareCheckCommand(shares),
		buildDarwinShareCheckCommand(shares),
	} {
		if !strings.Contains(command, "data%20set%2F2026") {
			t.Fatalf("检测命令应使用标记的百分号编码共享名：\n%s", command)
		}
		if !strings.Contains(command, "'/mnt/sshmgr/data set/2026'") {
			t.Fatalf("检测命令应按原始共享名检查挂载点：\n%s", command)
		}
	}
}

func TestShareCheckCommandsPassPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	for name, command := range map[string]string{
		"linux 全量":  buildLinuxShareCheckCommand(checkTestShares()),
		"darwin 全量": buildDarwinShareCheckCommand(checkTestShares()),
		"linux 受限":  buildLimitedShareCheckCommand(linuxCheckMarker, checkTestShares()),
		"darwin 受限": buildLimitedShareCheckCommand(darwinCheckMarker, checkTestShares()),
	} {
		script := t.TempDir() + "/check-command.sh"
		if err := os.WriteFile(script, []byte(command), 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("/bin/sh", "-n", script).CombinedOutput(); err != nil {
			t.Fatalf("%s 检测命令不是合法 POSIX shell 脚本：%v\n%s\n%s", name, err, output, command)
		}
	}
}

func TestParseShareCheckOutput(t *testing.T) {
	shares := checkTestShares()
	states, autoMaster, err := parseShareCheckOutput(strings.Join([]string{
		"SSHMGR-LINUX-CHECK mode=full",
		"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=2 mounted=yes",
		"SSHMGR-LINUX-CHECK index=2 managed=no share= version= mounted=no",
		"",
	}, "\n"), linuxCheckMarker, shares, true)
	if err != nil {
		t.Fatal(err)
	}
	if autoMaster {
		t.Fatal("Linux 检测没有主配置引用结论")
	}
	if len(states) != 2 || !states[0].Managed || states[0].Version != 2 || !states[0].Mounted ||
		states[1].Managed || states[1].Mounted {
		t.Fatalf("Linux 检测状态 = %+v", states)
	}

	states, autoMaster, err = parseShareCheckOutput(strings.Join([]string{
		"SSHMGR-DARWIN-CHECK mode=full",
		"SSHMGR-DARWIN-CHECK index=1 managed=yes share=datasets version=1 mounted=yes",
		"SSHMGR-DARWIN-CHECK index=2 managed=no share= version= mounted=no",
		"SSHMGR-DARWIN-CHECK auto-master=no",
		"",
	}, "\n"), darwinCheckMarker, shares, true)
	if err != nil {
		t.Fatal(err)
	}
	if autoMaster {
		t.Fatal("输出中的主配置引用应判定为缺失")
	}
	if len(states) != 2 || !states[0].Managed || states[0].Version != 1 || !states[0].Mounted ||
		states[1].Managed || states[1].Mounted {
		t.Fatalf("macOS 检测状态 = %+v", states)
	}

	states, _, err = parseShareCheckOutput(strings.Join([]string{
		"SSHMGR-LINUX-CHECK mode=limited",
		"SSHMGR-LINUX-CHECK index=1 mounted=yes",
		"SSHMGR-LINUX-CHECK index=2 mounted=no",
		"",
	}, "\n"), linuxCheckMarker, shares, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || !states[0].Mounted || states[1].Mounted ||
		states[0].Managed || states[1].Managed {
		t.Fatalf("受限检测状态 = %+v", states)
	}
}

func TestParseShareCheckOutputRejectsMalformedLines(t *testing.T) {
	shares := checkTestShares()
	for name, output := range map[string]string{
		"缺少模式行": "SSHMGR-LINUX-CHECK index=1 mounted=no\n",
		"缺少共享行": "SSHMGR-LINUX-CHECK mode=full\n",
		"非法版本号": strings.Join([]string{
			"SSHMGR-LINUX-CHECK mode=full",
			"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=x mounted=yes",
			"SSHMGR-LINUX-CHECK index=2 managed=no share= version= mounted=no",
		}, "\n"),
		"共享名不匹配": strings.Join([]string{
			"SSHMGR-LINUX-CHECK mode=full",
			"SSHMGR-LINUX-CHECK index=1 managed=yes share=other version=1 mounted=yes",
			"SSHMGR-LINUX-CHECK index=2 managed=no share= version= mounted=no",
		}, "\n"),
	} {
		if _, _, err := parseShareCheckOutput(output, linuxCheckMarker, shares, true); err == nil {
			t.Fatalf("%s 应解析失败", name)
		}
	}
}

type scriptedCheckExec struct {
	mu          sync.Mutex
	requests    []RemoteExecRequest
	uname       string
	checkStdout string
	execDelay   time.Duration
	execErr     error
}

func (s *scriptedCheckExec) exec(
	_ context.Context, _ *models.Server, request RemoteExecRequest,
) (RemoteExecResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	if s.execDelay > 0 {
		time.Sleep(s.execDelay)
	}
	if s.execErr != nil {
		return RemoteExecResult{}, s.execErr
	}
	if strings.TrimSpace(request.Command) == "uname -s" {
		return RemoteExecResult{Stdout: s.uname + "\n", ExitCode: 0}, nil
	}
	return RemoteExecResult{Stdout: s.checkStdout, ExitCode: 0}, nil
}

func (s *scriptedCheckExec) recorded() []RemoteExecRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RemoteExecRequest(nil), s.requests...)
}

func TestServiceCheckLinuxUsesPrivilegedFixedCommand(t *testing.T) {
	executor := &scriptedCheckExec{
		uname: "Linux",
		checkStdout: strings.Join([]string{
			"SSHMGR-LINUX-CHECK mode=full",
			"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=1 mounted=yes",
			"",
		}, "\n"),
	}
	service := &Service{RemoteExec: executor.exec}
	server := &models.Server{Name: "linux-web", SudoCredentialID: "sudo-1"}

	result, err := service.Check(context.Background(), CheckRequest{
		Server: server, Shares: []*models.SharedStorage{{ID: "share-001", Name: "datasets"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := executor.recorded()
	if len(requests) != 2 || requests[0].Command != "uname -s" || requests[0].Privileged {
		t.Fatalf("固定命令序列 = %+v", requests)
	}
	if !requests[1].Privileged || !strings.Contains(requests[1].Command, linuxCheckMarker) ||
		len(requests[1].StandardInput) != 0 {
		t.Fatalf("Linux 全量检测应走提权固定命令且不传标准输入：%+v", requests[1])
	}
	if result.System != CheckSystemLinux || result.Restricted ||
		len(result.States) != 1 || !result.States[0].Managed || result.States[0].Version != 1 {
		t.Fatalf("Linux 检测结论 = %+v", result)
	}
}

func TestServiceCheckWithoutSudoRunsLimitedCommand(t *testing.T) {
	executor := &scriptedCheckExec{
		uname: "Darwin",
		checkStdout: strings.Join([]string{
			"SSHMGR-DARWIN-CHECK mode=limited",
			"SSHMGR-DARWIN-CHECK index=1 mounted=no",
			"",
		}, "\n"),
	}
	service := &Service{RemoteExec: executor.exec}

	result, err := service.Check(context.Background(), CheckRequest{
		Server: &models.Server{Name: "darwin-web"},
		Shares: []*models.SharedStorage{{ID: "share-001", Name: "datasets"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := executor.recorded()
	if len(requests) != 2 || requests[1].Privileged ||
		!strings.Contains(requests[1].Command, darwinCheckMarker) {
		t.Fatalf("无 sudo 时应执行受限固定命令：%+v", requests)
	}
	if result.System != CheckSystemDarwin || !result.Restricted || result.States[0].Mounted {
		t.Fatalf("受限检测结论 = %+v", result)
	}
}

func TestServiceCheckRejectsUnsupportedSystem(t *testing.T) {
	executor := &scriptedCheckExec{uname: "SunOS"}
	service := &Service{RemoteExec: executor.exec}

	_, err := service.Check(context.Background(), CheckRequest{
		Server: &models.Server{Name: "solaris", SudoCredentialID: "sudo-1"},
		Shares: []*models.SharedStorage{{ID: "share-001", Name: "datasets"}},
	})
	if err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("不支持的系统应报错，实际：%v", err)
	}
}

func TestServiceCheckHasPerServerTimeout(t *testing.T) {
	executor := &scriptedCheckExec{uname: "Linux", execDelay: 300 * time.Millisecond}
	service := &Service{RemoteExec: executor.exec, CheckTimeout: 30 * time.Millisecond}

	start := time.Now()
	_, err := service.Check(context.Background(), CheckRequest{
		Server: &models.Server{Name: "linux-web", SudoCredentialID: "sudo-1"},
		Shares: []*models.SharedStorage{{ID: "share-001", Name: "datasets"}},
	})
	if err == nil {
		t.Fatal("每台服务器的检测超时应让慢服务器失败")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("检测超时没有及时生效：%v", time.Since(start))
	}
}
