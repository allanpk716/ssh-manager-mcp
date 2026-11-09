package softwareinventory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// CollectionStatus 描述一台设备的采集状态。采集状态表由另一张票实现；
// 在那之前，查询结果的这个字段固定表示「从未采集」，字段本身先在这里
// 定型，合并后由采集状态表填充。
type CollectionStatus struct {
	Collector         string     `json:"collector,omitempty"`
	ServerEntry       string     `json:"server_entry,omitempty"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt     *time.Time `json:"last_failure_at,omitempty"`
	LastFailureReason string     `json:"last_failure_reason,omitempty"`
	Incomplete        bool       `json:"incomplete,omitempty"`
	CommandSetVersion string     `json:"command_set_version,omitempty"`
	NeverCollected    bool       `json:"never_collected"`
	Stale             bool       `json:"stale"`
}

// QueryOptions 是软件清单查询的全部过滤条件。所有条件都可选；空值表示
// 不限制。
type QueryOptions struct {
	Device                string
	Name                  string
	MinimumExposure       Exposure
	Category              Category
	IncludeSystemPackages bool
	Visibility            Visibility
}

// QueryItem 是查询结果里的一条软件条目。CollectedAt 是实测采集时间；
// 声明来源或采集数据缺失时为空。
type QueryItem struct {
	ID             string          `json:"id,omitempty"`
	DeviceID       string          `json:"device_id"`
	Category       Category        `json:"category"`
	CategoryLabel  string          `json:"category_label"`
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	PackageURL     string          `json:"purl,omitempty"`
	CPE            string          `json:"cpe,omitempty"`
	ListeningPorts []ListeningPort `json:"listening_ports,omitempty"`
	BelongsTo      string          `json:"belongs_to,omitempty"`
	Note           string          `json:"note,omitempty"`
	Source         string          `json:"source"`
	CollectedAt    *time.Time      `json:"collected_at,omitempty"`
	// ObservedStableKey 供声明认领实测使用；声明-only 条目为空。
	ObservedStableKey string `json:"observed_stable_key,omitempty"`
	DeclaredVersion   string `json:"declared_version,omitempty"`
	VersionMismatch   bool   `json:"version_mismatch,omitempty"`
	Mismatch          bool   `json:"mismatch,omitempty"`
}

// QueryDeviceResult 是一台设备的查询结果：设备、采集状态与匹配的软件
// 条目。
type QueryDeviceResult struct {
	DeviceID   string           `json:"device_id"`
	DeviceName string           `json:"device_name"`
	Collection CollectionStatus `json:"collection"`
	Items      []QueryItem      `json:"items"`
}

// QueryResult 是一次软件清单查询的结果。设备按名字稳定排序；每台设备
// 内的条目保持类别、名称、版本的顺序。
type QueryResult struct {
	// ProfileName 是这次查询使用的可见范围名称。本地 owner 查询填
	// owner；在线查询由权威端按设备码绑定的 profile 填写，供导出文档头
	// 使用。
	ProfileName string              `json:"profile_name,omitempty"`
	Devices     []QueryDeviceResult `json:"devices"`
}

// exposureRank 给六档暴露级别排出从低到高的顺序。这个顺序只用于「最低
// 暴露级别」过滤，与领域词一节的档位列举一致。
func exposureRank(exposure Exposure) int {
	switch exposure {
	case ExposureLocalMachine:
		return 1
	case ExposureSpecificAddress:
		return 2
	case ExposureAllAddresses:
		return 3
	case ExposureLocalAreaNetwork:
		return 4
	case ExposureVirtualLocalAreaNetwork:
		return 5
	case ExposurePublicNetwork:
		return 6
	default:
		return 0
	}
}

// queryBundle 是统一查询在内存里合成前的一台设备数据。
type queryBundle struct {
	DeviceID     string
	DeviceName   string
	Managed      bool
	Declarations []SoftwareDeclarationView
	Observed     []models.SoftwareObservedItem
}

// queryItemFromView 把一条声明视图转换成查询条目。受管设备上的声明若没有
// 认领到当前实测，就带着「对不上」标记显示。
func queryItemFromView(row SoftwareDeclarationView, mismatch bool) QueryItem {
	item := QueryItem{
		ID: row.ID, DeviceID: row.DeviceID, Category: row.Category,
		CategoryLabel: row.Category.Label(), Name: row.Name, Version: row.Version,
		PackageURL: row.PackageURL, CPE: row.CPE, ListeningPorts: row.ListeningPorts,
		BelongsTo: row.BelongsTo, Note: row.Note, Source: SourceDeclared,
		Mismatch: mismatch,
	}
	if item.ListeningPorts == nil {
		item.ListeningPorts = []ListeningPort{}
	}
	return item
}

// queryItemFromObserved 把一条未认领的实测条目转换成查询条目。
func queryItemFromObserved(deviceID string, row models.SoftwareObservedItem) QueryItem {
	collectedAt := row.CollectedAt
	ports := append([]ListeningPort(nil), row.ListeningPorts...)
	if ports == nil {
		ports = []ListeningPort{}
	}
	return QueryItem{
		DeviceID: deviceID, Category: row.Category, CategoryLabel: row.Category.Label(),
		Name: row.Name, Version: row.Version, PackageURL: row.PackageURL,
		ListeningPorts: ports, BelongsTo: row.BelongsTo, Source: SourceObserved,
		CollectedAt: &collectedAt, ObservedStableKey: row.StableKey,
	}
}

// combineObservedAndDeclared 按认领关系合成一台设备的条目。认领关系存在且
// 实测仍存在时输出一条「两者合成」；其余声明保持声明来源，其余实测保持实
// 测来源。
func combineObservedAndDeclared(bundle queryBundle) []QueryItem {
	observedByKey := make(map[string]models.SoftwareObservedItem, len(bundle.Observed))
	for _, row := range bundle.Observed {
		observedByKey[row.StableKey] = row
	}
	out := make([]QueryItem, 0, len(bundle.Declarations)+len(bundle.Observed))
	for _, declaration := range bundle.Declarations {
		observed, claimed := observedByKey[declaration.ClaimedObservedKey]
		if declaration.ClaimedObservedKey == "" || !claimed {
			out = append(out, queryItemFromView(declaration, bundle.Managed))
			continue
		}
		delete(observedByKey, declaration.ClaimedObservedKey)
		collectedAt := observed.CollectedAt
		version := observed.Version
		if version == "" {
			version = declaration.Version
		}
		item := QueryItem{
			ID: declaration.ID, DeviceID: bundle.DeviceID, Category: declaration.Category,
			CategoryLabel: declaration.Category.Label(), Name: declaration.Name,
			Version: version, PackageURL: declaration.PackageURL,
			CPE: declaration.CPE, ListeningPorts: combinedPorts(observed.ListeningPorts, declaration.ListeningPorts),
			BelongsTo: declaration.BelongsTo, Note: declaration.Note, Source: SourceCombined,
			CollectedAt: &collectedAt, ObservedStableKey: observed.StableKey,
			DeclaredVersion: declaration.Version,
			VersionMismatch: declaration.Version != observed.Version && observed.Version != "",
		}
		if item.ListeningPorts == nil {
			item.ListeningPorts = []ListeningPort{}
		}
		out = append(out, item)
	}
	for _, row := range bundle.Observed {
		if _, claimed := observedByKey[row.StableKey]; !claimed {
			continue
		}
		out = append(out, queryItemFromObserved(bundle.DeviceID, row))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return categoryRank(out[i].Category) < categoryRank(out[j].Category)
		}
		if strings.ToLower(out[i].Name) != strings.ToLower(out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].ObservedStableKey+out[i].ID < out[j].ObservedStableKey+out[j].ID
	})
	return out
}

// combinedPorts 保留实测端口集合；声明对同一端口给出暴露级别时，用声明值
// 替换实测的三档推导值。
func combinedPorts(observed, declared []ListeningPort) []ListeningPort {
	out := append([]ListeningPort(nil), observed...)
	for i := range out {
		replacement := ListeningPort{}
		found := false
		for _, port := range declared {
			if port.Port != out[i].Port || port.Protocol != out[i].Protocol {
				continue
			}
			if port.Address == out[i].Address {
				replacement, found = port, true
				break
			}
			if !found || port.Address == "" {
				replacement, found = port, true
			}
		}
		if found {
			out[i].Exposure = replacement.Exposure
		}
	}
	return out
}

func categoryRank(category Category) int {
	switch category {
	case CategoryOperatingSystem:
		return 1
	case CategoryServiceProgram:
		return 2
	case CategoryContainerImage:
		return 3
	case CategoryDriverFirmware:
		return 4
	case CategorySystemPackage:
		return 5
	case CategoryUnidentified:
		return 6
	default:
		return 7
	}
}

// missingCollectionStatus 返回采集数据源缺失时的采集状态：从未采集，
// 因此清单过期。
func missingCollectionStatus() CollectionStatus {
	return CollectionStatus{NeverCollected: true, Stale: true}
}

// Query 按可见设备范围与过滤条件查询软件清单。指定设备但设备不在可见
// 范围内时返回 ErrDeviceNotVisible。
func (m *Manager) Query(options QueryOptions) (QueryResult, error) {
	options.Device = strings.TrimSpace(options.Device)
	options.Name = strings.TrimSpace(options.Name)
	if options.Category != "" {
		switch options.Category {
		case CategoryOperatingSystem, CategoryServiceProgram, CategoryContainerImage,
			CategoryDriverFirmware, CategorySystemPackage, CategoryUnidentified:
		default:
			return QueryResult{}, fmt.Errorf("软件类别代号不合法：%q", options.Category)
		}
	}
	if options.MinimumExposure != "" && exposureRank(options.MinimumExposure) == 0 {
		return QueryResult{}, fmt.Errorf("暴露级别代号不合法：%q", options.MinimumExposure)
	}

	var bundles []queryBundle
	if options.Device != "" {
		overview, err := m.st.SoftwareDeviceOverviewForDevice(options.Device, options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		stored, err := m.st.SoftwareDeclarationsForDevice(options.Device, options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		observed, err := m.st.SoftwareObservedItemsForDeviceInScope(options.Device, options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		bundles = []queryBundle{{
			DeviceID: overview.ID, DeviceName: overview.Name, Managed: overview.Managed,
			Declarations: stored.Declarations, Observed: observed.Items,
		}}
	} else {
		overviews, err := m.st.SoftwareDeviceOverviews(options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		index := make(map[string]int, len(overviews))
		for _, overview := range overviews {
			index[overview.ID] = len(bundles)
			bundles = append(bundles, queryBundle{
				DeviceID: overview.ID, DeviceName: overview.Name, Managed: overview.Managed,
			})
		}
		rows, err := m.st.SearchSoftwareDeclarations("", options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		for _, row := range rows {
			position, ok := index[row.DeviceID]
			if !ok {
				return QueryResult{}, ErrDeviceNotVisible
			}
			bundles[position].Declarations = append(bundles[position].Declarations, row)
		}
		observed, err := m.st.SoftwareObservedItemsForScope(options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		for _, inventory := range observed {
			position, ok := index[inventory.DeviceID]
			if !ok {
				return QueryResult{}, ErrDeviceNotVisible
			}
			bundles[position].Observed = inventory.Items
		}
	}

	devices := make([]QueryDeviceResult, 0)
	for _, bundle := range bundles {
		items := combineObservedAndDeclared(bundle)
		filtered := make([]QueryItem, 0, len(items))
		for _, item := range items {
			if options.Category != "" && item.Category != options.Category {
				continue
			}
			if options.Name != "" &&
				!strings.Contains(strings.ToLower(item.Name), strings.ToLower(options.Name)) {
				continue
			}
			if item.Category == CategorySystemPackage && !options.IncludeSystemPackages {
				continue
			}
			if options.MinimumExposure != "" &&
				!meetsMinimumExposure(item.ListeningPorts, options.MinimumExposure) {
				continue
			}
			filtered = append(filtered, item)
		}
		if len(filtered) == 0 {
			continue
		}
		devices = append(devices, QueryDeviceResult{
			DeviceID: bundle.DeviceID, DeviceName: bundle.DeviceName,
			Collection: m.collectionStatusFor(bundle.DeviceName), Items: filtered,
		})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].DeviceName < devices[j].DeviceName })
	return QueryResult{Devices: devices}, nil
}

// collectionStatusFor 读取一台设备的采集状态；设备从未采集过（或采集状
// 态读取失败）时保持「从未采集、清单过期」的显示。
func (m *Manager) collectionStatusFor(deviceName string) CollectionStatus {
	stored, err := m.st.SoftwareCollectionStatusForDevice(deviceName)
	if err != nil || stored.LastSuccessAt.Unix() <= 0 {
		return missingCollectionStatus()
	}
	status := CollectionStatus{
		Collector: stored.Collector, ServerEntry: stored.LastServerName,
		LastFailureReason: stored.LastFailureReason, Incomplete: stored.LastIncomplete,
		CommandSetVersion: stored.CommandSetVersion,
	}
	successAt := stored.LastSuccessAt
	status.LastSuccessAt = &successAt
	if stored.LastFailureAt.Unix() > 0 {
		failureAt := stored.LastFailureAt
		status.LastFailureAt = &failureAt
	}
	// 清单过期不落库，每次读取时按「当前时间减上次成功时间超过 7 天」
	// 计算；从未成功过的设备在上面已经按从未采集返回。
	status.Stale = m.now().Sub(successAt) > 7*24*time.Hour
	return status
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// meetsMinimumExposure 判断一组监听端口里是否有至少一个达到最低暴露
// 级别。没有监听端口的条目不满足任何最低级别。
func meetsMinimumExposure(ports []ListeningPort, minimum Exposure) bool {
	wanted := exposureRank(minimum)
	for _, port := range ports {
		if exposureRank(port.Exposure) >= wanted {
			return true
		}
	}
	return false
}
