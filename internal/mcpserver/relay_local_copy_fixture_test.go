package mcpserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// relayLocalCopyEnv 是共享地址本地拷贝的端到端夹具：一个进程内假 SSH 服务
// 器同时提供 SFTP 与固定命令执行，存储里有共享登记、拓扑与授权。
type relayLocalCopyEnv struct {
	store          *store.Store
	profileID      string
	serverID       string
	windowsID      string
	notMountableID string
	serverName     string
	root           string
	fake           *relayLocalCopyFakeServer
	connections    atomic.Int64
}

type relayLocalCopyFakeServer struct {
	mu           sync.Mutex
	commands     []string
	inputs       [][]byte
	mounted      bool
	systemName   string
	mapStatus    string
	robocopyExit int
	blockCopy    chan struct{}
	keepPartial  bool
}

func newRelayLocalCopyEnv(t *testing.T, mounted bool) *relayLocalCopyEnv {
	t.Helper()
	environment := &relayLocalCopyEnv{
		serverName: "本地拷贝服务器",
		fake:       &relayLocalCopyFakeServer{mounted: mounted, systemName: "Linux"},
	}
	address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudo-pw",
		Connections: &environment.connections,
		ExecStream: func(command string, stdin io.Reader, stdout, stderr io.Writer) int {
			return environment.serveCommand(command, stdin, stdout, stderr)
		},
	})
	t.Cleanup(cleanup)

	st := newStore(t)
	serverID := seedRealServer(t, st, environment.serverName, address, hostKey, "sudo-pw")
	notMountableCredential, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	notMountableServer := &models.Server{
		Name: "不可挂载服务器", Host: address[:strings.LastIndex(address, ":")],
		Port: portOfAddr(address), User: "u", AuthMethod: models.AuthPassword,
		CredentialID: notMountableCredential,
	}
	notMountableID, err := st.AddServer(notMountableServer)
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("共享本地拷贝 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{
		serverID, notMountableID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("本地拷贝NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("本地拷贝站点", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "本地拷贝物理网络", Type: models.NetworkPhysical, SiteName: "本地拷贝站点",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(
		"本地拷贝NAS", "本地拷贝物理网络", "192.0.2.10",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("本地拷贝Windows设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(
		"本地拷贝Windows设备", "本地拷贝物理网络", "192.0.2.20",
	); err != nil {
		t.Fatal(err)
	}
	windowsAdded, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "本地拷贝Windows", Host: address[:strings.LastIndex(address, ":")],
		Port: portOfAddr(address), User: "u", ShellType: models.ShellPowerShell,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "本地拷贝Windows设备")
	if err != nil {
		t.Fatal(err)
	}
	windowsID := windowsAdded.ServerID
	if err := st.GrantServers(profileID, []string{windowsID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"LOCAL_COPY_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"LOCAL_COPY_MOUNT_PASSWORD"}, []string{environment.serverName}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"LOCAL_COPY_MOUNT_PASSWORD"}, []string{"本地拷贝Windows"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "本地拷贝NAS", User: "mount-user",
		PasswordSecret: "LOCAL_COPY_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	environment.store = st
	environment.profileID = profileID
	environment.serverID = serverID
	environment.windowsID = windowsID
	environment.notMountableID = notMountableID
	environment.root = toSlash(t.TempDir())
	return environment
}

// openSession 按现有 relay 端到端夹具的形态建立内存 MCP 会话。
func (environment *relayLocalCopyEnv) openSession(t *testing.T) *relayE2ESession {
	t.Helper()
	server, tunnelManager, taskManager, err := NewServer(
		environment.store, environment.profileID, "proj-local-copy",
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	session := &relayE2ESession{
		cli: clientSession, srv: serverSession,
		mgr: tunnelManager, tm: taskManager,
	}
	t.Cleanup(session.close)
	return session
}

func (environment *relayLocalCopyEnv) serveCommand(
	command string, stdin io.Reader, stdout, stderr io.Writer,
) int {
	stdinBytes, err := io.ReadAll(stdin)
	if err != nil {
		stderr.Write([]byte("读取标准输入失败：" + err.Error() + "\n"))
		return 1
	}
	environment.fake.mu.Lock()
	environment.fake.commands = append(environment.fake.commands, command)
	environment.fake.inputs = append(environment.fake.inputs, append([]byte(nil), stdinBytes...))
	mapStatus := environment.fake.mapStatus
	environment.fake.mu.Unlock()

	switch {
	case strings.Contains(command, "probe-one ") ||
		strings.Contains(strings.Join(encodedCommandScripts(command), "\n"), "probe-one "):
		probeCommand := strings.Join(encodedCommandScripts(command), "\n")
		if probeCommand == "" {
			probeCommand = command
		}
		for _, match := range driveProbeInvocationPattern.FindAllStringSubmatch(probeCommand, -1) {
			fmt.Fprintf(stdout, "SSHMGR-PROBE %s ok 2 \n", match[1])
		}
		return 0
	case strings.Contains(command, "New-SmbMapping") || strings.Contains(command, "-EncodedCommand "):
		return environment.serveWindowsCopyCommand(command, mapStatus, stdout, stderr)
	case strings.TrimSpace(command) == "uname -s":
		stdout.Write([]byte(environment.fake.systemName + "\n"))
		return 0
	case strings.Contains(command, "SSHMGR-LINUX-CHECK mode=full") ||
		strings.Contains(command, "SSHMGR-DARWIN-CHECK mode=full"):
		mounted := "no"
		if environment.fake.mounted {
			mounted = "yes"
		}
		marker := "SSHMGR-LINUX-CHECK"
		if environment.fake.systemName == "Darwin" {
			marker = "SSHMGR-DARWIN-CHECK"
		}
		fmt.Fprintf(stdout, "%s mode=full\n"+
			"%s index=1 managed=yes share=datasets version=1 mounted=%s\n"+
			"%s auto-master=no\n", marker, marker, mounted, marker)
		return 0
	case strings.Contains(command, "wc -c < "):
		return environment.serveInspectCommand(command, stdout)
	case strings.Contains(command, "cp -- "):
		return environment.serveCopyCommand(command, stdout, stderr)
	default:
		stderr.Write([]byte("假服务器不认识这条固定命令\n"))
		return 1
	}
}

func (environment *relayLocalCopyEnv) serveWindowsCopyCommand(
	command, mapStatus string, stdout, stderr io.Writer,
) int {
	decodedCommand := strings.Join(encodedCommandScripts(command), "\n")
	statusCommand := command
	if marker := driveEncodedStatusPattern.FindStringSubmatch(decodedCommand); marker != nil {
		statusCommand = decodedCommand
	}
	if marker := driveEncodedStatusPattern.FindStringSubmatch(statusCommand); marker != nil {
		status := mapStatus
		if status == "" {
			status = "ok"
		}
		fmt.Fprintf(stdout, "SSHMGR-SMBMAP:%s:%s:%s\n", marker[1], marker[2], status)
		if status != "ok" {
			return 16
		}
	}
	for _, script := range encodedCommandScripts(command) {
		if strings.Contains(script, "$__sshmgr_robocopy_source_directory") {
			return environment.serveRobocopyScript(script, stdout, stderr)
		}
	}
	stderr.Write([]byte("假 Windows 服务器没有找到本地拷贝脚本\n"))
	return 1
}

func (environment *relayLocalCopyEnv) serveRobocopyScript(
	script string, stdout, stderr io.Writer,
) int {
	environment.fake.mu.Lock()
	robocopyExit := environment.fake.robocopyExit
	environment.fake.mu.Unlock()
	source := relayWindowsScriptString(script, "$__sshmgr_source")
	target := relayWindowsScriptString(script, "$__sshmgr_target")
	if source == "" || target == "" {
		stderr.Write([]byte("固定 Windows 拷贝脚本缺少路径\n"))
		return 1
	}
	sourceHost := environment.hostPath(source)
	targetHost := environment.hostPath(target)
	sourceInfo, err := os.Stat(sourceHost)
	if err != nil || sourceInfo.IsDir() {
		stdout.Write([]byte("source-not-regular\n"))
		return 3
	}
	if targetInfo, err := os.Stat(targetHost); err == nil && targetInfo.IsDir() {
		stdout.Write([]byte("target-directory\n"))
		return 4
	}
	partialHost := environment.hostPath(target + relayPartialSuffix)
	if err := os.MkdirAll(partialHost, 0o755); err != nil {
		fmt.Fprintf(stderr, "创建 Windows 半成品目录失败：%v\n", err)
		return 1
	}
	partialEntries, err := os.ReadDir(partialHost)
	if err != nil {
		fmt.Fprintf(stderr, "清理 Windows 半成品目录失败：%v\n", err)
		return 1
	}
	for _, entry := range partialEntries {
		if !entry.IsDir() {
			if err := os.Remove(filepath.Join(partialHost, entry.Name())); err != nil {
				fmt.Fprintf(stderr, "清理 Windows 旧半成品失败：%v\n", err)
				return 1
			}
		}
	}
	sourceName := filepath.Base(strings.ReplaceAll(source, `\`, `/`))
	partialFileHost := filepath.Join(partialHost, sourceName)
	content, err := os.ReadFile(sourceHost)
	if err != nil {
		fmt.Fprintf(stderr, "读取 Windows 源文件失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "本地拷贝模式：中断、停止或失败后不能续传，重跑会从文件开头覆盖旧半成品")
	fmt.Fprintf(stdout, "robocopy %s %s %s\n",
		relayWindowsScriptString(script, "$__sshmgr_robocopy_source_directory"),
		target+relayPartialSuffix, sourceName)
	if err := os.WriteFile(partialFileHost, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "写入 Windows 半成品失败：%v\n", err)
		return 1
	}
	if robocopyExit >= 8 {
		return robocopyExit
	}
	if environment.fake.blockCopy != nil {
		<-environment.fake.blockCopy
		environment.fake.blockCopy = nil
		if environment.fake.keepPartial {
			return 0
		}
	}
	if err := os.Rename(partialFileHost, targetHost); err != nil {
		fmt.Fprintf(stderr, "移动 Windows 半成品失败：%v\n", err)
		return 1
	}
	if err := os.Remove(partialHost); err != nil {
		fmt.Fprintf(stderr, "删除空的 Windows 半成品目录失败：%v\n", err)
		return 1
	}
	return 0
}

func relayWindowsScriptString(script, variable string) string {
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(variable) + ` = '(.*)'$`).
		FindStringSubmatch(script)
	if match == nil {
		return ""
	}
	return unescapePowerShellSingleQuotes(match[1])
}

// unescapePowerShellSingleQuotes 按 PowerShell 的单引号转义规则还原脚本里的
// 字符串值：ASCII 单引号与四个弯单引号写成两次都表示一个字面量字符。
func unescapePowerShellSingleQuotes(value string) string {
	replacer := strings.NewReplacer(
		"''", "'",
		"‘‘", "‘",
		"’’", "’",
		"‚‚", "‚",
		"‛‛", "‛",
	)
	return replacer.Replace(value)
}

func (environment *relayLocalCopyEnv) serveInspectCommand(command string, stdout io.Writer) int {
	words := splitPOSIXShellWords(command)
	var source, target string
	directories := 0
	for index, word := range words {
		switch word {
		case "-d":
			if index+1 >= len(words) {
				stdout.Write([]byte("inspect-malformed\n"))
				return 1
			}
			directories++
			if directories == 1 {
				source = words[index+1]
			} else {
				target = words[index+1]
			}
		case "-f":
			if index+1 < len(words) {
				source = words[index+1]
			}
		}
	}
	sourceInfo, err := os.Stat(environment.hostPath(source))
	if err != nil {
		stdout.Write([]byte("source-not-regular\n"))
		return 3
	}
	if sourceInfo.IsDir() {
		stdout.Write([]byte("source-directory\n"))
		return 2
	}
	if targetInfo, err := os.Stat(environment.hostPath(target)); err == nil && targetInfo.IsDir() {
		stdout.Write([]byte("target-directory\n"))
		return 4
	}
	fmt.Fprintf(stdout, "size=%d\n", sourceInfo.Size())
	return 0
}

func (environment *relayLocalCopyEnv) serveCopyCommand(
	command string, stdout, stderr io.Writer,
) int {
	words := splitPOSIXShellWords(command)
	cpIndex := -1
	for index, word := range words {
		if word == "cp" && index+3 < len(words) && words[index+1] == "--" {
			cpIndex = index
			break
		}
	}
	if cpIndex < 0 {
		stderr.Write([]byte("固定命令里没有找到 cp --\n"))
		return 1
	}
	source := environment.hostPath(words[cpIndex+2])
	sourceRemote := words[cpIndex+2]
	partialRemote := words[cpIndex+3]
	targetRemote := strings.TrimSuffix(partialRemote, ".sshmgr-partial")
	partial := environment.hostPath(partialRemote)
	target := environment.hostPath(targetRemote)
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		fmt.Fprintf(stderr, "创建目标目录失败：%v\n", err)
		return 1
	}
	content, err := os.ReadFile(source)
	if err != nil {
		fmt.Fprintf(stderr, "读取源文件失败：%v\n", err)
		return 1
	}
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "写入半成品失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "本地拷贝模式：中断、停止或失败后不能续传，重跑会从文件开头覆盖旧半成品")
	fmt.Fprintf(stdout, "cp -- '%s' '%s'\n", sourceRemote, partialRemote)
	if environment.fake.blockCopy != nil {
		<-environment.fake.blockCopy
		environment.fake.blockCopy = nil
		if environment.fake.keepPartial {
			return 0
		}
	}
	fmt.Fprintf(stdout, "mv -- '%s' '%s'\n", partialRemote, targetRemote)
	if err := os.Rename(partial, target); err != nil {
		fmt.Fprintf(stderr, "改名半成品失败：%v\n", err)
		return 1
	}
	return 0
}

func (environment *relayLocalCopyEnv) hostPath(remotePath string) string {
	relative := strings.TrimPrefix(remotePath, "/mnt/sshmgr/datasets/")
	normalized := strings.ReplaceAll(remotePath, `\`, `/`)
	for _, driveRoot := range []string{"Z:/", "C:/"} {
		if strings.HasPrefix(normalized, driveRoot) {
			relative = strings.TrimPrefix(normalized, driveRoot)
			break
		}
	}
	return filepath.Join(environment.root, filepath.FromSlash(relative))
}

func (fake *relayLocalCopyFakeServer) recordedCommands() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.commands...)
}

func (fake *relayLocalCopyFakeServer) recordedInputs() [][]byte {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([][]byte(nil), fake.inputs...)
}

func splitPOSIXShellWords(command string) []string {
	var words []string
	var current strings.Builder
	quoted := false
	for index := 0; index < len(command); index++ {
		character := command[index]
		switch {
		case quoted:
			if character != '\'' {
				current.WriteByte(character)
			} else {
				quoted = false
			}
		case character == '\'':
			quoted = true
		case character == '\\' && index+2 < len(command) &&
			command[index+1] == '\'' && command[index+2] == '\'':
			current.WriteByte('\'')
			quoted = true
			index += 2
		case character == ' ' || character == '\t' || character == '\n' || character == '\r':
			if current.Len() != 0 {
				words = append(words, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(character)
		}
	}
	if current.Len() != 0 {
		words = append(words, current.String())
	}
	return words
}
