// Package softwareinventory 提供与界面无关的软件清单领域模块，承担声明
// 写入、实测与声明的统一查询、认领合成、待办计算和固定命令采集。
package softwareinventory

import (
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// Manager 是软件清单领域模块的入口。它把校验、可见设备范围与分组查询
// 收在同一层，命令行和后续 Web UI 只做参数适配与展示。
type Manager struct {
	st *store.Store
	// Now 提供查询侧的时间读数（清单过期按读取时刻计算）；空值使用真实
	// 时钟，测试可以注入固定时钟。
	Now func() time.Time
}

// New 返回挂在一个已解锁 vault 上的软件清单领域模块。
func New(st *store.Store) *Manager { return &Manager{st: st} }

// Source 描述一次软件清单写操作来自哪里。当前命令行使用 CLISource；后续
// Web UI 使用 WebUISource。
type Source struct {
	origin store.WriteSource
}

// CLISource 返回命令行来源。
func CLISource() Source { return Source{origin: store.CLISource()} }

// WebUISource 返回 Web UI 来源，address 是请求来源地址。
func WebUISource(address string) Source {
	return Source{origin: store.WebUISource(address)}
}

// 领域类型直接使用存储与模型层的稳定形态，避免同一份数据在模块边界被
// 重复定义后产生第二套字段语义。
type (
	Declaration             = models.SoftwareDeclaration
	DeclarationEdit         = models.SoftwareDeclarationEdit
	Category                = models.SoftwareCategory
	Exposure                = models.SoftwareExposure
	ListeningPort           = models.SoftwareListeningPort
	Visibility              = store.SoftwareDeviceScope
	SoftwareDeclarationView = store.SoftwareDeclarationView
	DeviceOverview          = store.SoftwareDeviceOverview
)

// 类别代号。
const (
	CategoryOperatingSystem = models.SoftwareCategoryOperatingSystem
	CategoryServiceProgram  = models.SoftwareCategoryServiceProgram
	CategoryContainerImage  = models.SoftwareCategoryContainerImage
	CategoryDriverFirmware  = models.SoftwareCategoryDriverFirmware
	CategorySystemPackage   = models.SoftwareCategorySystemPackage
	CategoryUnidentified    = models.SoftwareCategoryUnidentified
)

// 暴露级别代号。
const (
	ExposureLocalMachine            = models.SoftwareExposureLocal
	ExposureSpecificAddress         = models.SoftwareExposureSpecificAddress
	ExposureAllAddresses            = models.SoftwareExposureAllAddresses
	ExposureLocalAreaNetwork        = models.SoftwareExposureLAN
	ExposureVirtualLocalAreaNetwork = models.SoftwareExposureVirtualLAN
	ExposurePublicNetwork           = models.SoftwareExposurePublic
)

// 条目来源的显示文本。
const (
	SourceDeclared = "声明"
	SourceObserved = "实测"
	SourceCombined = "两者合成"
)

// ErrDeviceNotVisible 表示请求的设备不在可见范围内。
var ErrDeviceNotVisible = store.ErrSoftwareDeviceNotVisible

// AllDevicesVisible 返回 owner 的可见设备范围。
func AllDevicesVisible() Visibility { return store.AllSoftwareDevices() }

// DevicesVisibleThroughServers 返回 profile 的可见设备范围。
func DevicesVisibleThroughServers(serverIDs ...string) Visibility {
	return store.SoftwareDevicesForServers(serverIDs...)
}

// Item 是按设备查看和跨设备搜索结果里的一条软件条目，来源可能是实测、
// 声明或两者合成。
type Item struct {
	ID             string          `json:"id"`
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
	// ObservedStableKey 是认领实测时使用的稳定键，只在条目有实测来源时填。
	ObservedStableKey string `json:"observed_stable_key,omitempty"`
	DeclaredVersion   string `json:"declared_version,omitempty"`
	VersionMismatch   bool   `json:"version_mismatch,omitempty"`
	Mismatch          bool   `json:"mismatch,omitempty"`
}

// ItemGroup 是按设备查看时的一个类别分组。系统包默认只返回数量；加参数
// 后 Items 才携带全部系统包。
type ItemGroup struct {
	Category              Category `json:"category"`
	CategoryLabel         string   `json:"category_label"`
	Items                 []Item   `json:"items"`
	SystemPackageCount    int      `json:"system_package_count,omitempty"`
	SystemPackagesOmitted bool     `json:"system_packages_omitted,omitempty"`
}

// DeviceInventory 是一台设备的软件清单。
type DeviceInventory struct {
	DeviceID   string      `json:"device_id"`
	DeviceName string      `json:"device_name"`
	Groups     []ItemGroup `json:"groups"`
}

// DeviceViewOptions 控制按设备查看时是否展开系统包。
type DeviceViewOptions struct {
	IncludeSystemPackages bool
}

// SearchResult 是跨设备搜索的一条结果。
type SearchResult struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Item       Item   `json:"item"`
}

