package softwareinventory

import (
	"context"

	"ssh-manager-mcp/internal/store"
)

// ImmediateCollectionOutcome 是一次「立即采集」请求的结论。
type ImmediateCollectionOutcome struct {
	DeviceName string `json:"device_name"`
	// Requested 为 true 表示这台设备登记的采集执行者是一个客户端实
	// 例：这次只登记立即采集请求，等待该实例下一轮分派执行。
	Requested bool `json:"requested"`
	// Collector 是登记的客户端实例的设备码编号；只有 Requested 为 true
	// 时填写。
	Collector string `json:"collector,omitempty"`
	// Report 是本实例同步执行采集时的报告。
	Report *CollectionReport `json:"report,omitempty"`
}

// CollectForScope 对一台可见设备请求立即采集。语义与命令行的「立即采
// 集」相同：采集执行者是权威端或单机时同步执行并返回报告；是客户端实
// 例时只登记立即采集请求。采集命令集固定随 sshmgr 发布，调用方不能传
// 任何命令参数。
func (m *Manager) CollectForScope(
	ctx context.Context,
	deviceName string,
	visibility Visibility,
	service *Service,
	source Source,
) (*ImmediateCollectionOutcome, error) {
	if _, err := m.st.SoftwareDeviceOverviewForDevice(deviceName, visibility); err != nil {
		if err == store.ErrSoftwareDeviceNotVisible {
			return nil, ErrDeviceNotVisible
		}
		return nil, err
	}
	status, err := m.st.SoftwareCollectionStatusForDevice(deviceName)
	if err != nil {
		return nil, err
	}
	if status.Collector != "" && status.Collector != CollectorAuthority {
		if err := m.st.RequestSoftwareImmediateCollection(deviceName, source.origin); err != nil {
			return nil, err
		}
		return &ImmediateCollectionOutcome{
			DeviceName: deviceName, Requested: true, Collector: status.Collector,
		}, nil
	}
	if service == nil {
		service = &Service{Store: m.st, Now: m.Now}
	}
	report, err := service.Collect(ctx, CollectRequest{
		DeviceName: deviceName, Source: source.origin,
	})
	if err != nil {
		return nil, err
	}
	return &ImmediateCollectionOutcome{DeviceName: deviceName, Report: report}, nil
}
