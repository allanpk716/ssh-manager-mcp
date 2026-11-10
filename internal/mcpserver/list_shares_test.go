package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type listSharesFixture struct {
	source        *store.Store
	profileID     string
	projectToken  string
	laptopTokenID string
	linuxWebID    string
	windowsWebID  string
	windowsCmdID  string
}

func seedListSharesVault(t *testing.T) listSharesFixture {
	t.Helper()
	source := newStore(t)
	profileID, err := source.AddProfile("共享存储agent")
	if err != nil {
		t.Fatal(err)
	}
	_, projectToken, err := source.AddProject("共享存储项目", profileID)
	if err != nil {
		t.Fatal(err)
	}
	laptopTokenID, _, err := source.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("NAS", "存放共享数据", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("DATASETS_MOUNT_PASSWORD", "数据集共享的挂载密码",
		[]byte("datasets-mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("SCRATCH_MOUNT_PASSWORD", "临时共享的挂载密码",
		[]byte("scratch-mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}

	linuxWebID, err := source.AddServer(&models.Server{
		Name: "linux-web", Host: "192.0.2.20", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	windowsWebID, err := source.AddServer(&models.Server{
		Name: "windows-web", Host: "192.0.2.21", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	windowsCmdID, err := source.AddServer(&models.Server{
		Name: "windows-cmd", Host: "192.0.2.24", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellWindowsCmd,
	})
	if err != nil {
		t.Fatal(err)
	}
	// outsider 在机密项可写入列表里、但不在 profile 授权内：单机工具面必须把它
	// 从会话可见范围裁掉。
	if _, err = source.AddServer(&models.Server{
		Name: "outsider", Host: "192.0.2.22", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = source.AddServer(&models.Server{
		Name: "island", Host: "192.0.2.23", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = source.AddServerWithCredentialsToDevice(&models.Server{
		Name: "nas-provider", Host: "192.0.2.25", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	}, nil, nil, "NAS"); err != nil {
		t.Fatal(err)
	}
	if err := source.GrantServers(profileID, []string{linuxWebID, windowsWebID, windowsCmdID}); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"},
		[]string{"linux-web", "windows-web", "windows-cmd", "outsider"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"SCRATCH_MOUNT_PASSWORD"},
		[]string{"island"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "数据集", Path: "datasets"},
		},
		Note: "模型数据集与临时数据",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	changeNote := "扩大了可挂载范围"
	if _, err := source.EditSharedStorage("datasets", store.SharedStorageChanges{
		ChangeNote: &changeNote,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "scratch", Device: "NAS", User: "scratch-user",
		PasswordSecret: "SCRATCH_MOUNT_PASSWORD", Drive: "Y",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	return listSharesFixture{
		source: source, profileID: profileID, projectToken: projectToken,
		laptopTokenID: laptopTokenID, linuxWebID: linuxWebID, windowsWebID: windowsWebID,
		windowsCmdID: windowsCmdID,
	}
}

func connectListSharesClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callListShares(t *testing.T, session *mcp.ClientSession, args map[string]any) (ListSharesOutput, string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_shares",
		Arguments: args,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}
	if result.IsError {
		return ListSharesOutput{}, text, true
	}
	var output ListSharesOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("解析 list_shares 输出失败：%v\n%s", err, text)
	}
	return output, text, false
}

// TestListSharesSingleVaultReturnsRegistration 检查单机工具面：每个共享只
// 返回登记字段与按服务器推算的挂载点，可挂载范围被会话可见服务器裁剪，
// 结果不含挂载密码，并带固定的「不检测挂载」说明。
func TestListSharesSingleVaultReturnsRegistration(t *testing.T) {
	fixture := seedListSharesVault(t)
	server, tunnels, tasks, err := NewServer(fixture.source, fixture.profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tunnels.CloseAll()
		tasks.CloseAll()
	})
	session := connectListSharesClient(t, server)

	output, text, isError := callListShares(t, session, map[string]any{})
	if isError {
		t.Fatalf("list_shares 失败：%s", text)
	}
	if output.Notice != "挂载点是约定路径，本工具不检测是否已挂载" {
		t.Fatalf("固定说明 = %q", output.Notice)
	}
	want := []SharedStorageInfo{{
		Name:   "datasets",
		Device: "NAS",
		Note:   "模型数据集与临时数据",
		Directories: []SharedStorageDirectoryInfo{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "数据集", Path: "datasets"},
		},
		Drive:      "Z",
		Version:    2,
		ChangeNote: "扩大了可挂载范围",
		Servers: []SharedStorageServerInfo{
			{ServerID: fixture.linuxWebID, Name: "linux-web", MountPoint: "/mnt/sshmgr/datasets"},
			{ServerID: fixture.windowsCmdID, Name: "windows-cmd", MountPoint: "Z:"},
			{ServerID: fixture.windowsWebID, Name: "windows-web", MountPoint: "Z:"},
		},
	}}
	assertListSharesEqual(t, want, output.Shares)
	for _, leaked := range []string{
		"datasets-mount-password", "scratch-mount-password",
		"DATASETS_MOUNT_PASSWORD", "SCRATCH_MOUNT_PASSWORD",
		"mount-user", "scratch-user", "outsider", "192.0.2",
	} {
		if strings.Contains(text, leaked) {
			t.Fatalf("list_shares 不应包含 %q：%s", leaked, text)
		}
	}
}

func assertListSharesEqual(t *testing.T, want, got []SharedStorageInfo) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantEncoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(wantEncoded) {
		t.Fatalf("共享存储列表不符\n得到：%s\n期待：%s", encoded, wantEncoded)
	}
}

func newListSharesCacheBroker(t *testing.T, snapshot *store.Snapshot, projectToken string) *mcp.Server {
	t.Helper()
	broker, tunnels, tasks, cleanup, err := NewCacheBroker(
		projectToken, snapshot, filepath.Join(t.TempDir(), "audit.log"), nil, nil, "laptop", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup()
		tunnels.CloseAll()
		tasks.CloseAll()
	})
	return broker
}

// TestListSharesOldAuthoritySnapshotRefuses 覆盖 ADR 0014 的新旧混跑规则：
// 客户端快照没有推荐版本字段时，说明权威端不支持网络拓扑与共享存储，
// 工具必须直接报错，而不是把缺字段当成零个共享。
func TestListSharesOldAuthoritySnapshotRefuses(t *testing.T) {
	fixture := seedListSharesVault(t)
	snapshot, err := fixture.source.ExportSnapshotForProfile(
		fixture.profileID, true, fixture.laptopTokenID,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.RecommendedVersion = ""
	broker := newListSharesCacheBroker(t, snapshot, fixture.projectToken)
	session := connectListSharesClient(t, broker)

	output, text, isError := callListShares(t, session, map[string]any{})
	if !isError {
		t.Fatalf("旧权威端快照应报错，实际返回：%+v", output)
	}
	want := "权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端"
	if text != want {
		t.Fatalf("报错文本 = %q，期待 %q", text, want)
	}
}

// TestListSharesSurvivesProfileSnapshotRoundTrip 走权威端按客户端 profile
// 裁剪导出、客户端缓存桥装配、工具调用三个现有外部接口。可见范围既来自
// 快照裁剪，也来自会话的 profile 授权；两种裁剪必须同时成立。
func TestListSharesSurvivesProfileSnapshotRoundTrip(t *testing.T) {
	fixture := seedListSharesVault(t)
	snapshot, err := fixture.source.ExportSnapshotForProfile(
		fixture.profileID, true, fixture.laptopTokenID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.SharedStorages) != 1 || snapshot.SharedStorages[0].Name != "datasets" {
		t.Fatalf("客户端快照的共享段 = %+v，应只含与授权范围有交集的 datasets", snapshot.SharedStorages)
	}
	if snapshot.SharedStorages[0].DeviceName != "NAS" {
		t.Fatalf("客户端快照应携带提供方设备名：%+v", snapshot.SharedStorages[0])
	}
	for _, device := range snapshot.Devices {
		if device.Name == "NAS" {
			t.Fatalf("profile 看不到的提供方设备不应进入客户端快照：%+v", snapshot.Devices)
		}
	}
	broker := newListSharesCacheBroker(t, snapshot, fixture.projectToken)
	session := connectListSharesClient(t, broker)

	output, text, isError := callListShares(t, session, map[string]any{})
	if isError {
		t.Fatalf("客户端 list_shares 失败：%s", text)
	}
	assertListSharesEqual(t, []SharedStorageInfo{{
		Name:   "datasets",
		Device: "NAS",
		Note:   "模型数据集与临时数据",
		Directories: []SharedStorageDirectoryInfo{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "数据集", Path: "datasets"},
		},
		Drive:      "Z",
		Version:    2,
		ChangeNote: "扩大了可挂载范围",
		Servers: []SharedStorageServerInfo{
			{ServerID: fixture.linuxWebID, Name: "linux-web", MountPoint: "/mnt/sshmgr/datasets"},
			{ServerID: fixture.windowsCmdID, Name: "windows-cmd", MountPoint: "Z:"},
			{ServerID: fixture.windowsWebID, Name: "windows-web", MountPoint: "Z:"},
		},
	}}, output.Shares)
	if strings.Contains(text, "scratch") || strings.Contains(text, "outsider") {
		t.Fatalf("客户端结果不应包含会话范围外的共享或服务器：%s", text)
	}
}

// TestListSharesServerFilterNarrowsToOneServer 覆盖可选 server 参数：按名字
// 或编号筛选时每个共享只带这一台的挂载点；范围外服务器必须报错，不能静默
// 返回空列表。
func TestListSharesServerFilterNarrowsToOneServer(t *testing.T) {
	fixture := seedListSharesVault(t)
	server, tunnels, tasks, err := NewServer(fixture.source, fixture.profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tunnels.CloseAll()
		tasks.CloseAll()
	})
	session := connectListSharesClient(t, server)

	output, text, isError := callListShares(t, session, map[string]any{"server": "windows-web"})
	if isError {
		t.Fatalf("按服务器筛选失败：%s", text)
	}
	assertListSharesEqual(t, []SharedStorageInfo{{
		Name:   "datasets",
		Device: "NAS",
		Note:   "模型数据集与临时数据",
		Directories: []SharedStorageDirectoryInfo{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "数据集", Path: "datasets"},
		},
		Drive:      "Z",
		Version:    2,
		ChangeNote: "扩大了可挂载范围",
		Servers: []SharedStorageServerInfo{
			{ServerID: fixture.windowsWebID, Name: "windows-web", MountPoint: "Z:"},
		},
	}}, output.Shares)

	byID, text, isError := callListShares(t, session, map[string]any{"server": fixture.linuxWebID})
	if isError {
		t.Fatalf("按服务器编号筛选失败：%s", text)
	}
	if len(byID.Shares) != 1 || len(byID.Shares[0].Servers) != 1 ||
		byID.Shares[0].Servers[0].MountPoint != "/mnt/sshmgr/datasets" {
		t.Fatalf("按编号筛选的结果 = %+v", byID.Shares)
	}

	for _, identifier := range []string{"outsider", "island", "missing"} {
		_, errorText, isError := callListShares(t, session, map[string]any{"server": identifier})
		if !isError {
			t.Fatalf("范围外服务器 %q 应报错", identifier)
		}
		if errorText != ErrNotInProfile.Error() {
			t.Fatalf("范围外服务器 %q 的报错 = %q，期待 %q", identifier, errorText, ErrNotInProfile.Error())
		}
	}
}
