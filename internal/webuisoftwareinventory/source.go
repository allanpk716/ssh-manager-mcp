// Package webuisoftwareinventory 把软件清单领域模块适配成 Web UI 的窄接
// 口。页面数据来自统一查询；立即采集按采集执行者分派。
package webuisoftwareinventory

import (
	"cmp"
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

func (s openerSource) SoftwareInventoryDevices(
	filter webui.SoftwareInventoryFilter,
) ([]webui.SoftwareInventoryDeviceSummary, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	manager := softwareinventory.New(st)
	statuses, err := manager.CollectionStatuses(softwareinventory.AllDevicesVisible(), "")
	if err != nil {
		return nil, err
	}
	statusByName := make(map[string]softwareinventory.CollectionStatusView, len(statuses))
	for _, status := range statuses {
		statusByName[status.DeviceName] = status
	}
	result, err := manager.Query(softwareinventory.QueryOptions{
		Site: filter.Site, Network: filter.Network, IncludeSystemPackages: true,
		Visibility: softwareinventory.AllDevicesVisible(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventoryDeviceSummary, 0, len(result.Devices))
	for _, device := range result.Devices {
		status, ok := statusByName[device.DeviceName]
		if !ok {
			status = softwareinventory.CollectionStatusView{
				DeviceName: device.DeviceName, Managed: true,
				CollectionStatus: softwareinventory.CollectionStatus{
					NeverCollected: true, Stale: true,
				},
			}
		}
		out = append(out, deviceSummary(status, device.Items))
	}
	return out, nil
}

// deviceSummary 是首页设备卡片与设备详情页共用的摘要口径：同一批实测条
// 目在这两处得到相同的系统、内核、数量与状态结论。
func deviceSummary(
	status softwareinventory.CollectionStatusView, items []softwareinventory.QueryItem,
) webui.SoftwareInventoryDeviceSummary {
	summary := webui.SoftwareInventoryDeviceSummary{
		Name: status.DeviceName, Status: statusView(status),
		StatusLabels:     softwareInventoryStatusLabels(status),
		AllSoftwareCount: len(items),
	}
	for _, item := range items {
		switch {
		case item.Category == softwareinventory.CategorySystemPackage:
		case item.Category == softwareinventory.CategoryOperatingSystem &&
			summary.SystemName == "":
			summary.SystemName, summary.SystemVersion = item.Name, item.Version
			summary.ImportantSoftwareCount++
		case softwareinventory.IsKernelItem(item) && summary.KernelName == "":
			summary.KernelName, summary.KernelVersion = item.Name, item.Version
			summary.ImportantSoftwareCount++
		case item.Category == softwareinventory.CategoryUnidentified:
			summary.UnidentifiedProcessCount++
			summary.ImportantSoftwareCount++
		default:
			summary.ImportantSoftwareCount++
		}
	}
	return summary
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
	var items []softwareinventory.QueryItem
	if len(result.Devices) == 1 {
		items = result.Devices[0].Items
		device.Groups = groupViews(items)
	}
	device.Summary = deviceSummary(statuses[0], items)
	advanced, preferredEntry, err := deviceAdvanced(st, statuses[0])
	if err != nil {
		return nil, deviceError(deviceName, err)
	}
	device.Advanced = advanced
	device.Status.PreferredServerEntry = cmp.Or(preferredEntry, statuses[0].ServerEntry)
	return device, nil
}

// deviceAdvanced 组装设备详情底部高级采集设置的展示数据：可作为采集执
// 行者的活动客户端实例，以及这台设备的采集用服务器条目与 sudo 开关。
func deviceAdvanced(
	st *store.Store, status softwareinventory.CollectionStatusView,
) (webui.SoftwareInventoryDeviceAdvanced, string, error) {
	out := webui.SoftwareInventoryDeviceAdvanced{
		ClientInstances: []webui.SoftwareInventoryClientInstance{},
		ServerEntries:   []webui.SoftwareInventoryDeviceServerEntry{},
	}
	if !status.Managed {
		return out, "", nil
	}
	servers, err := st.FullServersForDevice(status.DeviceName)
	if err != nil {
		return out, "", err
	}
	sudo, err := st.SoftwareCollectSudoEnabled()
	if err != nil {
		return out, "", err
	}
	preferred := ""
	for _, server := range servers {
		out.ServerEntries = append(out.ServerEntries, webui.SoftwareInventoryDeviceServerEntry{
			Name: server.Name, SudoEnabled: sudo[server.ID],
		})
		if server.ID == status.PreferredServerID {
			preferred = server.Name
		}
	}
	out.ClientInstances, err = activeClientInstances(st)
	return out, preferred, err
}

// activeClientInstances 列出可作为采集执行者的活动客户端实例与它绑定
// 的 profile 名。
func activeClientInstances(st *store.Store) ([]webui.SoftwareInventoryClientInstance, error) {
	profiles, err := st.ListProfiles()
	if err != nil {
		return nil, err
	}
	profileNames := make(map[string]string, len(profiles))
	for _, profile := range profiles {
		profileNames[profile.ID] = profile.Name
	}
	tokens, err := st.ListCacheTokens()
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventoryClientInstance, 0)
	for _, token := range tokens {
		if token.Status != models.CacheTokenActive {
			continue
		}
		out = append(out, webui.SoftwareInventoryClientInstance{
			Name: token.Name, ProfileName: profileNames[token.ProfileID],
		})
	}
	return out, nil
}

func (s openerSource) SearchSoftwareInventory(
	filter webui.SoftwareInventoryFilter,
) ([]webui.SoftwareInventorySearchDevice, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	result, err := softwareinventory.New(st).Query(softwareinventory.QueryOptions{
		Name: filter.Name, ExactVersion: filter.ExactVersion,
		Site: filter.Site, Network: filter.Network,
		IncludeSystemPackages: true, Visibility: softwareinventory.AllDevicesVisible(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventorySearchDevice, 0, len(result.Devices))
	for _, device := range result.Devices {
		searchDevice := webui.SoftwareInventorySearchDevice{
			DeviceName: device.DeviceName,
			Rows:       make([]webui.SoftwareInventorySearchRow, 0, len(device.Items)),
		}
		for _, item := range device.Items {
			if item.Category == softwareinventory.CategorySystemPackage {
				searchDevice.SystemPackageCount++
			}
			searchDevice.Rows = append(searchDevice.Rows, webui.SoftwareInventorySearchRow{
				DeviceName: device.DeviceName, Item: itemView(item),
			})
		}
		if len(searchDevice.Rows) > 0 {
			out = append(out, searchDevice)
		}
	}
	return out, nil
}

func (s openerSource) ExportSoftwareInventoryCSV(
	filter webui.SoftwareInventoryFilter,
) ([]byte, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	// 总表只导出重点清单：查询走默认口径，不带 include_system_packages；
	// MarshalCSV 内部对没有监听端口的系统包再过滤一次作为保险。
	result, err := softwareinventory.New(st).Query(softwareinventory.QueryOptions{
		Device: filter.Device, Name: filter.Name, ExactVersion: filter.ExactVersion,
		Site: filter.Site, Network: filter.Network,
		Visibility: softwareinventory.AllDevicesVisible(),
	})
	if err != nil {
		return nil, deviceError(filter.Device, err)
	}
	return softwareinventory.MarshalCSV(result)
}

func softwareInventoryStatusLabels(status softwareinventory.CollectionStatusView) []string {
	if !status.Managed {
		return []string{"不采集（不受管设备）"}
	}
	labels := make([]string, 0, 2)
	if status.LastFailureAt != nil && (status.LastSuccessAt == nil ||
		status.LastFailureAt.After(*status.LastSuccessAt)) {
		labels = append(labels, "采集失败")
	}
	if status.Stale {
		labels = append(labels, "清单过期")
	}
	if status.PendingImmediate {
		labels = append(labels, "等待客户端执行")
	}
	if len(labels) == 0 {
		labels = append(labels, "正常")
	}
	return labels
}

func (s openerSource) SoftwareInventoryTodos() (webui.SoftwareInventoryTodos, error) {
	st, done, err := s.open()
	if err != nil {
		return webui.SoftwareInventoryTodos{}, err
	}
	defer done()
	todos, err := softwareinventory.New(st).Todos(softwareinventory.AllDevicesVisible())
	if err != nil {
		return webui.SoftwareInventoryTodos{}, err
	}
	return todoViews(todos), nil
}

func (s openerSource) SetSoftwareInventoryCollector(
	form webui.SoftwareInventoryCollectorForm, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	if err := st.SetSoftwareCollectionCollector(
		form.Device, collectorValueForSettings(form), form.ServerEntry,
		store.WebUISource(source.RemoteAddress),
	); err != nil {
		return &webui.SoftwareInventoryFormError{Message: err.Error()}
	}
	return nil
}

func collectorValueForSettings(form webui.SoftwareInventoryCollectorForm) string {
	if form.Authority {
		return ""
	}
	return form.Collector
}

func (s openerSource) SetSoftwareInventorySudo(
	server string, enabled bool, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	if err := st.SetSoftwareCollectSudo(
		server, enabled, store.WebUISource(source.RemoteAddress),
	); err != nil {
		return &webui.SoftwareInventoryFormError{Message: err.Error()}
	}
	return nil
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

func todoViews(todos softwareinventory.TodoResult) webui.SoftwareInventoryTodos {
	out := webui.SoftwareInventoryTodos{
		PendingExposure:       make([]webui.SoftwareInventoryPendingExposureDevice, 0, len(todos.PendingExposure)),
		UnidentifiedProcesses: make([]webui.SoftwareInventoryUnidentifiedProcess, 0, len(todos.UnidentifiedProcesses)),
		Mismatches:            make([]webui.SoftwareInventoryMismatch, 0, len(todos.Mismatches)),
		StaleDevices:          make([]webui.SoftwareInventoryStaleDevice, 0, len(todos.StaleDevices)),
	}
	for _, device := range todos.PendingExposure {
		view := webui.SoftwareInventoryPendingExposureDevice{
			DeviceName: device.DeviceName,
			Ports:      make([]webui.SoftwareInventoryPendingPort, 0, len(device.Ports)),
		}
		for _, port := range device.Ports {
			view.Ports = append(view.Ports, webui.SoftwareInventoryPendingPort{
				Address: port.Address, Port: port.Port, Protocol: port.Protocol,
				ExposureLabel: port.Exposure.Label(), ObservedStableKey: port.ObservedStableKey,
			})
		}
		out.PendingExposure = append(out.PendingExposure, view)
	}
	for _, process := range todos.UnidentifiedProcesses {
		ports := make([]webui.SoftwareInventoryPort, 0, len(process.ListeningPorts))
		for _, port := range process.ListeningPorts {
			ports = append(ports, webui.SoftwareInventoryPort{
				Address: port.Address, Port: port.Port, Protocol: port.Protocol,
				ExposureLabel: port.Exposure.Label(),
			})
		}
		out.UnidentifiedProcesses = append(out.UnidentifiedProcesses, webui.SoftwareInventoryUnidentifiedProcess{
			DeviceName: process.DeviceName, Name: process.Name, ProcessName: process.ProcessName,
			ExecutablePath: process.ExecutablePath, ObservedStableKey: process.ObservedStableKey,
			ListeningPorts: ports,
		})
	}
	for _, mismatch := range todos.Mismatches {
		out.Mismatches = append(out.Mismatches, webui.SoftwareInventoryMismatch{
			DeviceName: mismatch.DeviceName, DeclarationID: mismatch.DeclarationID,
			Name: mismatch.Name, Reason: mismatch.Reason,
		})
	}
	for _, stale := range todos.StaleDevices {
		out.StaleDevices = append(out.StaleDevices, webui.SoftwareInventoryStaleDevice{
			DeviceName: stale.DeviceName, NeverCollected: stale.NeverCollected,
			LastSuccessAt: stale.LastSuccessAt,
		})
	}
	return out
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
	domainSource := softwareinventory.WebUISource(source.RemoteAddress)
	outcome, err := manager.CollectForScope(
		ctx, deviceName, softwareinventory.AllDevicesVisible(),
		&softwareinventory.Service{Store: st}, domainSource,
	)
	if err != nil {
		return webui.SoftwareInventoryCollectionResult{}, deviceError(deviceName, err)
	}
	if outcome.Requested {
		return webui.SoftwareInventoryCollectionResult{Requested: true}, nil
	}
	return webui.SoftwareInventoryCollectionResult{}, nil
}

func (s openerSource) SoftwareInventoryDailyTime() (string, error) {
	st, done, err := s.open()
	if err != nil {
		return "", err
	}
	defer done()
	return st.SoftwareCollectionDailyTime()
}

func (s openerSource) SetSoftwareInventoryDailyTime(
	value string, source webui.SoftwareInventoryWriteSource,
) error {
	if _, _, err := models.ParseSoftwareCollectionDailyTime(value); err != nil {
		return &webui.SoftwareInventoryFormError{Message: err.Error()}
	}
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	if err := st.SetSoftwareCollectionDailyTime(
		value, store.WebUISource(source.RemoteAddress),
	); err != nil {
		return err
	}
	return nil
}

func (s openerSource) RequestSoftwareInventoryCollectionAll(
	ctx context.Context, source webui.SoftwareInventoryWriteSource,
) (webui.SoftwareInventoryCollectionRoundResult, error) {
	st, done, err := s.open()
	if err != nil {
		return webui.SoftwareInventoryCollectionRoundResult{}, err
	}
	defer done()
	domainSource := softwareinventory.WebUISource(source.RemoteAddress)
	report, err := softwareinventory.New(st).CollectAllForScope(
		ctx, softwareinventory.AllDevicesVisible(),
		&softwareinventory.Service{Store: st},
		domainSource,
	)
	if report == nil {
		return webui.SoftwareInventoryCollectionRoundResult{}, err
	}
	result := webui.SoftwareInventoryCollectionRoundResult{
		Succeeded: report.Succeeded, Requested: report.Requested, Failed: report.Failed,
		Interrupted: report.Interrupted,
		Devices:     make([]webui.SoftwareInventoryCollectionDeviceResult, 0, len(report.Devices)),
	}
	for _, device := range report.Devices {
		result.Devices = append(result.Devices, webui.SoftwareInventoryCollectionDeviceResult{
			DeviceName: device.DeviceName, Requested: device.Requested,
			CollectorName: device.CollectorName, Err: device.Err,
		})
	}
	if err != nil {
		return result, err
	}
	return result, nil
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
		Managed: status.Managed, Collector: status.Collector,
		CollectorName: status.CollectorName, CollectorLabel: status.CollectorLabel,
		ServerEntry:   status.ServerEntry,
		LastSuccessAt: status.LastSuccessAt, LastFailureAt: status.LastFailureAt,
		LastFailureReason: status.LastFailureReason, Incomplete: status.Incomplete,
		NeverCollected: status.NeverCollected, Stale: status.Stale,
		PendingImmediate: status.PendingImmediate,
		UnusedImageCount: status.UnusedImageCount,
	}
}

func groupViews(items []softwareinventory.QueryItem) []webui.SoftwareInventoryGroup {
	// 设备详情的固定分组顺序：重点清单分组在前（操作系统与内核、监听
	// 程序、容器、驱动与固件），系统包最后并默认折叠。监听程序分组按全
	// 部端口的最高暴露级别降序排，仅本机监听的程序排后面。
	categories := softwareinventory.AllDisplayCategories()
	buckets := make([][]webui.SoftwareInventoryItem, len(categories))
	for _, item := range items {
		category := softwareinventory.ItemDisplayCategory(item)
		buckets[category] = append(buckets[category], itemView(item))
	}
	listening := make([]softwareinventory.QueryItem, 0)
	for _, item := range items {
		if softwareinventory.ItemDisplayCategory(item) == softwareinventory.DisplayCategoryListeningProgram {
			listening = append(listening, item)
		}
	}
	softwareinventory.SortListeningProgramItems(listening)
	for i, item := range listening {
		buckets[softwareinventory.DisplayCategoryListeningProgram][i] = itemView(item)
	}
	groups := make([]webui.SoftwareInventoryGroup, 0, len(categories))
	for _, category := range categories {
		bucket := buckets[category]
		if len(bucket) == 0 {
			continue
		}
		group := webui.SoftwareInventoryGroup{
			CategoryCode: category.Code(), CategoryLabel: category.Label(), Items: bucket,
			SystemPackages: category == softwareinventory.DisplayCategorySystemPackage,
		}
		if group.SystemPackages {
			group.SystemPackageCount = len(bucket)
		}
		groups = append(groups, group)
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
	displayCategory := softwareinventory.ItemDisplayCategory(item)
	imagePullabilityLabel := ""
	if item.ImagePullability != "" {
		imagePullabilityLabel = item.ImagePullability.Label()
	}
	return webui.SoftwareInventoryItem{
		CategoryCode: displayCategory.Code(), CategoryLabel: item.CategoryLabel,
		SearchCategoryLabel: displayCategory.Label(),
		Name:                item.Name, Version: item.Version,
		PackageURL: item.PackageURL, CPE: item.CPE, Source: item.Source,
		CollectedAt: item.CollectedAt, ConfirmedAt: item.ConfirmedAt,
		ListeningPorts: ports, BelongsTo: item.BelongsTo, Note: item.Note,
		ContainerName: item.ContainerName, ImagePullabilityLabel: imagePullabilityLabel,
		ContainerTag: item.ContainerTag, ContainerDigest: item.ContainerDigest,
		ContainerRunning: item.ContainerRunning,
		ProcessName:      item.ProcessName, ExecutablePath: item.ExecutablePath,
		ProcessID: item.ProcessID, MissingVersion: item.MissingVersion,
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
