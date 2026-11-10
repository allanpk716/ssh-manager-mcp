package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

const networkDriveTestPassword = `drive-p@ss%'\"|中文`

// fakePowerShellDriveServer 模拟登记为 PowerShell 的 Windows 目标机：按固定
// 探测命令的输出协议回报 445 探测结果，按命令文本里带随机标记的状态行格
// 式回报映射结果，再输出用户命令的结果。它记录收到的每条命令与标准输入。
type fakePowerShellDriveServer struct {
	mu sync.Mutex

	commands       []string
	standardInputs [][]byte
	reachable      map[string]bool
	// mapStatus 是映射状态行里回传的结果：ok、error-<编号> 或 timeout。
	mapStatus string
}

var driveProbeInvocationPattern = regexp.MustCompile(`probe-one (\d+) (\S+) 445 (\d+)`)
var driveStatusMarkerPattern = regexp.MustCompile(`SSHMGR-SMBMAP:([0-9a-f]+):`)

func (f *fakePowerShellDriveServer) handler(command string, stdin io.Reader) (string, string, int) {
	stdinBytes, err := io.ReadAll(stdin)
	if err != nil {
		return "", "read standard input: " + err.Error(), 1
	}
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.standardInputs = append(f.standardInputs, append([]byte(nil), stdinBytes...))
	reachable := f.reachable
	mapStatus := f.mapStatus
	f.mu.Unlock()

	if strings.Contains(command, "probe-one ") {
		output := strings.Builder{}
		for _, match := range driveProbeInvocationPattern.FindAllStringSubmatch(command, -1) {
			index, address := match[1], match[2]
			if reachable[address] {
				fmt.Fprintf(&output, "SSHMGR-PROBE %s ok 2 \n", index)
			} else {
				fmt.Fprintf(&output, "SSHMGR-PROBE %s fail 2 no-route\n", index)
			}
		}
		return output.String(), "", 0
	}
	if markerMatch := driveStatusMarkerPattern.FindStringSubmatch(command); markerMatch != nil {
		if mapStatus == "" {
			mapStatus = "ok"
		}
		stdout := ""
		if mapStatus != "missing" {
			stdout = fmt.Sprintf("SSHMGR-SMBMAP:%s:0:%s\n", markerMatch[1], mapStatus)
		}
		return stdout + "user-command-output\n", "", 0
	}
	return "plain-command-output\n", "", 0
}

func (f *fakePowerShellDriveServer) recorded() (commands []string, inputs [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), append([][]byte(nil), f.standardInputs...)
}

type networkDriveFixture struct {
	clientSession *mcp.ClientSession
	serverID      string
	store         *store.Store
	fake          *fakePowerShellDriveServer
}

