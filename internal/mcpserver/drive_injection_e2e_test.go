package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

const networkDriveTestPassword = `drive-p@ss%'\"|()中文` + "\r\n"
const networkDriveSecondTestPassword = `second-drive-p@ss!%()` + "\r\n"
const networkDriveEnvironmentValue = `cmd-env-%-!中文`

// fakePowerShellDriveServer 模拟登记为 PowerShell 的 Windows 目标机：按固定
// 探测命令的输出协议回报 445 探测结果，按命令文本里带随机标记的状态行格
// 式回报映射结果，再输出用户命令的结果。它记录收到的每条命令与标准输入。
type fakePowerShellDriveServer struct {
	mu sync.Mutex

	commands       []string
	standardInputs [][]byte
	reachable      map[string]bool
	// mapStatus 是第 0 个共享映射状态行里回传的结果；mapStatuses 按计划
	// 中的共享序号区分多个共享。结果可为 ok、error-<编号> 或 timeout。
	mapStatus       string
	mapStatuses     map[int]string
	statusesByDrive map[string]string
}

var driveProbeInvocationPattern = regexp.MustCompile(`probe-one (\d+) (\S+) 445 (\d+)`)
var driveStatusMarkerPattern = regexp.MustCompile(`SSHMGR-SMBMAP:([0-9a-f]+):`)
var driveEncodedStatusPattern = regexp.MustCompile(`SSHMGR-SMBMAP:([0-9a-f]+):(\d+):`)
var driveArgumentPattern = regexp.MustCompile(`AddArgument\('([A-Z]):'\)`)

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
	mapStatuses := f.mapStatuses
	statusesByDrive := f.statusesByDrive
	f.mu.Unlock()

	decodedCommand := command
	if strings.Contains(command, "-EncodedCommand ") {
		decodedCommand = strings.Join(encodedCommandScripts(command), "\n")
	}
	if strings.Contains(decodedCommand, "probe-one ") {
		output := strings.Builder{}
		for _, match := range driveProbeInvocationPattern.FindAllStringSubmatch(decodedCommand, -1) {
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
		stdout := ""
		for index, driveMatch := range driveArgumentPattern.FindAllStringSubmatch(decodedCommand, -1) {
			status := fakeNetworkDriveStatus(mapStatus, mapStatuses[index], statusesByDrive[driveMatch[1]])
			if status != "missing" {
				stdout += fmt.Sprintf("SSHMGR-SMBMAP:%s:%d:%s\n", markerMatch[1], index, status)
			}
		}
		return stdout + "user-command-output\n", "", 0
	}
	if strings.Contains(command, "-EncodedCommand ") {
		stdout := strings.Builder{}
		for _, script := range encodedCommandScripts(command) {
			for _, match := range driveEncodedStatusPattern.FindAllStringSubmatch(script, -1) {
				status := mapStatus
				var index int
				fmt.Sscanf(match[2], "%d", &index)
				driveMatch := driveArgumentPattern.FindStringSubmatch(script)
				statusByDrive := ""
				if driveMatch != nil {
					statusByDrive = statusesByDrive[driveMatch[1]]
				}
				status = fakeNetworkDriveStatus(status, mapStatuses[index], statusByDrive)
				if status != "missing" {
					fmt.Fprintf(&stdout, "SSHMGR-SMBMAP:%s:%s:%s\n", match[1], match[2], status)
				}
			}
		}
		return stdout.String() + "user-command-output\n", "", 0
	}
	return "plain-command-output\n", "", 0
}

func fakeNetworkDriveStatus(fallback, statusByIndex, statusByDrive string) string {
	status := fallback
	if statusByIndex != "" {
		status = statusByIndex
	}
	if statusByDrive != "" {
		status = statusByDrive
	}
	if status == "" {
		return "ok"
	}
	return status
}

func (f *fakePowerShellDriveServer) recorded() (commands []string, inputs [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), append([][]byte(nil), f.standardInputs...)
}

func (f *fakePowerShellDriveServer) setStatusesByDrive(statuses map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusesByDrive = statuses
}

// setMapStatus 让测试在同一条服务器连接上先回传网络类映射失败、再回传成
// 功，模拟第一候选地址实际不可达而下一候选可用的场景。
func (f *fakePowerShellDriveServer) setMapStatus(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapStatus = status
}

func encodedCommandScripts(command string) []string {
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	var scripts []string
	for _, line := range strings.Split(command, "\n") {
		line = strings.TrimSuffix(line, "\r")
		encoded, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		if len(blob)%2 != 0 {
			continue
		}
		units := make([]uint16, len(blob)/2)
		for index := range units {
			units[index] = binary.LittleEndian.Uint16(blob[index*2:])
		}
		scripts = append(scripts, string(utf16.Decode(units)))
	}
	return scripts
}

type networkDriveFixture struct {
	clientSession  *mcp.ClientSession
	serverID       string
	secondServerID string
	store          *store.Store
	fake           *fakePowerShellDriveServer
}

