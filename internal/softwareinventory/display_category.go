package softwareinventory

import "strings"

// DisplayCategory 是 Web UI 分组与 CSV 总表共用的软件条目展示类别。代号
// 固定，页面不再用中文文案反推类别。
type DisplayCategory int

// 展示类别的固定顺序。操作系统条目与内核条目合并展示。
const (
	DisplayCategoryOperatingSystemKernel DisplayCategory = iota
	DisplayCategoryServiceProgram
	DisplayCategoryContainerImage
	DisplayCategoryDriverFirmware
	DisplayCategorySystemPackage
	DisplayCategoryUnidentifiedProcess
)

var displayCategoryNames = map[DisplayCategory]struct{ code, label string }{
	DisplayCategoryOperatingSystemKernel: {"operating_system_kernel", "操作系统与内核"},
	DisplayCategoryServiceProgram:        {"service_program", "服务程序"},
	DisplayCategoryContainerImage:        {"container_image", "容器镜像"},
	DisplayCategoryDriverFirmware:        {"driver_firmware", "驱动与固件"},
	DisplayCategorySystemPackage:         {"system_package", "系统包"},
	DisplayCategoryUnidentifiedProcess:   {"unidentified_process", "未识别进程"},
}

// Code 返回展示类别的稳定代号。
func (c DisplayCategory) Code() string { return displayCategoryNames[c].code }

// Label 返回展示类别的中文显示名。
func (c DisplayCategory) Label() string { return displayCategoryNames[c].label }

// AllDisplayCategories 按页面分组顺序返回全部展示类别。
func AllDisplayCategories() []DisplayCategory {
	return []DisplayCategory{
		DisplayCategoryOperatingSystemKernel, DisplayCategoryServiceProgram,
		DisplayCategoryContainerImage, DisplayCategoryDriverFirmware,
		DisplayCategorySystemPackage, DisplayCategoryUnidentifiedProcess,
	}
}

// IsKernelItem 判断一条查询结果是否是采集得到的内核条目。固定命令集
// 中内核条目保存在驱动与固件类别里，名称以「内核」结尾。
func IsKernelItem(item QueryItem) bool {
	return item.Category == CategoryDriverFirmware && strings.HasSuffix(item.Name, "内核")
}

// ItemDisplayCategory 把一条查询结果映射为页面与 CSV 共用的展示类别。
func ItemDisplayCategory(item QueryItem) DisplayCategory {
	switch {
	case item.Category == CategoryOperatingSystem || IsKernelItem(item):
		return DisplayCategoryOperatingSystemKernel
	case item.Category == CategoryServiceProgram:
		return DisplayCategoryServiceProgram
	case item.Category == CategoryContainerImage:
		return DisplayCategoryContainerImage
	case item.Category == CategoryDriverFirmware:
		return DisplayCategoryDriverFirmware
	case item.Category == CategorySystemPackage:
		return DisplayCategorySystemPackage
	default:
		return DisplayCategoryUnidentifiedProcess
	}
}
