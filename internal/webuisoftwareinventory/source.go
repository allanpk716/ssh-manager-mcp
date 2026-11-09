// Package webuisoftwareinventory 把软件清单领域模块适配成 Web UI 的窄接
// 口。页面数据来自统一查询；立即采集按采集执行者分派。
package webuisoftwareinventory

import (
	"context"
	"errors"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

type openerSource struct {
	open func() (*store.Store, func(), error)
}

// FromOpener 每次页面读取或写操作重新打开存储，避免 Web UI 长期持有存储
// 句柄。
func FromOpener(open func() (*store.Store, func(), error)) webui.SoftwareInventorySource {
	return openerSource{open: open}
}

func (s openerSource) SoftwareInventoryDevices() ([]webui.SoftwareInventoryDeviceSummary, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	statuses, err := softwareinventory.New(st).CollectionStatuses(softwareinventory.AllDevicesVisible(), "")
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventoryDeviceSummary, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, webui.SoftwareInventoryDeviceSummary{
			Name: status.DeviceName, Status: statusView(status),
		})
	}
	return out, nil
}

func (s openerSource) SoftwareInventoryDevice(
	deviceName string,
) (*webui.SoftwareInventoryDevice, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	manager := softwareinventory.New(st)
	statuses, err := manager.CollectionStatuses(softwareinventory.AllDevicesVisible(), deviceName)
	if err != nil {
		return nil, deviceError(deviceName, err)
	}
	if len(statuses) != 1 {
		return nil, &webui.SoftwareInventoryDeviceNotFoundError{DeviceName: deviceName}
	}
	result, err := manager.Query(softwareinventory.QueryOptions{
		Device: deviceName, IncludeSystemPackages: true,
		Visibility: softwareinventory.AllDevicesVisible(),
	})
	if err != nil {
		return nil, deviceError(deviceName, err)
	}
	history, err := manager.ChangeHistory(deviceName, softwareinventory.AllDevicesVisible())
	if err != nil {
		return nil, deviceError(deviceName, err)
	}
	device := &webui.SoftwareInventoryDevice{
		Name: statuses[0].DeviceName, Status: statusView(statuses[0]),
		History: changeViews(history),
	}
	if len(result.Devices) == 1 {
		device.Groups = groupViews(result.Devices[0].Items)
	}
	return device, nil
}

func (s openerSource) SearchSoftwareInventory(
	namePart string,
) ([]webui.SoftwareInventorySearchRow, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := softwareinventory.New(st).SearchSoftware(
		namePart, softwareinventory.AllDevicesVisible(),
	)
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventorySearchRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, webui.SoftwareInventorySearchRow{
			DeviceName: row.DeviceName, Item: itemView(searchItemAsQueryItem(row.Item)),
		})
	}
	return out, nil
}

func searchItemAsQueryItem(item softwareinventory.Item) softwareinventory.QueryItem {
	return softwareinventory.QueryItem{
		ID: item.ID, DeviceID: item.DeviceID, Category: item.Category,
		CategoryLabel: item.CategoryLabel, Name: item.Name, Version: item.Version,
		PackageURL: item.PackageURL, CPE: item.CPE, ListeningPorts: item.ListeningPorts,
		BelongsTo: item.BelongsTo, Note: item.Note, Source: item.Source,
		CollectedAt: item.CollectedAt, ConfirmedAt: item.ConfirmedAt,
		ObservedStableKey: item.ObservedStableKey, DeclaredVersion: item.DeclaredVersion,
		VersionMismatch: item.VersionMismatch, Mismatch: item.Mismatch,
		ContainerTag: item.ContainerTag, ContainerDigest: item.ContainerDigest,
		ContainerRunning: item.ContainerRunning,
	}
}

func (s openerSource) RequestSoftwareInventoryCollection(
	ctx context.Context,
	deviceName string,
	source webui.SoftwareInventoryWriteSource,
) (webui.SoftwareInventoryCollectionResult, error) {
	st, done, err := s.open()
	if err != nil {
		return webui.SoftwareInventoryCollectionResult{}, err
	}
	defer done()
	manager := softwareinventory.New(st)
	statuses, err := manager.CollectionStatuses(softwareinventory.AllDevicesVisible(), deviceName)
	if err != nil || len(statuses) != 1 {
		if err == nil {
			err = &webui.SoftwareInventoryDeviceNotFoundError{DeviceName: deviceName}
		}
		return webui.SoftwareInventoryCollectionResult{}, deviceError(deviceName, err)
	}
	domainSource := softwareinventory.WebUISource(source.RemoteAddress)
	if collectorIsClient(statuses[0].Collector) {
		if err := manager.RequestImmediateCollection(deviceName, domainSource); err != nil {
			return webui.SoftwareInventoryCollectionResult{}, deviceError(deviceName, err)
		}
		return webui.SoftwareInventoryCollectionResult{Requested: true}, nil
	}
	service := &softwareinventory.Service{Store: st}
	if _, err := service.Collect(ctx, softwareinventory.CollectRequest{
		DeviceName: deviceName, Source: domainSource,
	}); err != nil {
		return webui.SoftwareInventoryCollectionResult{}, deviceError(deviceName, err)
	}
	return webui.SoftwareInventoryCollectionResult{}, nil
}

