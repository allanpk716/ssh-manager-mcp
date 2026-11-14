package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// fakePowerShellDriveServer 模拟 Windows 目标机：按固定探测命令的输出协议
// 回报 445 探测结果，按 PowerShell 命令文本或 cmd 脚本帧里带随机标记的状
// 态行格式回报映射结果，再输出用户命令的结果。它记录收到的每条命令与标
// 准输入。
type fakePowerShellDriveServer struct {
	mu sync.Mutex

	commands       []string
	standardInputs [][]byte
	reachable      map[string]bool
	// streamExec 非空时用实时输出回调模拟映射后仍长时间运行的任务，让测
	// 试能在任务未结束时读取中间输出。
	streamExec func(command string, stdin io.Reader, stdout, stderr io.Writer) int
	// mapStatus 是第 0 个共享映射状态行里回传的结果；mapStatuses 按计划
	// 中的共享序号区分多个共享。结果可为 ok、error-<编号> 或 timeout。
	mapStatus       string
	mapStatuses     map[int]string
	statusesByDrive map[string]string
	commandDelay    time.Duration
}

var driveProbeInvocationPattern = regexp.MustCompile(`probe-one (\d+) (\S+) 445 (\d+)`)
var driveStatusMarkerPattern = regexp.MustCompile(`SSHMGR-SMBMAP:([0-9a-f]+):`)
var driveEncodedStatusPattern = regexp.MustCompile(`SSHMGR-SMBMAP:([0-9a-f]+):(\d+):`)
var driveArgumentPattern = regexp.MustCompile(`AddArgument\('([A-Z]):'\)`)
var encodedCommandPattern = regexp.MustCompile(`powershell -NoProfile -NonInteractive -EncodedCommand ([A-Za-z0-9+/=]+)`)

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
	powerShellScripts := encodedCommandScripts(command)
	if strings.Contains(command, "-EncodedCommand ") {
		bootstrapAndScripts := append([]string(nil), powerShellScripts...)
		if injectionScript, _, ok := splitWindowsCmdInjectionFrames(stdinBytes); ok {
			bootstrapAndScripts = append(bootstrapAndScripts, injectionScript)
			powerShellScripts = bootstrapAndScripts
		}
		decodedCommand = strings.Join(bootstrapAndScripts, "\n")
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
		if f.commandDelay > 0 {
			time.Sleep(f.commandDelay)
		}
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
		if f.commandDelay > 0 {
			time.Sleep(f.commandDelay)
		}
		stdout := strings.Builder{}
		for _, script := range powerShellScripts {
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

func (f *fakePowerShellDriveServer) streamHandler(command string, stdin io.Reader, stdout, stderr io.Writer) int {
	if f.streamExec == nil {
		return 0
	}
	return f.streamExec(command, stdin, stdout, stderr)
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
	var scripts []string
	for _, match := range encodedCommandPattern.FindAllStringSubmatch(command, -1) {
		encoded := match[1]
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

// splitWindowsCmdInjectionFrames 解析 cmd 注入专用标准输入：前 8 字节是十
// 进制脚本长度，随后是 UTF-8 注入脚本，最后是环境变量值与挂载密码。
func splitWindowsCmdInjectionFrames(frames []byte) (string, []byte, bool) {
	if len(frames) < 8 {
		return "", nil, false
	}
	length, err := strconv.Atoi(string(frames[:8]))
	if err != nil || length < 0 || 8+length > len(frames) {
		return "", nil, false
	}
	return string(frames[8 : 8+length]), frames[8+length:], true
}

type networkDriveFixture struct {
	clientSession  *mcp.ClientSession
	serverID       string
	secondServerID string
	profileID      string
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

// TestNetworkDriveInjectionSkipsSilentlyWhenAuthorityTooOld 覆盖新客户端连
// 旧权威端：即使缓存里确有共享存储，执行命令也照常走兜底地址，不探测、
// 不注入、不追加任何警告；执行上下文只在盘符预览里说明原因。
func TestNetworkDriveInjectionSkipsSilentlyWhenAuthorityTooOld(t *testing.T) {
	fixture := seedNetworkDriveVault(t, nil)
	_, projectToken, err := fixture.store.AddProject("旧权威端项目", fixture.profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.ExportSnapshotForProfile(fixture.profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.RecommendedVersion = ""
	broker, tunnels, tasks, cleanup, err := NewCacheBroker(
		projectToken, snapshot, filepath.Join(t.TempDir(), "audit.log"),
		nil, nil, "laptop", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	session := connectListSharesClient(t, broker)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "Write-Output ran"},
	})
	if err != nil || result.IsError {
		t.Fatalf("旧权威端下 exec_command 失败：err=%v result=%+v", err, result)
	}
	var output struct {
		Stdout                string                 `json:"stdout"`
		InjectedNetworkDrives []InjectedNetworkDrive `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "plain-command-output\n" || len(output.InjectedNetworkDrives) != 0 {
		t.Fatalf("旧权威端下 exec_command 应静默跳过网络盘符：%q %#v", output.Stdout, output.InjectedNetworkDrives)
	}

	contextResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_context", Arguments: map[string]any{"server_id": fixture.serverID},
	})
	if err != nil || contextResult.IsError {
		t.Fatalf("旧权威端下 exec_context 失败：err=%v result=%+v", err, contextResult)
	}
	var contextOutput struct {
		UninjectedNetworkDrives []UninjectedNetworkDrive `json:"uninjected_network_drives"`
		NetworkDrivePreviewNote string                   `json:"network_drive_preview_note"`
	}
	unmarshalToolJSON(t, contextResult, &contextOutput)
	wantNote := "权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端"
	if len(contextOutput.UninjectedNetworkDrives) != 0 || contextOutput.NetworkDrivePreviewNote != wantNote {
		t.Fatalf("旧权威端盘符预览 = %#v，说明 = %q", contextOutput.UninjectedNetworkDrives, contextOutput.NetworkDrivePreviewNote)
	}

	backgroundResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "Write-Output background", "timeout_seconds": 10},
	})
	if err != nil || backgroundResult.IsError {
		t.Fatalf("旧权威端下 exec_background 失败：err=%v result=%+v", err, backgroundResult)
	}
	var start struct {
		TaskID            string   `json:"task_id"`
		InjectionWarnings []string `json:"injection_warnings"`
	}
	unmarshalToolJSON(t, backgroundResult, &start)
	finalBackground := waitBackgroundTerminal(t, session, start.TaskID)
	if finalBackground.Stdout != "plain-command-output\n" || len(start.InjectionWarnings) != 0 {
		t.Fatalf("旧权威端下 exec_background 应静默跳过网络盘符：start=%+v final=%+v", start, finalBackground)
	}

	commands, _ := fixture.fake.recorded()
	if len(commands) != 2 || countNetworkDriveProbeCommands(commands) != 0 {
		t.Fatalf("旧权威端不应探测或注入网络盘符：%#v", commands)
	}
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
	want := "user-command-output\n共享 datasets 的网络盘符 S：已使用虚拟局域网：办公虚拟局域网 第1个地址，性能可能下降；物理局域网地址不通：办公物理局域网 第1个地址（网络类映射失败）\n"
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
	if !strings.Contains(stdout, "共享 datasets 的网络盘符 S：未注入") {
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
	if strings.Contains(commands[4], "WNetAddConnection2") {
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
		streamExec:      settings.streamExec,
		commandDelay:    settings.commandDelay,
	}
	st := newStore(t)
	secondServerID := ""
	serverOptions := testsshd.Options{Password: "pw"}
	if settings.sudoTarget {
		serverOptions.SudoPassword = "sudopw"
	}
	if fake.streamExec != nil {
		serverOptions.ExecStream = fake.streamHandler
	} else {
		serverOptions.Exec = fake.handler
	}
	addr, hostKey, cleanup := testsshd.Start(t, serverOptions)
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
	providerPhysicalAddress := "192.0.2.10"
	if settings.unsafeProviderAddress {
		providerPhysicalAddress = `bad"address`
	}
	for _, membership := range []struct{ network, address string }{
		{"办公物理局域网", providerPhysicalAddress},
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
	if settings.sudoTarget {
		sudoCredentialID, err := st.SetCredential(&models.Credential{
			Type: models.CredPassword, Secret: []byte("sudopw"),
		})
		if err != nil {
			t.Fatal(err)
		}
		serverRow, err := st.GetServer(added.ServerID)
		if err != nil || serverRow == nil {
			t.Fatalf("read server for sudo credential: %v %v", serverRow, err)
		}
		serverRow.SudoCredentialID = sudoCredentialID
		if err := st.UpdateServer(serverRow); err != nil {
			t.Fatal(err)
		}
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
	mountUser := "mount-user"
	if settings.unsafeMountUser {
		mountUser = `mount"user`
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: mountUser,
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
		Regex:       `WNetAddConnection2|probe-one|SSHMGR-SMBMAP`,
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
		profileID:      profileID, store: st, fake: fake,
	}
}

type networkDriveSeedSetup struct {
	reachable              map[string]bool
	mapStatus              string
	mapStatuses            map[int]string
	statusesByDrive        map[string]string
	streamExec             func(command string, stdin io.Reader, stdout, stderr io.Writer) int
	notWritable            bool
	posixTarget            bool
	cmdTarget              bool
	secondShare            bool
	secondServer           bool
	invalidDatasetPassword bool
	environmentSecret      bool
	unsafeMountUser        bool
	unsafeProviderAddress  bool
	commandDelay           time.Duration
	disjointNetworks       bool
	noDevice               bool
	sudoTarget             bool
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
		"WNetAddConnection2",
		`AddArgument('\\192.0.2.10\datasets')`,
		"AddArgument('mount-user')",
		"AddArgument('S:')",
		"[SshmgrNative.Mpr]::WNetAddConnection2($resource, $d, $c, 0)",
		userCommand,
	} {
		if !strings.Contains(wrapped, want) {
			t.Fatalf("wrapped command missing %q:\n%s", want, wrapped)
		}
	}
	if strings.Contains(wrapped, `AddArgument('\\100.64.10.10`) {
		t.Fatalf("physical network address was not preferred: %q", wrapped)
	}
	if strings.Contains(wrapped, "New-SmbMapping") {
		t.Fatalf("wrapped command still uses New-SmbMapping:\n%s", wrapped)
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

// TestNetworkDriveInjectionTimeoutExcludesInjectionBudget 钉住前台与后台
// 的 SSH 会话超时：返回的生效秒数就是传给 SSH 会话的秒数，选址探测另用
// 固定超时，注入步骤不得再给用户命令超时追加时间。
func TestNetworkDriveInjectionTimeoutExcludesInjectionBudget(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "100ms")
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.commandDelay = 1200 * time.Millisecond
	})

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command",
		Arguments: map[string]any{
			"server_id": fixture.serverID, "command": "Get-ChildItem S:\\datasets",
			"timeout_seconds": 1,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_command failed: err=%v result=%+v", err, result)
	}
	var foreground struct {
		TimedOut                bool `json:"timed_out"`
		EffectiveTimeoutSeconds int  `json:"effective_timeout_seconds"`
	}
	unmarshalToolJSON(t, result, &foreground)
	if foreground.EffectiveTimeoutSeconds != 1 || !foreground.TimedOut {
		t.Fatalf("foreground timeout output = %+v, want effective timeout 1 and timed_out=true", foreground)
	}

	backgroundResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_background",
		Arguments: map[string]any{
			"server_id": fixture.serverID, "command": "Get-ChildItem S:\\datasets",
			"timeout_seconds": 1,
		},
	})
	if err != nil || backgroundResult.IsError {
		t.Fatalf("exec_background failed: err=%v result=%+v", err, backgroundResult)
	}
	var start BgStartOutput
	unmarshalToolJSON(t, backgroundResult, &start)
	if start.EffectiveTimeoutSeconds != 1 {
		t.Fatalf("background effective timeout = %d, want 1", start.EffectiveTimeoutSeconds)
	}
	final := waitBackgroundTerminal(t, fixture.clientSession, start.TaskID)
	if final.Status != bgStatusTimeout {
		t.Fatalf("background status = %q, want timeout", final.Status)
	}
}

// TestNetworkDriveInjectionTotalBudgetSkippedShare 钉住远端 skipped 状态：
// 注入步骤总时长达到上限时只警告、不映射；它不是网络失败，也不触发认证
// 失败暂停，下一条命令仍会发送密码重试。
func TestNetworkDriveInjectionTotalBudgetSkippedShare(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.statusesByDrive = map[string]string{"S": "skipped"}
	})

	stdout := execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\datasets")
	want := "user-command-output\n" +
		"共享 datasets 的网络盘符 S：未注入：注入步骤总时长达到上限，未映射\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	stdout = execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\datasets")
	if !strings.Contains(stdout, "共享 datasets 的网络盘符 S：未注入：注入步骤总时长达到上限，未映射") {
		t.Fatalf("second command did not retry after skipped status: %q", stdout)
	}
	commands, inputs := fixture.fake.recorded()
	probeCount := countNetworkDriveProbeCommands(commands)
	mapCount := 0
	for _, command := range commands {
		if strings.Contains(command, "WNetAddConnection2") {
			mapCount++
		}
	}
	if probeCount != 1 || mapCount != 2 {
		t.Fatalf("probe count=%d map count=%d; skipped must keep the address cache and retry the share", probeCount, mapCount)
	}
	if len(inputs) < 2 || !strings.Contains(string(inputs[len(inputs)-1]), networkDriveTestPassword) {
		t.Fatal("second command did not send the mount password after skipped status")
	}
}