// seedNetworkDriveVault 建立网络盘符注入的端到端数据：提供方设备与目标设备
// 有物理与虚拟两个共同网络、目标 PowerShell 服务器、挂载密码机密项与共享
// 登记。变体通过 setup 调整可写名单、shell 类型或探测结论。
func seedNetworkDriveVault(t *testing.T, setup func(*networkDriveSeedSetup)) networkDriveFixture {
	t.Helper()
	settings := networkDriveSeedSetup{
		reachable: map[string]bool{"192.0.2.10": true, "100.64.10.10": true},
	}
	if setup != nil {
		setup(&settings)
	}
	fake := &fakePowerShellDriveServer{reachable: settings.reachable, mapStatus: settings.mapStatus}
	st := newStore(t)
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: fake.handler})
	t.Cleanup(cleanup)

	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("windows-device", "挂载目标", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "办公虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct{ network, address string }{
		{"办公物理局域网", "192.0.2.10"},
		{"办公虚拟局域网", "100.64.10.10"},
	} {
		if _, err := st.AddNetworkMembership("NAS", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	for _, membership := range []struct{ network, address string }{
		{"办公物理局域网", "192.0.2.20"},
		{"办公虚拟局域网", "100.64.10.20"},
	} {
		if _, err := st.AddNetworkMembership("windows-device", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	shellType := models.ShellPowerShell
	if settings.posixTarget {
		shellType = models.ShellPOSIX
	}
	server := &models.Server{
		Name: "windows-web", Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: shellType,
	}
	added, err := st.AddServerWithCredentialsToDevice(server,
		&models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "windows-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(serverByAddr(st, server.Host, server.Port), hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("网络盘符 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte(networkDriveTestPassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	allowedTargets := []string{"windows-web"}
	if settings.notWritable {
		if _, err := st.AddServer(&models.Server{
			Name: "other-server", Host: "192.0.2.99", Port: 22, User: "deploy",
			AuthMethod: models.AuthPassword,
		}); err != nil {
			t.Fatal(err)
		}
		allowedTargets = []string{"other-server"}
	}
	if err := st.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, allowedTargets, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "S",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	// 注入步骤是固定生成的命令，绝不能进入命令策略判定；这条规则同时拦
	// 截探测与映射脚本的特征，用户命令本身不受影响。
	if err := st.AddPolicyRule(policy.CustomRule{
		ID:          "DRIVE-INJECTION-LEAK",
		Shells:      []models.ShellType{shellType},
		Regex:       `New-SmbMapping|probe-one|SSHMGR-SMBMAP`,
		Disposition: policy.DispositionBlock,
		Explanation: "test rule that catches an injection step leaked into policy",
	}); err != nil {
		t.Fatal(err)
	}

	serverFace, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := serverFace.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return networkDriveFixture{
		clientSession: clientSession, serverID: added.ServerID,
		store: st, fake: fake,
	}
}

type networkDriveSeedSetup struct {
	reachable   map[string]bool
	mapStatus   string
	notWritable bool
	posixTarget bool
}

// TestNetworkDriveInjectionExecCommandPowerShell 覆盖贯通路径：可写名单内的
// PowerShell 服务器在用户命令前完成 445 选址与盘符映射，输出里的状态行被
// 剔除，返回与审计只出现共享名与盘符，密码只经标准输入送达。
func TestNetworkDriveInjectionExecCommandPowerShell(t *testing.T) {
	fixture := seedNetworkDriveVault(t, nil)
	const userCommand = `Get-ChildItem S:\datasets`

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": userCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout                string `json:"stdout"`
		Stderr                string `json:"stderr"`
		InjectedNetworkDrives []struct {
			Share string `json:"share"`
			Drive string `json:"drive"`
		} `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "user-command-output\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if output.Stderr != "" {
		t.Fatalf("stderr = %q", output.Stderr)
	}
	if len(output.InjectedNetworkDrives) != 1 ||
		output.InjectedNetworkDrives[0].Share != "datasets" ||
		output.InjectedNetworkDrives[0].Drive != "S" {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	if strings.Contains(toolResultText(result), networkDriveTestPassword) {
		t.Fatalf("tool result leaked the mount password")
	}
	if markerMatch := driveStatusMarkerPattern.FindString(output.Stdout); markerMatch != "" {
		t.Fatalf("stdout leaked a status marker: %q", markerMatch)
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 2 {
		t.Fatalf("remote commands = %#v, want one probe and one wrapped command", commands)
	}
	if !strings.Contains(commands[0], "probe-one 0 192.0.2.10 445 ") ||
		!strings.Contains(commands[0], "probe-one 1 100.64.10.10 445 ") {
		t.Fatalf("probe command does not cover both common networks in order: %q", commands[0])
	}
	wrapped := commands[1]
	for _, want := range []string{
		"New-SmbMapping",
		`AddArgument('\\192.0.2.10\datasets')`,
		"AddArgument('mount-user')",
		"AddArgument('S:')",
		"-Persistent $false",
		userCommand,
	} {
		if !strings.Contains(wrapped, want) {
			t.Fatalf("wrapped command missing %q:\n%s", want, wrapped)
		}
	}
	if strings.Contains(wrapped, `AddArgument('\\100.64.10.10`) {
		t.Fatalf("physical network address was not preferred: %q", wrapped)
	}
	if strings.Contains(wrapped, networkDriveTestPassword) {
		t.Fatalf("wrapped command leaked the mount password")
	}
	for _, command := range commands {
		if strings.Contains(command, networkDriveTestPassword) {
			t.Fatalf("remote command leaked the mount password: %q", command)
		}
	}
	if len(inputs[1]) != len(networkDriveTestPassword) || string(inputs[1]) != networkDriveTestPassword {
		t.Fatalf("wrapped command standard input = %q, want only the mount password", inputs[1])
	}

	auditRows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(auditRows))
	}
	row := auditRows[0]
	if row.Command != userCommand {
		t.Fatalf("audit command = %q, want the user command only", row.Command)
	}
	if len(row.InjectedShareNames) != 1 || row.InjectedShareNames[0] != "datasets" {
		t.Fatalf("audit injected share names = %#v", row.InjectedShareNames)
	}
	encoded, err := json.Marshal(row.JSONMap())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), networkDriveTestPassword) ||
		strings.Contains(string(encoded), "192.0.2.10") ||
		strings.Contains(string(encoded), "mount-user") {
		t.Fatalf("audit row leaked the address, account, or password: %s", encoded)
	}
}

// TestNetworkDriveInjectionSkipsServerOutsideWritableList 钉住授权边界：不
// 在挂载密码机密项可写入服务器列表内的 Windows 服务器不探测、不映射、行
// 为与没有共享登记时完全一致。
func TestNetworkDriveInjectionSkipsServerOutsideWritableList(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.notWritable = true
	})

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-Location"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout                string `json:"stdout"`
		InjectedNetworkDrives []any  `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "plain-command-output\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if len(output.InjectedNetworkDrives) != 0 {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	commands, inputs := fixture.fake.recorded()
	if len(commands) != 1 {
		t.Fatalf("remote commands = %#v, want only the user command", commands)
	}
	if strings.Contains(commands[0], "New-SmbMapping") || strings.Contains(commands[0], "probe-one") {
		t.Fatalf("server outside the writable list received an injection step: %q", commands[0])
	}
	if len(inputs[0]) != 0 {
		t.Fatalf("standard input = %q, want empty", inputs[0])
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].InjectedShareNames) != 0 {
		t.Fatalf("audit rows = %#v", rows)
	}
}

// TestNetworkDriveInjectionLeavesPosixServerUnchanged 钉住非 Windows 服务器
// 的行为边界：即使在可写名单内也不探测、不映射、不改变命令与输出。
func TestNetworkDriveInjectionLeavesPosixServerUnchanged(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.posixTarget = true
	})

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "ls /mnt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout                string `json:"stdout"`
		InjectedNetworkDrives []any  `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "plain-command-output\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if len(output.InjectedNetworkDrives) != 0 {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	commands, inputs := fixture.fake.recorded()
	if len(commands) != 1 {
		t.Fatalf("remote commands = %#v, want only the user command", commands)
	}
	if strings.Contains(commands[0], "New-SmbMapping") || strings.Contains(commands[0], "probe-one") {
		t.Fatalf("POSIX server received a Windows injection step: %q", commands[0])
	}
	if len(inputs[0]) != 0 {
		t.Fatalf("standard input = %q, want empty", inputs[0])
	}
}

