package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// TestQueryTopologyToolRegistration 覆盖工具注册面：agent 必须能在工具清
// 单里看到 query_topology，并从说明知道它只读本地登记与观察、不会主动
// 探测，现场结论应改调 probe_connectivity。
func TestQueryTopologyToolRegistration(t *testing.T) {
	st := newStore(t)
	server, tunnels, tasks, err := NewServer(st, "profile-query", "project-query")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)

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

	toolList, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range toolList.Tools {
		if tool.Name != "query_topology" {
			continue
		}
		for _, wanted := range []string{"只读", "不主动探测", "probe_connectivity"} {
			if !strings.Contains(tool.Description, wanted) {
				t.Fatalf("query_topology 说明缺少 %q：%s", wanted, tool.Description)
			}
		}
		return
	}
	t.Fatalf("工具清单缺少 query_topology：%+v", toolList.Tools)
}

// queryTopologyFixture 是 query_topology 的单机部署进程内夹具：本机设备
// 同时在物理局域网与虚拟局域网，工作设备同在这两类网络，NAS 只在虚拟局
// 域网，另有无共同网络设备、授权外设备与未关联设备的服务器条目。
type queryTopologyFixture struct {
	store          *store.Store
	profileID      string
	unregisteredID string
	session        *mcp.ClientSession
}

func newQueryTopologyFixture(t *testing.T) *queryTopologyFixture {
	t.Helper()
	vaultDirectory := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDirectory, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDirectory, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	for _, network := range []models.Network{
		{Name: "家A物理", Type: models.NetworkPhysical, SiteName: "家A"},
		{Name: "虚拟A", Type: models.NetworkVirtual},
		{Name: "虚拟B", Type: models.NetworkVirtual},
	} {
		if _, err := st.AddNetwork(network); err != nil {
			t.Fatal(err)
		}
	}
	localDevice, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(localDevice.ID); err != nil {
		t.Fatal(err)
	}
	for _, deviceName := range []string{"工作设备", "NAS", "无观察设备", "旧观察设备", "隔离设备", "授权外设备"} {
		if _, err := st.AddDevice(deviceName, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"笔记本", "家A物理", "192.0.2.1"},
		{"笔记本", "虚拟A", "100.64.0.1"},
		{"工作设备", "家A物理", "192.0.2.2"},
		{"工作设备", "虚拟A", "100.64.0.2"},
		{"NAS", "虚拟A", "100.64.0.3"},
		{"无观察设备", "家A物理", "192.0.2.4"},
		{"旧观察设备", "家A物理", "192.0.2.6"},
		{"隔离设备", "虚拟B", "100.64.1.4"},
		{"授权外设备", "家A物理", "192.0.2.5"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	profileID, err := st.AddProfile("query")
	if err != nil {
		t.Fatal(err)
	}
	granted := make([]string, 0, 4)
	for _, server := range []struct {
		name, device string
	}{
		{name: "入口一", device: "工作设备"},
		{name: "入口二", device: "工作设备"},
		{name: "外部入口", device: "授权外设备"},
	} {
		result, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: server.name, Host: "192.0.2.100", Port: 22, User: "deploy",
		}, nil, nil, server.device)
		if err != nil {
			t.Fatal(err)
		}
		if server.device != "授权外设备" {
			granted = append(granted, result.ServerID)
		}
	}
	unregisteredID, err := st.AddServer(&models.Server{
		Name: "未登记入口", Host: "192.0.2.101", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st),
	})
	if err != nil {
		t.Fatal(err)
	}
	granted = append(granted, unregisteredID)
	if err := st.GrantServers(profileID, granted); err != nil {
		t.Fatal(err)
	}

	server, tunnels, tasks, err := NewServer(st, profileID, "project-query")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
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
	return &queryTopologyFixture{
		store: st, profileID: profileID,
		unregisteredID: unregisteredID, session: clientSession,
	}
}

func (f *queryTopologyFixture) callQuery(t *testing.T, arguments map[string]any) (string, bool) {
	t.Helper()
	result, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query_topology", Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("调用 query_topology：%v", err)
	}
	var text string
	for _, content := range result.Content {
		if value, ok := content.(*mcp.TextContent); ok {
			text += value.Text
		}
	}
	return text, result.IsError
}

