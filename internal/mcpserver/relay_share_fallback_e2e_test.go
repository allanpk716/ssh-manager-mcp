package mcpserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// relayShareFallbackEnv 是共享地址流式回退的端到端夹具。三台候选端点分别是
// 只在虚拟局域网中的服务器、两台与提供方设备同在物理局域网的服务器；普通
// 端点不在共享的可挂载范围内，因此每次调用都会进入本票测试的回退分支。
type relayShareFallbackEnv struct {
	store               *store.Store
	profileID           string
	ordinaryID          string
	virtualID           string
	physicalAID         string
	physicalBID         string
	windowsID           string
	ordinaryWindowsID   string
	hiddenID            string
	root                string
	ordinaryFake        *relayShareFallbackFake
	virtualFake         *relayShareFallbackFake
	physicalAFake       *relayShareFallbackFake
	physicalBFake       *relayShareFallbackFake
	windowsFake         *relayShareFallbackFake
	ordinaryWindowsFake *relayShareFallbackFake
	hiddenFake          *relayShareFallbackFake
	connections         atomic.Int64
}

type relayShareFallbackFake struct {
	mu         sync.Mutex
	commands   []string
	mounted    bool
	systemName string
}

func newRelayShareFallbackEnv(t *testing.T) *relayShareFallbackEnv {
	t.Helper()
	t.Setenv("SSHMGR_TRANSFER_CHUNK", "16777216")
	environment := &relayShareFallbackEnv{root: toSlash(t.TempDir())}
	st := newStore(t)

	startFake := func(name string, mounted bool) (string, *relayShareFallbackFake) {
		fake := &relayShareFallbackFake{mounted: mounted, systemName: "Linux"}
		address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
			Password: "pw", SudoPassword: "sudo-pw", Connections: &environment.connections,
			VirtualFilesystemRoot: environment.root,
			ExecStream: func(command string, _ io.Reader, stdout, stderr io.Writer) int {
				return environment.serveCommand(fake, command, stdout, stderr)
			},
		})
		t.Cleanup(cleanup)
		serverID := seedRealServer(t, st, name, address, hostKey, "sudo-pw")
		return serverID, fake
	}
	environment.ordinaryID, environment.ordinaryFake = startFake("普通端点", false)
	environment.virtualID, environment.virtualFake = startFake("A虚拟端点", true)
	environment.physicalAID, environment.physicalAFake = startFake("A物理端点", true)
	environment.physicalBID, environment.physicalBFake = startFake("B物理端点", true)
	environment.windowsID, environment.windowsFake = startFake("Windows端点", true)
	environment.ordinaryWindowsID, environment.ordinaryWindowsFake = startFake("普通Windows端点", false)
	environment.hiddenID, environment.hiddenFake = startFake("授权外端点", true)
	windowsServer, err := st.GetServer(environment.windowsID)
	if err != nil || windowsServer == nil {
		t.Fatalf("读取 Windows 端点：server=%v err=%v", windowsServer, err)
	}
	windowsServer.ShellType = models.ShellPowerShell
	if err := st.UpdateServer(windowsServer); err != nil {
		t.Fatal(err)
	}
	ordinaryWindowsServer, err := st.GetServer(environment.ordinaryWindowsID)
	if err != nil || ordinaryWindowsServer == nil {
		t.Fatalf("读取普通 Windows 端点：server=%v err=%v", ordinaryWindowsServer, err)
	}
	ordinaryWindowsServer.ShellType = models.ShellWindowsCmd
	if err := st.UpdateServer(ordinaryWindowsServer); err != nil {
		t.Fatal(err)
	}

	if _, err := st.AddDevice("共享提供方", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, deviceName := range []string{"A虚拟端点设备", "A物理端点设备", "B物理端点设备"} {
		if _, err := st.AddDevice(deviceName, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddSite("共享站点", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "物理局域网", Type: models.NetworkPhysical, SiteName: "共享站点",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "虚拟局域网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	member := func(device, network, address string) {
		t.Helper()
		if _, err := st.AddNetworkMembership(device, network, address); err != nil {
			t.Fatal(err)
		}
	}
	member("共享提供方", "物理局域网", "192.0.2.10")
	member("共享提供方", "虚拟局域网", "100.64.0.10")
	member("A虚拟端点设备", "虚拟局域网", "100.64.0.21")
	member("A物理端点设备", "物理局域网", "192.0.2.21")
	member("B物理端点设备", "物理局域网", "192.0.2.22")

	associate := func(serverID, deviceName string) {
		t.Helper()
		device, err := st.GetDevice(deviceName)
		if err != nil || device == nil {
			t.Fatalf("读取设备 %s：%v", deviceName, err)
		}
		if _, err := st.MoveServerToDevice(serverID, deviceName); err != nil {
			t.Fatal(err)
		}
	}
	associate(environment.virtualID, "A虚拟端点设备")
	associate(environment.physicalAID, "A物理端点设备")
	associate(environment.physicalBID, "B物理端点设备")

	if err := st.AddSecret(
		"FALLBACK_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	allowMount := func(serverName string) {
		t.Helper()
		if err := st.AllowSecrets(
			[]string{"FALLBACK_MOUNT_PASSWORD"}, []string{serverName}, store.CLISource(),
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, serverName := range []string{
		"A虚拟端点", "A物理端点", "B物理端点", "Windows端点", "授权外端点",
	} {
		allowMount(serverName)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "共享提供方", User: "mount-user",
		PasswordSecret: "FALLBACK_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("共享流式回退 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{
		environment.ordinaryID, environment.virtualID,
		environment.physicalAID, environment.physicalBID, environment.windowsID,
		environment.ordinaryWindowsID,
	}); err != nil {
		t.Fatal(err)
	}

	environment.store = st
	environment.profileID = profileID
	return environment
}

func (environment *relayShareFallbackEnv) serveCommand(
	fake *relayShareFallbackFake, command string, stdout, stderr io.Writer,
) int {
	fake.mu.Lock()
	fake.commands = append(fake.commands, command)
	fake.mu.Unlock()
	switch {
	case strings.TrimSpace(command) == "uname -s":
		stdout.Write([]byte(fake.systemName + "\n"))
		return 0
	case strings.Contains(command, "SSHMGR-LINUX-CHECK mode=full"):
		mounted := "no"
		if fake.isMounted() {
			mounted = "yes"
		}
		fmt.Fprintf(stdout, "SSHMGR-LINUX-CHECK mode=full\n"+
			"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=1 mounted=%s\n"+
			"SSHMGR-LINUX-CHECK auto-master=no\n", mounted)
		return 0
	default:
		stderr.Write([]byte("共享流式回退假服务器不认识这条固定命令\n"))
		return 1
	}
}

func (environment *relayShareFallbackEnv) hostPath(remotePath string) string {
	return filepath.Join(environment.root, filepath.FromSlash(strings.TrimPrefix(remotePath, "/")))
}

func (fake *relayShareFallbackFake) recordedCommands() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.commands...)
}

func (fake *relayShareFallbackFake) setMounted(mounted bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.mounted = mounted
}

func (fake *relayShareFallbackFake) isMounted() bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.mounted
}

func (environment *relayShareFallbackEnv) openSession(t *testing.T) *relayE2ESession {
	t.Helper()
	server, tunnelManager, taskManager, err := NewServer(
		environment.store, environment.profileID, "proj-share-fallback",
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

func (environment *relayShareFallbackEnv) write(t *testing.T, remotePath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(remotePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(remotePath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRelayFileShareFallbackPrefersPhysicalThenName 覆盖候选排序：物理局域网
// 优先，并列时服务器名字升序；选中的服务器充当共享地址端点并复用流式中继。
func TestRelayFileShareFallbackPrefersPhysicalThenName(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/Reports/source.bin"
	targetRemote := "/ordinary/target.bin"
	environment.write(t, sourceRemote, "fallback payload")

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `smb://192.0.2.10/datasets/Reports/source.bin`,
			"to_server_id":       environment.ordinaryID,
			"to_path":            targetRemote,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.Mode != "streaming_relay" || output.RestartAfterInterruption ||
		output.ShareEndpointServerID != environment.physicalAID {
		t.Fatalf("启动结果 = %+v，期待选择 A 物理端点并走可续传流式中继", output)
	}
	connectionText := strings.Join(output.ConnectionNotes, "\n")
	for _, forbidden := range []string{"192.0.2.", "100.64.0.", "mount-user", "mount-password"} {
		if strings.Contains(connectionText, forbidden) {
			t.Fatalf("启动结果泄露敏感信息 %q：%s", forbidden, connectionText)
		}
	}
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if !strings.Contains(stdout, "流式转发") ||
		!strings.Contains(stdout, environment.physicalAID) ||
		!strings.Contains(stdout, "块清单") {
		t.Fatalf("任务输出没有写明流式转发、端点与续传语义：\n%s", stdout)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "fallback payload" {
		t.Fatalf("目标文件内容 = %q err=%v", content, err)
	}
	if commands := environment.virtualFake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("排序在前的物理端点可用时不应检查虚拟端点：%#v", commands)
	}
	if commands := environment.physicalBFake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("名字排序第一的物理端点可用时不应检查第二台：%#v", commands)
	}
	if commands := environment.windowsFake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("Windows 不应作为共享地址端点被检查：%#v", commands)
	}
	if commands := environment.hiddenFake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("授权外服务器不应进入候选：%#v", commands)
	}
	rows := relayAuditRows(t, environment.store, 50)
	start := relayAuditFind(t, rows, "relay-bg-start", "ok")
	wantSummary := fmt.Sprintf(
		"mode=streaming_relay shared_storage=datasets shared_address_endpoint_server=%s",
		environment.physicalAID)
	if !strings.Contains(start.Command, wantSummary) {
		t.Fatalf("启动审计 = %q，期待包含 %q", start.Command, wantSummary)
	}
	for _, row := range rows {
		if row.Action != "relay-bg-start" && row.Action != "relay-bg-end" {
			continue
		}
		for _, forbidden := range []string{
			"192.0.2.", "100.64.0.", "mount-user", "mount-password", "授权外端点",
		} {
			if strings.Contains(row.Command, forbidden) {
				t.Fatalf("审计泄露敏感或范围外信息 %q：%+v", forbidden, row)
			}
		}
	}
}

// TestRelayFileLocalSourceToShareFallsBackToStreaming 覆盖源端是执行实例本
// 地文件、目标端是共享地址的回退：另一端不是受管服务器，不能本地拷贝，
// 按同一规则选择已挂载端点后复用本地源到服务器的流式中继。
func TestRelayFileLocalSourceToShareFallsBackToStreaming(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)
	localFile := filepath.Join(t.TempDir(), "local-source.bin")
	if err := os.WriteFile(localFile, []byte("local source payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetRemote := "/mnt/sshmgr/datasets/Reports/local-out.bin"

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_path":        localFile,
			"to_share_address": `smb://192.0.2.10/datasets/Reports/local-out.bin`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.Mode != "streaming_relay" || output.RestartAfterInterruption ||
		output.ShareEndpointServerID != environment.physicalAID {
		t.Fatalf("启动结果 = %+v，期待选择 A 物理端点并走本地源流式中继", output)
	}
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if !strings.Contains(stdout, "流式转发") ||
		!strings.Contains(stdout, environment.physicalAID) {
		t.Fatalf("任务输出没有写明流式转发与端点：\n%s", stdout)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "local source payload" {
		t.Fatalf("共享目标文件内容 = %q err=%v", content, err)
	}
}

// TestRelayFileShareSourceToLocalTargetRejected 钉住共享地址作为源端时不能
// 把文件中继到执行实例本地路径；报错文字准确说明方向限制。
func TestRelayFileShareSourceToLocalTargetRejected(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `smb://192.0.2.10/datasets/Reports/source.bin`,
			"to_path":            filepath.Join(t.TempDir(), "out.bin"),
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if !result.IsError {
		t.Fatalf("共享源端加本地目标应报错：%+v", result.Content)
	}
	text := toolResultText(result)
	if !strings.Contains(text, "源端是共享地址时，目标端必须是受管服务器上的常规文件路径") ||
		!strings.Contains(text, "不能把文件中继到执行实例本地路径") {
		t.Fatalf("报错 = %q", text)
	}
	if commands := environment.physicalAFake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("拒绝路径不应连接端点服务器：%#v", commands)
	}
}

// TestRelayFileShareFallbackViaOverridesTargetEndpoint 覆盖 `via` 覆盖自动
// 选择，以及共享地址在目标端时的方向对称性。
func TestRelayFileShareFallbackViaOverridesTargetEndpoint(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)
	sourceRemote := "/ordinary/source.bin"
	targetRemote := "/mnt/sshmgr/datasets/Reports/target.bin"
	environment.write(t, sourceRemote, "target direction payload")

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.ordinaryID,
			"from_path":        sourceRemote,
			"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
			"via":              environment.physicalBID,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.Mode != "streaming_relay" || output.RestartAfterInterruption ||
		output.ShareEndpointServerID != environment.physicalBID {
		t.Fatalf("启动结果 = %+v，期待 via 指定的 B 物理端点作为目标端点", output)
	}
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "target direction payload" {
		t.Fatalf("共享目标文件内容 = %q err=%v", content, err)
	}
	if commands := environment.physicalAFake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("via 指定端点时不应自动检查其他候选：%#v", commands)
	}
}

// TestRelayFileShareFallbackAllowsOrdinaryWindowsEndpoint 钉住 Windows 只是不
// 能充当共享地址端点；它仍可作为回退中继的普通目标端。
func TestRelayFileShareFallbackAllowsOrdinaryWindowsEndpoint(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/Reports/source.bin"
	targetRemote := "C:/Users/Public/target.bin"
	environment.write(t, sourceRemote, "windows ordinary endpoint payload")

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `smb://192.0.2.10/datasets/Reports/source.bin`,
			"to_server_id":       environment.ordinaryWindowsID,
			"to_path":            targetRemote,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.Mode != "streaming_relay" ||
		output.ShareEndpointServerID != environment.physicalAID {
		t.Fatalf("启动结果 = %+v，期待 Linux 端点与普通 Windows 目标走流式中继", output)
	}
	if _, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second); status != bgStatusDone ||
		errorText != "" {
		t.Fatalf("普通 Windows 目标终态 = %q 错误 = %q", status, errorText)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "windows ordinary endpoint payload" {
		t.Fatalf("普通 Windows 目标内容 = %q err=%v", content, err)
	}
}

// TestRelayFileShareFallbackSkipsUnmountedCandidate 证明自动选择只认现场检查
// 已挂载的服务器；排序第一但未挂载的候选会被跳过。
func TestRelayFileShareFallbackSkipsUnmountedCandidate(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	environment.physicalAFake.setMounted(false)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/Reports/source.bin"
	targetRemote := "/ordinary/target.bin"
	environment.write(t, sourceRemote, "second physical payload")

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `smb://192.0.2.10/datasets/Reports/source.bin`,
			"to_server_id":       environment.ordinaryID,
			"to_path":            targetRemote,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.ShareEndpointServerID != environment.physicalBID {
		t.Fatalf("端点 = %s（A=%s B=%s），期待跳过未挂载的 A 后选择 B：%+v",
			output.ShareEndpointServerID, environment.physicalAID,
			environment.physicalBID, output)
	}
	if _, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second); status != bgStatusDone ||
		errorText != "" {
		t.Fatalf("终态 = %q 错误 = %q", status, errorText)
	}
}

func relayShareFallbackReject(
	t *testing.T, session *relayE2ESession, arguments map[string]any, wantError string,
) string {
	t.Helper()
	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file", Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if !result.IsError {
		t.Fatalf("relay_file 应拒绝：%+v", result.Content)
	}
	text := toolResultText(result)
	if !strings.Contains(text, wantError) {
		t.Fatalf("报错 = %q，期待包含 %q", text, wantError)
	}
	for _, forbidden := range []string{"192.0.2.", "100.64.0.", "mount-user", "mount-password"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("报错泄露敏感信息 %q：%s", forbidden, text)
		}
	}
	return text
}

