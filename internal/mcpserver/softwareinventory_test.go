package mcpserver

// 票 #185：query_software_inventory 工具面。权威面走本地查询并按 profile
// 过滤；客户端面走注入的在线查询能力，未注入时固定报错、绝不退回本地
// 快照数据。

import (
	"bytes"
	"context"
	"encoding/csv"
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
// 与不受管设备各一台，可见设备上有服务程序与系统包实测条目；授权外与
// 不受管设备只有声明。
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
	if err := st.ReplaceSoftwareObservedItems("可见设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{
				Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.25.3",
				ListeningPorts: []softwareinventory.ListeningPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp",
					Exposure: softwareinventory.ExposureAllAddresses,
				}},
			},
			{
				Category: softwareinventory.CategorySystemPackage,
				Name:     "libc6", Version: "2.39-0ubuntu8",
			},
		},
		ServerID: visibleServer.ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	manager := softwareinventory.New(st)
	for _, declaration := range []softwareinventory.Declaration{
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
	if result.Devices[0].Collection.NeverCollected || result.Devices[0].Collection.Stale ||
		result.Devices[0].Collection.CommandSetVersion != "posix-v2" {
		t.Fatalf("采集状态应来自实测成功记录：%+v", result.Devices[0].Collection)
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

// TestQuerySoftwareInventoryToolExplainsObservedExposure 钉住工具说明：实
// 测清单只有三档监听暴露级别，声明专属档位仅为兼容旧参数继续接受。
func TestQuerySoftwareInventoryToolExplainsObservedExposure(t *testing.T) {
	session := softwareInventoryFaceSessionWithQuerier(t, &fakeSoftwareInventoryQuerier{})
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == "query_software_inventory" {
			tool = tools.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("工具清单缺少 query_software_inventory")
	}
	for _, wanted := range []string{
		"observed listening ports use only local_machine, specific_address, and all_addresses",
		"local_area_network, virtual_local_area_network, and public_network remain accepted for compatibility but do not match observed items",
	} {
		if !strings.Contains(tool.Description, wanted) {
			t.Fatalf("工具说明缺少 %q：%s", wanted, tool.Description)
		}
	}

	schemaBlob, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaBlob, &schema); err != nil {
		t.Fatal(err)
	}
	description := schema.Properties["minimum_exposure"].Description
	if !strings.Contains(description, "observed items use only local_machine | specific_address | all_addresses") ||
		!strings.Contains(description, "local_area_network | virtual_local_area_network | public_network are accepted for compatibility but do not match observed items") {
		t.Fatalf("minimum_exposure 参数说明不准确：%s", description)
	}
}

// TestQuerySoftwareInventoryAuthorityFaceReturnsCSVWithFilters 钉住 CSV 导
// 出面：format=csv 复用同一组查询条件，返回能用标准解析器读回的总表文
// 本，并且机密项遮蔽仍然生效。
func TestQuerySoftwareInventoryAuthorityFaceReturnsCSVWithFilters(t *testing.T) {
	st, profileID := softwareInventoryFaceFixture(t)
	secretValue := "nginx-secret-version"
	if err := st.AddSecret(
		"SOFTWARE_VERSION_TOKEN", "软件版本里的机密项", []byte(secretValue), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("可见设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{
				Category: softwareinventory.CategoryServiceProgram,
				Name:     "nginx", Version: secretValue,
				ListeningPorts: []softwareinventory.ListeningPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp",
					Exposure: softwareinventory.ExposureAllAddresses,
				}},
			},
			{
				Category: softwareinventory.CategorySystemPackage,
				Name:     "libc6", Version: "2.39-0ubuntu8",
			},
		},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)

	text, isError := callSoftwareInventoryTool(t, session, map[string]any{
		"format": "csv", "name": "nginx", "exact_version": secretValue,
		"include_system_packages": true,
	})
	if isError {
		t.Fatalf("CSV 查询报错：%s", text)
	}
	var output QuerySoftwareInventoryOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("解析工具输出失败：%v\n%s", err, text)
	}
	if output.CSV == "" {
		t.Fatalf("format=csv 应返回总表文本：%s", text)
	}
	if strings.Contains(output.CSV, secretValue) {
		t.Fatalf("CSV 里的机密值必须被遮蔽：%q", output.CSV)
	}
	if len(output.Masked) != 1 || output.Masked[0] != "SOFTWARE_VERSION_TOKEN" {
		t.Fatalf("遮蔽名单 = %#v", output.Masked)
	}
	blob := []byte(output.CSV)
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("CSV 缺少 UTF-8 字节顺序标记：%q", blob)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF")))).ReadAll()
	if err != nil {
		t.Fatalf("CSV 必须能被标准解析器读回：%v\n%q", err, output.CSV)
	}
	if len(rows) != 2 || rows[1][0] != "可见设备" || rows[1][1] != "nginx" ||
		rows[1][2] != "{{sshmgr-secret:SOFTWARE_VERSION_TOKEN}}" ||
		rows[1][6] != "监听所有地址" {
		t.Fatalf("筛选后的 CSV 行 = %#v", rows)
	}

	text, isError = callSoftwareInventoryTool(t, session, map[string]any{"format": "spdx"})
	if !isError || !strings.Contains(text, "format") {
		t.Fatalf("非法 format 应报错：%q %v", text, isError)
	}
}