func (f *queryTopologyFixture) recordObservation(t *testing.T, targetDevice, network, address string, age time.Duration, ok bool) {
	t.Helper()
	path, found, err := probeobserve.PathFor("")
	if err != nil || !found {
		t.Fatalf("解析本地探测结果文件：%v found=%v", err, found)
	}
	target, err := f.store.GetDevice(targetDevice)
	if err != nil {
		t.Fatal(err)
	}
	localDevice, err := f.store.GetDevice("笔记本")
	if err != nil {
		t.Fatal(err)
	}
	if err := proberesults.NewFileProbeResults(path).Record(proberesults.Observation{
		OriginDeviceID: localDevice.ID, TargetDeviceID: target.ID,
		Network: network, Address: address, Port: 22,
		OK: ok, Duration: 4 * time.Millisecond,
		ObservedAt: time.Now().Add(-age), Source: proberesults.SourceProbe,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestQueryTopologySelfGroups 覆盖四类邻居、同设备多条目、授权裁剪、无观
// 察空缺、新旧观察年龄，以及只读不变量。
func TestQueryTopologySelfGroups(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	fixture.recordObservation(t, "工作设备", "家A物理", "192.0.2.200", 4*time.Minute, true)
	fixture.recordObservation(t, "NAS", "虚拟A", "192.0.2.200", 2*time.Hour, true)
	fixture.recordObservation(t, "旧观察设备", "家A物理", "192.0.2.200", 3*24*time.Hour, true)
	fixture.recordObservation(t, "NAS", "虚拟A", "192.0.2.201", 8*24*time.Hour, true)
	observationPath, found, err := probeobserve.PathFor("")
	if err != nil || !found {
		t.Fatalf("解析本地探测结果文件：%v found=%v", err, found)
	}
	observationBlobBefore, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(observationBlobBefore), "192.0.2.201") {
		t.Fatalf("测试预置的过期观察不存在：%s", observationBlobBefore)
	}
	revision, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	auditRowsBefore, err := fixture.store.AuditRows(1000)
	if err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callQuery(t, map[string]any{"from": "self"})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	want := strings.Join([]string{
		"出发点：笔记本",
		"所属网络：家A物理（物理局域网，站点：家A）、虚拟A（虚拟局域网）",
		"同在物理局域网：",
		"  家A物理",
		"    工作设备 [入口一、入口二] ✓4ms 4m前",
		"    无观察设备",
		"    旧观察设备 ✓4ms 3d前 旧",
		"只能经虚拟局域网到达：",
		"  虚拟A",
		"    NAS ✓4ms 2h前 旧",
		"无共同网络不可达：隔离设备",
		"未登记服务器：" + fixture.unregisteredID + "（未登记入口）",
		"本工具不主动探测；需要现场结论请调用 probe_connectivity。",
		"",
	}, "\n")
	if text != want {
		t.Fatalf("分组输出不符\n得到：\n%s\n期待：\n%s", text, want)
	}
	for _, forbidden := range []string{"外部入口", "授权外设备", "192.0.2", "100.64"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("分组输出不应包含 %q：%s", forbidden, text)
		}
	}
	afterRevision, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision != revision {
		t.Fatalf("快照修订号从 %d 变为 %d", revision, afterRevision)
	}
	auditRowsAfter, err := fixture.store.AuditRows(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRowsAfter) != len(auditRowsBefore) {
		t.Fatalf("查询不应写审计：查询前 %d 行，查询后 %d 行", len(auditRowsBefore), len(auditRowsAfter))
	}
	observationBlobAfter, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(observationBlobAfter) != string(observationBlobBefore) {
		t.Fatal("查询不应清理或改写本地探测结果文件")
	}
}

// TestQueryTopologyOriginRejections 覆盖 from 必填、服务器标识符先授权检
// 查再换算设备、未知或授权范围外出发点拒绝，以及 self 找不到本机设备时
// 提示 owner 补登记。
func TestQueryTopologyOriginRejections(t *testing.T) {
	fixture := newQueryTopologyFixture(t)

	originServer, err := fixture.store.GetServerByName("入口一")
	if err != nil {
		t.Fatal(err)
	}
	text, isError := fixture.callQuery(t, map[string]any{"from": originServer.ID})
	if isError || !strings.Contains(text, "出发点：工作设备") {
		t.Fatalf("授权内服务器起点应换算到所属设备：%q error=%v", text, isError)
	}

	external, err := fixture.store.GetServerByName("外部入口")
	if err != nil {
		t.Fatal(err)
	}
	missingResult, missingErr := fixture.session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query_topology", Arguments: map[string]any{},
	})
	if missingErr == nil || !strings.Contains(missingErr.Error(), "from") || missingResult != nil {
		t.Fatalf("缺少 from 应在参数校验阶段拒绝：err=%v result=%+v", missingErr, missingResult)
	}
	for _, arguments := range []map[string]any{
		{"from": external.ID},
		{"from": "not-a-server"},
	} {
		text, isError = fixture.callQuery(t, arguments)
		if !isError {
			t.Fatalf("出发点参数 %v 应返回工具错误，实际：%q", arguments, text)
		}
	}
	if text == "" {
		t.Fatal("拒绝路径应返回错误文本")
	}

	if _, err := roles.ClearLocalDevice(); err != nil {
		t.Fatal(err)
	}
	text, isError = fixture.callQuery(t, map[string]any{"from": "self"})
	if !isError || !strings.Contains(text, "本机未登记设备，请 owner 补登记") {
		t.Fatalf("self 找不到设备时的错误 = %q error=%v", text, isError)
	}
}