// TestRelayFileShareFallbackViaFailures 覆盖 via 的可见性、可挂载范围、
// Windows 限制与现场挂载检查失败原因。
func TestRelayFileShareFallbackViaFailures(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)
	base := map[string]any{
		"from_share_address": `smb://192.0.2.10/datasets/Reports/source.bin`,
		"to_server_id":       environment.ordinaryID,
		"to_path":            "/ordinary/target.bin",
	}
	withVia := func(serverID string) map[string]any {
		arguments := make(map[string]any, len(base)+1)
		for key, value := range base {
			arguments[key] = value
		}
		arguments["via"] = serverID
		return arguments
	}

	relayShareFallbackReject(t, session, withVia("missing-server"), "via 指定的服务器不存在或不在当前授权范围内")
	relayShareFallbackReject(t, session, withVia(environment.hiddenID), "via 指定的服务器不存在或不在当前授权范围内")
	relayShareFallbackReject(t, session, withVia(environment.ordinaryID), "不在共享存储 datasets 的可挂载范围内")
	relayShareFallbackReject(t, session, withVia(environment.windowsID), "Windows 的共享盘符只存在于命令会话")
	environment.physicalAFake.setMounted(false)
	relayShareFallbackReject(t, session, withVia(environment.physicalAID), "尚未实际挂载共享存储 datasets")
}

