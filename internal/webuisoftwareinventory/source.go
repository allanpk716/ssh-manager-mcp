// Package webuisoftwareinventory 把软件清单领域模块适配成 Web UI 的窄接
// 口。页面数据来自统一查询；立即采集按采集执行者分派。
package webuisoftwareinventory

import (
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

func (s openerSource) SoftwareInventorySettings() (webui.SoftwareInventorySettings, error) {
	st, done, err := s.open()
	if err != nil {
		return webui.SoftwareInventorySettings{}, err
	}
	defer done()
	statuses, err := softwareinventory.New(st).CollectionStatuses(
		softwareinventory.AllDevicesVisible(), "",
	)
	if err != nil {
		return webui.SoftwareInventorySettings{}, err
	}
	sudo, err := st.SoftwareCollectSudoEnabled()
	if err != nil {
		return webui.SoftwareInventorySettings{}, err
	}
	settings := webui.SoftwareInventorySettings{
		Devices: make([]webui.SoftwareInventoryDeviceSummary, 0, len(statuses)),
		Servers: make([]webui.SoftwareInventorySettingsServer, 0),
	}
	for _, status := range statuses {
		settings.Devices = append(settings.Devices, webui.SoftwareInventoryDeviceSummary{
			Name: status.DeviceName, Status: statusView(status),
		})
		servers, err := st.FullServersForDevice(status.DeviceName)
		if err != nil {
			return webui.SoftwareInventorySettings{}, err
		}
		for _, server := range servers {
			settings.Servers = append(settings.Servers, webui.SoftwareInventorySettingsServer{
				Name: server.Name, DeviceName: status.DeviceName, SudoEnabled: sudo[server.ID],
			})
		}
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		return webui.SoftwareInventorySettings{}, err
	}
	profileNames := make(map[string]string, len(profiles))
	for _, profile := range profiles {
		profileNames[profile.ID] = profile.Name
	}
	tokens, err := st.ListCacheTokens()
	if err != nil {
		return webui.SoftwareInventorySettings{}, err
	}
	for _, token := range tokens {
		if token.Status != models.CacheTokenActive {
			continue
		}
		settings.ClientInstances = append(settings.ClientInstances, webui.SoftwareInventoryClientInstance{
			Name: token.Name, ProfileName: profileNames[token.ProfileID],
		})
	}
	return settings, nil
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
		form.Device, form.Collector, form.ServerEntry,
		store.WebUISource(source.RemoteAddress),
	); err != nil {
		return &webui.SoftwareInventoryFormError{Message: err.Error()}
	}
	return nil
}

func (s openerSource) SetSoftwareInventorySudo(
	server string, enabled bool, source webui.SoftwareInventoryWriteSource,
) error {
	st, done, err := s.open()
	if err != nil {
		return err
	}
	defer done()
	return st.SetSoftwareCollectSudo(
		server, enabled, store.WebUISource(source.RemoteAddress),
	)
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
