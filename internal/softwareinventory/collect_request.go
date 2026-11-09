package softwareinventory

import (
	"context"

	"ssh-manager-mcp/internal/store"
)

// CollectForScope 对一台可见设备立即采集一次。语义与命令行的「立即采集」
// 相同：先核对设备在可见范围内，再交给采集服务同步执行；采集命令集固
// 定随 sshmgr 发布，调用方不能传任何命令参数。
func (m *Manager) CollectForScope(
	ctx context.Context,
	deviceName string,
	visibility Visibility,
	service *Service,
	source Source,
) (*CollectionReport, error) {
	if _, err := m.st.SoftwareDeviceOverviewForDevice(deviceName, visibility); err != nil {
		if err == store.ErrSoftwareDeviceNotVisible {
			return nil, ErrDeviceNotVisible
		}
		return nil, err
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
	return report, nil
}