// TestNetworkDriveInjectionBackgroundStreamingRemovesStatusLines 覆盖后台注
// 入与最关键的安全属性：状态行是流式输出的，任务仍在运行、agent 已经能读
// 到中间输出时，内部状态行标记也绝不能进入任务日志或 exec_output。
func TestNetworkDriveInjectionBackgroundStreamingRemovesStatusLines(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var commandMu sync.Mutex
	var commands []string
	var standardInputs [][]byte
	runningOutput := strings.Repeat("running-output-", 32)

	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.streamExec = func(command string, stdin io.Reader, stdout, stderr io.Writer) int {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				_, _ = stderr.Write([]byte("read standard input: " + err.Error()))
				return 1
			}
			commandMu.Lock()
			commands = append(commands, command)
			standardInputs = append(standardInputs, append([]byte(nil), stdinBytes...))
			commandMu.Unlock()

			if strings.Contains(command, "probe-one ") {
				for _, match := range driveProbeInvocationPattern.FindAllStringSubmatch(command, -1) {
					_, _ = fmt.Fprintf(stdout, "SSHMGR-PROBE %s ok 2 \n", match[1])
				}
				return 0
			}
			markerMatch := driveStatusMarkerPattern.FindStringSubmatch(command)
			if markerMatch == nil {
				_, _ = stdout.Write([]byte("unexpected command\n"))
				return 1
			}
			_, _ = fmt.Fprintf(stdout, "SSHMGR-SMBMAP:%s:0:ok\n", markerMatch[1])
			_, _ = stdout.Write([]byte(runningOutput + "\n"))
			close(started)
			<-release
			_, _ = stdout.Write([]byte("final-output\n"))
			return 0
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-ChildItem S:\\datasets", "timeout_seconds": 10},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_background failed: err=%v result=%+v", err, result.Content)
	}
	var start struct {
		TaskID            string   `json:"task_id"`
		Status            string   `json:"status"`
		InjectionWarnings []string `json:"injection_warnings"`
	}
	unmarshalToolJSON(t, result, &start)
	if start.TaskID == "" || start.Status != bgStatusRunning || len(start.InjectionWarnings) != 0 {
		t.Fatalf("exec_background output = %+v", start)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("background command did not start")
	}
	var intermediate BgReadOutput
	intermediate.Status = bgStatusRunning
	deadline := time.Now().Add(5 * time.Second)
	for intermediate.Status == bgStatusRunning && time.Now().Before(deadline) {
		intermediate = readBackgroundOutput(t, fixture.clientSession, start.TaskID, 0, 0, "text")
		if strings.Contains(intermediate.Stdout, "running-output") {
			break
		}
	}
	if !strings.Contains(intermediate.Stdout, "running-output-") {
		t.Fatalf("running output was not visible before task completion: %+v", intermediate)
	}
	if strings.Contains(intermediate.Stdout, networkDriveStatusPrefix) ||
		strings.Contains(toolResultText(result), networkDriveStatusPrefix) {
		t.Fatalf("running output leaked a status line marker: %q", intermediate.Stdout)
	}
	if strings.Contains(intermediate.Stdout, networkDriveTestPassword) {
		t.Fatalf("running output leaked the mount password: %q", intermediate.Stdout)
	}

	close(release)
	final := waitBackgroundTerminal(t, fixture.clientSession, start.TaskID)
	if final.Status != bgStatusDone {
		t.Fatalf("background status = %q, want done", final.Status)
	}
	wantOutput := runningOutput + "\nfinal-output\n"
	if final.Stdout != wantOutput {
		t.Fatalf("background stdout = %q", final.Stdout)
	}
	if strings.Contains(final.Stdout, networkDriveStatusPrefix) {
		t.Fatalf("background stdout leaked a status line marker: %q", final.Stdout)
	}

	commandMu.Lock()
	gotCommands, gotInputs := append([]string(nil), commands...), append([][]byte(nil), standardInputs...)
	commandMu.Unlock()
	if len(gotCommands) != 2 || !strings.Contains(gotCommands[0], "probe-one ") ||
		!strings.Contains(gotCommands[1], "WNetAddConnection2") {
		t.Fatalf("background commands = %#v, want one probe and one wrapped command", gotCommands)
	}
	if len(gotInputs[1]) != len(networkDriveTestPassword) || string(gotInputs[1]) != networkDriveTestPassword {
		t.Fatalf("background standard input = %q, want only the mount password", gotInputs[1])
	}
	for _, command := range gotCommands {
		if strings.Contains(command, networkDriveTestPassword) {
			t.Fatalf("background command leaked the mount password: %q", command)
		}
	}

	endRows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec-bg-end"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(endRows) != 1 ||
		len(endRows[0].InjectedShareNames) != 1 || endRows[0].InjectedShareNames[0] != "datasets" {
		t.Fatalf("background end audit injected share names = %#v", endRows)
	}
}