// TestNetworkDriveInjectionMappingFailureWarnsWithoutBlocking 覆盖映射失败：
// 用户命令照常执行、退出码不变，警告按原因区分并附在标准输出末尾，状态
// 行标记与密码都不出现在任何返回里。
func TestNetworkDriveInjectionMappingFailureWarnsWithoutBlocking(t *testing.T) {
	cases := []struct {
		name       string
		mapStatus  string
		wantOutput string
	}{
		{
			name:       "drive letter taken",
			mapStatus:  "error-85",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：盘符 S: 在这台服务器上已被占用\n",
		},
		{
			name:       "mapping timeout",
			mapStatus:  "timeout",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：映射超时（3 秒上限），按网络类失败处理\n",
		},
		{
			name:       "missing status line",
			mapStatus:  "missing",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：注入步骤没有返回状态行\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
				setup.mapStatus = testCase.mapStatus
			})
			result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "exec_command",
				Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-ChildItem S:\\"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("exec_command failed: %+v", result.Content)
			}
			var output struct {
				Stdout                string `json:"stdout"`
				Stderr                string `json:"stderr"`
				ExitCode              int    `json:"exit_code"`
				InjectedNetworkDrives []any  `json:"injected_network_drives"`
			}
			unmarshalToolJSON(t, result, &output)
			if output.Stdout != testCase.wantOutput {
				t.Fatalf("stdout = %q, want %q", output.Stdout, testCase.wantOutput)
			}
			if output.Stderr != "" || output.ExitCode != 0 {
				t.Fatalf("mapping failure changed the command outcome: stderr=%q exit=%d", output.Stderr, output.ExitCode)
			}
			if len(output.InjectedNetworkDrives) != 0 {
				t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
			}
			if strings.Contains(toolResultText(result), networkDriveTestPassword) {
				t.Fatal("tool result leaked the mount password")
			}
			if markerMatch := driveStatusMarkerPattern.FindString(output.Stdout + output.Stderr); markerMatch != "" {
				t.Fatalf("output leaked a status marker: %q", markerMatch)
			}
			rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || len(rows[0].InjectedShareNames) != 0 {
				t.Fatalf("audit rows = %#v", rows)
			}
		})
	}
}