// TestQueryTopologyRepeatedVirtualNetworks 确认只共同处于多个虚拟局域网的
// 设备在每个虚拟局域网下各出现一行，且虚拟局域网按名字稳定排序。
func TestQueryTopologyRepeatedVirtualNetworks(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	if _, err := fixture.store.AddNetworkMembership("笔记本", "虚拟B", "100.64.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddDevice("双虚拟设备", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct{ network, address string }{
		{"虚拟A", "100.64.0.6"},
		{"虚拟B", "100.64.1.6"},
	} {
		if _, err := fixture.store.AddNetworkMembership("双虚拟设备", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	text, isError := fixture.callQuery(t, map[string]any{"from": "self"})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	want := strings.Join([]string{
		"只能经虚拟局域网到达：",
		"  虚拟A",
		"    NAS",
		"    双虚拟设备",
		"  虚拟B",
		"    双虚拟设备",
	}, "\n")
	if !strings.Contains(text, want) {
		t.Fatalf("多个虚拟局域网分组不符\n得到：\n%s\n期待片段：\n%s", text, want)
	}
}

// TestQueryTopologyPairDetailsDefault 覆盖多目标逐对输出：服务器目标换算
// 到设备并列出全部共同网络，不受管目标按设备名解析；无共同网络与未关联
// 设备分别给出明确结论，且不再输出全局邻居分组。
func TestQueryTopologyPairDetailsDefault(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	workServer, err := fixture.store.GetServerByName("入口一")
	if err != nil {
		t.Fatal(err)
	}
	fixture.recordObservation(t, "工作设备", "家A物理", "192.0.2.200", 4*time.Minute, true)
	fixture.recordObservation(t, "NAS", "虚拟A", "192.0.2.200", 2*time.Hour, true)
	observationPath, found, err := probeobserve.PathFor("")
	if err != nil || !found {
		t.Fatalf("解析本地探测结果文件：%v found=%v", err, found)
	}
	observationBefore, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := fixture.store.AuditRows(1000)
	if err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callQuery(t, map[string]any{
		"from": "self",
		"to":   []string{workServer.ID, "NAS", "隔离设备", fixture.unregisteredID},
	})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	want := strings.Join([]string{
		"出发点：笔记本",
		"目标：工作设备 [入口一、入口二]",
		"  家A物理（实际会选） ✓4ms 4m前",
		"  虚拟A",
		"目标：NAS",
		"  虚拟A ✓4ms 2h前 旧",
		"目标：隔离设备",
		"  无共同网络，不可达",
		"目标：" + fixture.unregisteredID + "（未登记入口）",
		"  服务器未关联拓扑设备，未登记",
		"本工具不主动探测；需要现场结论请调用 probe_connectivity。",
		"",
	}, "\n")
	if text != want {
		t.Fatalf("逐对输出不符\n得到：\n%s\n期待：\n%s", text, want)
	}
	observationAfter, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(observationAfter) != string(observationBefore) {
		t.Fatal("逐对查询不应清理或改写本地探测结果文件")
	}
	revisionAfter, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revision {
		t.Fatalf("快照修订号从 %d 变为 %d", revision, revisionAfter)
	}
	auditAfter, err := fixture.store.AuditRows(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditBefore) {
		t.Fatalf("逐对查询不应写审计：查询前 %d 行，查询后 %d 行", len(auditBefore), len(auditAfter))
	}
}

// TestQueryTopologyPairActualSelectionUsesDialSuccessCache 确认服务器目标
// 的「实际会选」沿用统一拨号候选规划：同级网络里仍有效的成功地址会排到
// 登记顺序之前；查询本身不改变成功地址缓存。
func TestQueryTopologyPairActualSelectionUsesDialSuccessCache(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	if _, err := fixture.store.AddSite("家B", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddNetwork(models.Network{
		Name: "家B物理", Type: models.NetworkPhysical, SiteName: "家B",
	}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct{ device, address string }{
		{"笔记本", "192.0.2.11"},
		{"工作设备", "192.0.2.12"},
	} {
		if _, err := fixture.store.AddNetworkMembership(
			membership.device, "家B物理", membership.address,
		); err != nil {
			t.Fatal(err)
		}
	}
	workServer, err := fixture.store.GetServerByName("入口一")
	if err != nil {
		t.Fatal(err)
	}
	localDevicePath, err := roles.LocalDevicePath()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(filepath.Dir(localDevicePath), "ssh-dial-successes.json")
	cache := sshbroker.NewFileDialSuccessCache(cachePath)
	if err := cache.Record(workServer, sshbroker.DialSuccessRecord{
		Address: "192.0.2.12", SucceededAt: time.Now(),
		Duration: 20 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callQuery(t, map[string]any{
		"from": "self", "to": []string{workServer.ID},
	})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	if !strings.Contains(text, "家B物理（实际会选）") || strings.Contains(text, "家A物理（实际会选）") {
		t.Fatalf("成功地址缓存应让同级网络家B物理排在前面：\n%s", text)
	}
	after, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("query_topology 不应改写成功地址缓存")
	}
}

// TestQueryTopologyPairDetail 覆盖展开档：设备角色、网络成员地址、标称带
// 宽与是否按流量计费；服务器条目的 ExposeHost 决定地址明文或隐藏，不受
// 管设备地址始终隐藏，网段不进入输出。
func TestQueryTopologyPairDetail(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	physicalBandwidth := int64(1000)
	physicalMetered := false
	if _, err := fixture.store.EditNetwork("家A物理", store.NetworkChanges{
		BandwidthMbps: &physicalBandwidth, Metered: &physicalMetered,
	}); err != nil {
		t.Fatal(err)
	}
	virtualBandwidth := int64(0)
	virtualMetered := true
	if _, err := fixture.store.EditNetwork("虚拟A", store.NetworkChanges{
		BandwidthMbps: &virtualBandwidth, Metered: &virtualMetered,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddDevice("可见设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddNetworkMembership("可见设备", "家A物理", "192.0.2.21"); err != nil {
		t.Fatal(err)
	}
	visibleResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "可见入口", Host: "192.0.2.121", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ExposeHost: true,
	}, nil, nil, "可见设备")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.GrantServers(fixture.profileID, []string{visibleResult.ServerID}); err != nil {
		t.Fatal(err)
	}
	workServer, err := fixture.store.GetServerByName("入口一")
	if err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callQuery(t, map[string]any{
		"from": "self", "detail": true,
		"to": []string{workServer.ID, visibleResult.ServerID, "NAS"},
	})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	for _, wanted := range []string{
		"角色：受管设备（入口一、入口二）",
		"家A物理（实际会选） 地址：隐藏；标称带宽：1000 Mbit/s；不按流量计费",
		"虚拟A 地址：隐藏；标称带宽：未登记；按流量计费",
		"角色：不受管设备",
		"家A物理（实际会选） 地址：192.0.2.21；标称带宽：1000 Mbit/s；不按流量计费",
	} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("展开档缺少 %q：\n%s", wanted, text)
		}
	}
	for _, forbidden := range []string{"192.0.2.2\n", "100.64.0.", "网段："} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("展开档不应包含 %q：\n%s", forbidden, text)
		}
	}
}

// TestQueryTopologyGroupDetailExposeHostCombination 覆盖分组展开档：同一
// 设备多个已授权条目里任一条目开启 ExposeHost 就显示地址，全部关闭则隐
// 藏；不受管设备地址始终隐藏。
func TestQueryTopologyGroupDetailExposeHostCombination(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	second, err := fixture.store.GetServerByName("入口二")
	if err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callQuery(t, map[string]any{"from": "self", "detail": true})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	if !strings.Contains(text, "工作设备 [入口一、入口二] 角色：受管设备（入口一、入口二） 家A物理 地址：隐藏") ||
		!strings.Contains(text, "NAS 角色：不受管设备 虚拟A 地址：隐藏") {
		t.Fatalf("全部条目关闭 ExposeHost 时分组地址应隐藏：\n%s", text)
	}

	second.ExposeHost = true
	if err := fixture.store.UpdateServer(second); err != nil {
		t.Fatal(err)
	}
	text, isError = fixture.callQuery(t, map[string]any{"from": "self", "detail": true})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	if !strings.Contains(text, "工作设备 [入口一、入口二] 角色：受管设备（入口一、入口二） 家A物理 地址：192.0.2.2；标称带宽：未登记；不按流量计费") ||
		!strings.Contains(text, "NAS 角色：不受管设备 虚拟A 地址：隐藏；标称带宽：未登记；不按流量计费") {
		t.Fatalf("任一条目开启 ExposeHost 时分组地址显示不正确：\n%s", text)
	}
}

// TestQueryTopologyPairTargetRejections 覆盖目标拒绝与去重习惯：未知、授权
// 范围外服务器编号、按设备名指称的授权外受管设备都返回同一种错误，不泄
// 露条目或设备存在；重复目标只呈现一次。
func TestQueryTopologyPairTargetRejections(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	external, err := fixture.store.GetServerByName("外部入口")
	if err != nil {
		t.Fatal(err)
	}
	workServer, err := fixture.store.GetServerByName("入口一")
	if err != nil {
		t.Fatal(err)
	}

	var firstError string
	for _, target := range []string{external.ID, "not-a-target", "授权外设备"} {
		text, isError := fixture.callQuery(t, map[string]any{
			"from": "self", "to": []string{workServer.ID, target},
		})
		if !isError {
			t.Fatalf("目标 %s 应返回工具错误，实际：%q", target, text)
		}
		if strings.Contains(text, "外部入口") || strings.Contains(text, "授权外设备") || strings.Contains(text, "192.0.2.5") {
			t.Fatalf("目标 %s 的错误泄露了范围外对象：%s", target, text)
		}
		if firstError == "" {
			firstError = text
			continue
		}
		if text != firstError {
			t.Fatalf("未知与授权范围外目标应返回一致错误：%q != %q", text, firstError)
		}
	}

	text, isError := fixture.callQuery(t, map[string]any{
		"from": "self", "to": []string{workServer.ID, workServer.ID},
	})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	if strings.Count(text, "目标：工作设备 [入口一、入口二]") != 1 {
		t.Fatalf("重复目标应去重后只呈现一次：\n%s", text)
	}
}

// TestQueryTopologyIgnoresExpiredObservationWithoutRewrite 确认查询把超过
// 保留期的观察当作没有观察，并且不通过「清理过期记录」改写本地文件。
func TestQueryTopologyIgnoresExpiredObservationWithoutRewrite(t *testing.T) {
	fixture := newQueryTopologyFixture(t)
	fixture.recordObservation(t, "NAS", "虚拟A", "192.0.2.200", 8*24*time.Hour, true)
	observationPath, found, err := probeobserve.PathFor("")
	if err != nil || !found {
		t.Fatalf("解析本地探测结果文件：%v found=%v", err, found)
	}
	before, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callQuery(t, map[string]any{"from": "self"})
	if isError {
		t.Fatalf("query_topology 失败：%s", text)
	}
	if !strings.Contains(text, "    NAS\n") {
		t.Fatalf("超过保留期的观察应按无观察呈现：\n%s", text)
	}
	after, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("查询不应清理或改写本地探测结果文件")
	}
}

