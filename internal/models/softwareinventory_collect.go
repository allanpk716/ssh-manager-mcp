package models

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Collector 是一台设备的采集执行者。空值表示权威端或单机；非空值只能是
// 权威端签发的设备码编号，不能使用设备码名字。
type Collector struct {
	CacheTokenID string `json:"-"`
}

// AuthorityCollector 返回权威端（或单机）采集执行者。
func AuthorityCollector() Collector { return Collector{} }

// ClientCollector 按设备码编号返回客户端实例采集执行者。
func ClientCollector(cacheTokenID string) Collector {
	return Collector{CacheTokenID: strings.TrimSpace(cacheTokenID)}
}

// IsClient 表示这个采集执行者是否是一台客户端实例。
func (c Collector) IsClient() bool { return c.CacheTokenID != "" }

// Scan 让数据库驱动直接把设备码编号读进采集执行者。
func (c *Collector) Scan(value any) error {
	text, ok := value.(string)
	if !ok {
		return errors.New("采集执行者必须是文本形式的设备码编号")
	}
	c.CacheTokenID = text
	return nil
}

// Value 让数据库驱动直接保存设备码编号。
func (c Collector) Value() (driver.Value, error) { return c.CacheTokenID, nil }

// MarshalJSON 把采集执行者输出为设备码编号；权威端输出空字符串。
func (c Collector) MarshalJSON() ([]byte, error) { return json.Marshal(c.CacheTokenID) }

// UnmarshalJSON 只接受设备码编号，避免把设备码名字误当成客户端实例。
func (c *Collector) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &c.CacheTokenID)
}

var _ sql.Scanner = (*Collector)(nil)
var _ driver.Valuer = Collector{}

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
	ProcessID        int                     `json:"process_id,omitempty"`
	StableKey        string                  `json:"stable_key"`
	ListeningPorts   []SoftwareListeningPort `json:"listening_ports,omitempty"`
	CollectedAt      time.Time               `json:"collected_at"`
}

// SoftwareCollectionStatus 是一台设备的采集状态。清单是否过期不在这里存
// 储，读取时按「当前时间减上次成功时间超过 7 天」计算。
type SoftwareCollectionStatus struct {
	DeviceID          string    `json:"device_id"`
	DeviceName        string    `json:"device_name"`
	Managed           bool      `json:"managed"`
	Collector         Collector `json:"collector"`
	CollectorName     string    `json:"collector_name,omitempty"`
	CollectorRevoked  bool      `json:"collector_revoked,omitempty"`
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

// SoftwareCollectionAssignment 是权威端下发给一个客户端实例的采集分派。
type SoftwareCollectionAssignment struct {
	DeviceID         string    `json:"device_id"`
	DeviceName       string    `json:"device_name"`
	ServerID         string    `json:"server_id"`
	ServerName       string    `json:"server_name"`
	Sudo             bool      `json:"sudo"`
	PendingImmediate bool      `json:"pending_immediate"`
	LastSuccessAt    time.Time `json:"last_success_at"`
	Due              bool      `json:"due"`
}
