package softwareinventory

import (
	"sort"
	"strings"
)

// DisplayCategory 是 Web UI 分组与 CSV 总表共用的软件条目展示类别。代号
// 固定，页面不再用中文文案反推类别。
type DisplayCategory int

// 展示类别的固定顺序，即设备页的分组顺序：重点清单在前（操作系统与内
// 核、监听程序、容器、驱动与固件），系统包最后并默认折叠。「容器」分组
// 同时承载第 3 版的容器条目与第 2 版结果里的容器镜像条目。
const (
	DisplayCategoryOperatingSystemKernel DisplayCategory = iota
	DisplayCategoryListeningProgram
	DisplayCategoryContainer
	DisplayCategoryDriverFirmware
	DisplayCategorySystemPackage
)

var displayCategoryNames = map[DisplayCategory]struct{ code, label string }{
	DisplayCategoryOperatingSystemKernel: {"operating_system_kernel", "操作系统与内核"},
	DisplayCategoryListeningProgram:      {"listening_program", "监听程序"},
	DisplayCategoryContainer:             {"container", "容器"},
	DisplayCategoryDriverFirmware:        {"driver_firmware", "驱动与固件"},
	DisplayCategorySystemPackage:         {"system_package", "系统包"},
}

// Code 返回展示类别的稳定代号。
func (c DisplayCategory) Code() string { return displayCategoryNames[c].code }

// Label 返回展示类别的中文显示名。
func (c DisplayCategory) Label() string { return displayCategoryNames[c].label }

// AllDisplayCategories 按页面分组顺序返回全部展示类别。
func AllDisplayCategories() []DisplayCategory {
	return []DisplayCategory{
		DisplayCategoryOperatingSystemKernel, DisplayCategoryListeningProgram,
		DisplayCategoryContainer, DisplayCategoryDriverFirmware,
		DisplayCategorySystemPackage,
	}
}

// IsKernelItem 判断一条查询结果是否是采集得到的内核条目。固定命令集
// 中内核条目保存在驱动与固件类别里，名称以「内核」结尾。
func IsKernelItem(item QueryItem) bool {
	return item.Category == CategoryDriverFirmware && strings.HasSuffix(item.Name, "内核")
}

// ItemDisplayCategory 把一条查询结果映射为页面与 CSV 共用的展示类别：
// 操作系统条目与内核条目合并展示；服务程序、未识别进程与带监听端口的
// 系统包都是「监听程序」（监听端口的程序）；容器与容器镜像归入「容器」；
// 其余系统包单独分组。
func ItemDisplayCategory(item QueryItem) DisplayCategory {
	switch {
	case item.Category == CategoryOperatingSystem || IsKernelItem(item):
		return DisplayCategoryOperatingSystemKernel
	case item.Category == CategoryServiceProgram || item.Category == CategoryUnidentified:
		return DisplayCategoryListeningProgram
	case item.Category == CategoryContainer || item.Category == CategoryContainerImage:
		return DisplayCategoryContainer
	case item.Category == CategoryDriverFirmware:
		return DisplayCategoryDriverFirmware
	case item.Category == CategorySystemPackage && len(item.ListeningPorts) > 0:
		return DisplayCategoryListeningProgram
	default:
		return DisplayCategorySystemPackage
	}
}

// SortListeningProgramItems 给监听程序分组的条目排序：按全部监听端口的
// 最高暴露级别从高到低，再按名称。仅本机监听的程序保留在后面，没有监听
// 端口的条目排最后。
func SortListeningProgramItems(items []QueryItem) {
	sort.SliceStable(items, func(i, j int) bool {
		left, right := highestExposureRank(items[i].ListeningPorts), highestExposureRank(items[j].ListeningPorts)
		if left != right {
			return left > right
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}

// highestExposureRank 返回一组监听端口里最高的暴露级别顺序值；没有端
// 口时为零，排在任何有端口的条目后面。
func highestExposureRank(ports []ListeningPort) int {
	highest := 0
	for _, port := range ports {
		if rank := exposureRank(port.Exposure); rank > highest {
			highest = rank
		}
	}
	return highest
}