// TestQueryTopologyClientCacheAndOldAuthority 走权威端按授权范围导出、客
// 户端缓存导入、进程内 MCP 调用的完整路径，并覆盖旧权威端快照与客户端实
// 例未记录本机设备两个拒绝分支。
func TestQueryTopologyClientCacheAndOldAuthority(t *testing.T) {
	source := newQueryTopologyFixture(t)
	_, projectToken, err := source.store.AddProject("拓扑查询项目", source.profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.store.ExportSnapshotForProfile(source.profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}

	cacheDirectory := t.TempDir()
	auditPath := filepath.Join(t.TempDir(), "query-topology-audit.log")
	broker, tunnels, tasks, cleanup, err := NewCacheBroker(
		projectToken, snapshot, auditPath, nil, nil, "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := broker.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	t.Setenv("SSHMGR_CACHE_DIR", cacheDirectory)
	t.Setenv("SSHMGR_CONFIG_ROOT", t.TempDir())
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "client-has-no-vault.db"))
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	localDevice, err := source.store.GetDevice("笔记本")
	if err != nil {
		t.Fatal(err)
	}
	metaBlob, err := json.Marshal(map[string]string{"device_id": localDevice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDirectory, "cache.meta.json"), metaBlob, 0o600); err != nil {
		t.Fatal(err)
	}

	call := func(arguments map[string]any) (string, bool) {
		t.Helper()
		result, callErr := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "query_topology", Arguments: arguments,
		})
		if callErr != nil {
			t.Fatalf("调用客户端 query_topology：%v", callErr)
		}
		var text string
		for _, content := range result.Content {
			if value, ok := content.(*mcp.TextContent); ok {
				text += value.Text
			}
		}
		return text, result.IsError
	}

	text, isError := call(map[string]any{"from": "self"})
	if isError || !strings.Contains(text, "出发点：笔记本") ||
		!strings.Contains(text, "只能经虚拟局域网到达：") ||
		strings.Contains(text, "授权外设备") {
		t.Fatalf("客户端缓存分组结果 = %q error=%v", text, isError)
	}
	auditBlob, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditBlob) != 0 {
		t.Fatalf("客户端查询不应写审计：%s", auditBlob)
	}

	emptyMeta, err := json.Marshal(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDirectory, "cache.meta.json"), emptyMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	text, isError = call(map[string]any{"from": "self"})
	if !isError || !strings.Contains(text, "本机未登记设备，请 owner 补登记") {
		t.Fatalf("客户端实例未记录设备时的错误 = %q error=%v", text, isError)
	}

	oldSnapshot, err := source.store.ExportSnapshotForProfile(source.profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	oldSnapshot.RecommendedVersion = ""
	oldBroker, oldTunnels, oldTasks, oldCleanup, err := NewCacheBroker(
		projectToken, oldSnapshot, filepath.Join(t.TempDir(), "old-authority-audit.log"),
		nil, nil, "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oldCleanup)
	t.Cleanup(oldTunnels.CloseAll)
	t.Cleanup(oldTasks.CloseAll)
	oldClient := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	oldServerTransport, oldClientTransport := mcp.NewInMemoryTransports()
	oldServerSession, err := oldBroker.Connect(context.Background(), oldServerTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oldServerSession.Close() })
	oldClientSession, err := oldClient.Connect(context.Background(), oldClientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oldClientSession.Close() })
	result, err := oldClientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query_topology", Arguments: map[string]any{"from": "self"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var oldText string
	for _, content := range result.Content {
		if value, ok := content.(*mcp.TextContent); ok {
			oldText += value.Text
		}
	}
	want := "权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端"
	if !result.IsError || oldText != want {
		t.Fatalf("旧权威端错误 = %q error=%v，期待 %q", oldText, result.IsError, want)
	}
}