// TestRelayFileShareFallbackNoMountedCandidates 保证无可用候选时不自动选择
// 未挂载服务器，并给出挂载或调整授权的行动指引。
func TestRelayFileShareFallbackNoMountedCandidates(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	environment.virtualFake.setMounted(false)
	environment.physicalAFake.setMounted(false)
	environment.physicalBFake.setMounted(false)
	session := environment.openSession(t)

	noCandidateText := relayShareFallbackReject(t, session, map[string]any{
		"from_share_address": `smb://192.0.2.10/datasets/Reports/source.bin`,
		"to_server_id":       environment.ordinaryID,
		"to_path":            "/ordinary/target.bin",
	}, "没有可作为共享地址端点的已实际挂载 Linux 或 macOS 服务器")
	if !strings.Contains(noCandidateText, "请先在当前授权范围内可挂载") ||
		!strings.Contains(noCandidateText, "调整授权") {
		t.Fatal("无候选错误应提示先挂载或调整授权")
	}
}

// TestRelayFileShareFallbackStopAndResume 证明共享地址端点没有改变既有流式
// 中继的块清单断点续传语义。
func TestRelayFileShareFallbackStopAndResume(t *testing.T) {
	environment := newRelayShareFallbackEnv(t)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/Reports/large.bin"
	targetRemote := "/ordinary/resume.bin"
	payloadSize := int64(128 << 20)
	sparseDirectory := filepath.Dir(environment.hostPath(sourceRemote))
	if err := os.MkdirAll(sparseDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	relaySparseFile(t, toSlash(sparseDirectory), filepath.Base(sourceRemote), payloadSize)
	arguments := map[string]any{
		"from_share_address": `smb://192.0.2.10/datasets/` + strings.TrimPrefix(sourceRemote, "/mnt/sshmgr/datasets/"),
		"to_server_id":       environment.ordinaryID,
		"to_path":            targetRemote,
	}

	start := func() RelayOutput {
		result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "relay_file", Arguments: arguments,
		})
		if err != nil {
			t.Fatalf("调用 relay_file：%v", err)
		}
		if result.IsError {
			t.Fatalf("relay_file 失败：%s", toolResultText(result))
		}
		var output RelayOutput
		unmarshalToolJSON(t, result, &output)
		return output
	}
	first := start()
	relayE2EWaitLine(t, session, first.TaskID, "chunk 1/8 ok", 30*time.Second)
	stopResult, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_stop", Arguments: map[string]any{"task_id": first.TaskID},
	})
	if err != nil || stopResult.IsError {
		t.Fatalf("exec_stop：err=%v result=%+v", err, stopResult.Content)
	}
	firstOutput, firstStatus, firstError := relayE2EPoll(t, session, first.TaskID, 30*time.Second)
	if firstStatus != bgStatusStopped {
		t.Fatalf("第一次终态 = %q 错误 = %q 输出 =\n%s", firstStatus, firstError, firstOutput)
	}
	if !strings.Contains(firstOutput, "流式转发") ||
		!strings.Contains(firstOutput, environment.physicalAID) ||
		!strings.Contains(firstOutput, "块清单") {
		t.Fatalf("停止后的任务终态输出没有写明模式、端点与续传语义：\n%s", firstOutput)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote)); !os.IsNotExist(err) {
		t.Fatalf("停止后真名不应出现：stat err=%v", err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote) + relayManifestSuffix); err != nil {
		t.Fatalf("停止后应保留块清单：stat err=%v", err)
	}

	second := start()
	if second.Mode != "streaming_relay" || second.RestartAfterInterruption ||
		second.ResumedChunks != 1 || second.ChunksTotal != 8 ||
		second.ShareEndpointServerID != environment.physicalAID {
		t.Fatalf("续传启动结果 = %+v，期待沿用同一端点并续传 1 块", second)
	}
	finalOutput, finalStatus, finalError := relayE2EPoll(t, session, second.TaskID, 30*time.Second)
	if finalStatus != bgStatusDone || finalError != "" {
		t.Fatalf("续传终态 = %q 错误 = %q 输出 =\n%s", finalStatus, finalError, finalOutput)
	}
	if !strings.Contains(finalOutput, "resumed 1") {
		t.Fatalf("续传任务输出应记录已完成块：\n%s", finalOutput)
	}
	target, err := os.ReadFile(environment.hostPath(targetRemote))
	if err != nil || int64(len(target)) != payloadSize {
		t.Fatalf("续传后目标大小 = %d err=%v，期待 %d", len(target), err, payloadSize)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote) + relayPartialSuffix); !os.IsNotExist(err) {
		t.Fatalf("续传完成后半成品应消失：stat err=%v", err)
	}
}
