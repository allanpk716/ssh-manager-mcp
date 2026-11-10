package sshbroker

import (
	"bytes"
	"strings"
	"testing"

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
