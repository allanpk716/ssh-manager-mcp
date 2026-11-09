package models

import "time"

// SoftwareObservedItem 是一次实测采集中的一条软件条目。Version 原样保留
// 包管理器或镜像仓库给出的字符串；StableKey 是整体替换实测结果后仍保持
// 稳定的条目键（设备 + 类别 + 名称 + 所属；未识别进程再加端口、协议与
// 可执行文件路径），声明认领实测时引用它。
type SoftwareObservedItem struct {
	Category         SoftwareCategory        `json:"category"`
	Name             string                  `json:"name"`
	Version          string                  `json:"version"`
	PackageURL       string                  `json:"purl,omitempty"`
	BelongsTo        string                  `json:"belongs_to,omitempty"`
	Architecture     string                  `json:"architecture,omitempty"`
	ContainerTag     string                  `json:"container_tag,omitempty"`
	ContainerDigest  string                  `json:"container_digest,omitempty"`
	ContainerRunning bool                    `json:"container_running,omitempty"`
	ExecutablePath   string                  `json:"executable_path,omitempty"`
	ProcessName      string                  `json:"process_name,omitempty"`
	StableKey        string                  `json:"stable_key"`
	ListeningPorts   []SoftwareListeningPort `json:"listening_ports,omitempty"`
	CollectedAt      time.Time               `json:"collected_at"`
}

// SoftwareCollectionStatus 是一台设备的采集状态。清单是否过期不在这里存
// 储，读取时按「当前时间减上次成功时间超过 7 天」计算。
type SoftwareCollectionStatus struct {
	DeviceID          string    `json:"device_id"`
	DeviceName        string    `json:"device_name"`
	Collector         string    `json:"collector"`
	PreferredServerID string    `json:"preferred_server_id,omitempty"`
	LastServerID      string    `json:"last_server_id,omitempty"`
	LastServerName    string    `json:"last_server_name,omitempty"`
	LastSuccessAt     time.Time `json:"last_success_at"`
	LastFailureAt     time.Time `json:"last_failure_at"`
	LastFailureReason string    `json:"last_failure_reason,omitempty"`
	LastIncomplete    bool      `json:"last_incomplete"`
	CommandSetVersion string    `json:"command_set_version,omitempty"`
	SystemType        string    `json:"system_type,omitempty"`
	PendingImmediate  bool      `json:"pending_immediate"`
}
