// Package webuisoftwareinventory 把软件清单领域模块适配成 Web UI 的窄接
// 口。页面数据来自统一查询；立即采集按采集执行者分派。
package webuisoftwareinventory

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"

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
		case item.Category == softwareinventory.CategoryDriverFirmware &&
			strings.HasSuffix(item.Name, "内核") && summary.KernelName == "":
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
	result, err := softwareinventory.New(st).Query(softwareinventory.QueryOptions{
		Device: filter.Device, Name: filter.Name, ExactVersion: filter.ExactVersion,
		Site: filter.Site, Network: filter.Network,
		IncludeSystemPackages: true, Visibility: softwareinventory.AllDevicesVisible(),
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

func (s openerSource) AddSoftwareInventoryDeclaration(
	form webui.SoftwareInventoryDeclarationForm, source webui.SoftwareInventoryWriteSource,
) (string, error) {
	st, done, err := s.open()
	if err != nil {
		return "", err
	}
	defer done()
	manager := softwareinventory.New(st)
	domainSource := softwareinventory.WebUISource(source.RemoteAddress)
	id, err := manager.AddDeclaration(declarationFromForm(form), domainSource)
	if err != nil {
		return "", softwareInventoryFormError(err)
	}
	if form.ObservedStableKey != "" {
		if err := manager.ClaimDeclaration(id, form.ObservedStableKey, domainSource); err != nil {
			return id, softwareInventoryFormError(err)
		}
	}
	return id, nil
}

func (s openerSource) SoftwareInventoryDeclaration(
	id string,
) (*webui.SoftwareInventoryDeclaration, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := st.SearchSoftwareDeclarations("", softwareinventory.AllDevicesVisible())
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.ID == id {
			return declarationView(row), nil
		}
	}
	return nil, &webui.SoftwareInventoryDeclarationNotFoundError{ID: id}
}

func (s openerSource) SoftwareInventoryDeclarations() ([]webui.SoftwareInventoryDeclaration, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := st.SearchSoftwareDeclarations("", softwareinventory.AllDevicesVisible())
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventoryDeclaration, 0, len(rows))
	for _, row := range rows {
		out = append(out, *declarationView(row))
	}
	return out, nil
}

func (s openerSource) EditSoftwareInventoryDeclaration(
	id string, form webui.SoftwareInventoryDeclarationForm, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	ports := formListeningPorts(form.ListeningPorts)
	category := softwareinventory.Category(form.Category)
	edit := softwareinventory.DeclarationEdit{
		Device: &form.Device, Category: &category,
		Name: &form.Name, Version: &form.Version, PackageURL: &form.PackageURL,
		CPE: &form.CPE, ListeningPorts: &ports, BelongsTo: &form.BelongsTo,
		Note: &form.Note,
	}
	if err := softwareinventory.New(st).EditDeclaration(
		id, edit, softwareinventory.WebUISource(source.RemoteAddress),
	); err != nil {
		return softwareInventoryFormError(err)
	}
	return nil
}

func (s openerSource) ClaimSoftwareInventoryDeclaration(
	id, observedStableKey string, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	if err := softwareinventory.New(st).ClaimDeclaration(
		id, observedStableKey, softwareinventory.WebUISource(source.RemoteAddress),
	); err != nil {
		return softwareInventoryFormError(err)
	}
	return nil
}

func (s openerSource) DeleteSoftwareInventoryDeclaration(
	id string, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	if err := softwareinventory.New(st).DeleteDeclaration(
		id, softwareinventory.WebUISource(source.RemoteAddress),
	); err != nil {
		if strings.Contains(err.Error(), "不存在") {
			return &webui.SoftwareInventoryDeclarationNotFoundError{ID: id}
		}
		return err
	}
	return nil
}

