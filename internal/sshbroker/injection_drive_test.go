package sshbroker

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"ssh-manager-mcp/internal/models"
)

// TestPrepareExecNetworkDrivePowerShellAssembly 钉住 PowerShell 服务器上网络
// 盘符注入的拼装形态：映射经异步调用执行、密码只走标准输入、封装脚本里
// 不出现双引号（Windows OpenSSH 对 PowerShell 命令文本会剥掉双引号）。
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
		"New-SmbMapping",
		"-LocalPath $localPath",
		"-RemotePath $remotePath",
		"-UserName $userName",
		"-Password $password",
		"-Persistent $false",
		"BeginInvoke",
		"Wait(3000)",
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
	if !bytes.Equal(prepared.frames, password) {
		t.Fatalf("standard input frames = %q, want only the password", prepared.frames)
	}
}

// TestPrepareExecNetworkDriveTimeoutStopIsAsynchronous 钉住超时分支的生成
// 形态：发起异步停止后立刻回传状态行，不等待 New-SmbMapping 返回，也不同
// 步释放仍在停止中的 PowerShell 管道。地址不可达时同步停止可能阻塞约
// 10 秒，会使 3 秒上限失效。
func TestPrepareExecNetworkDriveTimeoutStopIsAsynchronous(t *testing.T) {
	prepared, err := prepareExec("Get-ChildItem S:\\", ExecInjectionOptions{
		ShellType:   models.ShellPowerShell,
		TargetLayer: ExecLayerNormal,
		Items: []ExecInjection{NetworkDriveInjection{
			ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
			UserName: "mount-user", Password: []byte("drive-password"),
			StatusMarker: "3f2a9c8d1b7e4056",
		}},
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

// TestPrepareExecNetworkDriveWindowsCmdAssembly 钉住 cmd 服务器上网络盘符
// 注入的拼装形态：映射由独立的 PowerShell 子进程用 -EncodedCommand 执行，
// 密码只按长度走标准输入；环境变量值先读，网络盘符密码后读，帧顺序与封
// 装脚本读取顺序一致。每个共享单独一个子进程，避免多个 -EncodedCommand
// 合并后超过 cmd 单行命令长度上限。
func TestPrepareExecNetworkDriveWindowsCmdAssembly(t *testing.T) {
	environmentValue := []byte("cmd-value-%-^&!-中文")
	password := []byte("drive-p@ss%'\"|()中文\r\n")
	secondPassword := []byte("second-drive-p@ss")
	const userCommand = `dir S:\datasets`
	prepared, err := prepareExec(userCommand, ExecInjectionOptions{
		ShellType:   models.ShellWindowsCmd,
		TargetLayer: ExecLayerNormal,
		Items: []ExecInjection{
			EnvironmentInjection{EnvironmentName: "HF_TOKEN", Value: environmentValue},
			NetworkDriveInjection{
				ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
				UserName: "mount-user", Password: password,
				StatusMarker: "3f2a9c8d1b7e4056",
			},
			NetworkDriveInjection{
				ShareName: "reports", Drive: "T", Address: "192.0.2.10",
				UserName: "mount-user", Password: secondPassword,
				StatusMarker: "3f2a9c8d1b7e4057",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := prepared.command
	for _, want := range []string{
		"set /p HF_TOKEN=",
		"chcp %__sshmgr_original_code_page% >NUL",
		"powershell -NoProfile -NonInteractive -EncodedCommand ",
		userCommand,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("wrapped cmd command missing %q:\n%s", want, command)
		}
	}
	restore := strings.Index(command, "chcp %__sshmgr_original_code_page%")
	firstEncoded := strings.Index(command, "powershell -NoProfile -NonInteractive -EncodedCommand ")
	userStart := strings.Index(command, userCommand)
	if restore < 0 || firstEncoded < restore || userStart < firstEncoded {
		t.Fatalf("cmd command must read environment values, map drives, then run the user command:\n%s", command)
	}

	encodedPayloads := windowsCmdEncodedPayloads(t, command)
	if len(encodedPayloads) != 2 {
		t.Fatalf("EncodedCommand payload count = %d, want one per share", len(encodedPayloads))
	}
	for index, payload := range encodedPayloads {
		script := decodeWindowsEncodedCommand(t, payload)
		for _, want := range []string{"New-SmbMapping", "-Persistent $false", "BeginInvoke", "Wait(3000)"} {
			if !strings.Contains(script, want) {
				t.Fatalf("EncodedCommand script %d missing %q:\n%s", index, want, script)
			}
		}
		if bytes.Contains([]byte(script), password) || bytes.Contains([]byte(script), secondPassword) {
			t.Fatalf("EncodedCommand script %d leaked a password:\n%s", index, script)
		}
	}
	if !strings.Contains(decodeWindowsEncodedCommand(t, encodedPayloads[0]), `AddArgument('\\192.0.2.10\datasets')`) ||
		!strings.Contains(decodeWindowsEncodedCommand(t, encodedPayloads[0]), "SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0") {
		t.Fatalf("first EncodedCommand script does not map datasets:\n%s", decodeWindowsEncodedCommand(t, encodedPayloads[0]))
	}
	if !strings.Contains(decodeWindowsEncodedCommand(t, encodedPayloads[1]), `AddArgument('\\192.0.2.10\reports')`) ||
		!strings.Contains(decodeWindowsEncodedCommand(t, encodedPayloads[1]), "SSHMGR-SMBMAP:3f2a9c8d1b7e4057:1") {
		t.Fatalf("second EncodedCommand script does not map reports with its plan index:\n%s", decodeWindowsEncodedCommand(t, encodedPayloads[1]))
	}
	for _, line := range strings.Split(command, "\n") {
		if strings.Contains(line, "-EncodedCommand ") && len(line) > 8191 {
			t.Fatalf("EncodedCommand line is %d characters, above cmd limit", len(line))
		}
	}

	wantFrames := append([]byte{}, environmentValue...)
	wantFrames = append(wantFrames, '\r', '\n')
	wantFrames = append(wantFrames, password...)
	wantFrames = append(wantFrames, secondPassword...)
	if !bytes.Equal(prepared.frames, wantFrames) {
		t.Fatalf("standard input frames = %q, want environment value, line break, then both passwords", prepared.frames)
	}
	if bytes.Contains([]byte(command), password) || bytes.Contains([]byte(command), secondPassword) {
		t.Fatalf("wrapped cmd command leaked a password:\n%s", command)
	}
}

func windowsCmdEncodedPayloads(t *testing.T, command string) []string {
	t.Helper()
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	var payloads []string
	for _, line := range strings.Split(command, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if encoded, ok := strings.CutPrefix(line, prefix); ok {
			payloads = append(payloads, encoded)
		}
	}
	return payloads
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