// TestNetworkDriveInjectionContextPreviewDoesNotProbe 覆盖执行前预览：未缓
// 存地址只说明执行时探测，预览本身不触发 445 探测，也不改动进程内缓存。
func TestNetworkDriveInjectionContextPreviewDoesNotProbe(t *testing.T) {
	fixture := seedNetworkDriveVault(t, nil)

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_context",
		Arguments: map[string]any{"server_id": fixture.serverID},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("exec_context failed: err=%v result=%+v", err, result)
	}
	var output struct {
		PlannedNetworkDrives []struct {
			Share       string `json:"share"`
			Drive       string `json:"drive"`
			AddressPlan string `json:"address_plan"`
		} `json:"planned_network_drives"`
		UninjectedNetworkDrives []struct {
			Share  string `json:"share"`
			Drive  string `json:"drive"`
			Reason string `json:"reason"`
		} `json:"uninjected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if len(output.PlannedNetworkDrives) != 1 ||
		output.PlannedNetworkDrives[0].Share != "datasets" ||
		output.PlannedNetworkDrives[0].Drive != "S" ||
		output.PlannedNetworkDrives[0].AddressPlan != "执行时探测" {
		t.Fatalf("planned network drives = %#v", output.PlannedNetworkDrives)
	}
	if len(output.UninjectedNetworkDrives) != 0 {
		t.Fatalf("uninjected network drives = %#v", output.UninjectedNetworkDrives)
	}
	if strings.Contains(toolResultText(result), networkDriveTestPassword) ||
		strings.Contains(toolResultText(result), "192.0.2.10") {
		t.Fatal("exec_context preview leaked the mount password or address")
	}

	commands, _ := fixture.fake.recorded()
	if len(commands) != 0 {
		t.Fatalf("Windows 盘符预览不应下发任何命令：%#v", commands)
	}

	execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\datasets")
	commands, _ = fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 1 {
		t.Fatalf("probe commands after preview and execution = %d, want 1: %#v", probes, commands)
	}

	result, err = fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_context",
		Arguments: map[string]any{"server_id": fixture.serverID},
	})
	if err != nil || result.IsError {
		t.Fatalf("second exec_context failed: err=%v result=%+v", err, result.Content)
	}
	unmarshalToolJSON(t, result, &output)
	if len(output.PlannedNetworkDrives) != 1 ||
		output.PlannedNetworkDrives[0].AddressPlan != "已缓存地址" {
		t.Fatalf("cached planned network drives = %#v", output.PlannedNetworkDrives)
	}
}

// TestNetworkDriveInjectionContextPreviewReasons 覆盖执行前预览里的未注入
// 原因：认证失败暂停、读不到合法密码、没有共同网络。
func TestNetworkDriveInjectionContextPreviewReasons(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*networkDriveSeedSetup)
		wantShare string
		wantDrive string
		wantTexts []string
	}{
		{
			name:      "invalid password",
			setup:     func(setup *networkDriveSeedSetup) { setup.invalidDatasetPassword = true },
			wantShare: "datasets",
			wantDrive: "S",
			wantTexts: []string{"挂载密码在 Windows 上不是合法 UTF-8"},
		},
		{
			name:      "no common network",
			setup:     func(setup *networkDriveSeedSetup) { setup.disjointNetworks = true },
			wantShare: "datasets",
			wantDrive: "S",
			wantTexts: []string{"没有共同网络", "未探测"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedNetworkDriveVault(t, test.setup)
			result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "exec_context",
				Arguments: map[string]any{"server_id": fixture.serverID},
			})
			if err != nil || result.IsError {
				t.Fatalf("exec_context failed: err=%v result=%+v", err, result)
			}
			var output struct {
				PlannedNetworkDrives    []PlannedNetworkDrive    `json:"planned_network_drives"`
				UninjectedNetworkDrives []UninjectedNetworkDrive `json:"uninjected_network_drives"`
			}
			unmarshalToolJSON(t, result, &output)
			if len(output.PlannedNetworkDrives) != 0 {
				t.Fatalf("planned drives = %#v, want none", output.PlannedNetworkDrives)
			}
			if len(output.UninjectedNetworkDrives) != 1 ||
				output.UninjectedNetworkDrives[0].Share != test.wantShare ||
				output.UninjectedNetworkDrives[0].Drive != test.wantDrive {
				t.Fatalf("uninjected drives = %#v", output.UninjectedNetworkDrives)
			}
			reason := output.UninjectedNetworkDrives[0].Reason
			for _, want := range test.wantTexts {
				if !strings.Contains(reason, want) {
					t.Fatalf("reason %q missing %q", reason, want)
				}
			}
		})
	}
}

// TestNetworkDriveInjectionContextPreviewKeepsAuthenticationPause 证明预览只
// 读暂停状态：多次 exec_context 既显示暂停，也不清理它，后续执行仍不注入。
func TestNetworkDriveInjectionContextPreviewKeepsAuthenticationPause(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.mapStatus = "error-1326"
	})
	stdout := execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\datasets")
	if !strings.Contains(stdout, networkDriveReasonAuthentication) {
		t.Fatalf("first command did not report authentication failure: %q", stdout)
	}

	for range 2 {
		result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "exec_context",
			Arguments: map[string]any{"server_id": fixture.serverID},
		})
		if err != nil || result.IsError {
			t.Fatalf("exec_context failed: err=%v result=%+v", err, result)
		}
		var output struct {
			UninjectedNetworkDrives []UninjectedNetworkDrive `json:"uninjected_network_drives"`
		}
		unmarshalToolJSON(t, result, &output)
		if len(output.UninjectedNetworkDrives) != 1 ||
			output.UninjectedNetworkDrives[0].Share != "datasets" ||
			output.UninjectedNetworkDrives[0].Reason != networkDriveReasonPausedPreview {
			t.Fatalf("paused preview = %#v", output.UninjectedNetworkDrives)
		}
	}

	stdout = execNetworkDriveCommand(t, fixture, "Get-ChildItem S:\\datasets")
	if !strings.Contains(stdout, "共享 datasets 因认证失败已暂停注入") {
		t.Fatalf("pause was cleared by preview: %q", stdout)
	}
	commands, _ := fixture.fake.recorded()
	if probes := countNetworkDriveProbeCommands(commands); probes != 1 {
		t.Fatalf("probe commands = %d, want only the initial probe: %#v", probes, commands)
	}
}

// TestNetworkDriveInjectionWindowsSudoDoesNotInject 覆盖前台、后台与执行上
// 下文预览：Windows 提权层不注入网络盘符，并给出固定说明。
func TestNetworkDriveInjectionWindowsSudoDoesNotInject(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.sudoTarget = true
	})

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "Write-Output ran", "sudo": true},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_command(sudo) failed: err=%v result=%+v", err, result)
	}
	var output struct {
		Stdout                string                 `json:"stdout"`
		InjectedNetworkDrives []InjectedNetworkDrive `json:"injected_network_drives"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "plain-command-output\n"+networkDriveReasonPrivileged+"\n" {
		t.Fatalf("exec_command(sudo) stdout = %q", output.Stdout)
	}
	if len(output.InjectedNetworkDrives) != 0 {
		t.Fatalf("exec_command(sudo) injected drives = %#v", output.InjectedNetworkDrives)
	}

	contextResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_context",
		Arguments: map[string]any{"server_id": fixture.serverID, "sudo": true},
	})
	if err != nil || contextResult.IsError {
		t.Fatalf("exec_context(sudo) failed: err=%v result=%+v", err, contextResult)
	}
	var contextOutput struct {
		UninjectedNetworkDrives []UninjectedNetworkDrive `json:"uninjected_network_drives"`
	}
	unmarshalToolJSON(t, contextResult, &contextOutput)
	if len(contextOutput.UninjectedNetworkDrives) != 1 ||
		contextOutput.UninjectedNetworkDrives[0].Reason != networkDriveReasonPrivileged {
		t.Fatalf("exec_context(sudo) uninjected drives = %#v", contextOutput.UninjectedNetworkDrives)
	}
	commands, inputs := fixture.fake.recorded()
	if len(commands) != 1 || strings.Contains(commands[0], "WNetAddConnection2") {
		t.Fatalf("sudo commands unexpectedly injected a network drive: %#v", commands)
	}
	for _, input := range inputs {
		if strings.Contains(string(input), networkDriveTestPassword) {
			t.Fatal("sudo execution sent the mount password")
		}
	}

	backgroundFixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.sudoTarget = true
	})
	startResult, err := backgroundFixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": backgroundFixture.serverID, "command": "Write-Output ran", "sudo": true, "timeout_seconds": 5},
	})
	if err != nil || startResult.IsError {
		t.Fatalf("exec_background(sudo) failed: err=%v result=%+v", err, startResult)
	}
	var start struct {
		TaskID            string   `json:"task_id"`
		InjectionWarnings []string `json:"injection_warnings"`
	}
	unmarshalToolJSON(t, startResult, &start)
	if len(start.InjectionWarnings) != 1 || start.InjectionWarnings[0] != networkDriveReasonPrivileged {
		t.Fatalf("exec_background(sudo) start warnings = %#v", start.InjectionWarnings)
	}
	final := waitBackgroundTerminal(t, backgroundFixture.clientSession, start.TaskID)
	if final.Stdout != "plain-command-output\n"+networkDriveReasonPrivileged+"\n" {
		t.Fatalf("exec_background(sudo) stdout = %q", final.Stdout)
	}
	commands, _ = backgroundFixture.fake.recorded()
	if len(commands) != 1 || strings.Contains(commands[0], "WNetAddConnection2") {
		t.Fatalf("background sudo commands unexpectedly injected a network drive: %#v", commands)
	}
}

