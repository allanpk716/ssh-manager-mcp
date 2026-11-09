package softwareinventory

import (
	"fmt"
	"sort"
	"strings"
	"time"
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

// queryItemFromView 把一条声明视图转换成查询条目。
func queryItemFromView(row SoftwareDeclarationView) QueryItem {
	item := QueryItem{
		ID: row.ID, DeviceID: row.DeviceID, Category: row.Category,
		CategoryLabel: row.Category.Label(), Name: row.Name, Version: row.Version,
		PackageURL: row.PackageURL, CPE: row.CPE, ListeningPorts: row.ListeningPorts,
		BelongsTo: row.BelongsTo, Note: row.Note, Source: SourceDeclared,
	}
	if item.ListeningPorts == nil {
		item.ListeningPorts = []ListeningPort{}
	}
	return item
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

	var rows []SoftwareDeclarationView
	if options.Device != "" {
		stored, err := m.st.SoftwareDeclarationsForDevice(options.Device, options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
		rows = stored.Declarations
	} else {
		var err error
		rows, err = m.st.SearchSoftwareDeclarations(options.Name, options.Visibility)
		if err != nil {
			return QueryResult{}, err
		}
	}

	devices := make([]QueryDeviceResult, 0)
	index := map[string]int{}
	for _, row := range rows {
		if options.Category != "" && row.Category != options.Category {
			continue
		}
		if options.Device == "" && options.Name != "" &&
			!strings.Contains(strings.ToLower(row.Name), strings.ToLower(options.Name)) {
			continue
		}
		if row.Category == CategorySystemPackage && !options.IncludeSystemPackages {
			continue
		}
		if options.MinimumExposure != "" && !meetsMinimumExposure(row.ListeningPorts, options.MinimumExposure) {
			continue
		}
		position, ok := index[row.DeviceID]
		if !ok {
			devices = append(devices, QueryDeviceResult{
				DeviceID: row.DeviceID, DeviceName: row.DeviceName,
				Collection: missingCollectionStatus(), Items: []QueryItem{},
			})
			position = len(devices) - 1
			index[row.DeviceID] = position
		}
		devices[position].Items = append(devices[position].Items, queryItemFromView(row))
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].DeviceName < devices[j].DeviceName })
	return QueryResult{Devices: devices}, nil
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
