package mcpserver

// 票 #188：collect_software_inventory 工具。对一台 profile 可见设备请求
// 立即采集，语义与命令行的「立即采集」一致：固定命令集随 sshmgr 发布，
// 输入里没有任何可传命令的参数。权威面与单机直接执行；客户端缓存面需
// 要注入采集能力，未注入时固定报错。

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// CollectSoftwareInventoryInput 是 collect_software_inventory 的输入。
// 唯一的字段是设备名；采集命令集固定，不接受任何命令参数。
type CollectSoftwareInventoryInput struct {
	Device string `json:"device" jsonschema:"name of the visible device to collect now"`
}

// CollectSoftwareInventoryOutput 是工具的输出：这台设备的采集报告。
type CollectSoftwareInventoryOutput struct {
	MaskedOutput
	Report *softwareinventory.CollectionReport `json:"report" jsonschema:"collection report for the device"`
}

// SoftwareInventoryCollector 是立即采集能力。权威面与单机注入本地适配
// 层；客户端缓存面由部署注入（例如客户端实例自己就是采集执行者时）。
type SoftwareInventoryCollector interface {
	CollectSoftwareInventory(
		ctx context.Context,
		in CollectSoftwareInventoryInput,
	) (CollectSoftwareInventoryOutput, error)
}

// errSoftwareCollectNotWired 是客户端面没有接上采集能力时的固定错误文
// 本。
var errSoftwareCollectNotWired = errors.New(
	"软件清单立即采集需要权威端或登记的采集执行者实例执行；这个客户端桥没有接上采集能力")

// failClosedSoftwareInventoryCollector 只报错，不采集。
type failClosedSoftwareInventoryCollector struct{}

func (failClosedSoftwareInventoryCollector) QuerySoftwareInventory(
	QuerySoftwareInventoryInput,
) (softwareinventory.QueryResult, error) {
	return softwareinventory.QueryResult{}, errSoftwareInventoryNotWired
}

func (failClosedSoftwareInventoryCollector) CollectSoftwareInventory(
	context.Context, CollectSoftwareInventoryInput,
) (CollectSoftwareInventoryOutput, error) {
	return CollectSoftwareInventoryOutput{}, errSoftwareCollectNotWired
}

// softwareInventoryCollectService 构造立即采集服务。生产实现使用真实远
// 程执行链路；测试替换这个变量注入回放函数。
var softwareInventoryCollectService = func(s *store.Store) *softwareinventory.Service {
	return &softwareinventory.Service{Store: s}
}

// localSoftwareInventoryCollector 在权威端或单机上按 profile 可见范围立
// 即采集。
type localSoftwareInventoryCollector struct {
	storeFn   func() *store.Store
	profileID string
}

func (c localSoftwareInventoryCollector) CollectSoftwareInventory(
	ctx context.Context, in CollectSoftwareInventoryInput,
) (CollectSoftwareInventoryOutput, error) {
	st := c.storeFn()
	granted, err := st.ServersForProfile(c.profileID)
	if err != nil {
		return CollectSoftwareInventoryOutput{}, fmt.Errorf("读取 profile 授权失败: %w", err)
	}
	report, err := softwareinventory.New(st).CollectForScope(
		ctx, in.Device,
		softwareinventory.DevicesVisibleThroughServers(granted...),
		softwareInventoryCollectService(st),
		softwareinventory.AgentSource(),
	)
	if err != nil {
		return CollectSoftwareInventoryOutput{}, err
	}
	if report.Notes == nil {
		report.Notes = []string{}
	}
	return CollectSoftwareInventoryOutput{Report: report}, nil
}

// registerSoftwareInventoryCollectionTool 注册 collect_software_inventory。
func registerSoftwareInventoryCollectionTool(
	srv *mcp.Server,
	busy *BusyTracker,
	storeFn func() *store.Store,
	hkFn func() sshbroker.HostKeyStore,
	profileID string,
	inventory SoftwareInventoryQuerier,
) {
	var collector SoftwareInventoryCollector
	switch typed := inventory.(type) {
	case nil:
		collector = localSoftwareInventoryCollector{storeFn: storeFn, profileID: profileID}
	case SoftwareInventoryCollector:
		collector = typed
	default:
		collector = failClosedSoftwareInventoryCollector{}
	}
	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[18], // "collect_software_inventory"（票 #188）
			Description: "Request an immediate software inventory collection for one visible device. The command set is fixed and shipped with sshmgr — this tool accepts no command parameters, only the device name.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CollectSoftwareInventoryInput) (*mcp.CallToolResult, CollectSoftwareInventoryOutput, error) {
			output, err := collector.CollectSoftwareInventory(ctx, in)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, CollectSoftwareInventoryOutput{}, nil
			}
			return nil, output, nil
		},
	)
}