// TestNetworkDriveInjectionBackgroundAuthenticationFailurePauses 后台的认证
// 失败与前台使用同一份进程内暂停状态：下一条后台命令不再发送挂载密码。
func TestNetworkDriveInjectionBackgroundAuthenticationFailurePauses(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.mapStatus = "error-86"
	})

	startBackground := func() BgStartOutput {
		t.Helper()
		result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "exec_background",
			Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-ChildItem S:\\datasets", "timeout_seconds": 5},
		})
		if err != nil || result.IsError {
			t.Fatalf("exec_background failed: err=%v result=%+v", err, result)
		}
		if strings.Contains(toolResultText(result), networkDriveTestPassword) {
			t.Fatal("background start result leaked the mount password")
		}
		var start BgStartOutput
		unmarshalToolJSON(t, result, &start)
		if start.TaskID == "" {
			t.Fatalf("background start output = %+v", start)
		}
		return start
	}

	first := startBackground()
	firstFinal := waitBackgroundTerminal(t, fixture.clientSession, first.TaskID)
	if !strings.Contains(firstFinal.Stdout, networkDriveReasonAuthentication) {
		t.Fatalf("first background stdout missing authentication warning: %q", firstFinal.Stdout)
	}
	second := startBackground()
	secondFinal := waitBackgroundTerminal(t, fixture.clientSession, second.TaskID)
	if !strings.Contains(secondFinal.Stdout, "共享 datasets 因认证失败已暂停注入") {
		t.Fatalf("second background stdout missing pause notice: %q", secondFinal.Stdout)
	}

	commands, inputs := fixture.fake.recorded()
	if len(commands) != 3 {
		t.Fatalf("commands = %#v, want probe/mapping then one plain paused command", commands)
	}
	if strings.Contains(commands[2], "WNetAddConnection2") || len(inputs) != 3 || len(inputs[2]) != 0 {
		t.Fatalf("paused background command still injected: command=%q standard input=%q", commands[2], inputs[2])
	}
	for _, command := range commands {
		if strings.Contains(command, networkDriveTestPassword) {
			t.Fatal("background command leaked the mount password")
		}
	}
}

