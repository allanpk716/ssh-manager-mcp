package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// probeFixture 是 probe_connectivity 的进程内服务面夹具：单机角色、本机设
// 备「笔记本」、目标设备「NAS」，并返回 MCP 客户端会话与存储。
type probeFixture struct {
	store     *store.Store
	vaultDir  string
	laptopID  string
	profileID string
	session   *mcp.ClientSession
	server    *mcp.ServerSession
}

func newProbeFixture(t *testing.T, grantedServers ...string) *probeFixture {
	t.Helper()
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家B局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "NetBird", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	laptop, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(laptop.ID); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"笔记本", "家A局域网", "127.0.0.2"},
		{"笔记本", "家B局域网", "127.0.0.3"},
		{"笔记本", "NetBird", "127.0.0.4"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	profileID, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if len(grantedServers) > 0 {
		if err := st.GrantServers(profileID, grantedServers); err != nil {
			t.Fatal(err)
		}
	}
	server, manager, tasks, err := NewServer(st, profileID, "proj-probe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.CloseAll()
		tasks.CloseAll()
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return &probeFixture{store: st, vaultDir: vaultDir, laptopID: laptop.ID, profileID: profileID, session: clientSession, server: serverSession}
}

func (f *probeFixture) callProbe(t *testing.T, arguments map[string]any) (string, bool) {
	t.Helper()
	result, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: BrokerTools[15], Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("call probe_connectivity: %v", err)
	}
	var text string
	for _, content := range result.Content {
		if value, ok := content.(*mcp.TextContent); ok {
			text += value.Text
		}
	}
	return text, result.IsError
}