// collectorIsClient 是「这台设备是否由客户端实例采集」的唯一判断点。客
// 户端实例执行者使用活动设备码的名字登记；合并采集执行者功能后仍只需
// 要修改这里。
func collectorIsClient(collector string) bool {
	return collector != "" && collector != softwareinventory.CollectorAuthority
}

func deviceError(deviceName string, err error) error {
	if errors.Is(err, softwareinventory.ErrDeviceNotVisible) {
		return &webui.SoftwareInventoryDeviceNotFoundError{DeviceName: deviceName}
	}
	if errors.Is(err, store.ErrDeviceNotFound) {
		return &webui.SoftwareInventoryDeviceNotFoundError{DeviceName: deviceName}
	}
	return err
}

func statusView(status softwareinventory.CollectionStatusView) webui.SoftwareInventoryStatus {
	return webui.SoftwareInventoryStatus{
		Managed: status.Managed, CollectorLabel: status.CollectorLabel, ServerEntry: status.ServerEntry,
		LastSuccessAt: status.LastSuccessAt, LastFailureAt: status.LastFailureAt,
		LastFailureReason: status.LastFailureReason, Incomplete: status.Incomplete,
		NeverCollected: status.NeverCollected, Stale: status.Stale,
		PendingImmediate: status.PendingImmediate,
	}
}

func groupViews(items []softwareinventory.QueryItem) []webui.SoftwareInventoryGroup {
	groups := make([]webui.SoftwareInventoryGroup, 0)
	for _, item := range items {
		if len(groups) == 0 || groups[len(groups)-1].CategoryLabel != item.CategoryLabel {
			groups = append(groups, webui.SoftwareInventoryGroup{
				CategoryLabel:  item.CategoryLabel,
				SystemPackages: item.Category == softwareinventory.CategorySystemPackage,
			})
		}
		group := &groups[len(groups)-1]
		group.Items = append(group.Items, itemView(item))
		if group.SystemPackages {
			group.SystemPackageCount++
		}
	}
	return groups
}

func itemView(item softwareinventory.QueryItem) webui.SoftwareInventoryItem {
	ports := make([]webui.SoftwareInventoryPort, 0, len(item.ListeningPorts))
	for _, port := range item.ListeningPorts {
		ports = append(ports, webui.SoftwareInventoryPort{
			Address: port.Address, Port: port.Port, Protocol: port.Protocol,
			ExposureLabel: port.Exposure.Label(),
		})
	}
	return webui.SoftwareInventoryItem{
		CategoryLabel: item.CategoryLabel, Name: item.Name, Version: item.Version,
		PackageURL: item.PackageURL, CPE: item.CPE, Source: item.Source,
		CollectedAt: item.CollectedAt, ConfirmedAt: item.ConfirmedAt,
		ListeningPorts: ports, BelongsTo: item.BelongsTo, Note: item.Note,
		ContainerTag: item.ContainerTag, ContainerDigest: item.ContainerDigest,
		ContainerRunning: item.ContainerRunning,
	}
}

func changeViews(rows []softwareinventory.ChangeEntry) []webui.SoftwareInventoryChange {
	out := make([]webui.SoftwareInventoryChange, 0, len(rows))
	for _, row := range rows {
		oldValue, newValue := row.OldValue, row.NewValue
		if row.Kind == models.SoftwareChangeExposureChanged {
			oldValue = models.SoftwareExposure(row.OldValue).Label()
			newValue = models.SoftwareExposure(row.NewValue).Label()
		}
		out = append(out, webui.SoftwareInventoryChange{
			OccurredAt: row.OccurredAt, KindLabel: row.Kind.Label(),
			CategoryLabel: row.Category.Label(), Name: row.Name,
			OldValue: oldValue, NewValue: newValue,
		})
	}
	return out
}