// TestNetworkDriveInjectionBackgroundNetworkFailureUpdatesAddressCache 后台的
// 网络类映射失败同样作废缓存地址，下一条后台命令重新探测并改选下一候选。
func TestNetworkDriveInjectionBackgroundNetworkFailureUpdatesAddressCache(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.mapStatus = "error-64"
	})

	startBackground := func() BgStartOutput {
		t.Helper()
		result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "exec_background",
			Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-ChildItem S:\\datasets", "timeout_seconds": 5},
		})
		if err != nil || result.IsError {
			t.Fatalf("exec_background failed: err=%v result=%+v", err, result)
		}
		var start BgStartOutput
		unmarshalToolJSON(t, result, &start)
		if start.TaskID == "" {
			t.Fatalf("background start output = %+v", start)
		}
		return start
	}

	first := startBackground()
	firstFinal := waitBackgroundTerminal(t, fixture.clientSession, first.TaskID)
	if !strings.Contains(firstFinal.Stdout, "网络类失败（系统错误 64）") ||
		!strings.Contains(firstFinal.Stdout, networkDriveHintNetwork) {
		t.Fatalf("first background stdout missing network warning: %q", firstFinal.Stdout)
	}
	fixture.fake.setMapStatus("ok")
	second := startBackground()
	secondFinal := waitBackgroundTerminal(t, fixture.clientSession, second.TaskID)
	if strings.Contains(secondFinal.Stdout, "网络盘符未注入") {
		t.Fatalf("second background stdout unexpectedly warned: %q", secondFinal.Stdout)
	}

	commands, _ := fixture.fake.recorded()
	if len(commands) != 4 || countNetworkDriveProbeCommands(commands) != 2 {
		t.Fatalf("commands = %#v, want two probe and mapping pairs", commands)
	}
	if !strings.Contains(commands[1], `\\192.0.2.10\datasets`) {
		t.Fatalf("first mapping did not use the physical address: %q", commands[1])
	}
	if !strings.Contains(commands[3], `\\100.64.10.10\datasets`) {
		t.Fatalf("second mapping did not use the next address: %q", commands[3])
	}
	// 任务先对外进入终态，终态审计行随后在任务互斥锁外写入。这里等待
	// 两行都落库，避免轮询任务状态恰好抢在第二条审计写入之前断言。
	var endRows []store.AuditRow
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		endRows, err = fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec-bg-end"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(endRows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background end audit rows = %#v, want two rows", endRows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	auditByTask := map[string]store.AuditRow{}
	for _, row := range endRows {
		auditByTask[row.Command] = row
	}
	if len(endRows) != 2 || len(auditByTask[first.TaskID].InjectedShareNames) != 0 ||
		len(auditByTask[second.TaskID].InjectedShareNames) != 1 ||
		auditByTask[second.TaskID].InjectedShareNames[0] != "datasets" {
		t.Fatalf("background end audit rows = %#v", endRows)
	}
}

