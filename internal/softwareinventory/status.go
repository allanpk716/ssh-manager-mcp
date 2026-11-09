package softwareinventory

import (
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// ChangeEntry 是一条软件清单变化历史。领域层直接使用存储与模型层的稳
// 定形态。
type ChangeEntry = models.SoftwareChangeEntry

// CollectionStatusView 是命令行与后续 Web UI 显示的一台设备采集状态。
// Stale 不落库，每次读取时按「当前时间减上次成功时间超过 7 天」计算。
type CollectionStatusView struct {
	DeviceID          string     `json:"device_id"`
	DeviceName        string     `json:"device_name"`
	Managed           bool       `json:"managed"`
	Collector         string     `json:"collector,omitempty"`
	CollectorLabel    string     `json:"collector_label,omitempty"`
	ServerEntry       string     `json:"server_entry,omitempty"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt     *time.Time `json:"last_failure_at,omitempty"`
	LastFailureReason string     `json:"last_failure_reason,omitempty"`
	Incomplete        bool       `json:"incomplete,omitempty"`
	CommandSetVersion string     `json:"command_set_version,omitempty"`
	NeverCollected    bool       `json:"never_collected"`
	Stale             bool       `json:"stale"`
	PendingImmediate  bool       `json:"pending_immediate"`
}

// collectorLabel 把采集执行者代号翻译成显示名。客户端实例执行者登记在
// 后续票里实现，当前只识别权威端。
func collectorLabel(collector string) string {
	if collector == "authority" || collector == "" {
		return "权威端"
	}
	return collector
}

// collectionStatusView 把存储层采集状态转换成显示形态并计算清单过期。
func (m *Manager) collectionStatusView(stored models.SoftwareCollectionStatus) CollectionStatusView {
	view := CollectionStatusView{
		DeviceID: stored.DeviceID, DeviceName: stored.DeviceName, Managed: stored.Managed,
		Collector: stored.Collector, CollectorLabel: collectorLabel(stored.Collector),
		ServerEntry: stored.LastServerName, LastFailureReason: stored.LastFailureReason,
		Incomplete: stored.LastIncomplete, CommandSetVersion: stored.CommandSetVersion,
		PendingImmediate: stored.PendingImmediate,
	}
	if stored.LastSuccessAt.Unix() <= 0 {
		view.NeverCollected = true
		// 从未成功采集过的受管设备算过期；不受管设备没有采集执行者，
		// 不算过期。
		view.Stale = stored.Managed
		if stored.LastFailureAt.Unix() > 0 {
			failureAt := stored.LastFailureAt
			view.LastFailureAt = &failureAt
		}
		return view
	}
	successAt := stored.LastSuccessAt
	view.LastSuccessAt = &successAt
	if stored.LastFailureAt.Unix() > 0 {
		failureAt := stored.LastFailureAt
		view.LastFailureAt = &failureAt
	}
	view.Stale = m.now().Sub(successAt) > 7*24*time.Hour
	return view
}

// CollectionStatuses 列出采集状态。deviceName 为空时列出可见范围内的全
// 部设备；非空时只查那台设备，设备不可见时返回 ErrDeviceNotVisible。
func (m *Manager) CollectionStatuses(
	visibility Visibility,
	deviceName string,
) ([]CollectionStatusView, error) {
	deviceName = strings.TrimSpace(deviceName)
	if deviceName != "" {
		visible, err := m.st.SoftwareDeviceVisible(deviceName, visibility)
		if err != nil {
			return nil, err
		}
		if !visible {
			return nil, ErrDeviceNotVisible
		}
		statuses, err := m.st.SoftwareCollectionStatuses(deviceName)
		if err != nil {
			return nil, err
		}
		out := make([]CollectionStatusView, 0, len(statuses))
		for _, status := range statuses {
			out = append(out, m.collectionStatusView(status))
		}
		return out, nil
	}
	statuses, err := m.st.SoftwareCollectionStatuses("")
	if err != nil {
		return nil, err
	}
	out := make([]CollectionStatusView, 0, len(statuses))
	for _, status := range statuses {
		visible, err := m.st.SoftwareDeviceVisible(status.DeviceName, visibility)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		out = append(out, m.collectionStatusView(status))
	}
	return out, nil
}

// ChangeHistory 按时间倒序返回一台设备的变化历史。设备不在可见范围内
// 时返回 ErrDeviceNotVisible。
func (m *Manager) ChangeHistory(
	deviceName string,
	visibility Visibility,
) ([]ChangeEntry, error) {
	deviceName = strings.TrimSpace(deviceName)
	visible, err := m.st.SoftwareDeviceVisible(deviceName, visibility)
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, ErrDeviceNotVisible
	}
	return m.st.SoftwareChangeHistoryForDevice(deviceName)
}
