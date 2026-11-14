package sshbroker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

// TestExecSudoKeepsStandardInputAlignedWithPasswordConsumption 在进程内 SSH
// 服务器与本地 POSIX shell 上复现两种 sudo：需要密码时先消费第一行；免密
// 时不消费任何字节。两种情况下，Linux 挂载脚本的 cat 读法与 macOS 挂载脚
// 本的 read 读法都必须只看到挂载密码。
func TestExecSudoKeepsStandardInputAlignedWithPasswordConsumption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 测试主机没有行为等价的 POSIX /bin/sh")
	}
	const sudoPassword = "one-time-sudo-password"
	const mountPassword = "mount-password"

	commands := []struct {
		name string
		body string
		want string
	}{
		{
			name: "Linux 固定脚本用 cat 读取剩余标准输入",
			body: `printf 'password='; cat`,
			want: "password=" + mountPassword + "\n",
		},
		{
			name: "macOS 固定脚本用 read 读取第一行",
			body: `IFS= read -r sshmgr_password; printf 'password=%s\n' "$sshmgr_password"`,
			want: "password=" + mountPassword + "\n",
		},
	}
	modes := []struct {
		name              string
		targetDoesNotRead bool
	}{
		{name: "sudo 需要密码并消费第一行", targetDoesNotRead: false},
		{name: "免密 sudo 不消费标准输入", targetDoesNotRead: true},
	}

	for _, mode := range modes {
		for _, command := range commands {
			t.Run(mode.name+"/"+command.name, func(t *testing.T) {
				addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
					Password:                "pw",
					SudoPassword:            sudoPassword,
					SudoDoesNotReadPassword: mode.targetDoesNotRead,
					Exec: func(receivedCommand string, stdin io.Reader) (string, string, int) {
						input, err := io.ReadAll(stdin)
						if err != nil {
							return "", "read standard input: " + err.Error(), 1
						}
						if bytes.Contains(input, []byte(sudoPassword)) {
							return "", "sudo password entered the inner command", 1
						}
						localShell := exec.Command("/bin/sh", "-c", receivedCommand)
						localShell.Stdin = bytes.NewReader(input)
						var stdout, stderr bytes.Buffer
						localShell.Stdout = &stdout
						localShell.Stderr = &stderr
						if err := localShell.Run(); err != nil {
							return stdout.String(), stderr.String(), exitCodeForTest(err)
						}
						return stdout.String(), stderr.String(), 0
					},
				})
				defer cleanup()
				client := connectTest(t, addr, hostKey)
				defer client.Close()

				result, err := client.ExecSudo(
					context.Background(), command.body, []byte(sudoPassword), 0, 0,
					WithSudoStandardInput([]byte(mountPassword+"\n")),
				)
				if err != nil {
					t.Fatalf("ExecSudo: %v", err)
				}
				if result.ExitCode != 0 || result.Stdout != command.want {
					t.Fatalf("结果 = exit:%d stdout:%q stderr:%q，想要 exit:0 stdout:%q", result.ExitCode, result.Stdout, result.Stderr, command.want)
				}
				if result.Sudo == nil || result.Sudo.Outcome != SudoElevated || result.Sudo.UID == nil || *result.Sudo.UID != 0 {
					t.Fatalf("sudo 结论 = %+v，想要免密或密码路径都证实为 root", result.Sudo)
				}
			})
		}
	}
}

func exitCodeForTest(err error) int {
	type exitCodeError interface{ ExitCode() int }
	var exitErr exitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
