package models

import "time"

// SoftwareChangeKind 是一条软件清单变化历史的种类。
type SoftwareChangeKind string

const (
	SoftwareChangeAdded           SoftwareChangeKind = "added"
	SoftwareChangeRemoved         SoftwareChangeKind = "removed"
	SoftwareChangeVersionChanged  SoftwareChangeKind = "version_changed"
	SoftwareChangeExposureChanged SoftwareChangeKind = "exposure_changed"
)

// Label 返回变化种类的中文显示名。
func (k SoftwareChangeKind) Label() string {
	switch k {
	case SoftwareChangeAdded:
		return "新增"
	case SoftwareChangeRemoved:
		return "移除"
	case SoftwareChangeVersionChanged:
		return "版本变化"
	case SoftwareChangeExposureChanged:
		return "暴露级别变化"
	default:
		return string(k)
	}
}

// SoftwareChangeEntry 是一条软件清单变化历史。OldValue 与 NewValue 按变
// 化种类分别保存旧版本/新版本或旧暴露级别/新暴露级别的代号；条目稳定键
// 与实测条目使用同一套构造规则。
type SoftwareChangeEntry struct {
	ID         string             `json:"id"`
	DeviceID   string             `json:"device_id"`
	DeviceName string             `json:"device_name"`
	OccurredAt time.Time          `json:"occurred_at"`
	Kind       SoftwareChangeKind `json:"kind"`
	Category   SoftwareCategory   `json:"category"`
	Name       string             `json:"name"`
	StableKey  string             `json:"stable_key"`
	OldValue   string             `json:"old_value,omitempty"`
	NewValue   string             `json:"new_value,omitempty"`
}
