package sharemount

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestLinuxAndDarwinMountCommandsReceiveOnlyMountPassword 覆盖两条生产下发
// 路径：Linux 凭据文件与 macOS autofs 映射。进程内 SSH 服务器模拟两种 sudo
// 行为，并按对应固定脚本的标准输入读取方式还原脚本看到的密码。
func TestLinuxAndDarwinMountCommandsReceiveOnlyMountPassword(t *testing.T) {
	const sudoPassword = "one-time-sudo-password"
	const mountPassword = "mount-password"

	systems := []struct {
		name         string
		buildCommand func(*models.Server, *models.SharedStorage, []*Candidate) string
		marker       string
		scriptRead   func([]byte) string
		scriptWanted string
	}{
		{
			name:         "Linux 凭据文件",
			buildCommand: buildLinuxMountCommand,
			marker:       linuxMountMarker,
			scriptRead: func(input []byte) string {
				return "username=mount-user\npassword=" + string(input)
			},
			scriptWanted: "username=mount-user\npassword=" + mountPassword + "\n",
		},
		{
			name:         "macOS autofs 间接映射",
			buildCommand: buildDarwinMountCommand,
			marker:       darwinMountMarker,
			scriptRead: func(input []byte) string {
				password, _, _ := bytes.Cut(input, []byte("\n"))
				return string(password)
			},
			scriptWanted: mountPassword,
		},
	}
	modes := []struct {
		name                    string
		sudoDoesNotReadPassword bool
	}{
		{name: "sudo 需要密码并消费第一行", sudoDoesNotReadPassword: false},
		{name: "免密 sudo 不消费标准输入", sudoDoesNotReadPassword: true},
	}

	for _, system := range systems {
		for _, mode := range modes {
			t.Run(system.name+"/"+mode.name, func(t *testing.T) {
				var mutex sync.Mutex
				var receivedCommand string
				var scriptResult string
				addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
					Password:                "login-password",
					SudoPassword:            sudoPassword,
					SudoDoesNotReadPassword: mode.sudoDoesNotReadPassword,
					Exec: func(command string, stdin io.Reader) (string, string, int) {
						input, err := io.ReadAll(stdin)
						if err != nil {
							return "", "read standard input: " + err.Error(), 1
						}
						mutex.Lock()
						defer mutex.Unlock()
						receivedCommand = command
						scriptResult = system.scriptRead(input)
						return system.marker + " result=first-mount-success selected=1\n", "", 0
					},
				})
				defer cleanup()

				masterKey, err := store.GenerateMasterKey()
				if err != nil {
					t.Fatal(err)
				}
				st, err := store.Open(t.TempDir()+"/store.db", masterKey)
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				sudoCredentialID, err := st.SetCredential(&models.Credential{
					Type: models.CredPassword, Secret: []byte(sudoPassword),
				})
				if err != nil {
					t.Fatal(err)
				}

				host := addr[:strings.LastIndex(addr, ":")]
				port, err := strconv.Atoi(addr[strings.LastIndex(addr, ":")+1:])
				if err != nil {
					t.Fatal(err)
				}
				client, err := sshbroker.Connect(
					context.Background(), host, port, "deploy",
					sshbroker.PasswordAuth("login-password"), ssh.FixedHostKey(hostKey),
				)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()

				share := &models.SharedStorage{
					ID: "share-001", Name: "datasets", Version: 1, MountUser: "mount-user",
				}
				server := &models.Server{SudoCredentialID: sudoCredentialID}
				command := system.buildCommand(
					server, share, []*Candidate{{Address: "192.0.2.10"}},
				)
				result, err := RemoteExecOverClient(client, st, server, MaxOutputBytes)(
					context.Background(), server,
					RemoteExecRequest{
						Command: command, Privileged: true,
						StandardInput: []byte(mountPassword + "\n"),
					},
				)
				if err != nil {
					t.Fatalf("执行固定挂载命令失败：%v", err)
				}
				if result.ExitCode != 0 {
					t.Fatalf("固定挂载命令退出码 = %d，stderr = %q", result.ExitCode, result.Stderr)
				}

				mutex.Lock()
				gotCommand, gotScriptResult := receivedCommand, scriptResult
				mutex.Unlock()
				if !strings.Contains(gotCommand, system.marker) {
					t.Fatalf("没有执行 %s 的固定命令：%q", system.name, gotCommand)
				}
				if gotScriptResult != system.scriptWanted {
					t.Fatalf("%s 读到的密码 = %q，想要 %q", system.name, gotScriptResult, system.scriptWanted)
				}
				if strings.Contains(gotScriptResult, sudoPassword) {
					t.Fatalf("sudo 凭据进入了挂载脚本的载荷：%q", gotScriptResult)
				}
			})
		}
	}
}