func (s openerSource) SoftwareInventoryDrafts() ([]webui.SoftwareInventoryDraft, error) {
	st, done, err := s.open()
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := softwareinventory.New(st).DeclarationDrafts(softwareinventory.AllDevicesVisible())
	if err != nil {
		return nil, err
	}
	out := make([]webui.SoftwareInventoryDraft, 0, len(rows))
	for _, row := range rows {
		ports := make([]webui.SoftwareInventoryPort, 0, len(row.ListeningPorts))
		lines := make([]string, 0, len(row.ListeningPorts))
		for _, port := range row.ListeningPorts {
			ports = append(ports, webui.SoftwareInventoryPort{
				Address: port.Address, Port: port.Port, Protocol: port.Protocol,
				ExposureLabel: port.Exposure.Label(),
			})
			lines = append(lines, strings.Join([]string{
				port.Address, strconv.Itoa(port.Port), port.Protocol, string(port.Exposure),
			}, ","))
		}
		out = append(out, webui.SoftwareInventoryDraft{
			ID: row.ID, Device: row.DeviceName, Category: string(row.Category),
			CategoryLabel: row.Category.Label(), Name: row.Name, Version: row.Version,
			PackageURL: row.PackageURL, CPE: row.CPE, BelongsTo: row.BelongsTo,
			Note: row.Note, ClaimedObservedKey: row.ClaimedObservedKey,
			ListeningPorts: ports, ListeningPortsText: strings.Join(lines, "\n"),
		})
	}
	return out, nil
}

func (s openerSource) ConfirmSoftwareInventoryDraft(
	id string, form *webui.SoftwareInventoryDeclarationForm, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	var edit *softwareinventory.DeclarationEdit
	if form != nil {
		ports := formListeningPorts(form.ListeningPorts)
		category := softwareinventory.Category(form.Category)
		edit = &softwareinventory.DeclarationEdit{
			Device: &form.Device, Category: &category, Name: &form.Name,
			Version: &form.Version, PackageURL: &form.PackageURL, CPE: &form.CPE,
			ListeningPorts: &ports, BelongsTo: &form.BelongsTo, Note: &form.Note,
		}
	}
	if err := softwareinventory.New(st).ConfirmDeclarationDraft(
		id, edit, softwareinventory.WebUISource(source.RemoteAddress),
	); err != nil {
		return softwareInventoryFormError(err)
	}
	return nil
}