// manualNetworkDriveClock 是网络盘符选址缓存的测试时钟：时间只在测试明
// 确拨动时前进，缓存过期不需要真实等待。
type manualNetworkDriveClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualNetworkDriveClock() *manualNetworkDriveClock {
	return &manualNetworkDriveClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *manualNetworkDriveClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualNetworkDriveClock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// useManualNetworkDriveClock 给网络盘符选址缓存换上测试时钟，并在测试结
// 束后恢复生产时钟与空缓存，避免影响其他测试。
func useManualNetworkDriveClock(t *testing.T) *manualNetworkDriveClock {
	t.Helper()
	clock := newManualNetworkDriveClock()
	previous := networkDriveAddressCache
	networkDriveAddressCache = newNetworkDriveAddressCache(clock.Now)
	t.Cleanup(func() { networkDriveAddressCache = previous })
	return clock
}

func execNetworkDriveCommand(t *testing.T, fixture networkDriveFixture, command string) string {
	t.Helper()
	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": command},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)
	if strings.Contains(toolResultText(result), networkDriveTestPassword) {
		t.Fatal("tool result leaked the mount password")
	}
	return output.Stdout
}

func countNetworkDriveProbeCommands(commands []string) int {
	count := 0
	for _, command := range commands {
		if strings.Contains(command, "probe-one ") {
			count++
		}
	}
	return count
}

// TestNetworkDriveInjectionAddressCacheSkipsRepeatProbes 钉住选址缓存：同
// 一服务器条目、共享与共享存储版本号在 10 分钟内的后续命令不再探测 445；
// 缓存过期后重新探测。
func TestNetworkDriveInjectionAddressCacheSkipsRepeatProbes(t *testing.T) {
	clock := useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, nil)

	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 1 {
		t.Fatalf("probe commands within cache lifetime = %d, want 1: %#v", probes, commands)
	}
	if len(commands) != 3 {
		t.Fatalf("remote commands = %d, want two wrapped commands and one probe: %#v", len(commands), commands)
	}
	for _, wrapped := range []string{commands[1], commands[2]} {
		if !strings.Contains(wrapped, `AddArgument('\\192.0.2.10\datasets')`) {
			t.Fatalf("wrapped command did not reuse the cached physical address: %q", wrapped)
		}
	}

	clock.Advance(10*time.Minute + time.Second)
	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	commands, _ = fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 2 {
		t.Fatalf("probe commands after cache expiry = %d, want 2: %#v", probes, commands)
	}
}

// TestNetworkDriveInjectionAddressCacheInvalidatedByShareVersion 钉住版本
// 失效：共享存储登记被修改、版本号随之变化后，旧选址缓存立
// 即失效，下一条命令重新探测。
func TestNetworkDriveInjectionAddressCacheInvalidatedByShareVersion(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, nil)

	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	note := "网络成员变化后的新登记说明"
	if _, err := fixture.store.EditSharedStorage("datasets", store.SharedStorageChanges{
		Note: &note,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")

	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 2 {
		t.Fatalf("probe commands after share version change = %d, want 2: %#v", probes, commands)
	}
}

// TestNetworkDriveInjectionNetworkFailureSelectsNextCandidate 覆盖真机结论
// #206 的选址修订：445 探测可能被透明代理误报连通，注入遇到网络类映射失
// 败（64、53、67 或超时）后，该地址在进程内缓存里标为不可用，下一条命令
// 改选下一个候选，不再探测已作废的地址。
func TestNetworkDriveInjectionNetworkFailureSelectsNextCandidate(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.mapStatus = "error-64"
	})

	stdout := execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	if !strings.Contains(stdout, "网络类失败（系统错误 64）") {
		t.Fatalf("first command missing network failure warning: %q", stdout)
	}
	fixture.fake.setMapStatus("ok")
	stdout = execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	want := "user-command-output\n共享 datasets 的网络盘符 S: 已使用虚拟局域网：办公虚拟局域网 第1个地址，性能可能下降；物理局域网地址不通：办公物理局域网 第1个地址（网络类映射失败）\n"
	if stdout != want {
		t.Fatalf("second command stdout = %q, want %q", stdout, want)
	}

	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 2 {
		t.Fatalf("probe commands = %d, want one per failed selection round: %#v", probes, commands)
	}
	if strings.Contains(commands[2], "192.0.2.10") {
		t.Fatalf("second probe still covered the marked-unavailable address: %q", commands[2])
	}
	if !strings.Contains(commands[2], "probe-one 0 100.64.10.10 445 ") {
		t.Fatalf("second probe did not cover the next candidate: %q", commands[2])
	}
	if !strings.Contains(commands[3], `AddArgument('\\100.64.10.10\datasets')`) {
		t.Fatalf("second mapping did not use the next candidate address: %q", commands[3])
	}
	for _, command := range commands {
		if strings.Contains(command, networkDriveTestPassword) {
			t.Fatalf("remote command leaked the mount password: %q", command)
		}
	}
}

// TestNetworkDriveInjectionAllCandidatesUnavailableWaitsForExpiry 钉住全量
// 作废：每个候选都因网络类映射失败标为不可用后，后续命令不再探测，等缓
// 存过期后才重新探测。
func TestNetworkDriveInjectionAllCandidatesUnavailableWaitsForExpiry(t *testing.T) {
	clock := useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.mapStatus = "error-64"
	})

	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	stdout := execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	if !strings.Contains(stdout, "共享 datasets 的网络盘符 S: 未注入") {
		t.Fatalf("waiting warning missing: %q", stdout)
	}
	for _, want := range []string{"办公物理局域网", "办公虚拟局域网", "缓存过期"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("waiting warning missing %q: %q", want, stdout)
		}
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10", "192.0.2.20", "100.64.10.20"} {
		if strings.Contains(stdout, address) {
			t.Fatalf("waiting warning leaked address %s: %q", address, stdout)
		}
	}

	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 2 {
		t.Fatalf("probe commands before expiry = %d, want 2: %#v", probes, commands)
	}
	if len(commands) != 5 {
		t.Fatalf("remote commands = %d, want two probe and mapping pairs plus one plain command: %#v",
			len(commands), commands)
	}
	if strings.Contains(commands[4], "New-SmbMapping") {
		t.Fatalf("command with all candidates unavailable still injected: %q", commands[4])
	}

	clock.Advance(10*time.Minute + time.Second)
	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	commands, _ = fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 3 {
		t.Fatalf("probe commands after cache expiry = %d, want 3: %#v", probes, commands)
	}
}

