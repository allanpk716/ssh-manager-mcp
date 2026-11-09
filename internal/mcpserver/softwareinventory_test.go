package mcpserver

// 票 #185：query_software_inventory 工具面。权威面走本地查询并按 profile
// 过滤；客户端面走注入的在线查询能力，未注入时固定报错、绝不退回本地
// 快照数据。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// fakeSoftwareInventoryQuerier 顶替 clientops 的在线查询转发器：记录过滤
// 条件并返回固定结果或错误。
type fakeSoftwareInventoryQuerier struct {
	got    QuerySoftwareInventoryInput
	result softwareinventory.QueryResult
	err    error
	calls  int
}

func (f *fakeSoftwareInventoryQuerier) QuerySoftwareInventory(
	in QuerySoftwareInventoryInput,
) (softwareinventory.QueryResult, error) {
	f.calls++
	f.got = in
	return f.result, f.err
}

// softwareInventoryFaceFixture 造一个权威端 vault：可见设备、授权外设备
// 与不受管设备各一台，可见设备上有服务程序与系统包声明。
func softwareInventoryFaceFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	st := newStoreAt(t, filepath.Join(t.TempDir(), "inventory-face.db"))
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, deviceName := range []string{"可见设备", "授权外设备", "不受管设备"} {
		if _, err := st.AddDevice(deviceName, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, deviceName := range []string{"可见设备", "授权外设备"} {
		if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: deviceName + "-ssh", Host: "192.0.2.10", Port: 22, User: "u",
			AuthMethod: models.AuthPassword, CredentialID: credentialID,
		}, nil, nil, deviceName); err != nil {
			t.Fatal(err)
		}
	}
	visibleServer, err := st.GetServerByName("可见设备-ssh")
	if err != nil || visibleServer == nil {
		t.Fatalf("取可见设备服务器条目失败：%v %+v", err, visibleServer)
	}
	profileID, err := st.AddProfile("查询 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{visibleServer.ID}); err != nil {
		t.Fatal(err)
	}
	manager := softwareinventory.New(st)
	for _, declaration := range []softwareinventory.Declaration{
		{
			Device: "可见设备", Category: softwareinventory.CategoryServiceProgram,
			Name: "nginx", Version: "1.25.3",
			ListeningPorts: []softwareinventory.ListeningPort{{
				Address: "0.0.0.0", Port: 80, Protocol: "tcp",
				Exposure: softwareinventory.ExposureAllAddresses,
			}},
		},
		{
			Device: "可见设备", Category: softwareinventory.CategorySystemPackage,
			Name: "libc6", Version: "2.39-0ubuntu8",
		},
		{
			Device: "授权外设备", Category: softwareinventory.CategoryServiceProgram,
			Name: "PostgreSQL", Version: "16.2",
		},
		{
			Device: "不受管设备", Category: softwareinventory.CategoryOperatingSystem,
			Name: "NAS 系统", Version: "7.2.1",
		},
	} {
		if _, err := manager.AddDeclaration(declaration, softwareinventory.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	return st, profileID
}

func softwareInventorySession(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	clientSide, serverSide := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(context.Background(), serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callSoftwareInventoryTool(
	t *testing.T,
	session *mcp.ClientSession,
	arguments map[string]any,
) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query_software_inventory", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}
	return text, result.IsError
}

// softwareInventoryFaceSessionWithQuerier 构建一个客户端缓存面（本地快照
// 里没有任何软件清单数据），并注入在线查询能力；querier 为 nil 表示模拟
// 未接线的能力。
func softwareInventoryFaceSessionWithQuerier(
	t *testing.T,
	querier SoftwareInventoryQuerier,
) *mcp.ClientSession {
	t.Helper()
	st, profileID := softwareInventoryFaceFixture(t)
	_, token, err := st.AddProject("proj", profileID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	srv, tunnels, tasks, cleanup, err := NewCacheBroker(
		token, snap, filepath.Join(t.TempDir(), "audit.log"), nil, nil, "laptop", nil, querier,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	return softwareInventorySession(t, srv)
}

// TestQuerySoftwareInventoryAuthorityFace 钉住权威面的本地查询：只返回
// profile 可见设备，采集状态字段在场，系统包默认省略。
func TestQuerySoftwareInventoryAuthorityFace(t *testing.T) {
	st, profileID := softwareInventoryFaceFixture(t)
	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)

	text, isError := callSoftwareInventoryTool(t, session, map[string]any{})
	if isError {
		t.Fatalf("查询报错：%s", text)
	}
	var result softwareinventory.QueryResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("解析工具输出失败：%v\n%s", err, text)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "可见设备" {
		t.Fatalf("查询结果 = %+v，应只有可见设备", result.Devices)
	}
	if strings.Contains(text, "PostgreSQL") || strings.Contains(text, "NAS 系统") {
		t.Fatalf("授权外设备或不受管设备的清单泄漏：%s", text)
	}
	if strings.Contains(text, "libc6") {
		t.Fatalf("系统包默认不应返回：%s", text)
	}
	if !result.Devices[0].Collection.NeverCollected || !result.Devices[0].Collection.Stale {
		t.Fatalf("采集状态应表示从未采集：%+v", result.Devices[0].Collection)
	}
	if len(result.Devices[0].Items) != 1 ||
		result.Devices[0].Items[0].ListeningPorts[0].Exposure != softwareinventory.ExposureAllAddresses {
		t.Fatalf("查询条目 = %+v", result.Devices[0].Items)
	}

	// 类别无匹配：空结果不是错误；非法类别：报错。
	text, isError = callSoftwareInventoryTool(t, session, map[string]any{
		"category": "operating_system",
	})
	if isError || !strings.Contains(text, `"devices":[]`) {
		t.Fatalf("类别无匹配应返回空结果：%q %v", text, isError)
	}
	if text, isError = callSoftwareInventoryTool(t, session, map[string]any{
		"category": "not-a-category",
	}); !isError {
		t.Fatalf("非法类别应报错：%s", text)
	}
}

// TestQuerySoftwareInventoryCacheFaceUsesInjectedQuerier 客户端面必须把
// 过滤条件原样交给注入的在线查询能力，不读本地快照。
func TestQuerySoftwareInventoryCacheFaceUsesInjectedQuerier(t *testing.T) {
	querier := &fakeSoftwareInventoryQuerier{
		result: softwareinventory.QueryResult{Devices: []softwareinventory.QueryDeviceResult{{
			DeviceID: "device-1", DeviceName: "权威端设备",
			Collection: softwareinventory.CollectionStatus{NeverCollected: true, Stale: true},
			Items: []softwareinventory.QueryItem{{
				DeviceID: "device-1", Category: softwareinventory.CategoryServiceProgram,
				CategoryLabel: "服务程序", Name: "nginx", Version: "1.25.3",
				Source: softwareinventory.SourceDeclared,
				ListeningPorts: []softwareinventory.ListeningPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp",
					Exposure: softwareinventory.ExposureAllAddresses,
				}},
			}},
		}}},
	}
	session := softwareInventoryFaceSessionWithQuerier(t, querier)
	text, isError := callSoftwareInventoryTool(t, session, map[string]any{
		"device": "权威端设备", "name": "nginx", "minimum_exposure": "all_addresses",
		"category": "service_program", "include_system_packages": true,
	})
	if isError {
		t.Fatalf("在线查询报错：%s", text)
	}
	if querier.calls != 1 {
		t.Fatalf("注入的查询能力应被调用一次，实际 %d 次", querier.calls)
	}
	if querier.got != (QuerySoftwareInventoryInput{
		Device: "权威端设备", Name: "nginx", MinimumExposure: "all_addresses",
		Category: "service_program", IncludeSystemPackages: true,
	}) {
		t.Fatalf("过滤条件未原样传递：%+v", querier.got)
	}
	if !strings.Contains(text, "权威端设备") || !strings.Contains(text, "nginx") {
		t.Fatalf("在线查询结果未透出：%s", text)
	}
}

// TestQuerySoftwareInventoryCacheFaceNoQuerierFailsClosed 客户端面没有注入
// 在线查询能力时必须固定报错；本地快照里的任何数据都不能成为结果。
func TestQuerySoftwareInventoryCacheFaceNoQuerierFailsClosed(t *testing.T) {
	session := softwareInventoryFaceSessionWithQuerier(t, nil)
	text, isError := callSoftwareInventoryTool(t, session, map[string]any{})
	if !isError {
		t.Fatalf("未注入查询能力时应报错，实际：%s", text)
	}
	if !strings.Contains(text, "软件清单") || !strings.Contains(text, "权威端") {
		t.Fatalf("错误文本应说明软件清单必须在线查询权威端：%s", text)
	}
}

// TestQuerySoftwareInventoryCacheFaceSurfacesQuerierError 注入能力报错时，
// 错误文本原样透出给 agent。
func TestQuerySoftwareInventoryCacheFaceSurfacesQuerierError(t *testing.T) {
	querier := &fakeSoftwareInventoryQuerier{err: errors.New("权威端不可达")}
	session := softwareInventoryFaceSessionWithQuerier(t, querier)
	text, isError := callSoftwareInventoryTool(t, session, map[string]any{})
	if !isError || text != "权威端不可达" {
		t.Fatalf("错误应原样透出：%q %v", text, isError)
	}
}
