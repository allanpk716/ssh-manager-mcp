package mcpserver

import (
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

type fakeShareCheckState struct {
	managed bool
	version int64
	mounted bool
}

// fakeShareCheckServer 挂在 testsshd 上模拟一台接受现场检测的服务器：按
// 自己的内存状态返回固定检测命令协议的输出。
type fakeShareCheckServer struct {
	mu         sync.Mutex
	system     string
	shares     map[string]fakeShareCheckState
	autoMaster bool
	commands   []string
}

// sudoStartFailure 模拟 sudo 密码正确但提权启动失败；由需要它的夹具在
// testsshd 选项上开启。

func (f *fakeShareCheckServer) handler(command string, stdin io.Reader) (string, string, int) {
	_, _ = io.ReadAll(stdin)
	f.mu.Lock()
	f.commands = append(f.commands, command)
	names := make([]string, 0, len(f.shares))
	for name := range f.shares {
		names = append(names, name)
	}
	sort.Strings(names)
	system, states, autoMaster := f.system, f.shares, f.autoMaster
	f.mu.Unlock()

	marker := ""
	switch {
	case strings.TrimSpace(command) == "uname -s":
		return system + "\n", "", 0
	case strings.Contains(command, "SSHMGR-LINUX-CHECK"):
		marker = "SSHMGR-LINUX-CHECK"
		if !strings.Contains(command, "/proc/mounts") {
			return "", "Linux 检测命令必须核验 /proc/mounts 挂载表", 91
		}
	case strings.Contains(command, "SSHMGR-DARWIN-CHECK"):
		marker = "SSHMGR-DARWIN-CHECK"
		if !strings.Contains(command, `(smbfs`) {
			return "", "macOS 检测命令必须核验 mount 输出挂载表", 91
		}
	default:
		return "", "unexpected command: " + command, 90
	}
	full := strings.Contains(command, "mode=full")
	var output strings.Builder
	mode := "limited"
	if full {
		mode = "full"
	}
	fmt.Fprintf(&output, "%s mode=%s\n", marker, mode)
	for index, name := range names {
		state := states[name]
		if !full {
			fmt.Fprintf(&output, "%s index=%d mounted=%s\n", marker, index+1, boolString(state.mounted))
			continue
		}
		shareValue, versionValue := "", ""
		if state.managed {
			shareValue = name
			versionValue = strconv.FormatInt(state.version, 10)
		}
		fmt.Fprintf(&output, "%s index=%d managed=%s share=%s version=%s mounted=%s\n",
			marker, index+1, boolString(state.managed), shareValue, versionValue,
			boolString(state.mounted))
	}
	if full && marker == "SSHMGR-DARWIN-CHECK" {
		fmt.Fprintf(&output, "%s auto-master=%s\n", marker, boolString(autoMaster))
	}
	return output.String(), "", 0
}

func (f *fakeShareCheckServer) commandCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

type listSharesCheckFixture struct {
	store     *store.Store
	profileID string
	session   *mcp.ClientSession
	fakes     map[string]*fakeShareCheckServer
	serverIDs map[string]string
}

// newListSharesCheckFixture 建立现场检测端到端测试的种子：一台提供方设备、
// 一个版本为 2 的共享、七台状态各异的目标服务器和一台范围外服务器。
func newListSharesCheckFixture(t *testing.T) listSharesCheckFixture {
	t.Helper()
	source := newStore(t)
	profileID, err := source.AddProfile("共享检测 profile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码",
		[]byte(mountShareTestPassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}

	fixture := listSharesCheckFixture{
		store: source, profileID: profileID,
		fakes: map[string]*fakeShareCheckServer{}, serverIDs: map[string]string{},
	}
	addCheckedServer := func(name, system string, state fakeShareCheckState, autoMaster, sudo bool, sudoStartFailure string) {
		fake := &fakeShareCheckServer{
			system: system, autoMaster: autoMaster,
			shares: map[string]fakeShareCheckState{"datasets": state},
		}
		addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
			Password: "login-pw", SudoPassword: "sudo-pw", SudoStartFailure: sudoStartFailure,
			Exec: fake.handler,
		})
		t.Cleanup(cleanup)
		loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
		var sudoCredential *models.Credential
		if sudo {
			sudoCredential = &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
		}
		server := &models.Server{
			Name: name, Host: addr[:strings.LastIndex(addr, ":")], Port: portOfAddr(addr),
			User: "deploy", AuthMethod: models.AuthPassword,
		}
		added, err := source.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := source.SaveEntryPin(serverByAddr(source, server.Host, server.Port), hostKey.Marshal()); err != nil {
			t.Fatal(err)
		}
		fixture.fakes[name] = fake
		fixture.serverIDs[name] = added.ServerID
	}
	addCheckedServer("linux-mounted", "Linux", fakeShareCheckState{managed: true, version: 2, mounted: true}, false, true, "")
	// 挂载点目录存在但挂载表里没有对应文件系统：现场检测必须判为未挂载。
	addCheckedServer("linux-empty-dir", "Linux", fakeShareCheckState{managed: true, version: 2, mounted: false}, false, true, "")
	addCheckedServer("linux-missing", "Linux", fakeShareCheckState{}, false, true, "")
	addCheckedServer("linux-old", "Linux", fakeShareCheckState{managed: true, version: 1, mounted: true}, false, true, "")
	addCheckedServer("darwin-mounted", "Darwin", fakeShareCheckState{managed: true, version: 2, mounted: true}, true, true, "")
	addCheckedServer("darwin-empty-dir", "Darwin", fakeShareCheckState{managed: true, version: 2, mounted: false}, true, true, "")
	addCheckedServer("darwin-reset", "Darwin", fakeShareCheckState{managed: true, version: 2, mounted: true}, false, true, "")
	addCheckedServer("linux-nosudo", "Linux", fakeShareCheckState{mounted: true}, false, false, "")
	addCheckedServer("freebsd", "FreeBSD", fakeShareCheckState{managed: true, version: 2, mounted: true}, false, true, "")
	addCheckedServer("sudo-failed", "Linux", fakeShareCheckState{managed: true, version: 2, mounted: true}, false, true,
		"sudo: 拒绝执行")

	windowsCredential, err := source.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")})
	if err != nil {
		t.Fatal(err)
	}
	windowsID, err := source.AddServer(&models.Server{
		Name: "windows-web", Host: "192.0.2.21", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: windowsCredential,
		ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.serverIDs["windows-web"] = windowsID

	// 模拟 SSH 不可达：指向一个已经关闭的本机端口。
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closedListener.Addr().(*net.TCPAddr).Port
	if err := closedListener.Close(); err != nil {
		t.Fatal(err)
	}
	unreachableID, err := source.AddServer(&models.Server{
		Name: "unreachable", Host: "127.0.0.1", Port: closedPort, User: "deploy",
		AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.serverIDs["unreachable"] = unreachableID

	peerID, err := source.AddServer(&models.Server{
		Name: "granted-peer", Host: "192.0.2.31", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.serverIDs["granted-peer"] = peerID

	granted := make([]string, 0, len(fixture.serverIDs))
	writable := make([]string, 0, len(fixture.serverIDs)-1)
	for name, id := range fixture.serverIDs {
		granted = append(granted, id)
		if name != "granted-peer" {
			writable = append(writable, name)
		}
	}
	if err := source.GrantServers(profileID, granted); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, writable, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	changeNote := "现场检测测试的版本变更"
	if _, err := source.EditSharedStorage("datasets", store.SharedStorageChanges{
		ChangeNote: &changeNote,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	server, tunnels, tasks, err := NewServer(source, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	fixture.session = connectListSharesClient(t, server)
	return fixture
}

func listSharesServerEntry(t *testing.T, output ListSharesOutput, serverName string) SharedStorageServerInfo {
	t.Helper()
	for _, share := range output.Shares {
		for _, server := range share.Servers {
			if server.Name == serverName {
				return server
			}
		}
	}
	t.Fatalf("输出里没有服务器 %s：%+v", serverName, output.Shares)
	return SharedStorageServerInfo{}
}

func assertListSharesNotes(t *testing.T, entry SharedStorageServerInfo, want ...string) {
	t.Helper()
	if strings.Join(entry.MountNotes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("服务器 %s 的检测说明 = %#v，期待 %#v", entry.Name, entry.MountNotes, want)
	}
}

func auditCount(t *testing.T, st *store.Store) int {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestListSharesLiveCheckReportsEveryState(t *testing.T) {
	fixture := newListSharesCheckFixture(t)
	auditBefore := auditCount(t, fixture.store)

	output, text, isError := callListShares(t, fixture.session, map[string]any{"live_check": true})
	if isError {
		t.Fatalf("现场检测失败：%s", text)
	}
	if output.Notice != "已按服务器标注现场检测的挂载状态；检测命令只读，不修改远端文件" {
		t.Fatalf("现场检测说明 = %q", output.Notice)
	}
	for serverName, wantStatus := range map[string]string{
		"linux-mounted":  "mounted",
		"linux-missing":  "config-missing",
		"linux-old":      "mounted",
		"darwin-mounted": "mounted",
		"darwin-reset":   "not-mounted",
		"windows-web":    "not-checked",
		"linux-nosudo":   "mounted",
		"unreachable":    "unreachable",
		"freebsd":        "unreachable",
		"sudo-failed":    "unreachable",
	} {
		entry := listSharesServerEntry(t, output, serverName)
		if entry.MountStatus != wantStatus {
			t.Fatalf("服务器 %s 的状态 = %q，期待 %q", serverName, entry.MountStatus, wantStatus)
		}
	}
	assertListSharesNotes(t, listSharesServerEntry(t, output, "linux-mounted"))
	assertListSharesNotes(t, listSharesServerEntry(t, output, "linux-old"),
		"配置版本落后：远端版本 1，快照版本 2，再调用挂载工具即可更新")
	assertListSharesNotes(t, listSharesServerEntry(t, output, "darwin-reset"),
		"系统配置被重置，再调用挂载工具即可恢复")
	assertListSharesNotes(t, listSharesServerEntry(t, output, "windows-web"),
		sharedStorageWindowsNote)
	assertListSharesNotes(t, listSharesServerEntry(t, output, "linux-nosudo"),
		"服务器条目没有配置 sudo 凭据，只能确认挂载点是否可访问，未读取持久配置比较版本")
	assertListSharesNotes(t, listSharesServerEntry(t, output, "unreachable"), "SSH 连接失败")
	assertListSharesNotes(t, listSharesServerEntry(t, output, "freebsd"), "系统不受支持")
	assertListSharesNotes(t, listSharesServerEntry(t, output, "sudo-failed"), "提权失败")

	for _, leaked := range []string{mountShareTestPassword, "DATASETS_MOUNT_PASSWORD", "mount-user"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("现场检测结果泄漏 %q：%s", leaked, text)
		}
	}
	if after := auditCount(t, fixture.store); after != auditBefore {
		t.Fatalf("现场检测不应写审计：%d 行新增", after-auditBefore)
	}
}

func TestListSharesLiveCheckTreatsEmptyDirectoryAsNotMounted(t *testing.T) {
	fixture := newListSharesCheckFixture(t)

	output, text, isError := callListShares(t, fixture.session, map[string]any{"live_check": true})
	if isError {
		t.Fatalf("现场检测失败：%s", text)
	}
	for serverName, wantStatus := range map[string]string{
		"linux-empty-dir":  "not-mounted",
		"darwin-empty-dir": "not-mounted",
	} {
		entry := listSharesServerEntry(t, output, serverName)
		if entry.MountStatus != wantStatus {
			t.Fatalf("服务器 %s 的状态 = %q，期待 %q", serverName, entry.MountStatus, wantStatus)
		}
		if len(entry.MountNotes) != 0 {
			t.Fatalf("服务器 %s 不应给出多余说明：%#v", serverName, entry.MountNotes)
		}
	}
}

func TestListSharesLiveCheckNarrowsToOneServer(t *testing.T) {
	fixture := newListSharesCheckFixture(t)

	output, text, isError := callListShares(t, fixture.session, map[string]any{
		"live_check": true, "check_server": "linux-old",
	})
	if isError {
		t.Fatalf("单台现场检测失败：%s", text)
	}
	for serverName, wantStatus := range map[string]string{
		"linux-old":     "mounted",
		"linux-mounted": "",
		"darwin-reset":  "",
		"windows-web":   "",
		"unreachable":   "",
		"linux-nosudo":  "",
	} {
		entry := listSharesServerEntry(t, output, serverName)
		if entry.MountStatus != wantStatus {
			t.Fatalf("服务器 %s 的状态 = %q，期待 %q", serverName, entry.MountStatus, wantStatus)
		}
	}
	if fixture.fakes["linux-old"].commandCount() != 2 {
		t.Fatalf("只应检测指定服务器：%d", fixture.fakes["linux-old"].commandCount())
	}
	for name, fake := range fixture.fakes {
		if name == "linux-old" {
			continue
		}
		if fake.commandCount() != 0 {
			t.Fatalf("服务器 %s 不应被检测：%d", name, fake.commandCount())
		}
	}

	byID, byIDText, isError := callListShares(t, fixture.session, map[string]any{
		"live_check": true, "check_server": fixture.serverIDs["darwin-reset"],
	})
	if isError {
		t.Fatalf("按编号单台检测失败：%s", byIDText)
	}
	if entry := listSharesServerEntry(t, byID, "darwin-reset"); entry.MountStatus != "not-mounted" {
		t.Fatalf("按编号检测的状态 = %q", entry.MountStatus)
	}
	if entry := listSharesServerEntry(t, byID, "linux-mounted"); entry.MountStatus != "" {
		t.Fatalf("范围外服务器不应被标注：%+v", entry)
	}

	baseline := map[string]int{}
	for name, fake := range fixture.fakes {
		baseline[name] = fake.commandCount()
	}
	windowsOnly, windowsText, isError := callListShares(t, fixture.session, map[string]any{
		"live_check": true, "check_server": "windows-web",
	})
	if isError {
		t.Fatalf("只指定 Windows 服务器失败：%s", windowsText)
	}
	entry := listSharesServerEntry(t, windowsOnly, "windows-web")
	if entry.MountStatus != "not-checked" || len(entry.MountNotes) != 1 {
		t.Fatalf("Windows 单台检测结论 = %+v", entry)
	}
	for name, fake := range fixture.fakes {
		if fake.commandCount() != baseline[name] {
			t.Fatalf("只指定 Windows 时不应连接服务器 %s：%d", name, fake.commandCount()-baseline[name])
		}
	}
}

func TestListSharesLiveCheckRejectsInvalidServerArguments(t *testing.T) {
	fixture := newListSharesCheckFixture(t)
	for name, args := range map[string]map[string]any{
		"只给检测服务器不给开关": {"check_server": "linux-mounted"},
		"检测范围外服务器":    {"live_check": true, "check_server": "missing"},
		"检测不可挂载服务器":   {"live_check": true, "check_server": "granted-peer"},
	} {
		_, text, isError := callListShares(t, fixture.session, args)
		if !isError {
			t.Fatalf("%s 应报错", name)
		}
		if strings.Contains(text, mountShareTestPassword) {
			t.Fatalf("%s 的报错泄漏密码：%s", name, text)
		}
	}
	for name, fake := range fixture.fakes {
		if fake.commandCount() != 0 {
			t.Fatalf("非法参数不应触发检测：%s 有 %d 条命令", name, fake.commandCount())
		}
	}
}

func TestListSharesWithoutLiveCheckPerformsNoNetworkAction(t *testing.T) {
	fixture := newListSharesCheckFixture(t)
	auditBefore := auditCount(t, fixture.store)

	output, text, isError := callListShares(t, fixture.session, map[string]any{})
	if isError {
		t.Fatalf("不带参数的 list_shares 失败：%s", text)
	}
	if output.Notice != "挂载点是约定路径，本工具不检测是否已挂载" {
		t.Fatalf("固定说明 = %q", output.Notice)
	}
	for _, share := range output.Shares {
		for _, server := range share.Servers {
			if server.MountStatus != "" || len(server.MountNotes) != 0 {
				t.Fatalf("不带参数时不应携带检测结论：%+v", server)
			}
		}
	}
	for name, fake := range fixture.fakes {
		if fake.commandCount() != 0 {
			t.Fatalf("不带参数时不应连接服务器 %s：%d", name, fake.commandCount())
		}
	}
	if after := auditCount(t, fixture.store); after != auditBefore {
		t.Fatalf("只读列表不应写审计：%d 行新增", after-auditBefore)
	}
}

// TestListSharesLiveCheckSkipsInvisibleServers 覆盖可挂载列表与会话可见范
// 围不一致的情况：排在前面但未授权的服务器不能挤掉后面服务器的检测结论。
func TestListSharesLiveCheckSkipsInvisibleServers(t *testing.T) {
	source := newStore(t)
	profileID, err := source.AddProfile("裁剪检测 profile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码",
		[]byte(mountShareTestPassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	fake := &fakeShareCheckServer{
		system: "Linux",
		shares: map[string]fakeShareCheckState{"datasets": {managed: true, version: 1, mounted: true}},
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	grantedServer := &models.Server{
		Name: "zzz-mounted", Host: addr[:strings.LastIndex(addr, ":")], Port: portOfAddr(addr),
		User: "deploy", AuthMethod: models.AuthPassword,
	}
	added, err := source.AddServerWithCredentialsToDevice(grantedServer, loginCredential, sudoCredential, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SaveEntryPin(serverByAddr(source, grantedServer.Host, grantedServer.Port), hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	// aaa-outsider 在挂载密码机密项的可写入列表里，但没有授予 profile。
	if _, err := source.AddServer(&models.Server{
		Name: "aaa-outsider", Host: "192.0.2.40", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.GrantServers(profileID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"},
		[]string{"aaa-outsider", "zzz-mounted"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(source, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	session := connectListSharesClient(t, server)

	output, text, isError := callListShares(t, session, map[string]any{"live_check": true})
	if isError {
		t.Fatalf("裁剪后的现场检测失败：%s", text)
	}
	if len(output.Shares) != 1 || len(output.Shares[0].Servers) != 1 {
		t.Fatalf("输出应只剩会话内服务器：%+v", output.Shares)
	}
	entry := output.Shares[0].Servers[0]
	if entry.Name != "zzz-mounted" || entry.MountStatus != "mounted" || len(entry.MountNotes) != 0 {
		t.Fatalf("可见服务器的检测结论 = %+v", entry)
	}
	if strings.Contains(text, "aaa-outsider") || strings.Contains(text, "192.0.2.40") {
		t.Fatalf("输出不应包含会话外服务器或地址：%s", text)
	}
}

// TestListSharesLiveCheckWorksOnCacheSnapshot 验证客户端缓存面同样能做现
// 场检测：只读快照提供登记与拨号信息，检测结论与单机面一致。
func TestListSharesLiveCheckWorksOnCacheSnapshot(t *testing.T) {
	source := newStore(t)
	profileID, err := source.AddProfile("缓存检测 profile")
	if err != nil {
		t.Fatal(err)
	}
	_, projectToken, err := source.AddProject("缓存检测项目", profileID)
	if err != nil {
		t.Fatal(err)
	}
	laptopTokenID, _, err := source.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码",
		[]byte(mountShareTestPassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	fake := &fakeShareCheckServer{
		system: "Linux",
		shares: map[string]fakeShareCheckState{"datasets": {managed: true, version: 1, mounted: false}},
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	target := &models.Server{
		Name: "linux-cache", Host: addr[:strings.LastIndex(addr, ":")], Port: portOfAddr(addr),
		User: "deploy", AuthMethod: models.AuthPassword,
	}
	added, err := source.AddServerWithCredentialsToDevice(target, loginCredential, sudoCredential, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SaveEntryPin(serverByAddr(source, target.Host, target.Port), hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := source.GrantServers(profileID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"},
		[]string{"linux-cache"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.ExportSnapshotForProfile(profileID, true, laptopTokenID)
	if err != nil {
		t.Fatal(err)
	}
	broker := newListSharesCacheBroker(t, snapshot, projectToken)
	session := connectListSharesClient(t, broker)

	output, text, isError := callListShares(t, session, map[string]any{"live_check": true})
	if isError {
		t.Fatalf("缓存面现场检测失败：%s", text)
	}
	entry := listSharesServerEntry(t, output, "linux-cache")
	if entry.MountStatus != "not-mounted" || len(entry.MountNotes) != 0 {
		t.Fatalf("缓存面检测结论 = %+v", entry)
	}
	if strings.Contains(text, mountShareTestPassword) {
		t.Fatalf("缓存面检测结果泄漏密码：%s", text)
	}
}