// seedNetworkDriveVault 建立网络盘符注入的端到端数据：提供方设备与目标设备
// 有物理与虚拟两个共同网络、目标 Windows 服务器、挂载密码机密项与共享
// 登记。变体通过 setup 调整可写名单、shell 类型、映射结果或探测结论。
func seedNetworkDriveVault(t *testing.T, setup func(*networkDriveSeedSetup)) networkDriveFixture {
	t.Helper()
	settings := networkDriveSeedSetup{
		reachable: map[string]bool{"192.0.2.10": true, "100.64.10.10": true},
	}
	if setup != nil {
		setup(&settings)
	}
	fake := &fakePowerShellDriveServer{
		reachable: settings.reachable, mapStatus: settings.mapStatus,
		mapStatuses:     settings.mapStatuses,
		statusesByDrive: settings.statusesByDrive,
	}
	st := newStore(t)
	secondServerID := ""
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
	if settings.disjointNetworks {
		if _, err := st.AddNetwork(models.Network{Name: "办公管理网络", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("windows-device", "办公管理网络", "192.0.2.30"); err != nil {
			t.Fatal(err)
		}
	} else {
		for _, membership := range []struct{ network, address string }{
			{"办公物理局域网", "192.0.2.20"},
			{"办公虚拟局域网", "100.64.10.20"},
		} {
			if _, err := st.AddNetworkMembership("windows-device", membership.network, membership.address); err != nil {
				t.Fatal(err)
			}
		}
	}
	shellType := models.ShellPowerShell
	if settings.posixTarget {
		shellType = models.ShellPOSIX
	} else if settings.cmdTarget {
		shellType = models.ShellWindowsCmd
	}
	server := &models.Server{
		Name: "windows-web", Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: shellType,
	}
	var added *store.AddServerDeviceResult
	var err error
	if settings.noDevice {
		var serverID string
		serverID, err = st.AddServer(&models.Server{
			Name: "windows-web", Host: server.Host, Port: server.Port, User: server.User,
			AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st), ShellType: shellType,
		})
		added = &store.AddServerDeviceResult{ServerID: serverID}
	} else {
		added, err = st.AddServerWithCredentialsToDevice(server,
			&models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "windows-device")
	}
	if err != nil {
		t.Fatal(err)
	}
	grantedServers := []string{added.ServerID}
	if settings.secondServer {
		second, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: "windows-web-2", Host: server.Host, Port: server.Port,
			User: "deploy", ShellType: shellType,
		}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "windows-device")
		if err != nil {
			t.Fatal(err)
		}
		grantedServers = append(grantedServers, second.ServerID)
		secondServerID = second.ServerID
		secondServer, err := st.GetServer(second.ServerID)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveEntryPin(secondServer, hostKey.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveEntryPin(serverByAddr(st, server.Host, server.Port), hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("网络盘符 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, grantedServers); err != nil {
		t.Fatal(err)
	}
	datasetPassword := []byte(networkDriveTestPassword)
	if settings.invalidDatasetPassword {
		datasetPassword = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	}
	if err := st.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码", datasetPassword, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if settings.secondShare {
		if err := st.AddSecret("REPORTS_MOUNT_PASSWORD", "第二个共享挂载密码", []byte(networkDriveSecondTestPassword), store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if settings.environmentSecret {
		if err := st.AddSecretWithEnvironmentName("API_TOKEN", "", []byte(networkDriveEnvironmentValue), "HF_TOKEN", store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	allowedTargets := []string{"windows-web"}
	if settings.secondServer {
		allowedTargets = append(allowedTargets, "windows-web-2")
	}
	if settings.notWritable {
		if _, err := st.AddServer(&models.Server{
			Name: "other-server", Host: "192.0.2.99", Port: 22, User: "deploy",
			AuthMethod: models.AuthPassword,
		}); err != nil {
			t.Fatal(err)
		}
		allowedTargets = []string{"other-server"}
	}
	secretNames := []string{"DATASETS_MOUNT_PASSWORD"}
	if settings.secondShare {
		secretNames = append(secretNames, "REPORTS_MOUNT_PASSWORD")
	}
	if settings.environmentSecret {
		secretNames = append(secretNames, "API_TOKEN")
	}
	if err := st.AllowSecrets(secretNames, allowedTargets, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "S",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if settings.secondShare {
		if _, err := st.AddSharedStorage(store.SharedStorageInput{
			Name: "reports", Device: "NAS", User: "mount-user",
			PasswordSecret: "REPORTS_MOUNT_PASSWORD", Drive: "T",
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
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
		secondServerID: secondServerID,
		store:          st, fake: fake,
	}
}

type networkDriveSeedSetup struct {
	reachable              map[string]bool
	mapStatus              string
	mapStatuses            map[int]string
	statusesByDrive        map[string]string
	notWritable            bool
	posixTarget            bool
	cmdTarget              bool
	secondShare            bool
	secondServer           bool
	invalidDatasetPassword bool
	environmentSecret      bool
	disjointNetworks       bool
	noDevice               bool
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

// TestNetworkDriveInjectionExecCommandWindowsCmd 覆盖 cmd 贯通路径：先选址，
// 再由 cmd 启动 PowerShell 子进程映射盘符。cmd 在读取环境变量后先恢复原代
// 码页；密码包含换行、引号、百分号、竖线与括号，仍只按长度经标准输入送
// 达，不进入命令文本、返回或审计。
func TestNetworkDriveInjectionExecCommandWindowsCmd(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.cmdTarget = true
	})
	const userCommand = `dir S:\datasets`

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
	if output.Stdout != "user-command-output\n" || output.Stderr != "" {
		t.Fatalf("stdout=%q stderr=%q", output.Stdout, output.Stderr)
	}
	if len(output.InjectedNetworkDrives) != 1 ||
		output.InjectedNetworkDrives[0].Share != "datasets" ||
		output.InjectedNetworkDrives[0].Drive != "S" {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	if strings.Contains(toolResultText(result), networkDriveTestPassword) {
		t.Fatal("tool result leaked the mount password")
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 2 {
		t.Fatalf("remote commands = %#v, want one probe and one wrapped command", commands)
	}
	wrapped := commands[1]
	scripts := encodedCommandScripts(wrapped)
	if len(scripts) != 1 {
		t.Fatalf("EncodedCommand scripts = %d, want one per share", len(scripts))
	}
	for _, want := range []string{
		"New-SmbMapping",
		`AddArgument('\\192.0.2.10\datasets')`,
		"AddArgument('mount-user')",
		"AddArgument('S:')",
		"-Persistent $false",
		fmt.Sprintf("New-Object byte[] %d", len(networkDriveTestPassword)),
	} {
		if !strings.Contains(scripts[0], want) {
			t.Fatalf("EncodedCommand script missing %q:\n%s", want, scripts[0])
		}
	}
	restore := strings.Index(wrapped, "chcp %__sshmgr_original_code_page%")
	encodedStart := strings.Index(wrapped, "powershell -NoProfile -NonInteractive -EncodedCommand ")
	userCommandStart := strings.Index(wrapped, userCommand)
	if restore < 0 || encodedStart < restore || userCommandStart < encodedStart {
		t.Fatalf("cmd wrapper must restore the code page before drive mapping and the user command:\n%s", wrapped)
	}
	for _, line := range strings.Split(wrapped, "\n") {
		if strings.Contains(line, "-EncodedCommand ") && len(line) > 8191 {
			t.Fatalf("EncodedCommand line is %d characters, above cmd limit", len(line))
		}
	}
	if strings.Contains(wrapped, networkDriveTestPassword) ||
		strings.Contains(scripts[0], networkDriveTestPassword) {
		t.Fatal("wrapped command or EncodedCommand script leaked the mount password")
	}
	if string(inputs[1]) != networkDriveTestPassword {
		t.Fatalf("wrapped command standard input = %q, want only the mount password", inputs[1])
	}

	auditRows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 || auditRows[0].Command != userCommand ||
		len(auditRows[0].InjectedShareNames) != 1 || auditRows[0].InjectedShareNames[0] != "datasets" {
		t.Fatalf("audit rows = %#v", auditRows)
	}
	encoded, err := json.Marshal(auditRows[0].JSONMap())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), networkDriveTestPassword) {
		t.Fatal("audit row leaked the mount password")
	}
}

// TestNetworkDriveInjectionWindowsCmdMultipleSharesAreIndependent 覆盖多个共
// 享同时注入：每个共享单独选址、单独启动 PowerShell 子进程、单独回传状态；
// 一个共享盘符被占用不影响另一个共享映射成功。
func TestNetworkDriveInjectionWindowsCmdMultipleSharesAreIndependent(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.cmdTarget = true
		setup.secondShare = true
		setup.mapStatuses = map[int]string{0: "error-85", 1: "ok"}
	})
	const userCommand = `dir S:\datasets T:\reports`

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
		InjectedNetworkDrives []struct {
			Share string `json:"share"`
			Drive string `json:"drive"`
		} `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	wantOutput := "user-command-output\n" +
		"共享 datasets 的网络盘符 S: 未注入：盘符 S: 在这台服务器上已被占用\n"
	if output.Stdout != wantOutput {
		t.Fatalf("stdout = %q, want %q", output.Stdout, wantOutput)
	}
	if len(output.InjectedNetworkDrives) != 1 ||
		output.InjectedNetworkDrives[0].Share != "reports" ||
		output.InjectedNetworkDrives[0].Drive != "T" {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	if strings.Contains(toolResultText(result), networkDriveTestPassword) ||
		strings.Contains(toolResultText(result), networkDriveSecondTestPassword) {
		t.Fatal("tool result leaked a mount password")
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 3 {
		t.Fatalf("remote commands = %d, want two probes and one wrapped command", len(commands))
	}
	wrapped := commands[len(commands)-1]
	scripts := encodedCommandScripts(wrapped)
	if len(scripts) != 2 {
		t.Fatalf("EncodedCommand scripts = %d, want one per share", len(scripts))
	}
	if !strings.Contains(scripts[0], `AddArgument('\\192.0.2.10\datasets')`) ||
		!strings.Contains(scripts[1], `AddArgument('\\192.0.2.10\reports')`) {
		t.Fatalf("EncodedCommand scripts do not cover both shares in frame order:\n%s\n%s", scripts[0], scripts[1])
	}
	for index, script := range scripts {
		if strings.Contains(script, networkDriveTestPassword) ||
			strings.Contains(script, networkDriveSecondTestPassword) {
			t.Fatalf("EncodedCommand script %d leaked a password:\n%s", index, script)
		}
	}
	if strings.Contains(wrapped, networkDriveTestPassword) ||
		strings.Contains(wrapped, networkDriveSecondTestPassword) {
		t.Fatal("wrapped command leaked a mount password")
	}
	wantFrames := append([]byte{}, networkDriveTestPassword...)
	wantFrames = append(wantFrames, networkDriveSecondTestPassword...)
	if string(inputs[len(inputs)-1]) != string(wantFrames) {
		t.Fatalf("standard input frames = %q, want both passwords in script order", inputs[len(inputs)-1])
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Command != userCommand ||
		len(rows[0].InjectedShareNames) != 1 || rows[0].InjectedShareNames[0] != "reports" {
		t.Fatalf("audit rows = %#v", rows)
	}
	encoded, err := json.Marshal(rows[0].JSONMap())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), networkDriveTestPassword) ||
		strings.Contains(string(encoded), networkDriveSecondTestPassword) {
		t.Fatal("audit row leaked a mount password")
	}
}

// TestNetworkDriveInjectionWindowsCmdMixesEnvironmentInjection 钉住两类注入
// 在同一条 cmd 命令里的标准输入帧顺序：环境变量值先带行分隔读取，网络盘
// 密码随后按长度精确读取；两类注入都进入同一个封装并留下各自的审计名单。
func TestNetworkDriveInjectionWindowsCmdMixesEnvironmentInjection(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.cmdTarget = true
		setup.environmentSecret = true
	})
	const userCommand = `set HF_TOKEN`

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
		InjectedNetworkDrives []struct {
			Share string `json:"share"`
			Drive string `json:"drive"`
		} `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "user-command-output\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if len(output.InjectedNetworkDrives) != 1 ||
		output.InjectedNetworkDrives[0].Share != "datasets" ||
		output.InjectedNetworkDrives[0].Drive != "S" {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	if strings.Contains(toolResultText(result), networkDriveEnvironmentValue) ||
		strings.Contains(toolResultText(result), networkDriveTestPassword) {
		t.Fatal("tool result leaked an injected secret or mount password")
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 2 {
		t.Fatalf("remote commands = %d, want one probe and one wrapped command", len(commands))
	}
	wrapped := commands[1]
	envRead := strings.Index(wrapped, "set /p HF_TOKEN=")
	restore := strings.Index(wrapped, "chcp %__sshmgr_original_code_page%")
	encodedStart := strings.Index(wrapped, "powershell -NoProfile -NonInteractive -EncodedCommand ")
	if envRead < 0 || restore < envRead || encodedStart < restore {
		t.Fatalf("cmd wrapper must read the environment value before mapping the drive:\n%s", wrapped)
	}
	scripts := encodedCommandScripts(wrapped)
	if len(scripts) != 1 || !strings.Contains(scripts[0], "New-SmbMapping") {
		t.Fatalf("EncodedCommand scripts = %#v, want one network drive mapping script", scripts)
	}
	if strings.Contains(wrapped, networkDriveEnvironmentValue) ||
		strings.Contains(wrapped, networkDriveTestPassword) ||
		strings.Contains(scripts[0], networkDriveEnvironmentValue) ||
		strings.Contains(scripts[0], networkDriveTestPassword) {
		t.Fatal("wrapped command or EncodedCommand script leaked an injected value")
	}
	wantFrames := append([]byte{}, networkDriveEnvironmentValue...)
	wantFrames = append(wantFrames, '\r', '\n')
	wantFrames = append(wantFrames, networkDriveTestPassword...)
	if string(inputs[1]) != string(wantFrames) {
		t.Fatalf("standard input frames = %q, want environment value, line break, then mount password", inputs[1])
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Command != userCommand ||
		len(rows[0].InjectedEnvironmentNames) != 1 || rows[0].InjectedEnvironmentNames[0] != "HF_TOKEN" ||
		len(rows[0].InjectedShareNames) != 1 || rows[0].InjectedShareNames[0] != "datasets" {
		t.Fatalf("audit rows = %#v", rows)
	}
	encoded, err := json.Marshal(rows[0].JSONMap())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), networkDriveEnvironmentValue) ||
		strings.Contains(string(encoded), networkDriveTestPassword) {
		t.Fatal("audit row leaked an injected value")
	}
}

// TestNetworkDriveInjectionWindowsCmdRejectsInvalidUTF8Password 覆盖非法
// UTF-8 挂载密码：该共享不选址、不映射、不进入标准输入，只附警告；另一个
// 合法共享仍照常注入。
func TestNetworkDriveInjectionWindowsCmdRejectsInvalidUTF8Password(t *testing.T) {
	invalidPassword := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.cmdTarget = true
		setup.secondShare = true
		setup.invalidDatasetPassword = true
	})
	const userCommand = `dir T:\reports`

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
		InjectedNetworkDrives []struct {
			Share string `json:"share"`
			Drive string `json:"drive"`
		} `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	wantOutput := "user-command-output\n" +
		"共享 datasets 的网络盘符 S: 未注入：挂载密码在 Windows 上不是合法 UTF-8\n"
	if output.Stdout != wantOutput {
		t.Fatalf("stdout = %q, want %q", output.Stdout, wantOutput)
	}
	if len(output.InjectedNetworkDrives) != 1 ||
		output.InjectedNetworkDrives[0].Share != "reports" ||
		output.InjectedNetworkDrives[0].Drive != "T" {
		t.Fatalf("injected_network_drives = %#v", output.InjectedNetworkDrives)
	}
	if strings.Contains(toolResultText(result), string(invalidPassword)) {
		t.Fatal("tool result leaked the invalid password bytes")
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 2 || len(inputs) != 2 {
		t.Fatalf("remote commands and inputs = %#v %#v, want one probe and one wrapped command", commands, inputs)
	}
	for _, command := range commands {
		if strings.Contains(command, "datasets") || strings.Contains(command, string(invalidPassword)) {
			t.Fatalf("invalid-password share reached the target: %q", command)
		}
	}
	if string(inputs[1]) != networkDriveSecondTestPassword {
		t.Fatalf("wrapped command standard input = %q, want only the valid password", inputs[1])
	}
	scripts := encodedCommandScripts(commands[1])
	if len(scripts) != 1 || !strings.Contains(scripts[0], `AddArgument('\\192.0.2.10\reports')`) {
		t.Fatalf("EncodedCommand scripts = %#v, want only the reports share", scripts)
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Command != userCommand ||
		len(rows[0].InjectedShareNames) != 1 || rows[0].InjectedShareNames[0] != "reports" {
		t.Fatalf("audit rows = %#v", rows)
	}
	encoded, err := json.Marshal(rows[0].JSONMap())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(invalidPassword)) {
		t.Fatal("audit row leaked the invalid password bytes")
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
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：映射超时（3 秒上限）：提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:      "network error 53",
			mapStatus: "error-53",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：网络类失败（系统错误 53）：" +
				"提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:      "network error 64",
			mapStatus: "error-64",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：网络类失败（系统错误 64）：" +
				"提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:      "network error 67",
			mapStatus: "error-67",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：网络类失败（系统错误 67）：" +
				"提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:       "different credentials already connected",
			mapStatus:  "error-1219",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：同一服务器已有不同凭据的连接（系统错误 1219）\n",
		},
		{
			name:       "authentication error 86",
			mapStatus:  "error-86",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n",
		},
		{
			name:       "authentication error 1326",
			mapStatus:  "error-1326",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S: 未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n",
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

// TestNetworkDriveInjectionAuthenticationFailurePausesOnlyFailedShare 钉住
// 认证失败暂停的进程内效果：失败的共享不再尝试映射，其他共享仍照常注入，
// 且暂停期间的每条命令都会得到简短说明。
func TestNetworkDriveInjectionAuthenticationFailurePausesOnlyFailedShare(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.secondShare = true
		setup.statusesByDrive = map[string]string{"S": "error-86", "T": "ok"}
	})
	call := func(command string) (string, []struct {
		Share string `json:"share"`
		Drive string `json:"drive"`
	}, bool) {
		t.Helper()
		result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "exec_command",
			Arguments: map[string]any{
				"server_id": fixture.serverID, "command": command,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("exec_command failed: %s", toolResultText(result))
		}
		var output struct {
			Stdout                string `json:"stdout"`
			InjectedNetworkDrives []struct {
				Share string `json:"share"`
				Drive string `json:"drive"`
			} `json:"injected_network_drives"`
		}
		unmarshalToolJSON(t, result, &output)
		resultText := toolResultText(result)
		leaked := strings.Contains(resultText, networkDriveTestPassword) ||
			strings.Contains(resultText, networkDriveSecondTestPassword)
		return output.Stdout, output.InjectedNetworkDrives, leaked
	}

	stdout, injected, leaked := call("Get-ChildItem S:\\datasets")
	if leaked {
		t.Fatal("first tool result leaked a mount password")
	}
	wantFirst := "user-command-output\n" +
		"共享 datasets 的网络盘符 S: 未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n"
	if stdout != wantFirst {
		t.Fatalf("first stdout = %q, want %q", stdout, wantFirst)
	}
	if len(injected) != 1 || injected[0].Share != "reports" || injected[0].Drive != "T" {
		t.Fatalf("first injected_network_drives = %#v, want only the other share", injected)
	}

	stdout, injected, leaked = call("Get-ChildItem T:\\reports")
	if leaked {
		t.Fatal("second tool result leaked a mount password")
	}
	wantSecond := "user-command-output\n共享 datasets 因认证失败已暂停注入\n"
	if stdout != wantSecond {
		t.Fatalf("paused stdout = %q, want %q", stdout, wantSecond)
	}
	if len(injected) != 1 || injected[0].Share != "reports" || injected[0].Drive != "T" {
		t.Fatalf("paused injected_network_drives = %#v, want only the other share", injected)
	}

	commands, _ := fixture.fake.recorded()
	if len(commands) != 4 {
		t.Fatalf("remote commands = %#v, want two first-command probes and two wrapped commands", commands)
	}
	wrappedCommands := make([]string, 0, 2)
	for _, command := range commands {
		if strings.Contains(command, "New-SmbMapping") {
			wrappedCommands = append(wrappedCommands, command)
		}
	}
	if len(wrappedCommands) != 2 {
		t.Fatalf("wrapped commands = %#v", wrappedCommands)
	}
	if !strings.Contains(wrappedCommands[0], `AddArgument('\\192.0.2.10\datasets')`) {
		t.Fatalf("first wrapped command omitted the failed share before it was paused:\n%s", wrappedCommands[0])
	}
	if strings.Contains(wrappedCommands[1], `AddArgument('\\192.0.2.10\datasets')`) {
		t.Fatalf("paused share was mapped again:\n%s", wrappedCommands[1])
	}
	if !strings.Contains(wrappedCommands[1], `AddArgument('\\192.0.2.10\reports')`) {
		t.Fatalf("other share was not mapped after authentication failure:\n%s", wrappedCommands[1])
	}
}

// TestNetworkDriveInjectionAuthenticationPauseCoversAllServers 钉住暂停范
// 围：同一条共享的旧密码不能换另一台 Windows 服务器继续尝试。
func TestNetworkDriveInjectionAuthenticationPauseCoversAllServers(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.secondServer = true
		setup.mapStatus = "error-1326"
	})
	call := func(serverID string) string {
		t.Helper()
		result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "exec_command",
			Arguments: map[string]any{
				"server_id": serverID, "command": "Get-ChildItem S:\\datasets",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("exec_command failed: %s", toolResultText(result))
		}
		var output struct {
			Stdout string `json:"stdout"`
		}
		unmarshalToolJSON(t, result, &output)
		return output.Stdout
	}

	first := call(fixture.serverID)
	wantFirst := "user-command-output\n" +
		"共享 datasets 的网络盘符 S: 未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n"
	if first != wantFirst {
		t.Fatalf("first server stdout = %q, want %q", first, wantFirst)
	}
	second := call(fixture.secondServerID)
	wantSecond := "plain-command-output\n共享 datasets 因认证失败已暂停注入\n"
	if second != wantSecond {
		t.Fatalf("second server stdout = %q, want %q", second, wantSecond)
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 3 {
		t.Fatalf("remote commands = %#v, want one probe and one mapping on the first server only", commands)
	}
	if strings.Contains(commands[2], "New-SmbMapping") || strings.Contains(commands[2], "probe-one") {
		t.Fatalf("second server received the paused share mapping:\n%s", commands[2])
	}
	if len(inputs[2]) != 0 {
		t.Fatalf("second server standard input = %q, want empty", inputs[2])
	}
}

// TestNetworkDriveInjectionAuthenticationPauseRecoversAfterVersionChange 钉住
// 自动恢复：共享存储版本号变化后，同一进程不再沿用旧的暂停记录。
func TestNetworkDriveInjectionAuthenticationPauseRecoversAfterVersionChange(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.mapStatus = "error-86"
	})
	call := func() (string, []struct {
		Share string `json:"share"`
		Drive string `json:"drive"`
	}) {
		t.Helper()
		result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "exec_command",
			Arguments: map[string]any{
				"server_id": fixture.serverID, "command": "Get-ChildItem S:\\datasets",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("exec_command failed: %+v", result.Content)
		}
		var output struct {
			Stdout                string `json:"stdout"`
			InjectedNetworkDrives []struct {
				Share string `json:"share"`
				Drive string `json:"drive"`
			} `json:"injected_network_drives"`
		}
		unmarshalToolJSON(t, result, &output)
		return output.Stdout, output.InjectedNetworkDrives
	}

	failureOutput, injected := call()
	if len(injected) != 0 || !strings.Contains(failureOutput, networkDriveReasonAuthentication) {
		t.Fatalf("first output = %q, injected = %#v, want an authentication warning", failureOutput, injected)
	}
	pausedOutput, injected := call()
	if len(injected) != 0 || pausedOutput != "plain-command-output\n共享 datasets 因认证失败已暂停注入\n" {
		t.Fatalf("paused output = %q, injected = %#v", pausedOutput, injected)
	}

	note := "共享登记已更新，暂停应自动解除"
	edited, err := fixture.store.EditSharedStorage("datasets", store.SharedStorageChanges{
		Note: &note,
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if edited.Version == 1 {
		t.Fatal("editing the shared storage did not bump its version")
	}
	fixture.fake.setStatusesByDrive(map[string]string{"S": "ok"})

	recoveredOutput, injected := call()
	if recoveredOutput != "user-command-output\n" {
		t.Fatalf("recovered output = %q, want only the user command output", recoveredOutput)
	}
	if len(injected) != 1 || injected[0].Share != "datasets" || injected[0].Drive != "S" {
		t.Fatalf("recovered injected_network_drives = %#v", injected)
	}
}

// TestNetworkDriveInjectionNoCommonNetworkWarnsWithoutProbe 钉住拓扑缺口
// 呈现：目标设备与提供方设备没有共同网络时不探测、不盲试全部地址，警告
// 说明原因并列出双方各自所在的网络名。
func TestNetworkDriveInjectionNoCommonNetworkWarnsWithoutProbe(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.disjointNetworks = true
	})

	stdout := execNetworkDriveCommand(t, fixture, "Get-Location")
	wantParts := []string{
		"共享 datasets 的网络盘符 S: 未注入：目标服务器所属设备与共享存储提供方设备没有共同网络，未探测",
		"共享存储提供方设备所在网络：办公物理局域网、办公虚拟局域网",
		"目标服务器所属设备所在网络：办公管理网络",
	}
	for _, want := range wantParts {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q: %q", want, stdout)
		}
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10", "192.0.2.30"} {
		if strings.Contains(stdout, address) {
			t.Fatalf("warning leaked address %s: %q", address, stdout)
		}
	}
	commands, inputs := fixture.fake.recorded()
	if len(commands) != 1 || strings.Contains(commands[0], "probe-one") ||
		strings.Contains(commands[0], "New-SmbMapping") {
		t.Fatalf("disjoint topology must not probe or inject: %#v", commands)
	}
	if len(inputs[0]) != 0 {
		t.Fatalf("standard input = %q, want empty", inputs[0])
	}
}

// TestNetworkDriveInjectionNoDeviceWarnsWithoutProbe 钉住服务器没有关联设
// 备时的呈现：不探测，警告列出提供方网络名，并说明目标侧没有拓扑登记。
func TestNetworkDriveInjectionNoDeviceWarnsWithoutProbe(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.noDevice = true
	})

	stdout := execNetworkDriveCommand(t, fixture, "Get-Location")
	wantParts := []string{
		"共享 datasets 的网络盘符 S: 未注入：目标服务器未关联拓扑设备，未探测",
		"共享存储提供方设备所在网络：办公物理局域网、办公虚拟局域网",
		"目标服务器所属设备所在网络：（无）",
	}
	for _, want := range wantParts {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q: %q", want, stdout)
		}
	}
	commands, inputs := fixture.fake.recorded()
	if len(commands) != 1 || strings.Contains(commands[0], "probe-one") ||
		strings.Contains(commands[0], "New-SmbMapping") {
		t.Fatalf("server without a device must not probe or inject: %#v", commands)
	}
	if len(inputs[0]) != 0 {
		t.Fatalf("standard input = %q, want empty", inputs[0])
	}
}

// TestNetworkDriveInjectionAllCandidatesUnreachableWarnsSafely 钉住全部候选
// 不通时的呈现：命令照常执行，警告沿用 mount_share 的逐地址格式，只含网
// 络名、登记序号、错误分类与耗时，不含 IP 或域名。
func TestNetworkDriveInjectionAllCandidatesUnreachableWarnsSafely(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.reachable = map[string]bool{"192.0.2.10": false, "100.64.10.10": false}
	})

	stdout := execNetworkDriveCommand(t, fixture, "Get-Location")
	wantParts := []string{
		"全部候选地址在目标服务器上探测失败，登记与实际不一致：",
		"办公物理局域网 第1个地址  ✗无路由 2ms",
		"办公虚拟局域网 第1个地址  ✗无路由 2ms",
	}
	for _, want := range wantParts {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q: %q", want, stdout)
		}
	}
	if !strings.HasPrefix(stdout, "plain-command-output\n") {
		t.Fatalf("unreachable candidates blocked the user command: %q", stdout)
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10", "192.0.2.20", "100.64.10.20"} {
		if strings.Contains(stdout, address) {
			t.Fatalf("warning leaked address %s: %q", address, stdout)
		}
	}
	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 1 {
		t.Fatalf("probe commands = %d, want 1: %#v", probes, commands)
	}
	if strings.Contains(commands[1], "New-SmbMapping") {
		t.Fatalf("all-unreachable selection still injected: %q", commands[1])
	}
}

// TestNetworkDriveInjectionUsesVirtualNetworkWithNote 钉住降级呈现：物理局
// 域网候选探测全部不通、改走虚拟局域网时注入照常成功，输出末尾说明走了
// 虚拟局域网、性能可能下降。
func TestNetworkDriveInjectionUsesVirtualNetworkWithNote(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.reachable = map[string]bool{"192.0.2.10": false, "100.64.10.10": true}
	})

	stdout := execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\")
	want := "user-command-output\n共享 datasets 的网络盘符 S: 已使用虚拟局域网：办公虚拟局域网 第1个地址，性能可能下降；物理局域网地址不通：办公物理局域网 第1个地址  ✗无路由 2ms\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	commands, _ := fixture.fake.recorded()
	if !strings.Contains(commands[1], `AddArgument('\\100.64.10.10\datasets')`) {
		t.Fatalf("wrapped command did not use the virtual network address: %q", commands[1])
	}
	if strings.Contains(commands[1], `AddArgument('\\192.0.2.10`) {
		t.Fatalf("wrapped command used the unreachable physical address: %q", commands[1])
	}
}

// TestNetworkDriveInjectionConcurrentCommandsStaySafe 钉住并发安全：多条
// MCP 调用同时执行时共享同一份选址缓存，不破坏结果、不泄漏密码；探测至
// 少发生一次，至多每条命令一次（本票不要求并发合并成一次探测）。
func TestNetworkDriveInjectionConcurrentCommandsStaySafe(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, nil)

	const callers = 6
	var waitGroup sync.WaitGroup
	outputs := make([]string, callers)
	errors := make([]error, callers)
	for index := 0; index < callers; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			outputs[index], errors[index] = func() (string, error) {
				result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
					Name:      "exec_command",
					Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-ChildItem S:\\"},
				})
				if err != nil {
					return "", err
				}
				if result.IsError {
					return "", fmt.Errorf("exec_command failed: %v", result.Content)
				}
				var output struct {
					Stdout string `json:"stdout"`
				}
				unmarshalToolJSON(t, result, &output)
				if strings.Contains(toolResultText(result), networkDriveTestPassword) {
					return "", fmt.Errorf("tool result leaked the mount password")
				}
				return output.Stdout, nil
			}()
		}(index)
	}
	waitGroup.Wait()
	for index, err := range errors {
		if err != nil {
			t.Fatalf("caller %d failed: %v", index, err)
		}
		if outputs[index] != "user-command-output\n" {
			t.Fatalf("caller %d stdout = %q", index, outputs[index])
		}
	}
	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes < 1 || probes > callers {
		t.Fatalf("probe commands = %d, want between 1 and %d", probes, callers)
	}
	for _, command := range commands {
		if strings.Contains(command, networkDriveTestPassword) {
			t.Fatalf("remote command leaked the mount password: %q", command)
		}
	}
}
