package softwareinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newCollectService 打开一个真实 SQLite 临时库并返回挂在上面的采集服务。
// 远程执行与时钟全部注入，生产链路的真实执行函数不在这里触发。
func newCollectService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return now }
	return &Service{Store: st, Now: func() time.Time { return now }}, st
}

// addCollectDevice 新增一台设备并挂若干 POSIX 服务器条目，返回条目名到完
// 整条目的映射。
func addCollectDevice(t *testing.T, st *store.Store, deviceName string, serverNames ...string) map[string]*models.Server {
	t.Helper()
	return addCollectDeviceWithShell(t, st, deviceName, models.ShellPOSIX, serverNames...)
}

func addCollectDeviceWithShell(
	t *testing.T,
	st *store.Store,
	deviceName string,
	shell models.ShellType,
	serverNames ...string,
) map[string]*models.Server {
	t.Helper()
	if _, err := st.AddDevice(deviceName, "", ""); err != nil {
		t.Fatal(err)
	}
	out := map[string]*models.Server{}
	for _, serverName := range serverNames {
		result, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: serverName, Host: "192.0.2.10", Port: 22, User: "owner",
			ShellType: shell,
		}, &models.Credential{Type: models.CredPassword, Secret: []byte("password")}, nil, deviceName)
		if err != nil {
			t.Fatal(err)
		}
		server, err := st.GetServer(result.ServerID)
		if err != nil {
			t.Fatal(err)
		}
		out[serverName] = server
	}
	return out
}

type recordedExec struct {
	server      *models.Server
	shell       models.ShellType
	command     string
	sudo        bool
	hasDeadline bool
}

func replayCollectExec(output string, recorded *recordedExec, resultErr error) RemoteExecFunc {
	return func(ctx context.Context, server *models.Server, shell models.ShellType, command string, sudo bool) (RemoteExecResult, error) {
		_, hasDeadline := ctx.Deadline()
		*recorded = recordedExec{server: server, shell: shell, command: command, sudo: sudo, hasDeadline: hasDeadline}
		if resultErr != nil {
			return RemoteExecResult{}, resultErr
		}
		return RemoteExecResult{Stdout: output}, nil
	}
}