// TestNetworkDriveFileTransferAndForwardingDoNotInject 文件传输与端口转发走
// 独立 SSH 会话，不触发网络盘符注入或 445 探测。
func TestNetworkDriveFileTransferAndForwardingDoNotInject(t *testing.T) {
	fixture := seedNetworkDriveVault(t, nil)
	localPath := filepath.Join(t.TempDir(), "source.txt")
	remotePath := filepath.ToSlash(filepath.Join(t.TempDir(), "uploaded.txt"))
	if err := os.WriteFile(localPath, []byte("transfer-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	uploadResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "upload_file",
		Arguments: map[string]any{"server_id": fixture.serverID, "local_path": localPath, "remote_path": remotePath},
	})
	if err != nil || uploadResult.IsError {
		t.Fatalf("upload_file failed: err=%v result=%+v", err, uploadResult)
	}
	downloadResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "download_file",
		Arguments: map[string]any{"server_id": fixture.serverID, "path": remotePath},
	})
	if err != nil || downloadResult.IsError {
		t.Fatalf("download_file failed: err=%v result=%+v", err, downloadResult)
	}
	var download struct {
		Content string `json:"content"`
	}
	unmarshalToolJSON(t, downloadResult, &download)
	if download.Content != "transfer-content" {
		t.Fatalf("downloaded content = %q", download.Content)
	}

	forwardResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forward_port",
		Arguments: map[string]any{"server_id": fixture.serverID, "remote_host": "127.0.0.1", "remote_port": 22},
	})
	if err != nil || forwardResult.IsError {
		t.Fatalf("forward_port failed: err=%v result=%+v", err, forwardResult)
	}
	var forward struct {
		TunnelID string `json:"tunnel_id"`
	}
	unmarshalToolJSON(t, forwardResult, &forward)
	closeResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "close_port",
		Arguments: map[string]any{"tunnel_id": forward.TunnelID},
	})
	if err != nil || closeResult.IsError {
		t.Fatalf("close_port failed: err=%v result=%+v", err, closeResult)
	}

	relaySource := filepath.Join(t.TempDir(), "relay-source.txt")
	relayDestination := filepath.ToSlash(filepath.Join(t.TempDir(), "relay-destination.txt"))
	if err := os.WriteFile(relaySource, []byte("relay-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	relayResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_path": relaySource, "to_server_id": fixture.serverID, "to_path": relayDestination,
		},
	})
	if err != nil || relayResult.IsError {
		t.Fatalf("relay_file failed: err=%v result=%+v", err, relayResult)
	}
	var relay RelayOutput
	unmarshalToolJSON(t, relayResult, &relay)
	relayFinal := waitBackgroundTerminal(t, fixture.clientSession, relay.TaskID)
	if relayFinal.Status != bgStatusDone {
		t.Fatalf("relay task status = %q, error=%q", relayFinal.Status, relayFinal.Error)
	}

	commands, _ := fixture.fake.recorded()
	if len(commands) != 0 {
		t.Fatalf("file transfer or forwarding executed commands: %#v", commands)
	}
	if strings.Contains(toolResultText(uploadResult), networkDriveTestPassword) ||
		strings.Contains(toolResultText(downloadResult), networkDriveTestPassword) ||
		strings.Contains(toolResultText(forwardResult), networkDriveTestPassword) {
		t.Fatal("file transfer or forwarding leaked the mount password")
	}
}

// TestNetworkDriveToolDescriptionsExplainVisibility 钉住工具说明：说明网络
// 盘符只在命令会话内可见、密码不出现在目标机进程命令行，并提醒文件传输
// 会话看不到盘符；relay_file 的 Windows 本地拷贝例外，因为它自己创建注入命令会话。
func TestNetworkDriveToolDescriptionsExplainVisibility(t *testing.T) {
	fixture := seedNetworkDriveVault(t, nil)
	toolList, err := fixture.clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptions := map[string]string{}
	for _, tool := range toolList.Tools {
		descriptions[tool.Name] = tool.Description
	}
	for _, name := range []string{"exec_command", "exec_background"} {
		for _, want := range []string{
			"Windows network drives are mapped before each command in that command's SSH session",
			"the mapping password never appears in a target process command line",
			"Desktop sessions, Windows services, scheduled tasks, upload_file, upload_content, download_file, streaming relay sessions, port forwarding, and processes that keep running after the command ends cannot see those drives; relay_file Windows local-copy tasks are the exception because they run in one injected command session",
		} {
			if !strings.Contains(descriptions[name], want) {
				t.Fatalf("%s description missing %q: %s", name, want, descriptions[name])
			}
		}
	}
	for _, name := range []string{"upload_file", "upload_content", "download_file"} {
		if !strings.Contains(descriptions[name],
			"This separate file transfer session cannot see Windows network drives mapped for exec_command or exec_background") {
			t.Fatalf("%s description missing the network-drive visibility limit: %s", name, descriptions[name])
		}
	}
	if !strings.Contains(descriptions["relay_file"],
		"Separate streaming file-transfer sessions cannot see Windows network drives mapped for exec_command or exec_background; a Windows local copy creates its own injected command session and is therefore the exception") {
		t.Fatalf("relay_file description missing the Windows local-copy exception: %s", descriptions["relay_file"])
	}
	for _, want := range []string{
		"Windows maps the fixed drive with WNetAddConnection2 in the same SSH command session and then runs robocopy",
		"whose exit codes 0 through 7 are treated as success and 8 or higher as failure",
		"The Windows drive letter is visible only for that copy session and leaves no persistent mapping",
	} {
		if !strings.Contains(descriptions["relay_file"], want) {
			t.Fatalf("relay_file description missing Windows local-copy contract %q: %s", want, descriptions["relay_file"])
		}
	}
	if !strings.Contains(descriptions["list_shares"],
		"For Windows servers the fixed drive letter is mapped before each exec_command or exec_background command in that command's SSH session; relay_file Windows local-copy tasks create the same kind of injected command session") ||
		!strings.Contains(descriptions["list_shares"], "Desktop sessions, Windows services, scheduled tasks, separate file-transfer sessions, and processes that outlive the command cannot see it") {
		t.Fatalf("list_shares description missing Windows drive usage: %s", descriptions["list_shares"])
	}
	if !strings.Contains(descriptions["exec_context"], "planned_network_drives") ||
		!strings.Contains(descriptions["exec_context"], "uninjected_network_drives") {
		t.Fatalf("exec_context description missing drive preview fields: %s", descriptions["exec_context"])
	}

	listResult, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_shares",
		Arguments: map[string]any{"server": fixture.serverID, "live_check": true},
	})
	if err != nil || listResult.IsError {
		t.Fatalf("list_shares failed: err=%v result=%+v", err, listResult)
	}
	var listOutput ListSharesOutput
	unmarshalToolJSON(t, listResult, &listOutput)
	if len(listOutput.Shares) != 1 || len(listOutput.Shares[0].Servers) != 1 ||
		len(listOutput.Shares[0].Servers[0].MountNotes) != 1 {
		t.Fatalf("list_shares output = %+v", listOutput)
	}
	note := listOutput.Shares[0].Servers[0].MountNotes[0]
	for _, want := range []string{"会话内注入", "文件传输", "密码不出现在目标机进程命令行中"} {
		if !strings.Contains(note, want) {
			t.Fatalf("list_shares Windows note %q missing %q", note, want)
		}
	}
}

