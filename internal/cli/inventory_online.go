package cli

// 票 #185：软件清单查看与搜索的查询来源。权威端与单机读本地 vault；
// 客户端没有本地清单副本，必须在线查询权威端——权威端不可达时返回固定
// 错误文本并由这里映射专门退出码，绝不退回本地数据。

import (
	"errors"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// exitCodeAuthorityUnreachable 是「权威端不可达」的专门退出码：成功 0、
// 一般错误 1 之外，脚本用它区分网络问题与数据问题。
const exitCodeAuthorityUnreachable = 4

// inventoryQuerySource 统一查看与搜索两种查询入口。本地与在线两个形态
// 只有一个非空。
type inventoryQuerySource struct {
	local     *store.Store
	forwarder *clientops.SoftwareInventoryForwarder
}

// openInventoryQuerySource 按本机角色打开查询来源。instance 为空时用默
// 认缓存槽；客户端可以传命名实例。
func openInventoryQuerySource(instance string) (*inventoryQuerySource, error) {
	role, err := roleForVaultBackedCommand(instance, errInventoryNeedVault)
	if err != nil {
		return nil, err
	}
	if role == policyRoleClient {
		credential, err := clientops.ReadCacheCredFor(instance)
		if err != nil {
			return nil, err
		}
		if credential == nil {
			return nil, errors.New("本机登记为客户端但缺少缓存凭据；请先运行 cache pull 完成登记")
		}
		forwarder, err := clientops.NewSoftwareInventoryForwarder(*credential)
		if err != nil {
			return nil, err
		}
		return &inventoryQuerySource{forwarder: forwarder}, nil
	}
	local, err := openUnlockedStore()
	if err != nil {
		return nil, err
	}
	return &inventoryQuerySource{local: local}, nil
}

// Close 释放查询来源持有的资源。
func (s *inventoryQuerySource) Close() {
	if s.forwarder != nil {
		s.forwarder.Close()
	}
	if s.local != nil {
		_ = s.local.Close()
	}
}

// mapError 把在线查询的错误映射到命令行退出码；其余错误原样返回。
func (s *inventoryQuerySource) mapError(err error) error {
	if err == nil {
		return nil
	}
	if s.forwarder != nil && clientops.IsSoftwareInventoryUnreachable(err) {
		return NewExitCodeError(exitCodeAuthorityUnreachable, err)
	}
	return err
}

// ViewDevice 按设备查看软件清单。owner 范围读本地；客户端在线查询并把
// 结果转换成与本地相同的分组形态。
func (s *inventoryQuerySource) ViewDevice(
	deviceName string,
	includeSystemPackages bool,
) (*softwareinventory.DeviceInventory, error) {
	if s.local != nil {
		return softwareinventory.New(s.local).ViewDevice(
			deviceName, softwareinventory.AllDevicesVisible(),
			softwareinventory.DeviceViewOptions{IncludeSystemPackages: includeSystemPackages},
		)
	}
	result, err := s.forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Device: deviceName, IncludeSystemPackages: true,
	})
	if err != nil {
		return nil, s.mapError(err)
	}
	return deviceInventoryFromQuery(result, includeSystemPackages), nil
}

// SearchSoftware 跨设备搜索。owner 范围读本地；客户端在线查询。
func (s *inventoryQuerySource) SearchSoftware(namePart string) ([]softwareinventory.SearchResult, error) {
	if s.local != nil {
		return softwareinventory.New(s.local).SearchSoftware(
			namePart, softwareinventory.AllDevicesVisible())
	}
	result, err := s.forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Name: namePart,
	})
	if err != nil {
		return nil, s.mapError(err)
	}
	out := make([]softwareinventory.SearchResult, 0)
	for i := range result.Devices {
		for _, item := range result.Devices[i].Items {
			out = append(out, softwareinventory.SearchResult{
				DeviceID:   result.Devices[i].DeviceID,
				DeviceName: result.Devices[i].DeviceName,
				Item:       inventoryItemFromQuery(item),
			})
		}
	}
	return out, nil
}

// inventoryItemFromQuery 把在线查询条目转换成本地查看使用的条目形态。
func inventoryItemFromQuery(item softwareinventory.QueryItem) softwareinventory.Item {
	return softwareinventory.Item{
		ID: item.ID, DeviceID: item.DeviceID, Category: item.Category,
		CategoryLabel: item.CategoryLabel, Name: item.Name, Version: item.Version,
		PackageURL: item.PackageURL, CPE: item.CPE, ListeningPorts: item.ListeningPorts,
		BelongsTo: item.BelongsTo, Note: item.Note, Source: item.Source,
	}
}

// deviceInventoryFromQuery 把在线查询结果按类别分组，保持本地查看的输
// 出形态：系统包默认只计数，加参数后列出。
func deviceInventoryFromQuery(
	result softwareinventory.QueryResult,
	includeSystemPackages bool,
) *softwareinventory.DeviceInventory {
	inventory := &softwareinventory.DeviceInventory{}
	for i := range result.Devices {
		if i > 0 {
			continue // 按设备查询只可能有一台设备；多余结果防御性丢弃。
		}
		inventory.DeviceID = result.Devices[i].DeviceID
		inventory.DeviceName = result.Devices[i].DeviceName
		for _, item := range result.Devices[i].Items {
			if len(inventory.Groups) == 0 ||
				inventory.Groups[len(inventory.Groups)-1].Category != item.Category {
				inventory.Groups = append(inventory.Groups, softwareinventory.ItemGroup{
					Category: item.Category, CategoryLabel: item.CategoryLabel, Items: []softwareinventory.Item{},
				})
			}
			group := &inventory.Groups[len(inventory.Groups)-1]
			if item.Category == softwareinventory.CategorySystemPackage && !includeSystemPackages {
				group.SystemPackageCount++
				group.SystemPackagesOmitted = true
				continue
			}
			group.Items = append(group.Items, inventoryItemFromQuery(item))
		}
	}
	return inventory
}
