package mcpserver

// 票 #188：draft_software_declarations 与 collect_software_inventory 两个
// 工具面。权威端与单机直接写本机 vault；客户端缓存面把草稿经注入的受审
// 计转发写能力提交，立即采集没有可传命令的参数。

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

// TestSoftwareInventoryAgentToolSurface 钉住工具清单：新增草稿与立即采集两
// 个工具；不提供任何确认草稿或修改生效声明的 MCP 工具。
func TestSoftwareInventoryAgentToolSurface(t *testing.T) {
	if BrokerTools[18] != "draft_software_declarations" ||
		BrokerTools[19] != "collect_software_inventory" {
		t.Fatalf("BrokerTools 应包含两个新工具：%v", BrokerTools)
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
		"query_software_inventory":    true,
		"draft_software_declarations": true,
		"collect_software_inventory":  true,
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

// TestDraftSoftwareDeclarationsAuthorityFace 权威端与单机直接写草稿：一次
// 多条返回编号；确认前查询不受影响；越界设备被拒。
func TestDraftSoftwareDeclarationsAuthorityFace(t *testing.T) {
	st, profileID := softwareInventoryFaceFixture(t)
	srv, tunnels, tasks, err := newLocalVaultServer(st, profileID, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := softwareInventorySession(t, srv)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "draft_software_declarations",
		Arguments: map[string]any{"declarations": []map[string]any{
			{
				"device": "可见设备", "category": "service_program",
				"name": "业务网关", "version": "2.1",
				"listening_ports": []map[string]any{{
					"address": "0.0.0.0", "port": 8443, "protocol": "tcp",
					"exposure": "virtual_local_area_network",
				}},
			},
			{
				"device": "可见设备", "category": "operating_system",
				"name": "Ubuntu", "version": "24.04",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("提交草稿报错：%+v", result.Content)
	}
	text := toolResultText(result)
	var output struct {
		DraftIDs []string `json:"draft_ids"`
	}
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("解析草稿输出失败：%v\n%s", err, text)
	}
	if len(output.DraftIDs) != 2 || output.DraftIDs[0] == "" || output.DraftIDs[1] == "" {
		t.Fatalf("应返回两个草稿编号：%+v", output)
	}

	queryText, isError := callSoftwareInventoryTool(t, session, map[string]any{"name": "业务网关"})
	if isError || strings.Contains(queryText, "业务网关") {
		t.Fatalf("草稿确认前不应出现在查询工具结果里：%q %v", queryText, isError)
	}

	drafts, err := st.SoftwareDeclarationDrafts(store.AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 {
		t.Fatalf("权威端应直接落两条草稿：%+v", drafts)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-declaration-draft"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Source != store.AuditSourceAgent {
		t.Fatalf("本地提交草稿应以 agent 来源写审计：%+v", rows)
	}

	rejected, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "draft_software_declarations",
		Arguments: map[string]any{"declarations": []map[string]any{{
			"device": "授权外设备", "category": "service_program",
			"name": "redis", "version": "7.2",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rejected.IsError || !strings.Contains(toolResultText(rejected), "可见范围") {
		t.Fatalf("越界设备应被拒：%+v", rejected.Content)
	}
}

// TestDraftSoftwareDeclarationsCacheFaceForwards 客户端面把草稿原样交给注
// 入的受审计转发写能力。
func TestDraftSoftwareDeclarationsCacheFaceForwards(t *testing.T) {
	agent := &fakeSoftwareInventoryAgent{
		draftResult: DraftSoftwareDeclarationsOutput{DraftIDs: []string{"draft-1", "draft-2"}},
	}
	session := softwareInventoryFaceSessionWithQuerier(t, agent)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "draft_software_declarations",
		Arguments: map[string]any{"declarations": []map[string]any{{
			"device": "权威端设备", "category": "service_program",
			"name": "nginx", "version": "1.25.3",
			"claimed_observed_key": "observed-key-1",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("转发草稿报错：%+v", result.Content)
	}
	if agent.draftCalls != 1 || len(agent.draftGot.Declarations) != 1 ||
		agent.draftGot.Declarations[0].Name != "nginx" ||
		agent.draftGot.Declarations[0].ClaimedObservedKey != "observed-key-1" {
		t.Fatalf("草稿未原样转发：%+v", agent.draftGot)
	}
	if !strings.Contains(toolResultText(result), "draft-2") {
		t.Fatalf("转发结果未透出：%s", toolResultText(result))
	}
}

// TestDraftSoftwareDeclarationsCacheFaceFailsClosed 客户端面没有注入转发写
// 能力时固定报错，不写任何本地数据。
func TestDraftSoftwareDeclarationsCacheFaceFailsClosed(t *testing.T) {
	session := softwareInventoryFaceSessionWithQuerier(t, &fakeSoftwareInventoryQuerier{})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "draft_software_declarations",
		Arguments: map[string]any{"declarations": []map[string]any{{
			"device": "任意设备", "category": "service_program",
			"name": "nginx", "version": "1.25.3",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(result)
	if !result.IsError || !strings.Contains(text, "权威端") || !strings.Contains(text, "草稿") {
		t.Fatalf("未注入转发写能力时应固定报错：%q %v", text, result.IsError)
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
