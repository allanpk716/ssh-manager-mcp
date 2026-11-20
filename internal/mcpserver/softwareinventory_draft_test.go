package mcpserver

// 票 #188：软件清单立即采集工具面。声明草稿提交不再注册为 MCP 工具。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// fakeSoftwareInventoryAgent 顶替客户端的在线能力：同时实现查询与草稿提
// 交两个方法，记录输入并返回固定结果。
type fakeSoftwareInventoryAgent struct {
	fakeSoftwareInventoryQuerier
	draftGot    DraftSoftwareDeclarationsInput
	draftResult DraftSoftwareDeclarationsOutput
	draftErr    error
	draftCalls  int
}

func (f *fakeSoftwareInventoryAgent) DraftSoftwareDeclarations(
	in DraftSoftwareDeclarationsInput,
) (DraftSoftwareDeclarationsOutput, error) {
	f.draftCalls++
	f.draftGot = in
	return f.draftResult, f.draftErr
}

// fakeSoftwareInventoryCollector 顶替客户端的立即采集能力。
type fakeSoftwareInventoryCollector struct {
	got    CollectSoftwareInventoryInput
	result CollectSoftwareInventoryOutput
	err    error
	calls  int
}

func (f *fakeSoftwareInventoryCollector) QuerySoftwareInventory(
	QuerySoftwareInventoryInput,
) (softwareinventory.QueryResult, error) {
	return softwareinventory.QueryResult{}, nil
}

func (f *fakeSoftwareInventoryCollector) CollectSoftwareInventory(
	_ context.Context, in CollectSoftwareInventoryInput,
) (CollectSoftwareInventoryOutput, error) {
	f.calls++
	f.got = in
	return f.result, f.err
}

// TestSoftwareInventoryAgentToolSurface 钉住工具清单：查询与立即采集两个
// 工具；声明草稿不再注册，也不提供确认草稿或修改生效声明的 MCP 工具。
func TestSoftwareInventoryAgentToolSurface(t *testing.T) {
	for _, name := range BrokerTools {
		if name == "draft_software_declarations" {
			t.Fatal("BrokerTools 不应再宣传 draft_software_declarations")
		}
	}
	st, profileID := softwareInventoryFaceFixture(t)
	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"query_software_inventory":   true,
		"collect_software_inventory": true,
	}
	for _, tool := range listed.Tools {
		if !strings.Contains(tool.Name, "software") {
			continue
		}
		if !allowed[tool.Name] {
			t.Fatalf("出现了软件清单工具 %q；不得提供确认草稿或修改生效声明的工具", tool.Name)
		}
		delete(allowed, tool.Name)
	}
	if len(allowed) != 0 {
		t.Fatalf("工具清单缺少 %v", allowed)
	}
}

// TestCollectSoftwareInventoryInputShape 断言立即采集工具的输入结构字段集
// 合：只有设备名，没有任何可传命令的参数。
func TestCollectSoftwareInventoryInputShape(t *testing.T) {
	collectType := reflect.TypeFor[CollectSoftwareInventoryInput]()
	if got := collectType.NumField(); got != 1 {
		t.Fatalf("collect_software_inventory 输入应只有 device 一个字段，实际 %d 个", got)
	}
	field := collectType.Field(0)
	if field.Name != "Device" || field.Type.Kind() != reflect.String ||
		field.Tag.Get("json") != "device" {
		t.Fatalf("输入字段应为 Device string `json:\"device\"`：%s %s %s",
			field.Name, field.Type, field.Tag.Get("json"))
	}
	draftType := reflect.TypeFor[DraftSoftwareDeclarationsInput]()
	if draftType.NumField() != 1 || draftType.Field(0).Name != "Declarations" {
		t.Fatal("draft_software_declarations 输入应只有 declarations 一个字段")
	}
}

// TestCollectSoftwareInventoryAuthorityFace 权威端面立即采集：语义与命令
// 行相同，且只能针对 profile 可见设备。
func TestCollectSoftwareInventoryAuthorityFace(t *testing.T) {
	st, profileID := softwareInventoryFaceFixture(t)
	fixture, err := os.ReadFile(filepath.Join(
		"..", "softwareinventory", "testdata", "posix", "ubuntu2404-full.txt",
	))
	if err != nil {
		t.Fatal(err)
	}
	previous := softwareInventoryCollectService
	softwareInventoryCollectService = func(s *store.Store) *softwareinventory.Service {
		return &softwareinventory.Service{
			Store: s,
			RemoteExec: func(_ context.Context, _ *models.Server, _ models.ShellType, _ string, _ bool) (softwareinventory.RemoteExecResult, error) {
				return softwareinventory.RemoteExecResult{Stdout: string(fixture)}, nil
			},
		}
	}
	t.Cleanup(func() { softwareInventoryCollectService = previous })

	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "collect_software_inventory",
		Arguments: map[string]any{"device": "可见设备"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("可见设备立即采集报错：%+v", result.Content)
	}
	text := toolResultText(result)
	if !strings.Contains(text, "可见设备") || !strings.Contains(text, "posix-v2") {
		t.Fatalf("采集报告未透出：%s", text)
	}
	status, err := st.SoftwareCollectionStatusForDevice("可见设备")
	if err != nil || status.LastSuccessAt.IsZero() {
		t.Fatalf("立即采集应落库采集状态：%+v %v", status, err)
	}

	rejected, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "collect_software_inventory",
		Arguments: map[string]any{"device": "授权外设备"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rejected.IsError || !strings.Contains(toolResultText(rejected), "可见范围") {
		t.Fatalf("越界设备的立即采集应被拒：%+v", rejected.Content)
	}
}

// TestCollectSoftwareInventoryCacheFaceFailsClosed 客户端面没有注入立即采
// 集能力时固定报错。
func TestCollectSoftwareInventoryCacheFaceFailsClosed(t *testing.T) {
	session := softwareInventoryFaceSessionWithQuerier(t, &fakeSoftwareInventoryQuerier{})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "collect_software_inventory",
		Arguments: map[string]any{"device": "权威端设备"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(result)
	if !result.IsError || !strings.Contains(text, "立即采集") {
		t.Fatalf("客户端面未接采集能力时应固定报错：%q %v", text, result.IsError)
	}
}

// TestCollectSoftwareInventoryClientCollectorRegistersRequest 采集执行者是
// 客户端实例时，工具只登记立即采集请求并告知由谁执行。
func TestCollectSoftwareInventoryClientCollectorRegistersRequest(t *testing.T) {
	st, profileID := softwareInventoryFaceFixture(t)
	visibleServer, err := st.GetServerByName("可见设备-ssh")
	if err != nil || visibleServer == nil {
		t.Fatalf("取可见设备服务器条目失败：%v %+v", err, visibleServer)
	}
	mini6ID, _, err := st.AddCacheToken("mini6", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(
		"可见设备", "mini6", "", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "collect_software_inventory",
		Arguments: map[string]any{"device": "可见设备"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("登记立即采集请求报错：%+v", result.Content)
	}
	text := toolResultText(result)
	var output struct {
		Requested bool   `json:"requested"`
		Collector string `json:"collector"`
	}
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("解析输出失败：%v\n%s", err, text)
	}
	if !output.Requested || output.Collector != mini6ID {
		t.Fatalf("客户端实例采集的设备只应登记请求：%s", text)
	}
	status, err := st.SoftwareCollectionStatusForDevice("可见设备")
	if err != nil || !status.PendingImmediate {
		t.Fatalf("立即采集请求应已登记：%+v %v", status, err)
	}
}