// TestProbeConnectivitySelfOrigin: 从本机起点的成功、失败、超时与分组呈现；
// 审计行动作、来源与摘要；摘要与结果不暴露地址；单机 vault 的快照修订号
// 不变；观察写入本地探测结果文件。
func TestProbeConnectivitySelfOrigin(t *testing.T) {
	fixture := newProbeFixture(t)
	openListener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = openListener.Close() })
	openPort := openListener.Addr().(*net.TCPAddr).Port
	t.Setenv("SSHMGR_SSH_ADDRESS_TIMEOUT", "250ms")
	for _, membership := range []struct {
		network, address string
	}{
		{"家A局域网", "127.0.0.1"},
		{"家B局域网", "240.0.0.1"},
		{"NetBird", "::1"},
	} {
		if _, err := fixture.store.AddNetworkMembership("NAS", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	revision, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	// 家A局域网使用 127.0.0.5（没有监听，连接被拒绝）；家B使用不可路由
	// 地址（连接超时或无路由）；NetBird 使用开放端口所在地址。
	text, isError := fixture.callProbe(t, map[string]any{"from": "self", "targets": []string{"NAS:" + itoa(openPort)}})
	if isError {
		t.Fatalf("tool must return normally:\n%s", text)
	}
	if !strings.Contains(text, "NAS:"+itoa(openPort)+"  家A局域网 第1个地址  ✗连接被拒绝") {
		t.Fatalf("text missing refused line:\n%s", text)
	}
	if !strings.Contains(text, "NAS:"+itoa(openPort)+"  NetBird 第1个地址  ✓") {
		t.Fatalf("text missing successful line:\n%s", text)
	}
	if !strings.Contains(text, "登记与实际不一致：家A局域网（连接被拒绝）") {
		t.Fatalf("text missing mismatch note:\n%s", text)
	}
	if !strings.Contains(text, "物理局域网不通，实际可经 NetBird 到达") {
		t.Fatalf("text missing virtual route note:\n%s", text)
	}
	if strings.Contains(text, "127.0.0.1") || strings.Contains(text, "203.0.113.1") {
		t.Fatalf("text must not expose addresses:\n%s", text)
	}

	rows, err := fixture.store.AuditRows(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v %v", rows, err)
	}
	row := rows[0]
	if row.Action != "probe-connectivity" || row.Source != store.AuditSourceAgent || row.Status != "ok" {
		t.Fatalf("audit row = %+v", row)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(row.Command), &summary); err != nil {
		t.Fatalf("audit summary is not JSON: %q", row.Command)
	}
	if summary["from"] != "self" || !strings.Contains(row.Command, "NAS:") || strings.Contains(row.Command, "127.0.0.1") {
		t.Fatalf("audit summary = %q", row.Command)
	}

	afterRevision, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision != revision {
		t.Fatalf("snapshot revision changed: %d -> %d", revision, afterRevision)
	}

	probePath, ok, err := probeobserve.PathFor("")
	if err != nil || !ok {
		t.Fatalf("probe path = %q %v %v", probePath, ok, err)
	}
	observations, err := proberesults.NewFileProbeResults(probePath).List(fixture.laptopID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 3 {
		t.Fatalf("observations = %+v, want three addresses", observations)
	}
}

func itoa(value int) string {
	digits := ""
	if value == 0 {
		return "0"
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// TestProbeConnectivityRemoteOriginFixedCommand: 经服务器起点按 shell 类型
// 生成固定命令并在一次会话执行；命令策略全部拦截时探测照常执行；审计摘
// 要注明固定命令不经命令策略。
func TestProbeConnectivityRemoteOriginFixedCommand(t *testing.T) {
	fixture := newProbeFixture(t)
	var seenCommands sync.Map
	execServer, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(command string, _ io.Reader) (string, string, int) {
			seenCommands.Store("last", command)
			return "SSHMGR-PROBE 0 fail 1 connection-refused\nSSHMGR-PROBE 1 ok 4 \n", "", 0
		},
	})
	defer cleanup()
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	originResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "笔记本 SSH", Host: hostOfAddr(execServer), Port: portOfAddr(execServer), User: "u",
		ShellType: models.ShellPowerShell,
	}, credential, nil, "笔记本")
	if err != nil {
		t.Fatal(err)
	}
	origin, err := fixture.store.GetServer(originResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SaveEntryPin(origin, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.AddPolicyRule(policy.CustomRule{
		ID: "DENY-ALL", Regex: ".*", Disposition: policy.DispositionBlock,
		Explanation: "测试用全部拦截",
	}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		network, address string
	}{
		{"家A局域网", "192.0.2.10"},
		{"NetBird", "100.64.10.5"},
	} {
		if _, err := fixture.store.AddNetworkMembership("NAS", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	// 授权范围补充起点服务器（夹具构造时还拿不到编号，这里补授权）。
	if err := fixture.store.GrantServers(fixture.profileID, []string{origin.ID}); err != nil {
		t.Fatal(err)
	}

	text, isError := fixture.callProbe(t, map[string]any{
		"from": origin.ID, "targets": []string{"NAS:445"},
	})
	if isError {
		t.Fatalf("deny-all policy must not block the fixed command:\n%s", text)
	}
	if !strings.Contains(text, "NAS:445  家A局域网 第1个地址  ✗连接被拒绝 刚刚") ||
		!strings.Contains(text, "NAS:445  NetBird 第1个地址  ✓4ms 刚刚") {
		t.Fatalf("remote text =\n%s", text)
	}
	command, _ := seenCommands.Load("last")
	commandText, _ := command.(string)
	if !strings.Contains(commandText, "SSHMGR-PROBE") || !strings.Contains(commandText, "192.0.2.10") {
		t.Fatalf("fixed command = %q", commandText)
	}

	rows, err := fixture.store.AuditRows(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v %v", rows, err)
	}
	row := rows[0]
	if row.Action != "probe-connectivity" || row.Source != store.AuditSourceAgent || row.ServerID != origin.ID {
		t.Fatalf("audit row = %+v", row)
	}
	if !strings.Contains(row.Command, "固定命令，不经命令策略") || strings.Contains(row.Command, "192.0.2.10") {
		t.Fatalf("audit summary = %q", row.Command)
	}
}

// TestProbeConnectivityRejections: 本机未登记设备、授权范围外起点与目标、
// 超过 10 个目标、写错设备名的拒绝路径，以及失败路径审计。
func TestProbeConnectivityRejections(t *testing.T) {
	fixture := newProbeFixture(t)
	// 授权范围外起点。
	text, isError := fixture.callProbe(t, map[string]any{"from": "srv-out", "targets": []string{"NAS"}})
	if !isError || !strings.Contains(text, "not in your profile") {
		t.Fatalf("out-of-profile origin result = %q %v", text, isError)
	}

	// 写错设备名。
	text, isError = fixture.callProbe(t, map[string]any{"from": "self", "targets": []string{"不存在设备"}})
	if !isError || !strings.Contains(text, "拓扑中没有这台设备") {
		t.Fatalf("unknown device result = %q %v", text, isError)
	}

	// 超过 10 个目标。
	tooMany := make([]string, 11)
	for index := range tooMany {
		tooMany[index] = "NAS:" + itoa(7000+index)
	}
	text, isError = fixture.callProbe(t, map[string]any{"from": "self", "targets": tooMany})
	if !isError || !strings.Contains(text, "10") {
		t.Fatalf("too many targets result = %q %v", text, isError)
	}

	// 本机未登记设备（放在最后：前面用例还要用 self 起点）。
	if _, err := roles.ClearLocalDevice(); err != nil {
		t.Fatal(err)
	}
	text, isError = fixture.callProbe(t, map[string]any{"from": "self", "targets": []string{"NAS"}})
	if !isError || !strings.Contains(text, "本机未设定设备") {
		t.Fatalf("self without device result = %q %v", text, isError)
	}

	rows, err := fixture.store.AuditRows(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v %v", rows, err)
	}
	if rows[0].Action != "probe-connectivity" || rows[0].Status != "error" {
		t.Fatalf("failure audit row = %+v", rows[0])
	}
}
