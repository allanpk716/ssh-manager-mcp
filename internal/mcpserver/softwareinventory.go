package mcpserver

// 票 #185：query_software_inventory 工具。权威面与单机用本地适配层按
// profile 查询 vault；客户端缓存面注入在线查询能力（设备码认证、固定传
// 输层安全的转发器），未注入时固定报错，绝不回退本地快照数据。

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// QuerySoftwareInventoryInput 是 query_software_inventory 的输入。五个参
// 数全部可选；可见设备范围不在这里传，由查询执行方决定：本地查询按本
// 面 profile 计算，在线查询由权威端按设备码绑定的 profile 计算。
type QuerySoftwareInventoryInput struct {
	Device                string `json:"device,omitempty" jsonschema:"device name filter; omit for all visible devices"`
	Name                  string `json:"name,omitempty" jsonschema:"case-insensitive substring match on the software name; omit for all names"`
	MinimumExposure       string `json:"minimum_exposure,omitempty" jsonschema:"minimum exposure level of any listening port: local_machine | specific_address | all_addresses | local_area_network | virtual_local_area_network | public_network; omit for any"`
	Category              string `json:"category,omitempty" jsonschema:"category filter: operating_system | service_program | container_image | driver_firmware | system_package | unidentified_process; omit for any"`
	IncludeSystemPackages bool   `json:"include_system_packages,omitempty" jsonschema:"include system packages (default false — system package lists can be very long)"`
}

// SoftwareInventoryQuerier 是软件清单查询能力。权威面与单机注入本地适配
// 层；客户端缓存面注入 clientops 的在线转发器（实现方负责设备码认证与
// 权威端不可达时的固定错误文本）。
type SoftwareInventoryQuerier interface {
	QuerySoftwareInventory(in QuerySoftwareInventoryInput) (softwareinventory.QueryResult, error)
}

// QuerySoftwareInventoryOutput 是工具的输出形态。MaskedOutput 是全部工具
// 输出的统一遮蔽字段（见 masking.go 的测试约束）。
type QuerySoftwareInventoryOutput struct {
	MaskedOutput
	Devices []softwareinventory.QueryDeviceResult `json:"devices" jsonschema:"matching devices, each with its software items, listening ports and exposure levels, source, collection time, and collection status"`
}

// errSoftwareInventoryNotWired 是客户端面未注入在线查询能力时的固定错误
// 文本：明确指出不回退本地数据。
var errSoftwareInventoryNotWired = errors.New(
	"软件清单查询需要在线访问权威端，但这个客户端桥没有接上转发器；不会读取本地快照数据，请修复部署后重试")

// failClosedSoftwareInventoryQuerier 是客户端面的保底能力：只报错，不查询。
type failClosedSoftwareInventoryQuerier struct{}

func (failClosedSoftwareInventoryQuerier) QuerySoftwareInventory(
	QuerySoftwareInventoryInput,
) (softwareinventory.QueryResult, error) {
	return softwareinventory.QueryResult{}, errSoftwareInventoryNotWired
}

// localSoftwareInventoryQuerier 在权威端或单机的 vault 上按 profile 查询。
type localSoftwareInventoryQuerier struct {
	storeFn   func() *store.Store
	profileID string
}

func (q localSoftwareInventoryQuerier) QuerySoftwareInventory(
	in QuerySoftwareInventoryInput,
) (softwareinventory.QueryResult, error) {
	st := q.storeFn()
	granted, err := st.ServersForProfile(q.profileID)
	if err != nil {
		return softwareinventory.QueryResult{}, fmt.Errorf("读取 profile 授权失败: %w", err)
	}
	return softwareinventory.New(st).Query(softwareinventory.QueryOptions{
		Device:                in.Device,
		Name:                  in.Name,
		MinimumExposure:       softwareinventory.Exposure(in.MinimumExposure),
		Category:              softwareinventory.Category(in.Category),
		IncludeSystemPackages: in.IncludeSystemPackages,
		Visibility:            softwareinventory.DevicesVisibleThroughServers(granted...),
	})
}

// registerSoftwareInventoryTool 注册 query_software_inventory。inventory 为
// nil 时使用本地适配层（权威端与单机）；客户端面由 newCacheBroker 注入在
// 线转发器或保底报错能力。
func registerSoftwareInventoryTool(
	srv *mcp.Server,
	busy *BusyTracker,
	storeFn func() *store.Store,
	hkFn func() sshbroker.HostKeyStore,
	profileID string,
	inventory SoftwareInventoryQuerier,
) {
	if inventory == nil {
		inventory = localSoftwareInventoryQuerier{storeFn: storeFn, profileID: profileID}
	}
	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[17], // "query_software_inventory"（票 #185）
			Description: "Query the software inventory (devices' installed software with precise versions, listening ports with exposure levels, source, collection time, and each device's collection status including whether its inventory is stale). All five filters are optional: device (name), name (case-insensitive substring), minimum_exposure, category, include_system_packages (default false — package lists can be huge). Visibility follows YOUR profile: only managed devices with at least one server entry you may use; unmanaged devices never appear. On a cache client this tool queries the authority online — if the authority is unreachable it fails with a fixed error and never falls back to local data.",
		},
		func(_ context.Context, _ *mcp.CallToolRequest, in QuerySoftwareInventoryInput) (*mcp.CallToolResult, QuerySoftwareInventoryOutput, error) {
			result, err := inventory.QuerySoftwareInventory(in)
			if err != nil {
				// 空数组而不是 nil：工具输出的 JSON 结构里 devices 恒为数组。
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, QuerySoftwareInventoryOutput{Devices: []softwareinventory.QueryDeviceResult{}}, nil
			}
			return nil, QuerySoftwareInventoryOutput{Devices: result.Devices}, nil
		},
	)
}
