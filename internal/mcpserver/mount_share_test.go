package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

const mountShareTestPassword = `mount-P@$$ 'quotes' "double" \ end`

type fakeLinuxMountServer struct {
	mu sync.Mutex

	commands       []string
	standardInputs [][]byte
	credentialFile string
	fstabLines     []string

	missingMountCIFS bool
	workingVersion   string
	shareID          string
	probeOutput      string
}

func (f *fakeLinuxMountServer) handler(command string, stdin io.Reader) (string, string, int) {
	stdinBytes, err := io.ReadAll(stdin)
	if err != nil {
		return "", "read standard input: " + err.Error(), 1
	}
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.standardInputs = append(f.standardInputs, append([]byte(nil), stdinBytes...))
	f.mu.Unlock()

	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one "):
		if f.probeOutput != "" {
			return f.probeOutput, "", 0
		}
		return "SSHMGR-PROBE 0 ok 2 \n", "", 0
	case strings.Contains(command, "SSHMGR-LINUX-MOUNT"):
		if f.missingMountCIFS {
			return "", "sshmgr: mount.cifs not found", 83
		}
		selectedVersion := f.workingVersion
		if selectedVersion == "" {
			selectedVersion = "3.1.1"
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.credentialFile = "username=mount-user\npassword=" + string(stdinBytes)
		if selectedVersion == "none" {
			f.credentialFile = ""
			return "", "SSHMGR-LINUX-MOUNT protocol-failed", 84
		}
		selectedAddress := "192.0.2.10"
		if strings.Contains(command, "100.64.10.10") {
			selectedAddress = "100.64.10.10"
		}
		f.fstabLines = append(f.fstabLines, "//"+selectedAddress+"/datasets /mnt/sshmgr/datasets cifs "+
			"credentials=/etc/sshmgr/shares/"+f.shareID+".credentials,vers="+selectedVersion+
			",_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0")
		return "SSHMGR-LINUX-MOUNT ok\n", "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func (f *fakeLinuxMountServer) recorded() (commands []string, inputs [][]byte, credential string, fstab []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), append([][]byte(nil), f.standardInputs...), f.credentialFile, append([]string(nil), f.fstabLines...)
}

type mountShareFixture struct {
	store     *store.Store
	profileID string
	targetID  string
	shareID   string
	session   *mcp.ClientSession
	fake      *fakeLinuxMountServer
}

type mountShareSeed struct {
	store     *store.Store
	profileID string
	targetID  string
	shareID   string
	fake      *fakeLinuxMountServer
}

// seedMountShareVault 建立共享存储挂载测试共用的权威端数据：提供方与目标
// 设备、两个共同网络、目标服务器条目与凭据、挂载密码机密项、共享登记和
// 授权。单机工具面与客户端缓存面都在这份种子上继续装配。
func seedMountShareVault(t *testing.T, setup mountShareFixtureSetup) mountShareSeed {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "mount-share-test.db")
	storeForTest := newStoreAt(t, databasePath)
	fake := &fakeLinuxMountServer{
		missingMountCIFS: setup.missingMountCIFS, workingVersion: setup.workingVersion,
		probeOutput: setup.probeOutput,
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)

	_, err := storeForTest.AddDevice("NAS", "共享存储提供方", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = storeForTest.AddDevice("linux-device", "挂载目标", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddNetwork(models.Network{Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddNetwork(models.Network{Name: "办公虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if setup.targetNoCommonNetwork {
		if _, err := storeForTest.AddNetwork(models.Network{Name: "目标独立虚拟局域网", Type: models.NetworkVirtual}); err != nil {
			t.Fatal(err)
		}
	}
	for _, membership := range []struct{ network, address string }{
		{"办公物理局域网", "192.0.2.10"},
		{"办公虚拟局域网", "100.64.10.10"},
	} {
		if _, err := storeForTest.AddNetworkMembership("NAS", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	targetNetworks := []struct{ network, address string }{
		{"办公物理局域网", "192.0.2.20"}, {"办公虚拟局域网", "100.64.10.20"},
	}
	if setup.targetNoCommonNetwork {
		targetNetworks = []struct{ network, address string }{{"目标独立虚拟局域网", "198.51.100.20"}}
	}
	for _, membership := range targetNetworks {
		if _, err := storeForTest.AddNetworkMembership("linux-device", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	var sudoCredential *models.Credential
	if setup.sudo {
		sudoCredential = &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	}
	server := &models.Server{
		Name: "linux-web", Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: models.ShellPOSIX,
	}
	added, err := storeForTest.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, "linux-device")
	if err != nil {
		t.Fatal(err)
	}
	targetID := added.ServerID
	if setup.targetWithoutDevice {
		rawDatabase, err := sql.Open("sqlite", databasePath)
		if err != nil {
			t.Fatal(err)
		}
		_, unlinkErr := rawDatabase.Exec(`UPDATE servers SET device_id=NULL WHERE id=?`, targetID)
		closeErr := rawDatabase.Close()
		if unlinkErr != nil || closeErr != nil {
			t.Fatalf("解除服务器设备关联失败：unlink=%v close=%v", unlinkErr, closeErr)
		}
	}
	if err := storeForTest.SaveEntryPin(serverByAddr(storeForTest, server.Host, server.Port), hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}

	profileID, err := storeForTest.AddProfile("共享挂载 profile")
	if err != nil {
		t.Fatal(err)
	}
	granted := []string{targetID}
	if setup.targetOutOfProfile || setup.notWritable {
		visibleID, err := storeForTest.AddServer(&models.Server{
			Name: "visible-peer", Host: "192.0.2.31", Port: 22, User: "deploy",
			AuthMethod: models.AuthPassword,
		})
		if err != nil {
			t.Fatal(err)
		}
		granted = []string{visibleID}
		if setup.notWritable {
			granted = append(granted, targetID)
		}
	}
	if err := storeForTest.GrantServers(profileID, granted); err != nil {
		t.Fatal(err)
	}
	if err := storeForTest.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte(mountShareTestPassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	allowedTargets := []string{"linux-web"}
	if setup.notWritable {
		allowedTargets = []string{"visible-peer"}
	}
	if err := storeForTest.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, allowedTargets, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "数据集", Path: "datasets"},
		},
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	addedShare, err := storeForTest.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	fake.shareID = addedShare.ID
	if setup.shareInvisible {
		if err := storeForTest.DisallowSecrets(
			[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-web"}, store.CLISource(),
		); err != nil {
			t.Fatal(err)
		}
	}
	if setup.windowsTarget {
		windowsCredential, err := storeForTest.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")})
		if err != nil {
			t.Fatal(err)
		}
		windowsID, err := storeForTest.AddServer(&models.Server{
			Name: "windows-web", Host: "192.0.2.30", Port: 22, User: "deploy",
			AuthMethod: models.AuthPassword, CredentialID: windowsCredential,
			ShellType: models.ShellPowerShell,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := storeForTest.GrantServers(profileID, []string{windowsID}); err != nil {
			t.Fatal(err)
		}
		if err := storeForTest.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, []string{"windows-web"}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
		targetID = windowsID
	}
	return mountShareSeed{
		store: storeForTest, profileID: profileID, targetID: targetID,
		shareID: addedShare.ID, fake: fake,
	}
}

func newMountShareFixture(t *testing.T, options ...func(*mountShareFixtureSetup)) mountShareFixture {
	t.Helper()
	setup := mountShareFixtureSetup{sudo: true}
	for _, option := range options {
		option(&setup)
	}
	seed := seedMountShareVault(t, setup)
	serverFace, tunnels, tasks, err := NewServer(seed.store, seed.profileID, "project-test")
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
	return mountShareFixture{
		store: seed.store, profileID: seed.profileID, targetID: seed.targetID,
		shareID: seed.shareID, session: clientSession, fake: seed.fake,
	}
}

type mountShareFixtureSetup struct {
	sudo                  bool
	missingMountCIFS      bool
	workingVersion        string
	probeOutput           string
	notWritable           bool
	targetOutOfProfile    bool
	windowsTarget         bool
	shareInvisible        bool
	targetWithoutDevice   bool
	targetNoCommonNetwork bool
	oldAuthoritySnapshot  bool
}

func TestMountShareTargetWithoutDeviceFailsWithoutProbe(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.targetWithoutDevice = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "目标服务器未关联拓扑设备，请 owner 先把服务器归到设备上"
	if !isError || text != want {
		t.Fatalf("报错 = %q (%v)，期待 %q", text, output, want)
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 1 || strings.TrimSpace(commands[0]) != "uname -s" ||
		credential != "" || len(fstab) != 0 {
		t.Fatalf("缺少设备关联时只应识别系统，不应探测地址：%v %q %v", commands, credential, fstab)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].ServerID != fixture.targetID ||
		strings.Contains(rows[0].Command, mountShareTestPassword) ||
		strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "100.64.10.10") {
		t.Fatalf("失败审计 = %+v", rows)
	}
}

func TestMountShareNoCommonNetworkListsBothSides(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.targetNoCommonNetwork = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "目标服务器所属设备与共享存储提供方设备没有共同网络，未探测\n" +
		"共享存储提供方设备所在网络：办公物理局域网、办公虚拟局域网\n" +
		"目标服务器所属设备所在网络：目标独立虚拟局域网\n" +
		"请 owner 先补齐网络拓扑登记"
	if !isError || text != want {
		t.Fatalf("报错 = %q (%v)，期待 %q", text, output, want)
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 1 || strings.TrimSpace(commands[0]) != "uname -s" {
		t.Fatalf("没有共同网络时只应识别系统，不应探测地址：%v", commands)
	}
	if credential != "" || len(fstab) != 0 {
		t.Fatalf("没有共同网络时不应写配置：credential=%q fstab=%v", credential, fstab)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].ServerID != fixture.targetID {
		t.Fatalf("失败审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "address-selection-error" ||
		summary["selection_error"] != "no-common-network" ||
		fmt.Sprintf("%v", summary["provider_networks"]) != "[办公物理局域网 办公虚拟局域网]" ||
		fmt.Sprintf("%v", summary["target_networks"]) != "[目标独立虚拟局域网]" {
		t.Fatalf("选地址失败审计摘要 = %#v", summary)
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10", "198.51.100.20"} {
		if strings.Contains(text, address) || strings.Contains(rows[0].Command, address) {
			t.Fatalf("选地址失败泄漏地址 %s：%s\n%s", address, text, rows[0].Command)
		}
	}
}

func TestMountShareAllCandidatesFailListsEveryAttempt(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.probeOutput = "SSHMGR-PROBE 0 fail 12 connection-refused\n" +
			"SSHMGR-PROBE 1 fail 15 timeout\n"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "全部候选地址在目标服务器上探测失败，登记与实际不一致：\n" +
		"办公物理局域网 第1个地址  ✗连接被拒绝 12ms\n" +
		"办公虚拟局域网 第1个地址  ✗连接超时 15ms\n" +
		"未写入任何挂载配置"
	if !isError || text != want {
		t.Fatalf("报错 = %q (%v)，期待 %q", text, output, want)
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 2 || !strings.Contains(commands[1], "192.0.2.10") ||
		!strings.Contains(commands[1], "100.64.10.10") {
		t.Fatalf("全部失败前仍应一次探测所有候选：%v", commands)
	}
	if credential != "" || len(fstab) != 0 {
		t.Fatalf("全部候选失败后不应写配置：credential=%q fstab=%v", credential, fstab)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].ServerID != fixture.targetID {
		t.Fatalf("失败审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "address-selection-error" ||
		summary["selection_error"] != "all-candidates-failed" {
		t.Fatalf("选地址失败审计摘要 = %#v", summary)
	}
	attempts, ok := summary["attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("逐地址审计 = %#v", summary["attempts"])
	}
	first := attempts[0].(map[string]any)
	second := attempts[1].(map[string]any)
	if first["network_name"] != "办公物理局域网" || first["network_sequence"] != float64(1) ||
		first["error_category"] != "connection-refused" || first["duration_ms"] != float64(12) ||
		second["network_name"] != "办公虚拟局域网" || second["network_sequence"] != float64(1) ||
		second["error_category"] != "timeout" || second["duration_ms"] != float64(15) {
		t.Fatalf("逐地址审计 = %#v", attempts)
	}
	for _, leakText := range []string{text, rows[0].Command} {
		for _, address := range []string{"192.0.2.10", "100.64.10.10"} {
			if strings.Contains(leakText, address) {
				t.Fatalf("选地址失败泄漏地址 %s：%s", address, leakText)
			}
		}
		if strings.Contains(leakText, mountShareTestPassword) {
			t.Fatalf("选地址失败泄漏挂载密码：%s", leakText)
		}
	}
}

func TestMountSharePhysicalFailuresFallBackToVirtualNetwork(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.probeOutput = "SSHMGR-PROBE 0 fail 12 connection-refused\n" +
			"SSHMGR-PROBE 1 ok 18 \n"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("物理地址失败但虚拟地址可用时应成功：%s", text)
	}
	if output["network_name"] != "办公虚拟局域网" || output["network_sequence"] != float64(1) {
		t.Fatalf("降级输出 = %#v", output)
	}
	notes, ok := output["mount_notes"].([]any)
	if !ok || len(notes) != 2 ||
		notes[0] != "已使用虚拟局域网：办公虚拟局域网 第1个地址" ||
		notes[1] != "物理局域网地址不通：办公物理局域网 第1个地址  ✗连接被拒绝 12ms" {
		t.Fatalf("降级说明 = %#v", output["mount_notes"])
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 3 || strings.Contains(commands[2], "192.0.2.10") ||
		!strings.Contains(commands[2], "100.64.10.10") {
		t.Fatalf("降级后固定挂载命令应只使用虚拟地址：%v", commands)
	}
	if credential == "" || len(fstab) != 1 || !strings.Contains(fstab[0], "//100.64.10.10/datasets") {
		t.Fatalf("降级后的远端配置 = %q %#v", credential, fstab)
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10"} {
		if strings.Contains(text, address) {
			t.Fatalf("降级成功泄漏地址 %s：%s", address, text)
		}
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("降级成功审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "virtual-network-fallback" ||
		summary["network_name"] != "办公虚拟局域网" ||
		fmt.Sprintf("%v", summary["mount_notes"]) != fmt.Sprintf("%v", output["mount_notes"]) {
		t.Fatalf("降级成功审计摘要 = %#v", summary)
	}
	if strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "100.64.10.10") ||
		strings.Contains(rows[0].Command, mountShareTestPassword) {
		t.Fatalf("降级成功审计泄漏敏感信息：%s", rows[0].Command)
	}
}

func callMountShare(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (map[string]any, string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "mount_share", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(result)
	output := map[string]any{}
	if !result.IsError {
		if err := json.Unmarshal([]byte(text), &output); err != nil {
			t.Fatalf("解析 mount_share 输出失败：%v\n%s", err, text)
		}
	}
	return output, text, result.IsError
}

func TestMountShareLinuxFirstMountSelectsPhysicalNetwork(t *testing.T) {
	fixture := newMountShareFixture(t)
	if err := fixture.store.AddPolicyRule(policy.CustomRule{
		ID:          "MOUNT-SHARE-POLICY-LEAK",
		Shells:      []models.ShellType{models.ShellPOSIX},
		Regex:       `SSHMGR-LINUX-MOUNT|mount -t cifs`,
		Disposition: policy.DispositionBlock,
		Explanation: "测试规则：若固定挂载命令进入命令策略，这次调用必须被拦截",
	}); err != nil {
		t.Fatal(err)
	}

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("mount_share 失败：%s", text)
	}
	if output["mount_point"] != "/mnt/sshmgr/datasets" || output["version"] != float64(1) ||
		output["network_name"] != "办公物理局域网" || output["network_sequence"] != float64(1) {
		t.Fatalf("首次挂载输出 = %#v", output)
	}
	paths, ok := output["paths"].([]any)
	if !ok || len(paths) != 2 ||
		paths[0].(map[string]any)["full_path"] != "/mnt/sshmgr/datasets/tmp" ||
		paths[1].(map[string]any)["full_path"] != "/mnt/sshmgr/datasets/datasets" {
		t.Fatalf("目录约定路径 = %#v", output["paths"])
	}

	commands, inputs, credential, fstab := fixture.fake.recorded()
	remoteAll := strings.Join(commands, "\n")
	if len(commands) != 3 || !strings.Contains(commands[1], "192.0.2.10") ||
		!strings.Contains(commands[1], "100.64.10.10") ||
		strings.Index(commands[1], "192.0.2.10") > strings.Index(commands[1], "100.64.10.10") {
		t.Fatalf("候选探测命令应一次跑完并保持物理局域网在前：%s", commands[1])
	}
	if !strings.Contains(commands[2], "192.0.2.10") || strings.Contains(commands[2], "100.64.10.10") {
		t.Fatalf("应只把物理局域网地址写入挂载固定命令：%s", commands[2])
	}
	for _, want := range []string{
		"_netdev", "nofail", "x-systemd.automount", "x-systemd.mount-timeout=10",
		"/etc/sshmgr/shares/" + fixture.shareID + ".credentials", "systemctl daemon-reload",
		`install -d -m 755 "$mount_point"`,
		"install -d -m 700 '/etc/sshmgr/shares'\ninstall -d -m 755 '/mnt/sshmgr'",
	} {
		if !strings.Contains(remoteAll, want) {
			t.Fatalf("远端固定命令缺少 %q：%s", want, remoteAll)
		}
	}
	if len(inputs) != 3 || string(inputs[2]) != mountShareTestPassword+"\n" {
		t.Fatalf("提权命令标准输入 = %q，全部输入数 = %d", inputs, len(inputs))
	}
	if credential != "username=mount-user\npassword="+mountShareTestPassword+"\n" {
		t.Fatalf("最终凭据文件 = %q", credential)
	}
	if len(fstab) != 1 || !strings.Contains(fstab[0], "vers=3.1.1") {
		t.Fatalf("fstab 行 = %#v", fstab)
	}
	for _, leakText := range []string{text, remoteAll} {
		if strings.Contains(leakText, mountShareTestPassword) {
			t.Fatalf("挂载密码泄漏：%s", leakText)
		}
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10"} {
		if strings.Contains(text, address) {
			t.Fatalf("工具结果泄漏地址 %s：%s", address, text)
		}
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" || rows[0].ServerID != fixture.targetID {
		t.Fatalf("mount-share 审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" || summary["version"] != float64(1) ||
		summary["network_name"] != "办公物理局域网" || summary["network_sequence"] != float64(1) ||
		summary["result"] != "first-mount-success" {
		t.Fatalf("mount-share 审计摘要 = %#v", summary)
	}
	if strings.Contains(rows[0].Command, mountShareTestPassword) ||
		strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "100.64.10.10") {
		t.Fatalf("mount-share 审计泄漏密码或地址：%s", rows[0].Command)
	}

	toolList, err := fixture.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var description string
	for _, tool := range toolList.Tools {
		if tool.Name == "mount_share" {
			description = tool.Description
		}
	}
	for _, want := range []string{"idempotent", "cannot unmount", "Windows servers do not need it"} {
		if !strings.Contains(description, want) {
			t.Fatalf("mount_share 描述缺少 %q：%s", want, description)
		}
	}
}

func TestMountShareLinuxSMBVersionFallsBackTo20(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.workingVersion = "2.0"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("协议降级到 2.0 后应成功：%s", text)
	}
	if output["network_name"] != "办公物理局域网" {
		t.Fatalf("协议降级输出 = %#v", output)
	}
	_, _, _, fstab := fixture.fake.recorded()
	if len(fstab) != 1 || !strings.Contains(fstab[0], "vers=2.0") {
		t.Fatalf("协议降级后的 fstab = %#v", fstab)
	}
	commands, _, _, _ := fixture.fake.recorded()
	if !strings.Contains(commands[2], "for sshmgr_version in 3.1.1 3.0 2.1 2.0; do") {
		t.Fatalf("协议版本必须按 3.1.1、3.0、2.1、2.0 依次尝试：%s", commands[2])
	}
	if strings.Contains(text, "2.0") {
		t.Fatalf("试出的 SMB 协议版本不应回传：%s", text)
	}
}

func TestMountShareLinuxAllSMBVersionsFailWritesNoFstab(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.workingVersion = "none"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置" {
		t.Fatalf("全部协议失败的报错 = %q (%v)", text, output)
	}
	_, _, credential, fstab := fixture.fake.recorded()
	if credential != "" || len(fstab) != 0 {
		t.Fatalf("全部协议失败后不应留下配置：credential=%q fstab=%v", credential, fstab)
	}
}

func TestMountShareLinuxMissingSudoAndMountCIFS(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*mountShareFixtureSetup)
		wantErr string
	}{
		{
			name:    "缺少 sudo 凭据",
			setup:   func(setup *mountShareFixtureSetup) { setup.sudo = false },
			wantErr: "服务器条目没有配置 sudo 凭据，请先由 owner 配置 sudo",
		},
		{
			name:    "缺少 mount.cifs",
			setup:   func(setup *mountShareFixtureSetup) { setup.missingMountCIFS = true },
			wantErr: "目标服务器缺少 mount.cifs，请先由 owner 安装 cifs-utils；挂载工具不会自动安装",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMountShareFixture(t, testCase.setup)
			output, text, isError := callMountShare(t, fixture.session, map[string]any{
				"share": "datasets", "server_id": fixture.targetID,
			})
			if !isError || text != testCase.wantErr {
				t.Fatalf("报错 = %q (%v)，期待 %q", text, output, testCase.wantErr)
			}
			commands, inputs, credential, fstab := fixture.fake.recorded()
			for _, leakText := range []string{text, strings.Join(commands, "\n"), credential, strings.Join(fstab, "\n")} {
				if strings.Contains(leakText, mountShareTestPassword) {
					t.Fatalf("失败路径泄漏挂载密码：%s", leakText)
				}
			}
			if testCase.name == "缺少 sudo 凭据" &&
				(len(commands) != 1 || strings.TrimSpace(commands[0]) != "uname -s") {
				t.Fatalf("缺少 sudo 时应先识别系统并立即停止：%v", commands)
			}
			if testCase.name == "缺少 mount.cifs" && (len(inputs) != 3 || credential != "") {
				t.Fatalf("缺少 mount.cifs 时不应生成凭据文件：inputs=%q credential=%q", inputs, credential)
			}
			rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != "error" {
				t.Fatalf("失败审计 = %+v", rows)
			}
		})
	}
}

func TestMountShareAuthorizationFailures(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*mountShareFixtureSetup)
		share   string
		wantErr string
	}{
		{
			name:  "服务器不在 profile 内",
			share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.targetOutOfProfile = true
			},
			wantErr: "共享存储或服务器不存在",
		},
		{
			name:  "共享对 profile 不可见",
			share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.shareInvisible = true
			},
			wantErr: "共享存储或服务器不存在",
		},
		{
			name:  "服务器不在可写入列表内",
			share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.notWritable = true
			},
			wantErr: "服务器不在挂载密码机密项的可写入服务器列表内，请先由 owner 把服务器加入该机密项的可写入服务器列表",
		},
		{
			name:  "Windows 服务器",
			share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.windowsTarget = true
			},
			wantErr: "Windows 服务器不能持久挂载共享存储；网络盘符由每条命令执行前的会话内注入提供，不需要 mount_share",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMountShareFixture(t, testCase.setup)
			output, text, isError := callMountShare(t, fixture.session, map[string]any{
				"share": testCase.share, "server_id": fixture.targetID,
			})
			if !isError || text != testCase.wantErr {
				t.Fatalf("报错 = %q (%v)，期待 %q", text, output, testCase.wantErr)
			}
			commands, _, credential, fstab := fixture.fake.recorded()
			if len(commands) != 0 || credential != "" || len(fstab) != 0 {
				t.Fatalf("授权失败不应触达目标服务器：%v %q %v", commands, credential, fstab)
			}
			rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != "error" ||
				strings.Contains(rows[0].Command, mountShareTestPassword) {
				t.Fatalf("失败审计 = %+v", rows)
			}
		})
	}
}