// TestCollectLinuxFullFixture 覆盖一次成功的权威端采集：选条目、生成固定
// 命令、执行、解析、整体替换实测条目、更新采集状态并写审计。
func TestCollectLinuxFullFixture(t *testing.T) {
	service, st := newCollectService(t)
	servers := addCollectDevice(t, st, "工作站", "entry-a")
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2404-full.txt"), &recorded, nil)

	report, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeviceName != "工作站" || report.ServerEntryName != "entry-a" || report.Sudo ||
		report.SystemType != "ubuntu" || report.CommandSetVersion != POSIXCommandSetVersion ||
		report.Incomplete {
		t.Fatalf("采集报告 = %+v", report)
	}
	if report.ItemCounts[models.SoftwareCategorySystemPackage] != 1041 ||
		report.ItemCounts[models.SoftwareCategoryContainerImage] != 9 ||
		report.ItemCounts[models.SoftwareCategoryUnidentified] != 3 {
		t.Fatalf("采集报告条目数量 = %+v", report.ItemCounts)
	}
	expectedCommand, err := BuildInventoryCommand(models.ShellPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.server.ID != servers["entry-a"].ID || recorded.shell != models.ShellPOSIX ||
		recorded.command != expectedCommand {
		t.Fatalf("固定命令没有按条目生成：server=%s shell=%s command 前缀=%q",
			recorded.server.Name, recorded.shell, firstLine(recorded.command))
	}
	if strings.Contains(recorded.command, "sudo -S") {
		t.Fatal("sudo 开关关闭时命令不得经 sudo 提权包装")
	}
	if !recorded.hasDeadline {
		t.Fatal("单次采集必须带超时执行")
	}

	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	var openssh *models.SoftwareObservedItem
	for i := range items {
		if items[i].Name == "openssh-server" {
			openssh = &items[i]
		}
	}
	if openssh == nil || len(openssh.ListeningPorts) != 2 || openssh.StableKey == "" ||
		openssh.CollectedAt != service.Now() {
		t.Fatalf("openssh-server 实测条目 = %+v", openssh)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastSuccessAt != service.Now() || status.LastIncomplete || status.Collector != "authority" ||
		status.LastServerName != "entry-a" || status.SystemType != "ubuntu" {
		t.Fatalf("采集状态 = %+v", status)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("每次采集应写一行审计，得到 %d 行", len(rows))
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["device"] != "工作站" || summary["server"] != "entry-a" ||
		summary["collector"] != "authority" || summary["sudo"] != false || summary["result"] != "ok" {
		t.Fatalf("采集审计摘要 = %v", summary)
	}
}

// TestCollectWindowsFullFixtureForEachShell 覆盖 PowerShell 与 cmd 两种条目
// 的一次成功采集：命令按条目包装，Windows 输出解析后落库并写入采集状态。
func TestCollectWindowsFullFixtureForEachShell(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDeviceWithShell(t, st, "工作站", models.ShellPowerShell, "powershell-entry")
	addCollectDeviceWithShell(t, st, "命令行工作站", models.ShellWindowsCmd, "cmd-entry")
	tests := []struct {
		device      string
		serverEntry string
		shell       models.ShellType
		fixture     string
		itemCount   int
	}{
		{
			device: "工作站", serverEntry: "powershell-entry", shell: models.ShellPowerShell,
			fixture: "win10-powershell-full.txt", itemCount: 813,
		},
		{
			device: "命令行工作站", serverEntry: "cmd-entry", shell: models.ShellWindowsCmd,
			fixture: "win10-cmd-full.txt", itemCount: 203,
		},
	}
	for _, test := range tests {
		var recorded recordedExec
		service.RemoteExec = replayCollectExec(windowsFixtureOutput(t, test.fixture), &recorded, nil)
		report, err := service.Collect(context.Background(), CollectRequest{
			DeviceName: test.device, ServerEntry: test.serverEntry,
		})
		if err != nil {
			t.Fatal(err)
		}
		expectedCommand, err := BuildInventoryCommand(test.shell)
		if err != nil {
			t.Fatal(err)
		}
		if report.SystemType != "windows" || report.CommandSetVersion != WindowsCommandSetVersion ||
			report.Incomplete || recorded.shell != test.shell || recorded.command != expectedCommand {
			t.Fatalf("shell %s 的采集报告或命令包装不正确：report=%+v shell=%s command=%s",
				test.shell, report, recorded.shell, recorded.command)
		}
		items, err := st.SoftwareObservedItemsForDevice(test.device)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != test.itemCount {
			t.Fatalf("shell %s 落库条目数量 = %d，应为 %d", test.shell, len(items), test.itemCount)
		}
		status, err := st.SoftwareCollectionStatusForDevice(test.device)
		if err != nil {
			t.Fatal(err)
		}
		if status.SystemType != "windows" || status.CommandSetVersion != WindowsCommandSetVersion ||
			status.LastIncomplete || status.LastServerName != test.serverEntry {
			t.Fatalf("shell %s 的采集状态 = %+v", test.shell, status)
		}
	}
}

// TestCollectWindowsNoProcessNamesFixture 覆盖权限不足的 Windows 输出：看
// 不到进程名的监听端口落成未识别进程，采集本身仍算成功。
func TestCollectWindowsNoProcessNamesFixture(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDeviceWithShell(t, st, "工作站", models.ShellWindowsCmd, "entry-a")
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(
		windowsFixtureOutput(t, "win10-cmd-no-process-names-derived.txt"), &recorded, nil,
	)

	report, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.SystemType != "windows" ||
		report.ItemCounts[models.SoftwareCategoryUnidentified] != 36 {
		t.Fatalf("权限不足输出的采集报告 = %+v", report)
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	unknownWithEmptyProcess := 0
	for _, item := range items {
		if item.Category != models.SoftwareCategoryUnidentified {
			continue
		}
		if item.ProcessName == "" && item.ExecutablePath == "" && len(item.ListeningPorts) > 0 {
			unknownWithEmptyProcess++
		}
	}
	if unknownWithEmptyProcess != 36 {
		t.Fatalf("看不到进程名的未识别进程落库数量 = %d", unknownWithEmptyProcess)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastIncomplete || status.SystemType != "windows" {
		t.Fatalf("权限不足但输出完整时采集状态不应失败：%+v", status)
	}
}

// TestCollectWindowsTruncatedFixture 覆盖某一节中途截断并超时的输出：已解
// 析条目保留，状态与报告都标为不完整。
func TestCollectWindowsTruncatedFixture(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDeviceWithShell(t, st, "工作站", models.ShellWindowsCmd, "entry-a")
	service.RemoteExec = func(
		ctx context.Context,
		server *models.Server,
		shell models.ShellType,
		command string,
		sudo bool,
	) (RemoteExecResult, error) {
		return RemoteExecResult{
			Stdout: windowsFixtureOutput(t, "win10-cmd-truncated.txt"), TimedOut: true,
		}, nil
	}

	report, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"})
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(report.Notes, "；")
	if !report.Incomplete || !strings.Contains(notes, "listen-udp 节未完成") ||
		!strings.Contains(notes, "固定命令执行超时") {
		t.Fatalf("截断输出的采集报告 = %+v", report)
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("截断前已解析的条目应保留")
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if !status.LastIncomplete || status.LastSuccessAt.IsZero() ||
		status.CommandSetVersion != WindowsCommandSetVersion {
		t.Fatalf("截断输出的采集状态 = %+v", status)
	}
}

// TestCollectTruncatedOutputKeepsParsedParts 覆盖输出被截断的情况：已解析
// 的部分保留并整体替换，状态标为不完整；执行超时与输出超限同样归为不完
// 整而不是失败。
func TestCollectTruncatedOutputKeepsParsedParts(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDevice(t, st, "工作站", "entry-a")
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2404-full.txt"), &recorded, nil)
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}

	service.RemoteExec = func(_ context.Context, server *models.Server, shell models.ShellType, command string, sudo bool) (RemoteExecResult, error) {
		recorded = recordedExec{server: server, shell: shell, command: command, sudo: sudo}
		return RemoteExecResult{Stdout: fixtureOutput(t, "ubuntu2404-truncated.txt"), TimedOut: true}, nil
	}
	report, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete {
		t.Fatal("截断输出应标为不完整")
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || len(items) >= 1057 {
		t.Fatalf("截断结果应保留已解析部分，得到 %d 条", len(items))
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if !status.LastIncomplete || status.LastSuccessAt.IsZero() {
		t.Fatalf("截断后的状态 = %+v", status)
	}

	service.RemoteExec = func(_ context.Context, _ *models.Server, _ models.ShellType, _ string, _ bool) (RemoteExecResult, error) {
		return RemoteExecResult{Stdout: fixtureOutput(t, "ubuntu2404-full.txt"), Truncated: true}, errors.New("会话中断")
	}
	report, err = service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete {
		t.Fatal("输出超限应标为不完整")
	}
	if !strings.Contains(strings.Join(report.Notes, "；"), "会话中断") {
		t.Fatalf("执行中断应写进不完整原因：%v", report.Notes)
	}
}

// TestCollectLinuxNoProcessNamesFixture 覆盖权限不足的真实输出：看不到进
// 程名的监听端口全部落成未识别进程条目。
func TestCollectLinuxNoProcessNamesFixture(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDevice(t, st, "工作站", "entry-a")
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2204-no-process-names.txt"), &recorded, nil)

	report, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.SystemType != "ubuntu" ||
		report.ItemCounts[models.SoftwareCategoryUnidentified] != 7 {
		t.Fatalf("采集报告 = %+v", report)
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	unidentified, withProcess := 0, 0
	for _, item := range items {
		if item.Category != models.SoftwareCategoryUnidentified {
			continue
		}
		unidentified++
		if item.ProcessName != "" && item.ExecutablePath != "" {
			withProcess++
		}
		if len(item.ListeningPorts) == 0 {
			t.Fatalf("未识别进程必须保留监听端口：%+v", item)
		}
	}
	if unidentified != 7 || withProcess != 1 {
		t.Fatalf("未识别进程 %d 条（看得到进程的 %d 条）", unidentified, withProcess)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastIncomplete || status.SystemType != "ubuntu" {
		t.Fatalf("采集状态 = %+v", status)
	}
}

// TestCollectFailureKeepsPreviousResult 覆盖连接失败：上一次结果保留，失败
// 时间与原因写入状态，命令行拿到错误，审计记失败。
func TestCollectFailureKeepsPreviousResult(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDevice(t, st, "工作站", "entry-a")
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2404-full.txt"), &recorded, nil)
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}

	connectionFailure := errors.New("dial timeout")
	service.RemoteExec = replayCollectExec("", &recorded, connectionFailure)
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"}); err == nil {
		t.Fatal("采集失败应返回错误")
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1057 {
		t.Fatalf("失败应保留上一次结果，得到 %d 条", len(items))
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastFailureAt.IsZero() || !strings.Contains(status.LastFailureReason, "dial timeout") {
		t.Fatalf("失败状态 = %+v", status)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect"}})
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "failed" {
		t.Fatalf("失败采集审计 = %v", summary)
	}
}

// TestCollectEntrySelectionAndSudo 覆盖条目选择与 sudo：默认优先打开 sudo
// 开关的条目，否则名字排序第一个；命令行可指定；开关打开时执行走 sudo
// 包装，且没有 sudo 凭据时明确失败。
func TestCollectEntrySelectionAndSudo(t *testing.T) {
	service, st := newCollectService(t)
	servers := addCollectDevice(t, st, "工作站", "alpha", "gamma")
	// beta 带 sudo 凭据，用于验证打开开关后执行路径切到 sudo 包装。
	betaResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "beta", Host: "192.0.2.10", Port: 22, User: "owner",
		ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("password")},
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-password")},
		"工作站")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := st.GetServer(betaResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	servers["beta"] = beta
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2204-no-process-names.txt"), &recorded, nil)

	// 未打开任何开关：名字排序第一个（ASCII 名字，避免中文排序歧义）。
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}
	if recorded.server.Name != "alpha" || recorded.sudo {
		t.Fatalf("默认条目 = %s sudo=%v", recorded.server.Name, recorded.sudo)
	}

	// beta 打开 sudo 开关后默认选 beta，执行带 sudo。
	if err := st.SetSoftwareCollectSudo("beta", true, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}
	if recorded.server.Name != "beta" || !recorded.sudo {
		t.Fatalf("sudo 开关打开后默认条目 = %s sudo=%v", recorded.server.Name, recorded.sudo)
	}
	if strings.Contains(recorded.command, "sudo -S") {
		t.Fatal("sudo 包装由执行函数完成，命令模板本身不应变化")
	}

	// 显式指定 gamma 时优先于默认规则；gamma 没开 sudo，按普通用户执行。
	if _, err := service.Collect(context.Background(), CollectRequest{
		DeviceName: "工作站", ServerEntry: "gamma",
	}); err != nil {
		t.Fatal(err)
	}
	if recorded.server.Name != "gamma" || recorded.sudo {
		t.Fatalf("指定条目 = %s sudo=%v", recorded.server.Name, recorded.sudo)
	}

	// 指定不在这台设备上的条目应报错；beta 打开 sudo 但没有 sudo 凭据也
	// 应明确失败并记录原因。
	if _, err := service.Collect(context.Background(), CollectRequest{
		DeviceName: "工作站", ServerEntry: servers["alpha"].ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Collect(context.Background(), CollectRequest{
		DeviceName: "工作站", ServerEntry: "别的设备条目",
	}); err == nil {
		t.Fatal("指定不属于这台设备的条目应报错")
	}
	// beta 仍有开关；删掉它的 sudo 凭据需要改服务器条目，这里改为直接
	// 验证已记录的行为：开关打开但条目缺少 sudo 凭据会明确失败。先给
	// gamma 打开开关（gamma 没有 sudo 凭据）。
	if err := st.SetSoftwareCollectSudo("gamma", true, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	_, err = service.Collect(context.Background(), CollectRequest{DeviceName: "工作站", ServerEntry: "gamma"})
	if err == nil || !strings.Contains(err.Error(), "sudo 凭据") {
		t.Fatalf("sudo 开关打开但缺少 sudo 凭据应明确失败：%v", err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.LastFailureReason, "sudo 凭据") {
		t.Fatalf("配置错误也应记录失败原因：%+v", status)
	}
}

// TestCollectValidation 覆盖设备与条目的前置校验。
func TestCollectValidation(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDevice(t, st, "工作站", "entry-a")
	_, err := service.Collect(context.Background(), CollectRequest{DeviceName: "不存在的设备"})
	if err == nil {
		t.Fatal("设备不存在应报错")
	}
	if _, err := st.AddDevice("无条目设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "无条目设备"}); err == nil {
		t.Fatal("设备没有服务器条目应报错")
	}
}

// TestQueryShowsCollectionStatus 验证 #185 的查询接口在采集状态表存在后
// 填真实状态：采过的设备显示上次成功时间，未采过的设备仍显示从未采集。
func TestQueryShowsCollectionStatus(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDevice(t, st, "工作站", "entry-a")
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2404-full.txt"), &recorded, nil)
	if _, err := service.Collect(context.Background(), CollectRequest{DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st).AddDeclaration(Declaration{
		Device: "工作站", Category: CategoryServiceProgram,
		Name: "OpenSSH", Version: "1:9.6p1-3ubuntu13.20",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}

	manager := New(st)
	manager.Now = service.Now
	result, err := manager.Query(QueryOptions{Device: "工作站", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 {
		t.Fatalf("查询设备数量 = %d", len(result.Devices))
	}
	status := result.Devices[0].Collection
	if status.NeverCollected || status.Stale || status.LastSuccessAt == nil ||
		status.LastSuccessAt.Unix() != service.Now().Unix() ||
		status.CommandSetVersion != POSIXCommandSetVersion || status.ServerEntry != "entry-a" {
		t.Fatalf("查询里的采集状态 = %+v", status)
	}
}

func firstLine(text string) string {
	return strings.SplitN(text, "\n", 2)[0]
}