// TestNetworkDriveInjectionExecCommandWindowsCmd 覆盖 cmd 贯通路径：先选址，
// 再由短 PowerShell 引导命令读取脚本帧并在进程内映射盘符。密码包含换行、
// 引号、百分号、竖线与括号，仍只按长度经标准输入送达，不进入命令文本、返
// 回或审计。
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
	bootstrap := encodedCommandScripts(wrapped)
	if len(bootstrap) != 1 || !strings.HasPrefix(bootstrap[0], "$ProgressPreference='SilentlyContinue'") {
		t.Fatalf("cmd 引导命令 = %#v，期待固定 PowerShell 引导脚本", bootstrap)
	}
	script, values, ok := splitWindowsCmdInjectionFrames(inputs[1])
	if !ok {
		t.Fatalf("cmd 标准输入缺少脚本帧：%q", inputs[1])
	}
	for _, want := range []string{
		"WNetAddConnection2",
		`AddArgument('\\192.0.2.10\datasets')`,
		"AddArgument('mount-user')",
		"AddArgument('S:')",
		"[SshmgrNative.Mpr]::WNetAddConnection2($resource, $d, $c, 0)",
		fmt.Sprintf("New-Object byte[] %d", len(networkDriveTestPassword)),
		`$__sshmgr_process.StartInfo.Arguments = '/d /s /c "' + 'dir S:\datasets' + '"'`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("cmd 注入脚本缺少 %q：\n%s", want, script)
		}
	}
	if strings.Contains(wrapped, "\n") || strings.Contains(wrapped, "\r") || len(wrapped) >= 1500 {
		t.Fatalf("cmd 引导命令必须保持单行且低于 1500 字符：%q", wrapped)
	}
	if strings.Contains(wrapped, networkDriveTestPassword) ||
		strings.Contains(script, networkDriveTestPassword) {
		t.Fatal("wrapped command or cmd injection script leaked the mount password")
	}
	if string(values) != networkDriveTestPassword {
		t.Fatalf("cmd 标准输入值 = %q，期待只有挂载密码", values)
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
// 享同时注入：每个共享单独选址，在同一个脚本里按顺序映射并单独回传状态；
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
		"共享 datasets 的网络盘符 S：未注入：盘符 S：在这台服务器上已被占用\n"
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
	if bootstrap := encodedCommandScripts(wrapped); len(bootstrap) != 1 {
		t.Fatalf("cmd 引导脚本数 = %d，期待一个固定引导脚本：%q", len(bootstrap), wrapped)
	}
	script, values, ok := splitWindowsCmdInjectionFrames(inputs[len(inputs)-1])
	if !ok {
		t.Fatalf("cmd 标准输入缺少脚本帧：%q", inputs[len(inputs)-1])
	}
	if !strings.Contains(script, `AddArgument('\\192.0.2.10\datasets')`) ||
		!strings.Contains(script, `AddArgument('\\192.0.2.10\reports')`) {
		t.Fatalf("cmd 注入脚本没有按顺序覆盖两个共享：\n%s", script)
	}
	if strings.Contains(wrapped, networkDriveTestPassword) ||
		strings.Contains(wrapped, networkDriveSecondTestPassword) {
		t.Fatal("wrapped command leaked a mount password")
	}
	if strings.Contains(script, networkDriveTestPassword) ||
		strings.Contains(script, networkDriveSecondTestPassword) {
		t.Fatal("cmd 注入脚本泄漏挂载密码")
	}
	wantValues := append([]byte{}, networkDriveTestPassword...)
	wantValues = append(wantValues, networkDriveSecondTestPassword...)
	if string(values) != string(wantValues) {
		t.Fatalf("cmd 标准输入值 = %q，期待两个挂载密码按脚本顺序排列", values)
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
// 在同一条 cmd 命令里的标准输入帧顺序：脚本帧之后环境变量值在前，网络盘
// 密码在后，均按长度精确读取；两类注入都留下各自的审计名单。
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
	script, values, ok := splitWindowsCmdInjectionFrames(inputs[1])
	if !ok {
		t.Fatalf("cmd 标准输入缺少脚本帧：%q", inputs[1])
	}
	if !strings.Contains(script, "$env:HF_TOKEN") || !strings.Contains(script, "WNetAddConnection2") ||
		!strings.Contains(script, `'/d /s /c "' + 'set HF_TOKEN' + '"'`) {
		t.Fatalf("cmd 注入脚本必须先读机密项、再映射盘符、最后运行用户命令：\n%s", script)
	}
	if strings.Contains(wrapped, networkDriveEnvironmentValue) ||
		strings.Contains(wrapped, networkDriveTestPassword) ||
		strings.Contains(script, networkDriveEnvironmentValue) ||
		strings.Contains(script, networkDriveTestPassword) {
		t.Fatal("wrapped command or cmd injection script leaked an injected value")
	}
	wantValues := append([]byte{}, networkDriveEnvironmentValue...)
	wantValues = append(wantValues, networkDriveTestPassword...)
	if string(values) != string(wantValues) {
		t.Fatalf("cmd 标准输入值 = %q，期待环境变量值在前、挂载密码在后", values)
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
		"共享 datasets 的网络盘符 S：未注入：挂载密码在 Windows 上不是合法 UTF-8\n"
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
	script, values, ok := splitWindowsCmdInjectionFrames(inputs[1])
	if !ok {
		t.Fatalf("cmd 标准输入缺少脚本帧：%q", inputs[1])
	}
	if string(values) != networkDriveSecondTestPassword {
		t.Fatalf("cmd 标准输入值 = %q，期待只有合法共享的挂载密码", values)
	}
	if !strings.Contains(script, `AddArgument('\\192.0.2.10\reports')`) || strings.Contains(script, "datasets") {
		t.Fatalf("cmd 注入脚本应只包含 reports 共享：\n%s", script)
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

// TestNetworkDriveInjectionUnsafeRegistrationWarnsButRunsCommand 钉住登记
// 值的失败边界：地址含会让封装脚本变形的字符时，只跳过这个共享并警告，
// 不能让 sshbroker 的最后防线挡住整条用户命令。
func TestNetworkDriveInjectionUnsafeRegistrationWarnsButRunsCommand(t *testing.T) {
	useManualNetworkDriveClock(t)
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.unsafeProviderAddress = true
	})
	server, err := fixture.store.GetServer(fixture.serverID)
	if err != nil || server == nil {
		t.Fatalf("read server: %v %v", server, err)
	}
	shares, err := fixture.store.ListSharedStorages(store.SharedStorageFilter{Server: fixture.serverID})
	if err != nil || len(shares) != 1 {
		t.Fatalf("read shares: %v %#v", err, shares)
	}
	candidatePlan := networkDrivePlanAddressCandidates(fixture.store, server, shares[0])
	if candidatePlan.Reason != "" || len(candidatePlan.Candidates) == 0 {
		t.Fatalf("unsafe address candidates = %+v", candidatePlan)
	}
	networkDriveAddressCache.record(
		networkDriveAddressCacheKey{ServerID: server.ID, Share: shares[0].Name, Version: shares[0].Version},
		candidatePlan.Candidates, candidatePlan.Candidates[0], "",
	)

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": fixture.serverID, "command": "Get-Location"},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_command failed: err=%v result=%+v", err, result)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)
	want := "plain-command-output\n" +
		"共享 datasets 的网络盘符 S：未注入：" + networkDriveReasonUnsafeValue + "\n"
	if output.Stdout != want {
		t.Fatalf("stdout = %q, want %q", output.Stdout, want)
	}
	commands, _ := fixture.fake.recorded()
	if len(commands) != 1 || strings.Contains(commands[0], "WNetAddConnection2") {
		t.Fatalf("unsafe registration reached the mapping wrapper: %#v", commands)
	}
}