// TestQuerySoftwareInventoryAuthorityFaceIncludesObservedOnlyDevices 钉住
// 统一查询的缺口修复：没有任何声明的实测条目也必须能被 MCP 工具查到。
func TestQuerySoftwareInventoryAuthorityFaceIncludesObservedOnlyDevices(t *testing.T) {
	st, profileID := softwareInventoryFaceFixture(t)
	observed := models.SoftwareObservedItem{
		Category: softwareinventory.CategoryServiceProgram,
		Name:     "observed-only", Version: "2.0",
	}
	missingVersion := models.SoftwareObservedItem{
		Category: softwareinventory.CategoryServiceProgram,
		Name:     "observed-no-version",
	}
	if err := st.ReplaceSoftwareObservedItems("可见设备", store.SoftwareObservedWrite{
		Items:             []models.SoftwareObservedItem{observed, missingVersion},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)
	text, isError := callSoftwareInventoryTool(t, session, map[string]any{"name": "observed"})
	if isError {
		t.Fatalf("查询只有实测的设备报错：%s", text)
	}
	var result softwareinventory.QueryResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("解析工具输出失败：%v\n%s", err, text)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 2 ||
		result.Devices[0].Items[0].Source != softwareinventory.SourceObserved {
		t.Fatalf("只有实测的设备应出现在 MCP 查询里：%+v", result.Devices)
	}
	// 输出 json 每条带缺版本标志：版本为空的条目为 true。名字排序里
	// observed-no-version 在 observed-only 前。
	if !result.Devices[0].Items[0].MissingVersion || result.Devices[0].Items[1].MissingVersion {
		t.Fatalf("缺版本标志应随查询条目输出：%+v", result.Devices[0].Items)
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
		"category": "service_program", "exact_version": "1.25.3", "site": "总部",
		"network": "总部物理局域网", "include_system_packages": true,
	})
	if isError {
		t.Fatalf("在线查询报错：%s", text)
	}
	if querier.calls != 1 {
		t.Fatalf("注入的查询能力应被调用一次，实际 %d 次", querier.calls)
	}
	if querier.got != (QuerySoftwareInventoryInput{
		Device: "权威端设备", Name: "nginx", MinimumExposure: "all_addresses",
		Category: "service_program", ExactVersion: "1.25.3", Site: "总部",
		Network: "总部物理局域网", IncludeSystemPackages: true,
	}) {
		t.Fatalf("过滤条件未原样传递：%+v", querier.got)
	}
	if !strings.Contains(text, "权威端设备") || !strings.Contains(text, "nginx") {
		t.Fatalf("在线查询结果未透出：%s", text)
	}
}

// TestQuerySoftwareInventoryCacheFaceReturnsCSVFromOnlineResult 客户端面的
// CSV 与权威端共用同一个序列化函数：转发回来的结构化结果在本机转成总表
// 文本，查询条件仍原样交给在线能力。
func TestQuerySoftwareInventoryCacheFaceReturnsCSVFromOnlineResult(t *testing.T) {
	querier := &fakeSoftwareInventoryQuerier{
		result: softwareinventory.QueryResult{Devices: []softwareinventory.QueryDeviceResult{{
			DeviceID: "device-1", DeviceName: "权威端设备",
			Items: []softwareinventory.QueryItem{{
				DeviceID: "device-1", Category: softwareinventory.CategoryServiceProgram,
				CategoryLabel: "服务程序", Name: "nginx", Version: "1.25.3",
			}},
		}}},
	}
	session := softwareInventoryFaceSessionWithQuerier(t, querier)
	text, isError := callSoftwareInventoryTool(t, session, map[string]any{
		"format": "csv", "name": "nginx", "exact_version": "1.25.3",
	})
	if isError {
		t.Fatalf("客户端 CSV 查询报错：%s", text)
	}
	if querier.got.Format != "csv" || querier.got.Name != "nginx" ||
		querier.got.ExactVersion != "1.25.3" {
		t.Fatalf("客户端面应原样传递查询条件：%+v", querier.got)
	}
	var output QuerySoftwareInventoryOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("解析工具输出失败：%v\n%s", err, text)
	}
	blob := []byte(output.CSV)
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("客户端 CSV 缺少字节顺序标记：%q", blob)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF")))).ReadAll()
	if err != nil {
		t.Fatalf("客户端 CSV 必须能被标准解析器读回：%v\n%q", err, output.CSV)
	}
	if len(rows) != 2 || rows[1][0] != "权威端设备" || rows[1][1] != "nginx" {
		t.Fatalf("客户端 CSV 行 = %#v", rows)
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

// TestQuerySoftwareInventoryToolExplainsFocusList 钉住工具说明的重点清单
// 口径：默认不返回没有监听端口的系统包，带监听端口的系统包作为监听程
// 序出现，include_system_packages=true 才返回完整系统包列表。
func TestQuerySoftwareInventoryToolExplainsFocusList(t *testing.T) {
	session := softwareInventoryFaceSessionWithQuerier(t, &fakeSoftwareInventoryQuerier{})
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == "query_software_inventory" {
			tool = tools.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("工具清单缺少 query_software_inventory")
	}
	for _, wanted := range []string{
		"the result is the focus list",
		"system packages without listening ports are omitted",
		"packages with listening ports appear as listening programs",
	} {
		if !strings.Contains(tool.Description, wanted) {
			t.Fatalf("工具说明缺少 %q：%s", wanted, tool.Description)
		}
	}

	schemaBlob, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaBlob, &schema); err != nil {
		t.Fatal(err)
	}
	description := schema.Properties["include_system_packages"].Description
	for _, wanted := range []string{"focus list", "listening programs", "full package list"} {
		if !strings.Contains(description, wanted) {
			t.Fatalf("include_system_packages 参数说明缺少 %q：%s", wanted, description)
		}
	}
}