// AddDeclaration 新增一条生效声明，返回声明标识符。
func (m *Manager) AddDeclaration(declaration Declaration, source Source) (string, error) {
	return m.st.AddSoftwareDeclaration(declaration, source.origin)
}

// EditDeclaration 修改一条声明。字段指针为空表示不修改；端口切片指针表示
// 用给定集合整体替换端口。
func (m *Manager) EditDeclaration(id string, edit DeclarationEdit, source Source) error {
	_, err := m.st.EditSoftwareDeclaration(id, edit, source.origin)
	return err
}

// DeleteDeclaration 删除一条声明。删除前确认由命令行或 Web UI 适配层负责。
func (m *Manager) DeleteDeclaration(id string, source Source) error {
	_, err := m.st.DeleteSoftwareDeclaration(id, source.origin)
	return err
}

// ClaimDeclaration 用一条声明认领同一设备上的实测条目。observedStableKey
// 是实测条目的稳定键，不是数据库行号。
func (m *Manager) ClaimDeclaration(
	declarationID, observedStableKey string,
	source Source,
) error {
	_, err := m.st.ClaimSoftwareDeclaration(declarationID, observedStableKey, source.origin)
	return err
}

// UnclaimDeclaration 取消一条声明的认领关系。
func (m *Manager) UnclaimDeclaration(declarationID string, source Source) error {
	_, err := m.st.UnclaimSoftwareDeclaration(declarationID, source.origin)
	return err
}

// ViewDevice 按可见设备范围查看一台设备的软件清单，并按类别分组。它复用
// 统一查询，因此实测、声明与两者合成的结果和查询工具保持一致。
func (m *Manager) ViewDevice(
	deviceName string,
	visibility Visibility,
	options DeviceViewOptions,
) (*DeviceInventory, error) {
	result, err := m.Query(QueryOptions{
		Device:                deviceName,
		IncludeSystemPackages: true,
		Visibility:            visibility,
	})
	if err != nil {
		return nil, err
	}
	if len(result.Devices) == 0 {
		return &DeviceInventory{}, nil
	}
	device := result.Devices[0]
	inventory := &DeviceInventory{DeviceID: device.DeviceID, DeviceName: device.DeviceName}
	for _, row := range device.Items {
		if len(inventory.Groups) == 0 || inventory.Groups[len(inventory.Groups)-1].Category != row.Category {
			inventory.Groups = append(inventory.Groups, ItemGroup{
				Category: row.Category, CategoryLabel: row.CategoryLabel, Items: []Item{},
			})
		}
		group := &inventory.Groups[len(inventory.Groups)-1]
		item := queryItemAsItem(row)
		if row.Category == CategorySystemPackage && !options.IncludeSystemPackages {
			group.SystemPackageCount++
			group.SystemPackagesOmitted = true
			continue
		}
		group.Items = append(group.Items, item)
	}
	return inventory, nil
}

// SearchSoftware 按软件名做大小写不敏感的跨设备搜索，结果同样来自实测与
// 声明的统一合成。
func (m *Manager) SearchSoftware(namePart string, visibility Visibility) ([]SearchResult, error) {
	result, err := m.Query(QueryOptions{
		Name: namePart, IncludeSystemPackages: true, Visibility: visibility,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0)
	for i := range result.Devices {
		for _, row := range result.Devices[i].Items {
			out = append(out, SearchResult{
				DeviceID:   result.Devices[i].DeviceID,
				DeviceName: result.Devices[i].DeviceName,
				Item:       queryItemAsItem(row),
			})
		}
	}
	return out, nil
}

func queryItemAsItem(row QueryItem) Item {
	return Item{
		ID: row.ID, DeviceID: row.DeviceID, Category: row.Category,
		CategoryLabel: row.CategoryLabel, Name: row.Name, Version: row.Version,
		PackageURL: row.PackageURL, CPE: row.CPE, ListeningPorts: row.ListeningPorts,
		BelongsTo: row.BelongsTo, Note: row.Note, Source: row.Source,
		ObservedStableKey: row.ObservedStableKey, VersionMismatch: row.VersionMismatch,
		DeclaredVersion: row.DeclaredVersion, Mismatch: row.Mismatch,
	}
}