// TestNetworkDriveInjectionSudoOutsideWritableListStaysQuiet 钉住提权层说
// 明的泄露边界：普通层本来就没有可注入共享时，提权执行不能额外暴露这台
// 服务器登记过共享存储的信息。
func TestNetworkDriveInjectionSudoOutsideWritableListStaysQuiet(t *testing.T) {
	fixture := seedNetworkDriveVault(t, func(setup *networkDriveSeedSetup) {
		setup.sudoTarget = true
		setup.notWritable = true
	})

	result, err := fixture.clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command",
		Arguments: map[string]any{
			"server_id": fixture.serverID, "command": "whoami", "sudo": true,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_command(sudo) failed: err=%v result=%+v", err, result)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "plain-command-output\n" {
		t.Fatalf("sudo output leaked the storage registration: %q", output.Stdout)
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
	if strings.Contains(commands[0], "WNetAddConnection2") || strings.Contains(commands[0], "probe-one") {
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

// TestNetworkDriveInjectionStopsAfterServerLeavesWritableList 钉住撤销的
// Windows 侧效果：同一条服务先完成一次注入，owner 把服务器移出挂载密码机
// 密项可写入列表（共享存储版本号随之加一）后，下一条命令不探测、不映射、
// 不输出共享警告，行为与从未登记时一致。
func TestNetworkDriveInjectionStopsAfterServerLeavesWritableList(t *testing.T) {
	fixture := seedNetworkDriveVault(t, nil)
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

	beforeOutput, beforeInjected := call()
	if beforeOutput != "user-command-output\n" || len(beforeInjected) != 1 {
		t.Fatalf("first output = %q, injected = %#v, want one injected drive", beforeOutput, beforeInjected)
	}
	if err := fixture.store.DisallowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"windows-web"}, store.CLISource(),
	); err != nil {
		t.Fatalf("disallow the mount password for the target server: %v", err)
	}

	afterOutput, afterInjected := call()
	if afterOutput != "plain-command-output\n" {
		t.Fatalf("output after revoke = %q, want only the user command output without a share warning", afterOutput)
	}
	if len(afterInjected) != 0 {
		t.Fatalf("injected_network_drives after revoke = %#v, want none", afterInjected)
	}
	commands, inputs := fixture.fake.recorded()
	if len(commands) != 3 {
		t.Fatalf("remote commands = %#v, want one probe and one mapping before revoke, then only the user command", commands)
	}
	if strings.Contains(commands[2], "probe-one") || strings.Contains(commands[2], "WNetAddConnection2") ||
		strings.Contains(commands[2], "SSHMGR-SMBMAP") {
		t.Fatalf("command after revoke still contains an injection step: %q", commands[2])
	}
	if len(inputs[2]) != 0 {
		t.Fatalf("standard input after revoke = %q, want empty", inputs[2])
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	// 审计查询按编号倒序返回：第一行是撤销后的第二条命令。
	if len(rows) != 2 || len(rows[0].InjectedShareNames) != 0 {
		t.Fatalf("audit rows = %#v, want the second exec to record no injected share", rows)
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
	if strings.Contains(commands[0], "WNetAddConnection2") || strings.Contains(commands[0], "probe-one") {
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
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：盘符 S：在这台服务器上已被占用\n",
		},
		{
			name:       "mapping timeout",
			mapStatus:  "timeout",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：映射超时（3 秒上限）：提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:      "network error 53",
			mapStatus: "error-53",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：网络类失败（系统错误 53）：" +
				"提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:      "network error 64",
			mapStatus: "error-64",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：网络类失败（系统错误 64）：" +
				"提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:      "network error 67",
			mapStatus: "error-67",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：网络类失败（系统错误 67）：" +
				"提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制\n",
		},
		{
			name:       "different credentials already connected",
			mapStatus:  "error-1219",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：同一服务器已有不同凭据的连接（系统错误 1219）\n",
		},
		{
			name:       "authentication error 86",
			mapStatus:  "error-86",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n",
		},
		{
			name:       "authentication error 1326",
			mapStatus:  "error-1326",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n",
		},
		{
			name:       "missing status line",
			mapStatus:  "missing",
			wantOutput: "user-command-output\n共享 datasets 的网络盘符 S：未注入：注入步骤没有返回状态行\n",
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
		"共享 datasets 的网络盘符 S：未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n"
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
		if strings.Contains(command, "WNetAddConnection2") {
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
		"共享 datasets 的网络盘符 S：未注入：共享密码认证失败，已暂停注入，请同步或检查机密项\n"
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
	if strings.Contains(commands[2], "WNetAddConnection2") || strings.Contains(commands[2], "probe-one") {
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
		"共享 datasets 的网络盘符 S：未注入：目标服务器所属设备与共享存储提供方设备没有共同网络，未探测",
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
		strings.Contains(commands[0], "WNetAddConnection2") {
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
		"共享 datasets 的网络盘符 S：未注入：目标服务器未关联拓扑设备，未探测",
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
		strings.Contains(commands[0], "WNetAddConnection2") {
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
	if strings.Contains(commands[1], "WNetAddConnection2") {
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
	want := "user-command-output\n共享 datasets 的网络盘符 S：已使用虚拟局域网：办公虚拟局域网 第1个地址，性能可能下降；物理局域网地址不通：办公物理局域网 第1个地址  ✗无路由 2ms\n"
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
// 少发生一次，至多每条命令一次，不要求并发合并探测。
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