func (s openerSource) RejectSoftwareInventoryDraft(
	id string, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	if err := softwareinventory.New(st).RejectDeclarationDraft(
		id, softwareinventory.WebUISource(source.RemoteAddress),
	); err != nil {
		return softwareInventoryFormError(err)
	}
	return nil
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

func declarationFromForm(form webui.SoftwareInventoryDeclarationForm) softwareinventory.Declaration {
	return softwareinventory.Declaration{
		Device: form.Device, Category: softwareinventory.Category(form.Category),
		Name: form.Name, Version: form.Version, PackageURL: form.PackageURL,
		CPE: form.CPE, ListeningPorts: formListeningPorts(form.ListeningPorts),
		BelongsTo: form.BelongsTo, Note: form.Note,
	}
}

func formListeningPorts(ports []webui.SoftwareInventoryFormPort) []softwareinventory.ListeningPort {
	out := make([]softwareinventory.ListeningPort, 0, len(ports))
	for _, port := range ports {
		out = append(out, softwareinventory.ListeningPort{
			Address: port.Address, Port: port.Port,
			Protocol: port.Protocol, Exposure: softwareinventory.Exposure(port.Exposure),
		})
	}
	return out
}

func declarationView(row softwareinventory.SoftwareDeclarationView) *webui.SoftwareInventoryDeclaration {
	ports := make([]webui.SoftwareInventoryFormPort, 0, len(row.ListeningPorts))
	for _, port := range row.ListeningPorts {
		ports = append(ports, webui.SoftwareInventoryFormPort{
			Address: port.Address, Port: port.Port,
			Protocol: port.Protocol, Exposure: string(port.Exposure),
		})
	}
	return &webui.SoftwareInventoryDeclaration{
		ID: row.ID, Device: row.DeviceName, Category: string(row.Category),
		CategoryLabel: row.Category.Label(), Name: row.Name, Version: row.Version,
		PackageURL: row.PackageURL, CPE: row.CPE, BelongsTo: row.BelongsTo,
		Note: row.Note, ListeningPorts: ports, ClaimedObservedKey: row.ClaimedObservedKey,
	}
}

func softwareInventoryFormError(err error) error {
	switch {
	case errors.Is(err, models.ErrSoftwareCategoryInvalid),
		errors.Is(err, models.ErrSoftwareNameRequired),
		errors.Is(err, models.ErrSoftwareVersionRequired),
		errors.Is(err, models.ErrSoftwareExposureInvalid),
		errors.Is(err, models.ErrSoftwarePortInvalid),
		errors.Is(err, models.ErrSoftwareProtocolInvalid):
		return &webui.SoftwareInventoryFormError{Message: err.Error()}
	default:
		return err
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
	}
}

func groupViews(items []softwareinventory.QueryItem) []webui.SoftwareInventoryGroup {
	// 设备详情的固定分组顺序：操作系统条目与内核条目合并展示，系统包
	// 排在未识别进程前并默认折叠。
	labels := []string{"操作系统与内核", "服务程序", "容器镜像", "驱动与固件", "系统包", "未识别进程"}
	buckets := make([][]webui.SoftwareInventoryItem, len(labels))
	for _, item := range items {
		rank := deviceGroupRank(item)
		buckets[rank] = append(buckets[rank], itemView(item))
	}
	groups := make([]webui.SoftwareInventoryGroup, 0, len(labels))
	for rank, label := range labels {
		if len(buckets[rank]) == 0 {
			continue
		}
		group := webui.SoftwareInventoryGroup{
			CategoryLabel: label, Items: buckets[rank],
			SystemPackages: label == "系统包",
		}
		if group.SystemPackages {
			group.SystemPackageCount = len(buckets[rank])
		}
		groups = append(groups, group)
	}
	return groups
}

// deviceGroupRank 给一条软件条目标出设备详情分组顺序。操作系统条目与
// 名称以「内核」结尾的驱动固件条目合入第一组。
func deviceGroupRank(item softwareinventory.QueryItem) int {
	switch {
	case item.Category == softwareinventory.CategoryOperatingSystem:
		return 0
	case item.Category == softwareinventory.CategoryDriverFirmware &&
		strings.HasSuffix(item.Name, "内核"):
		return 0
	case item.Category == softwareinventory.CategoryServiceProgram:
		return 1
	case item.Category == softwareinventory.CategoryContainerImage:
		return 2
	case item.Category == softwareinventory.CategoryDriverFirmware:
		return 3
	case item.Category == softwareinventory.CategorySystemPackage:
		return 4
	default:
		return 5
	}
}

func itemView(item softwareinventory.QueryItem) webui.SoftwareInventoryItem {
	ports := make([]webui.SoftwareInventoryPort, 0, len(item.ListeningPorts))
	for _, port := range item.ListeningPorts {
		ports = append(ports, webui.SoftwareInventoryPort{
			Address: port.Address, Port: port.Port, Protocol: port.Protocol,
			ExposureLabel: port.Exposure.Label(),
		})
	}
	searchCategoryLabel := item.CategoryLabel
	if item.Category == softwareinventory.CategoryOperatingSystem ||
		(item.Category == softwareinventory.CategoryDriverFirmware && strings.HasSuffix(item.Name, "内核")) {
		searchCategoryLabel = "操作系统与内核"
	}
	return webui.SoftwareInventoryItem{
		CategoryLabel: item.CategoryLabel, SearchCategoryLabel: searchCategoryLabel,
		Name: item.Name, Version: item.Version,
		PackageURL: item.PackageURL, CPE: item.CPE, Source: item.Source,
		CollectedAt: item.CollectedAt, ConfirmedAt: item.ConfirmedAt,
		ListeningPorts: ports, BelongsTo: item.BelongsTo, Note: item.Note,
		ContainerTag: item.ContainerTag, ContainerDigest: item.ContainerDigest,
		ContainerRunning: item.ContainerRunning,
		ProcessName:      item.ProcessName, ExecutablePath: item.ExecutablePath,
		ProcessID: item.ProcessID,
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
