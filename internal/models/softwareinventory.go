package models

import "errors"

// SoftwareCategory 是软件清单条目的类别。代号保存在数据库与 JSON 里；
// Label 给命令行和 Web UI 显示中文。
type SoftwareCategory string

const (
	SoftwareCategoryOperatingSystem SoftwareCategory = "operating_system"
	SoftwareCategoryServiceProgram  SoftwareCategory = "service_program"
	SoftwareCategoryContainerImage  SoftwareCategory = "container_image"
	SoftwareCategoryDriverFirmware  SoftwareCategory = "driver_firmware"
	SoftwareCategorySystemPackage   SoftwareCategory = "system_package"
	SoftwareCategoryUnidentified    SoftwareCategory = "unidentified_process"
)

// Label 返回软件类别的中文显示名。
func (c SoftwareCategory) Label() string {
	switch c {
	case SoftwareCategoryOperatingSystem:
		return "操作系统"
	case SoftwareCategoryServiceProgram:
		return "服务程序"
	case SoftwareCategoryContainerImage:
		return "容器镜像"
	case SoftwareCategoryDriverFirmware:
		return "驱动与固件"
	case SoftwareCategorySystemPackage:
		return "系统包"
	case SoftwareCategoryUnidentified:
		return "未识别进程"
	default:
		return string(c)
	}
}

// SoftwareExposure 是一个监听端口的暴露级别。六个代号都保存在数据库与
// JSON 里；后三档只能来自声明。
type SoftwareExposure string

const (
	SoftwareExposureLocal           SoftwareExposure = "local_machine"
	SoftwareExposureSpecificAddress SoftwareExposure = "specific_address"
	SoftwareExposureAllAddresses    SoftwareExposure = "all_addresses"
	SoftwareExposureLAN             SoftwareExposure = "local_area_network"
	SoftwareExposureVirtualLAN      SoftwareExposure = "virtual_local_area_network"
	SoftwareExposurePublic          SoftwareExposure = "public_network"
)

// Label 返回暴露级别的中文显示名。
func (e SoftwareExposure) Label() string {
	switch e {
	case SoftwareExposureLocal:
		return "仅本机"
	case SoftwareExposureSpecificAddress:
		return "监听特定地址"
	case SoftwareExposureAllAddresses:
		return "监听所有地址"
	case SoftwareExposureLAN:
		return "局域网"
	case SoftwareExposureVirtualLAN:
		return "虚拟局域网"
	case SoftwareExposurePublic:
		return "公网"
	default:
		return string(e)
	}
}

// SoftwareListeningPort 是软件条目的一个监听端口。地址、端口与协议原样
// 保存；暴露级别使用上面的六档代号。
type SoftwareListeningPort struct {
	Address  string           `json:"address"`
	Port     int              `json:"port"`
	Protocol string           `json:"protocol"`
	Exposure SoftwareExposure `json:"exposure"`
}

// SoftwareDeclaration 是 owner 登记的一条声明。版本字段必须原样保存，不能
// 规格化或截断；可选标识、所属与备注同样不改变原文。
type SoftwareDeclaration struct {
	Device         string                  `json:"device"`
	Category       SoftwareCategory        `json:"category"`
	Name           string                  `json:"name"`
	Version        string                  `json:"version"`
	PackageURL     string                  `json:"purl,omitempty"`
	CPE            string                  `json:"cpe,omitempty"`
	ListeningPorts []SoftwareListeningPort `json:"listening_ports,omitempty"`
	BelongsTo      string                  `json:"belongs_to,omitempty"`
	Note           string                  `json:"note,omitempty"`
}

// SoftwareDeclarationDraft 是 agent 提交的一条声明草稿。它复用生效声明
// 的字段，另带一个可选的实测条目稳定键：草稿确认时这条声明会认领该实
// 测条目（认领实测、认领未识别进程与补充暴露级别都通过这个键表达）。
// 草稿在 owner 确认之前不影响任何查询结果与待办。
type SoftwareDeclarationDraft struct {
	SoftwareDeclaration
	ClaimedObservedKey string `json:"claimed_observed_key,omitempty"`
}

// SoftwareDeclarationEdit 的指针为空表示不修改字段；ListeningPorts 为空
// 指针时不修改端口，指向空切片表示清除全部端口。
type SoftwareDeclarationEdit struct {
	Device         *string
	Category       *SoftwareCategory
	Name           *string
	Version        *string
	PackageURL     *string
	CPE            *string
	ListeningPorts *[]SoftwareListeningPort
	BelongsTo      *string
	Note           *string
}

var (
	ErrSoftwareCategoryInvalid = errors.New("软件类别不合法")
	ErrSoftwareNameRequired    = errors.New("软件名字不能为空")
	ErrSoftwareVersionRequired = errors.New("软件版本不能为空")
	ErrSoftwareExposureInvalid = errors.New("端口暴露级别不合法")
	ErrSoftwarePortInvalid     = errors.New("监听端口不合法：端口范围为 1 到 65535，协议只能是 tcp 或 udp")
	ErrSoftwareProtocolInvalid = errors.New("监听端口协议只能是 tcp 或 udp")
)

// ValidateSoftwareDeclaration 校验一条 owner 声明。声明阶段可用的类别不
// 包含未识别进程；后续认领未识别进程的票会把它转换成真实类别。
func ValidateSoftwareDeclaration(declaration SoftwareDeclaration) error {
	switch declaration.Category {
	case SoftwareCategoryOperatingSystem, SoftwareCategoryServiceProgram,
		SoftwareCategoryContainerImage, SoftwareCategoryDriverFirmware,
		SoftwareCategorySystemPackage:
	default:
		return ErrSoftwareCategoryInvalid
	}
	if declaration.Name == "" {
		return ErrSoftwareNameRequired
	}
	if declaration.Version == "" {
		return ErrSoftwareVersionRequired
	}
	return ValidateSoftwarePorts(declaration.ListeningPorts)
}

// ValidateSoftwarePorts 校验端口、协议与暴露级别。
func ValidateSoftwarePorts(ports []SoftwareListeningPort) error {
	for _, port := range ports {
		if port.Port < 1 || port.Port > 65535 {
			return ErrSoftwarePortInvalid
		}
		if port.Protocol != "tcp" && port.Protocol != "udp" {
			return ErrSoftwareProtocolInvalid
		}
		switch port.Exposure {
		case SoftwareExposureLocal, SoftwareExposureSpecificAddress,
			SoftwareExposureAllAddresses, SoftwareExposureLAN,
			SoftwareExposureVirtualLAN, SoftwareExposurePublic:
		default:
			return ErrSoftwareExposureInvalid
		}
	}
	return nil
}
