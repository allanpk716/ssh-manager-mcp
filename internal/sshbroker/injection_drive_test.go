package sshbroker

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"ssh-manager-mcp/internal/models"
)

// TestPrepareExecNetworkDrivePowerShellAssembly 钉住 PowerShell 服务器上网络
// 盘符注入的拼装形态：WNetAddConnection2 经异步调用执行、返回码决定状态、
// 密码只走标准输入、封装脚本里不出现双引号（Windows OpenSSH 对 PowerShell
// 命令文本会剥掉双引号）。
func TestPrepareExecNetworkDrivePowerShellAssembly(t *testing.T) {
	password := []byte("drive-p@ss%'\"|中文\r\n")
	const userCommand = `Get-ChildItem S:\datasets`
	prepared, err := prepareExec(userCommand, ExecInjectionOptions{
		ShellType:   models.ShellPowerShell,
		TargetLayer: ExecLayerNormal,
		Items: []ExecInjection{NetworkDriveInjection{
			ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
			UserName: "mount'user", Password: password,
			StatusMarker: "3f2a9c8d1b7e4056",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := prepared.command
	for _, want := range []string{
		"Add-Type -Namespace SshmgrNative -Name Mpr",
		"WNetAddConnection2(NetResource resource, string password, string username, int flags)",
		"[SshmgrNative.Mpr]::WNetAddConnection2($resource, $d, $c, 0)",
		"if ($__sshmgr_return -eq 0) { $__sshmgr_status = 'ok' } else { $__sshmgr_status = 'error-' + $__sshmgr_return }",
		"if ($__sshmgr_return -isnot [int]) { throw 'sshmgr network drive injection returned no integer result' }",
		"BeginInvoke",
		"AsyncWaitHandle.WaitOne(3000)",
		"AddArgument('S:')",
		`AddArgument('\\192.0.2.10\datasets')`,
		"AddArgument('mount''user')",
		"SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0",
		userCommand,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("wrapped command missing %q:\n%s", want, command)
		}
	}
	if bytes.Contains([]byte(command), password) {
		t.Fatalf("wrapped command leaked the password:\n%s", command)
	}
	if strings.Contains(command, "\"") {
		t.Fatalf("wrapped command contains a double quote:\n%s", command)
	}
	if strings.Contains(command, "New-SmbMapping") {
		t.Fatalf("wrapped command still uses New-SmbMapping:\n%s", command)
	}
	if !bytes.Equal(prepared.frames, password) {
		t.Fatalf("standard input frames = %q, want only the password", prepared.frames)
	}
}

// TestPrepareExecNetworkDriveTimeoutStopIsAsynchronous 钉住超时分支的生成
// 形态：发起异步停止后立刻回传状态行，不等待映射调用返回，也不同
// 步释放仍在停止中的 PowerShell 管道。地址不可达时同步停止可能阻塞约
// 10 秒，会使 3 秒上限失效。
func TestPrepareExecNetworkDriveTimeoutStopIsAsynchronous(t *testing.T) {
	prepared, err := prepareExec("Get-ChildItem S:\\", ExecInjectionOptions{
		ShellType:   models.ShellPowerShell,
		TargetLayer: ExecLayerNormal,
		Items:       []ExecInjection{newNetworkDriveInjection()},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := prepared.command
	for _, want := range []string{
		"$__sshmgr_timed_out = $false",
		"try { $null = $__sshmgr_pipeline.BeginStop($null, $null) } catch { }",
		"if ($null -ne $__sshmgr_pipeline -and -not $__sshmgr_timed_out) { $__sshmgr_pipeline.Dispose() }",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("wrapped command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "$__sshmgr_pipeline.Stop()") {
		t.Fatalf("wrapped command uses synchronous pipeline stop:\n%s", command)
	}
}

// TestPrepareExecNetworkDriveAddsWNetTypeOnce 钉住 WNetAddConnection2 的类型
// 声明只出现一次；声明失败时每个共享都走 error-wrapper，且仍会读走自己的
// 密码帧，保证后续注入与用户命令可以继续。
func TestPrepareExecNetworkDriveAddsWNetTypeOnce(t *testing.T) {
	first := newNetworkDriveInjection()
	second := newNetworkDriveInjection()
	second.ShareName = "reports"
	second.Drive = "T"
	second.Password = []byte("second-drive-password")
	second.StatusMarker = "3f2a9c8d1b7e4057"
	prepared, err := prepareExec("Get-ChildItem S:\\ T:\\", ExecInjectionOptions{
		ShellType:   models.ShellPowerShell,
		TargetLayer: ExecLayerNormal,
		Items:       []ExecInjection{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(prepared.command, "Add-Type -Namespace SshmgrNative -Name Mpr"); got != 1 {
		t.Fatalf("Add-Type declaration count = %d, want 1:\n%s", got, prepared.command)
	}
	for _, want := range []string{
		"if ($__sshmgr_can_map -and $__sshmgr_wnet_ready) {",
		"} elseif (-not $__sshmgr_wnet_ready) {\n  $__sshmgr_status = 'error-wrapper'",
		"SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0",
		"SSHMGR-SMBMAP:3f2a9c8d1b7e4057:1",
	} {
		if !strings.Contains(prepared.command, want) {
			t.Fatalf("wrapped command missing %q:\n%s", want, prepared.command)
		}
	}
	wantFrames := append(append([]byte{}, first.Password...), second.Password...)
	if !bytes.Equal(prepared.frames, wantFrames) {
		t.Fatalf("standard input frames = %q, want both passwords", prepared.frames)
	}
}

func newNetworkDriveInjection() NetworkDriveInjection {
	return NetworkDriveInjection{
		ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
		UserName: "mount-user", Password: []byte("drive-password"),
		StatusMarker: "3f2a9c8d1b7e4056",
	}
}

// TestPrepareExecNetworkDrivePowerShellTotalBudget 钉住注入步骤的固定总时
// 长上限：脚本记录开始时间，轮到一个共享时若「已用时间 + 3 秒」会超过
// 10 秒，就不再发起映射并回传 skipped。
func TestPrepareExecNetworkDrivePowerShellTotalBudget(t *testing.T) {
	prepared, err := prepareExec("Get-ChildItem S:\\", ExecInjectionOptions{
		ShellType:   models.ShellPowerShell,
		TargetLayer: ExecLayerNormal,
		Items:       []ExecInjection{newNetworkDriveInjection()},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"$__sshmgr_injection_started = [DateTime]::UtcNow.Ticks",
		"$__sshmgr_elapsed = ([DateTime]::UtcNow.Ticks - $__sshmgr_started_ticks) / [TimeSpan]::TicksPerMillisecond",
		"$__sshmgr_can_map = ($__sshmgr_elapsed + 3000) -le 10000",
		"$__sshmgr_status = 'skipped'",
	} {
		if !strings.Contains(prepared.command, want) {
			t.Fatalf("wrapped command missing %q:\n%s", want, prepared.command)
		}
	}
}

// TestPrepareExecWindowsCmdUsesShortPowerShellBootstrap 钉住 cmd 服务器上所有
// 注入形态共用的短命令：命令行只含固定引导脚本，注入脚本与用户命令走第
// 一条标准输入帧，长度不随机密项、共享数量或用户命令增长。
func TestPrepareExecWindowsCmdUsesShortPowerShellBootstrap(t *testing.T) {
	environmentValue := []byte("cmd-value-%-^&!-中文")
	password := []byte("drive-p@ss%'\"|()中文\r\n")
	secondPassword := []byte("second-drive-p@ss")
	longShareName := "long-" + strings.Repeat("share-", 100)
	longUserCommand := "echo " + strings.Repeat("x", 6000) + " & exit 42"

	tests := []struct {
		name        string
		userCommand string
		items       []ExecInjection
		wantScript  []string
		wantValues  []byte
	}{
		{
			name:        "仅机密项环境变量",
			userCommand: `set HF_TOKEN`,
			items: []ExecInjection{
				EnvironmentInjection{EnvironmentName: "HF_TOKEN", Value: environmentValue},
			},
			wantScript: []string{"$env:HF_TOKEN", "New-Object byte[] 22"},
			wantValues: environmentValue,
		},
		{
			name:        "一个网络盘符",
			userCommand: `dir S:\datasets`,
			items: []ExecInjection{NetworkDriveInjection{
				ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
				UserName: "mount-user", Password: password,
				StatusMarker: "3f2a9c8d1b7e4056",
			}},
			wantScript: []string{
				"WNetAddConnection2",
				`AddArgument('\\192.0.2.10\datasets')`,
				"SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0",
			},
			wantValues: password,
		},
		{
			name:        "机密项与两个网络盘符",
			userCommand: `dir S:\datasets T:\reports`,
			items: []ExecInjection{
				EnvironmentInjection{EnvironmentName: "HF_TOKEN", Value: environmentValue},
				NetworkDriveInjection{
					ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
					UserName: "mount-user", Password: password,
					StatusMarker: "3f2a9c8d1b7e4056",
				},
				NetworkDriveInjection{
					ShareName: "reports", Drive: "T", Address: "192.0.2.20",
					UserName: "mount-user", Password: secondPassword,
					StatusMarker: "3f2a9c8d1b7e4057",
				},
			},
			wantScript: []string{
				"$env:HF_TOKEN",
				"WNetAddConnection2",
				`AddArgument('\\192.0.2.10\datasets')`,
				`AddArgument('\\192.0.2.20\reports')`,
				"SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0",
				"SSHMGR-SMBMAP:3f2a9c8d1b7e4057:1",
			},
			wantValues: append(append([]byte{}, environmentValue...), append(password, secondPassword...)...),
		},
		{
			name:        "长共享名与长用户命令",
			userCommand: longUserCommand,
			items: []ExecInjection{NetworkDriveInjection{
				ShareName: longShareName, Drive: "L", Address: "192.0.2.30",
				UserName: "mount-user", Password: password,
				StatusMarker: "3f2a9c8d1b7e4058",
			}},
			wantScript: []string{
				`AddArgument('\\192.0.2.30\` + longShareName + `')`,
				PowerShellSingleQuote(longUserCommand),
			},
			wantValues: password,
		},
	}

	var fixedCommand string
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			prepared, err := prepareExec(testCase.userCommand, ExecInjectionOptions{
				ShellType:   models.ShellWindowsCmd,
				TargetLayer: ExecLayerNormal,
				Items:       testCase.items,
			})
			if err != nil {
				t.Fatal(err)
			}
			if fixedCommand == "" {
				fixedCommand = prepared.command
			} else if prepared.command != fixedCommand {
				t.Fatalf("cmd 引导命令随注入内容变化：\n第一次：%q\n本次：%q", fixedCommand, prepared.command)
			}
			if len(prepared.command) >= 1500 {
				t.Fatalf("cmd 引导命令长度 = %d，期待低于 1500", len(prepared.command))
			}
			if strings.ContainsRune(prepared.command, '\n') || strings.ContainsRune(prepared.command, '\r') {
				t.Fatalf("cmd 引导命令不是单行：%q", prepared.command)
			}
			for _, character := range prepared.command {
				if character > 0x7f {
					t.Fatalf("cmd 引导命令含非 ASCII 字符 %q", character)
				}
			}

			bootstrap := decodeWindowsEncodedCommand(t, windowsCmdEncodedPayload(t, prepared.command))
			if !strings.HasPrefix(bootstrap, "$ProgressPreference='SilentlyContinue'") {
				t.Fatalf("引导脚本必须先关闭进度输出：\n%s", bootstrap)
			}
			if !strings.Contains(bootstrap, "function Read-SshmgrFrame") {
				t.Fatalf("引导脚本缺少不会与内置别名冲突的读帧函数：\n%s", bootstrap)
			}

			script, values := splitWindowsCmdInjectionFrames(t, prepared.frames)
			for _, want := range append([]string{
				"$__sshmgr_process.StartInfo.FileName = $env:ComSpec",
				`$__sshmgr_process.StartInfo.Arguments = '/d /s /c "' + ` + PowerShellSingleQuote(testCase.userCommand) + ` + '"'`,
				"$__sshmgr_process.StartInfo.UseShellExecute = $false",
				"[void]$__sshmgr_process.Start()",
				"exit $__sshmgr_process.ExitCode",
			}, testCase.wantScript...) {
				if !strings.Contains(script, want) {
					t.Fatalf("cmd 注入脚本缺少 %q：\n%s", want, script)
				}
			}
			if !bytes.Equal(values, testCase.wantValues) {
				t.Fatalf("标准输入值 = %q，期待 %q", values, testCase.wantValues)
			}
		})
	}
}

func TestPrepareExecWindowsCmdRejectsInvalidUserCommand(t *testing.T) {
	for name, command := range map[string]string{
		"空字节":      "echo " + string([]byte{0}),
		"非法 UTF-8": "echo " + string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := prepareExec(command, ExecInjectionOptions{
				ShellType:   models.ShellWindowsCmd,
				TargetLayer: ExecLayerNormal,
				Items: []ExecInjection{
					EnvironmentInjection{EnvironmentName: "HF_TOKEN", Value: []byte("value")},
				},
			})
			if err == nil {
				t.Fatal("prepareExec 接受了非法的 cmd 用户命令")
			}
		})
	}
}

// TestPrepareExecWindowsCmdQuotesUserCommand 钉住用户命令进入子 cmd 前的
// PowerShell 单引号转义；ASCII 单引号与弯单引号都必须保持字面量语义。
func TestPrepareExecWindowsCmdQuotesUserCommand(t *testing.T) {
	const userCommand = `echo '‘’‚‛ ^& "quoted" %HF_TOKEN%`
	prepared, err := prepareExec(userCommand, ExecInjectionOptions{
		ShellType:   models.ShellWindowsCmd,
		TargetLayer: ExecLayerNormal,
		Items: []ExecInjection{
			EnvironmentInjection{EnvironmentName: "HF_TOKEN", Value: []byte("value")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	script, _ := splitWindowsCmdInjectionFrames(t, prepared.frames)
	want := `'/d /s /c "' + ` + PowerShellSingleQuote(userCommand) + ` + '"'`
	if !strings.Contains(script, want) {
		t.Fatalf("cmd 注入脚本里的用户命令转义不正确，期待 %q：\n%s", want, script)
	}
}

// TestPrepareExecWindowsCmdBootstrapWithLocalPowerShell 在本机安装 PowerShell 7
// 时执行固定引导脚本：机密值先进入环境，再由子进程读取，用户命令退出码原
// 样返回；帧提前结束时引导脚本返回内部失败码。
func TestPrepareExecWindowsCmdBootstrapWithLocalPowerShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		// 这里用 POSIX shell 脚本冒充 ComSpec，Windows 上无法执行；真实 cmd
		// 的行为由 mcpserver 的 TestSecretEnvironmentInjectionWindowsCmd 覆盖。
		t.Skip("替身 ComSpec 是 POSIX shell 脚本，只在非 Windows 主机上运行")
	}
	powerShellPath, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("本机没有 pwsh，跳过 PowerShell 引导脚本执行检查")
	}
	const secretValue = "bootstrap-secret"
	const userCommand = `printf '%s\n' "$HF_TOKEN"; exit 42`
	prepared, err := prepareExec(userCommand, ExecInjectionOptions{
		ShellType:   models.ShellWindowsCmd,
		TargetLayer: ExecLayerNormal,
		Items: []ExecInjection{
			EnvironmentInjection{EnvironmentName: "HF_TOKEN", Value: []byte(secretValue)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	fakeComSpec := filepath.Join(t.TempDir(), "fake-cmd")
	if err := os.WriteFile(fakeComSpec, []byte("#!/bin/sh\nshift 3\neval \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimPrefix(prepared.command, "powershell -NoProfile -NonInteractive -EncodedCommand ")
	run := func(stdinBytes []byte) (string, string, int) {
		localShell := exec.Command(powerShellPath, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
		localShell.Env = append(os.Environ(), "ComSpec="+fakeComSpec)
		localShell.Stdin = bytes.NewReader(stdinBytes)
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		localShell.Stdout = stdout
		localShell.Stderr = stderr
		if err := localShell.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return stdout.String(), stderr.String(), exitErr.ExitCode()
			}
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), 0
	}

	stdout, stderr, exitCode := run(prepared.frames)
	if stdout != secretValue+"\n" || stderr != "" || exitCode != 42 {
		t.Fatalf("引导执行结果 stdout=%q stderr=%q exit=%d", stdout, stderr, exitCode)
	}
	stdout, stderr, exitCode = run(prepared.frames[:4])
	if stdout != "" || stderr != "" || exitCode != windowsCmdInjectionFailureExitCode {
		t.Fatalf("提前结束帧结果 stdout=%q stderr=%q exit=%d", stdout, stderr, exitCode)
	}
	script, _ := splitWindowsCmdInjectionFrames(t, prepared.frames)
	stdout, stderr, exitCode = run(prepared.frames[:8+len(script)+1])
	if stdout != "" || stderr != "" || exitCode != windowsCmdInjectionFailureExitCode {
		t.Fatalf("机密值帧提前结束结果 stdout=%q stderr=%q exit=%d", stdout, stderr, exitCode)
	}
}

func windowsCmdEncodedPayload(t *testing.T, command string) string {
	t.Helper()
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	encoded, ok := strings.CutPrefix(command, prefix)
	if !ok {
		t.Fatalf("cmd 命令不是 PowerShell EncodedCommand：%q", command)
	}
	return encoded
}

func splitWindowsCmdInjectionFrames(t *testing.T, frames []byte) (string, []byte) {
	t.Helper()
	if len(frames) < 8 {
		t.Fatalf("标准输入帧过短：%q", frames)
	}
	length, err := strconv.Atoi(string(frames[:8]))
	if err != nil || length < 0 || 8+length > len(frames) {
		t.Fatalf("cmd 脚本帧头部 %q 非法：%v", frames[:8], err)
	}
	return string(frames[8 : 8+length]), frames[8+length:]
}

func decodeWindowsEncodedCommand(t *testing.T, encoded string) string {
	t.Helper()
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("EncodedCommand payload is not Base64: %v", err)
	}
	if len(blob)%2 != 0 {
		t.Fatalf("EncodedCommand payload is %d bytes, not UTF-16LE", len(blob))
	}
	units := make([]uint16, len(blob)/2)
	for index := range units {
		units[index] = binary.LittleEndian.Uint16(blob[index*2:])
	}
	return string(utf16.Decode(units))
}
