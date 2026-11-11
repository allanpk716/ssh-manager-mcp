// Package webuitopology 把权威 vault 与客户端本地快照里的网络拓扑整理成
// Web UI 拓扑图页面使用的只读展示数据。
package webuitopology

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"ssh-manager-mcp/internal/clientcache"
	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// openerSource 每次请求重新打开 vault，读取后关闭，不长期持有存储句柄。
type openerSource struct {
	open func() (*store.Store, func(), error)
}

// FromStoreOpener 返回按请求打开权威 vault 的拓扑来源，供单机与权威端适
// 配层使用。
func FromStoreOpener(open func() (*store.Store, func(), error)) webui.TopologySource {
	return openerSource{open: open}
}

// WriterFromOpener 返回按请求打开权威 vault 的布局写入口，供单机适配层使
// 用。
func WriterFromOpener(open func() (*store.Store, func(), error)) webui.TopologyWriter {
	return openerWriter{open: open}
}

// openerWriter 每次写请求重新打开 vault，写入后关闭。
type openerWriter struct {
	open func() (*store.Store, func(), error)
}

// SaveTopologyLayout 保存一个视图的完整布局。
func (w openerWriter) SaveTopologyLayout(save webui.TopologyLayoutSave, source webui.TopologyWriteSource) error {
	st, closeStore, err := w.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return WriterFromStore(st).SaveTopologyLayout(save, source)
}

// ResetTopologyLayout 清空一个视图保存的全部坐标。
func (w openerWriter) ResetTopologyLayout(view string, source webui.TopologyWriteSource) error {
	st, closeStore, err := w.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return WriterFromStore(st).ResetTopologyLayout(view, source)
}

// Topology 读取本机 vault 的全部拓扑数据。instance 是客户端实例名；单机
// 与权威端来源忽略它。
func (s openerSource) Topology(instance string) (*webui.TopologyData, error) {
	st, closeStore, err := s.open()
	if err != nil {
		return nil, err
	}
	defer closeStore()
	return FromStore(st).Topology(instance)
}

// storeSource 直接读取一个已打开的 vault，供 serve 进程复用长期存储句柄。
type storeSource struct {
	store *store.Store
}

// FromStore 返回绑定指定存储的拓扑来源。
func FromStore(st *store.Store) webui.TopologySource {
	return storeSource{store: st}
}

// Topology 读取 vault 里的站点、网络、设备、网络成员、服务器条目摘要、共
// 享存储、既有布局坐标，并按运行时状态解析本机设备。
func (s storeSource) Topology(string) (*webui.TopologyData, error) {
	st := s.store
	sites, err := st.ListSites()
	if err != nil {
		return nil, err
	}
	networks, err := st.ListNetworks(store.NetworkFilter{})
	if err != nil {
		return nil, err
	}
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	coordinates, err := st.ListDeviceLayoutCoordinates()
	if err != nil {
		return nil, err
	}
	containerCoordinates, err := st.ListContainerLayoutCoordinates()
	if err != nil {
		return nil, err
	}
	local, err := dialdevice.Current(st, "")
	if err != nil {
		return nil, err
	}

	data := &webui.TopologyData{
		Sites:             make([]webui.TopologySite, 0, len(sites)),
		Networks:          make([]webui.TopologyNetwork, 0, len(networks)),
		Devices:           make([]webui.TopologyDevice, 0, len(devices)),
		Memberships:       make([]webui.TopologyMembership, 0),
		LayoutCoordinates: make([]webui.TopologyLayoutCoordinate, 0, len(coordinates)),
		ContainerLayoutCoordinates: make(
			[]webui.TopologyContainerLayoutCoordinate, 0, len(containerCoordinates)),
		LocalDevice: localDeviceView(local),
	}
	for _, site := range sites {
		data.Sites = append(data.Sites, webui.TopologySite{ID: site.ID, Name: site.Name, Note: site.Note})
	}
	sort.Slice(data.Sites, func(i, j int) bool { return data.Sites[i].Name < data.Sites[j].Name })

	networkByID := make(map[string]*models.Network, len(networks))
	for _, network := range networks {
		networkByID[network.ID] = network
		data.Networks = append(data.Networks, networkView(network))
	}
	sort.Slice(data.Networks, func(i, j int) bool { return data.Networks[i].Name < data.Networks[j].Name })

	for _, device := range devices {
		servers, err := st.ListServersForDevice(device.Name)
		if err != nil {
			return nil, err
		}
		shared, err := st.ListSharedStorages(store.SharedStorageFilter{Device: device.Name})
		if err != nil {
			return nil, err
		}
		serverViews := make([]webui.TopologyDeviceServer, 0, len(servers))
		for _, server := range servers {
			serverViews = append(serverViews, webui.TopologyDeviceServer{
				ID: server.ID, Name: server.Name, User: server.User,
				Host: serverHost(st, server.ID), Port: server.Port, Pinned: server.Pinned,
			})
		}
		sharedViews := make([]webui.TopologySharedStorage, 0, len(shared))
		for _, item := range shared {
			sharedViews = append(sharedViews, webui.TopologySharedStorage{ID: item.ID, Name: item.Name})
		}
		candidates, notice := candidatesView(device.Memberships, local, serverViews)
		data.Devices = append(data.Devices, webui.TopologyDevice{
			ID: device.ID, Name: device.Name, Note: device.Note,
			Kind: deviceKind(len(serverViews), device.ClientName), ClientName: device.ClientName,
			Servers: serverViews, SharedStorages: sharedViews,
			Candidates: candidates, CandidateNotice: notice,
		})
		for _, membership := range device.Memberships {
			data.Memberships = append(data.Memberships, membershipView(membership))
		}
	}
	sort.Slice(data.Devices, func(i, j int) bool { return data.Devices[i].Name < data.Devices[j].Name })
	sort.SliceStable(data.Memberships, func(i, j int) bool {
		return data.Memberships[i].RegistrationOrder < data.Memberships[j].RegistrationOrder
	})
	for _, coordinate := range coordinates {
		data.LayoutCoordinates = append(data.LayoutCoordinates, webui.TopologyLayoutCoordinate{
			ViewKey: coordinate.ViewKey, DeviceID: coordinate.DeviceID, X: coordinate.X, Y: coordinate.Y,
		})
	}
	for _, coordinate := range containerCoordinates {
		data.ContainerLayoutCoordinates = append(data.ContainerLayoutCoordinates, webui.TopologyContainerLayoutCoordinate{
			ViewKey: coordinate.ViewKey, EntityType: coordinate.EntityType,
			EntityID: coordinate.EntityID, X: coordinate.X, Y: coordinate.Y,
		})
	}
	return data, nil
}

// WriterFromStore 返回绑定指定存储的布局写入口。
func WriterFromStore(st *store.Store) webui.TopologyWriter {
	return storeWriter{store: st}
}

// storeWriter 把 Web UI 的布局写请求换算成存储层调用。
type storeWriter struct {
	store *store.Store
}

// SaveTopologyLayout 保存一个视图的完整布局，并把存储层的内容错误换算成
// Web UI 的内容非法错误。
func (w storeWriter) SaveTopologyLayout(save webui.TopologyLayoutSave, source webui.TopologyWriteSource) error {
	input := store.ViewLayoutInput{
		Devices:    make([]store.DeviceLayoutInput, 0, len(save.Devices)),
		Containers: make([]store.ContainerLayoutInput, 0, len(save.Containers)),
	}
	for _, coordinate := range save.Devices {
		input.Devices = append(input.Devices, store.DeviceLayoutInput{
			DeviceID: coordinate.DeviceID, X: coordinate.X, Y: coordinate.Y,
		})
	}
	for _, coordinate := range save.Containers {
		input.Containers = append(input.Containers, store.ContainerLayoutInput{
			EntityType: coordinate.EntityType, EntityID: coordinate.EntityID,
			X: coordinate.X, Y: coordinate.Y,
		})
	}
	if err := w.store.SaveViewLayout(save.ViewKey, input, source.RemoteAddress); err != nil {
		return mapLayoutError(err)
	}
	return nil
}

// ResetTopologyLayout 清空一个视图保存的全部坐标。
func (w storeWriter) ResetTopologyLayout(view string, source webui.TopologyWriteSource) error {
	if err := w.store.ResetViewLayout(view, source.RemoteAddress); err != nil {
		return mapLayoutError(err)
	}
	return nil
}

// mapLayoutError 把存储层的布局校验错误换算成 Web UI 的内容非法错误；其它
// 错误原样返回，让处理器按服务器错误处理。
func mapLayoutError(err error) error {
	switch {
	case errors.Is(err, store.ErrLayoutViewInvalid),
		errors.Is(err, store.ErrLayoutEntityInvalid),
		errors.Is(err, store.ErrLayoutEntityDuplicate),
		errors.Is(err, store.ErrLayoutCoordinateInvalid),
		errors.Is(err, store.ErrLayoutEntityMissing),
		errors.Is(err, store.ErrLayoutIdentifierInvalid):
		return fmt.Errorf("%w：%v", webui.ErrTopologyLayoutInvalid, err)
	default:
		return err
	}
}

// snapshotSource 读取客户端本地快照，按快照里的授权裁剪结果呈现。
type snapshotSource struct {
	load func(instance string) (*store.Snapshot, error)
}

// FromClientSnapshot 返回按客户端实例读取本地快照的拓扑来源。
func FromClientSnapshot(load func(instance string) (*store.Snapshot, error)) webui.TopologySource {
	return snapshotSource{load: load}
}

// Topology 把客户端快照里的拓扑段整理成与权威端一致的展示数据。
func (s snapshotSource) Topology(instance string) (*webui.TopologyData, error) {
	snapshot, err := s.load(instance)
	if err != nil {
		return nil, err
	}
	data := &webui.TopologyData{
		Sites:             make([]webui.TopologySite, 0, len(snapshot.Sites)),
		Networks:          make([]webui.TopologyNetwork, 0, len(snapshot.Networks)),
		Devices:           make([]webui.TopologyDevice, 0, len(snapshot.Devices)),
		Memberships:       make([]webui.TopologyMembership, 0, len(snapshot.NetworkMemberships)),
		LayoutCoordinates: make([]webui.TopologyLayoutCoordinate, 0, len(snapshot.DeviceLayoutCoordinates)),
		ContainerLayoutCoordinates: make(
			[]webui.TopologyContainerLayoutCoordinate, 0, len(snapshot.ContainerLayoutCoordinates)),
	}
	for _, site := range snapshot.Sites {
		data.Sites = append(data.Sites, webui.TopologySite{ID: site.ID, Name: site.Name, Note: site.Note})
	}
	sort.Slice(data.Sites, func(i, j int) bool { return data.Sites[i].Name < data.Sites[j].Name })

	networks := make(map[string]store.SnapshotNetwork, len(snapshot.Networks))
	for _, network := range snapshot.Networks {
		networks[network.ID] = network
		data.Networks = append(data.Networks, webui.TopologyNetwork{
			ID: network.ID, Name: network.Name, Type: network.Type,
			SiteID: network.SiteID, Note: network.Note, NetworkRange: network.CIDR,
			BandwidthMbps: network.BandwidthMbps, Metered: network.Metered,
		})
	}
	for _, network := range snapshot.Networks {
		if network.Type == string(models.NetworkPhysical) {
			for _, site := range snapshot.Sites {
				if site.ID == network.SiteID {
					for i := range data.Networks {
						if data.Networks[i].ID == network.ID {
							data.Networks[i].SiteName = site.Name
						}
					}
				}
			}
		}
	}
	sort.Slice(data.Networks, func(i, j int) bool { return data.Networks[i].Name < data.Networks[j].Name })

	pinned := make(map[string]bool, len(snapshot.EntryHostKeys))
	for _, pin := range snapshot.EntryHostKeys {
		pinned[pin.ServerID] = true
	}
	membershipsByDevice := make(map[string][]*models.NetworkMembership)
	for i := range snapshot.NetworkMemberships {
		membership := snapshot.NetworkMemberships[i]
		network, ok := networks[membership.NetworkID]
		if !ok {
			continue
		}
		model := &models.NetworkMembership{
			DeviceID: membership.DeviceID, NetworkID: membership.NetworkID,
			NetworkType: models.NetworkType(network.Type), Address: membership.Address,
			NetworkName: network.Name, RegistrationOrder: membership.RegistrationOrder,
		}
		membershipsByDevice[membership.DeviceID] = append(membershipsByDevice[membership.DeviceID], model)
		data.Memberships = append(data.Memberships, webui.TopologyMembership{
			DeviceID: membership.DeviceID, NetworkID: membership.NetworkID,
			Address: membership.Address, RegistrationOrder: membership.RegistrationOrder,
		})
	}

	local := localDeviceFromSnapshot(snapshot, instance, membershipsByDevice)
	data.LocalDevice = webui.TopologyLocalDevice{Known: false, Reason: string(local.Reason)}
	if local.Known && local.Device != nil {
		data.LocalDevice = webui.TopologyLocalDevice{Known: true, DeviceID: local.Device.ID, DeviceName: local.Device.Name}
	}
	for _, device := range snapshot.Devices {
		serverViews := make([]webui.TopologyDeviceServer, 0)
		for _, server := range snapshot.Servers {
			if server.DeviceID != device.ID {
				continue
			}
			serverViews = append(serverViews, webui.TopologyDeviceServer{
				ID: server.ID, Name: server.Name, User: server.User, Host: server.Host,
				Port: server.Port, Pinned: pinned[server.ID],
			})
		}
		sort.Slice(serverViews, func(i, j int) bool { return serverViews[i].Name < serverViews[j].Name })
		sharedViews := make([]webui.TopologySharedStorage, 0)
		for _, shared := range snapshot.SharedStorages {
			if shared.DeviceID == device.ID {
				sharedViews = append(sharedViews, webui.TopologySharedStorage{ID: shared.ID, Name: shared.Name})
			}
		}
		sort.Slice(sharedViews, func(i, j int) bool { return sharedViews[i].Name < sharedViews[j].Name })
		candidates, notice := candidatesView(membershipsByDevice[device.ID], local, serverViews)
		data.Devices = append(data.Devices, webui.TopologyDevice{
			ID: device.ID, Name: device.Name, Note: device.Note,
			Kind: deviceKind(len(serverViews), device.ClientName), ClientName: device.ClientName,
			Servers: serverViews, SharedStorages: sharedViews,
			Candidates: candidates, CandidateNotice: notice,
		})
	}
	sort.Slice(data.Devices, func(i, j int) bool { return data.Devices[i].Name < data.Devices[j].Name })
	sort.SliceStable(data.Memberships, func(i, j int) bool {
		return data.Memberships[i].RegistrationOrder < data.Memberships[j].RegistrationOrder
	})
	for _, coordinate := range snapshot.DeviceLayoutCoordinates {
		data.LayoutCoordinates = append(data.LayoutCoordinates, webui.TopologyLayoutCoordinate{
			ViewKey: coordinate.ViewKey, DeviceID: coordinate.DeviceID, X: coordinate.X, Y: coordinate.Y,
		})
	}
	for _, coordinate := range snapshot.ContainerLayoutCoordinates {
		data.ContainerLayoutCoordinates = append(data.ContainerLayoutCoordinates, webui.TopologyContainerLayoutCoordinate{
			ViewKey: coordinate.ViewKey, EntityType: coordinate.EntityType,
			EntityID: coordinate.EntityID, X: coordinate.X, Y: coordinate.Y,
		})
	}
	return data, nil
}

func networkView(network *models.Network) webui.TopologyNetwork {
	return webui.TopologyNetwork{
		ID: network.ID, Name: network.Name, Type: string(network.Type),
		SiteID: network.SiteID, SiteName: network.SiteName, Note: network.Note,
		NetworkRange: network.NetworkRange, BandwidthMbps: network.BandwidthMbps,
		Metered: network.Metered,
	}
}

func membershipView(membership *models.NetworkMembership) webui.TopologyMembership {
	return webui.TopologyMembership{
		DeviceID: membership.DeviceID, NetworkID: membership.NetworkID,
		Address: membership.Address, RegistrationOrder: membership.RegistrationOrder,
	}
}

func deviceKind(serverCount int, clientName string) string {
	if serverCount > 0 {
		return "server"
	}
	if clientName != "" {
		return "client"
	}
	return "unmanaged"
}

func localDeviceView(local dialdevice.Result) webui.TopologyLocalDevice {
	if !local.Known || local.Device == nil {
		return webui.TopologyLocalDevice{Known: false, Reason: string(local.Reason)}
	}
	return webui.TopologyLocalDevice{
		Known: true, DeviceID: local.Device.ID, DeviceName: local.Device.Name,
	}
}

// localDeviceFromSnapshot 解析客户端本机设备：优先读实例 cache.meta.json 记
// 录的设备标识；旧缓存没有这个字段时，用快照里唯一带设备码关联的设备推断。
func localDeviceFromSnapshot(
	snapshot *store.Snapshot, instance string, memberships map[string][]*models.NetworkMembership,
) dialdevice.Result {
	deviceID := cacheDeviceID(instance)
	devices := make([]*models.Device, 0, len(snapshot.Devices))
	var associated []*models.Device
	for i := range snapshot.Devices {
		device := snapshot.Devices[i]
		model := &models.Device{
			ID: device.ID, Name: device.Name, Note: device.Note,
			ClientTokenID: device.ClientTokenID, ClientName: device.ClientName,
			Memberships: memberships[device.ID],
		}
		devices = append(devices, model)
		if device.ClientTokenID != "" {
			associated = append(associated, model)
		}
	}
	if deviceID != "" {
		for _, device := range devices {
			if device.ID == deviceID {
				return dialdevice.Result{Known: true, Device: device}
			}
		}
		return dialdevice.Result{Known: false, Reason: dialdevice.ReasonDeviceMissingFromSnapshot}
	}
	if len(associated) == 1 {
		return dialdevice.Result{Known: true, Device: associated[0]}
	}
	return dialdevice.Result{Known: false, Reason: dialdevice.ReasonClientDeviceNotRecorded}
}

// cacheDeviceID 读取客户端实例 cache.meta.json 里的设备标识；读取失败按没
// 有记录处理，后续再走旧缓存的推断路径。
func cacheDeviceID(instance string) string {
	directory, err := clientcache.DirectoryFor(instance)
	if err != nil {
		return ""
	}
	file, err := os.Open(filepath.Join(directory, "cache.meta.json"))
	if err != nil {
		return ""
	}
	defer file.Close()
	var meta struct {
		DeviceID string `json:"device_id"`
	}
	if json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&meta) != nil {
		return ""
	}
	return meta.DeviceID
}

// candidatesView 复用统一拨号入口的共同网络规划：物理局域网优先、虚拟局
// 域网其次、服务器条目兜底主机最后。返回的提示在人无法按共同网络排序时说
// 明原因。
func candidatesView(
	targetMemberships []*models.NetworkMembership, dialer dialdevice.Result, servers []webui.TopologyDeviceServer,
) ([]webui.TopologyCandidate, string) {
	candidates := make([]webui.TopologyCandidate, 0)
	if !dialer.Known || dialer.Device == nil {
		for _, server := range servers {
			candidates = append(candidates, fallbackCandidate(server))
		}
		return candidates, unknownDialerNotice(dialer.Reason, len(targetMemberships))
	}
	plan := sshbroker.PlanCommonNetworkAddresses(targetMemberships, dialer.Device.Memberships)
	for _, candidate := range plan.Candidates {
		candidates = append(candidates, webui.TopologyCandidate{
			Address: candidate.Address, NetworkName: candidate.NetworkName,
			Source: string(candidate.Source),
		})
	}
	seen := make(map[string]bool, len(candidates)+len(servers))
	for _, candidate := range candidates {
		seen[store.NormalizeSSHHost(candidate.Address)] = true
	}
	for _, server := range servers {
		if server.Host == "" {
			continue
		}
		fallback := fallbackCandidate(server)
		normalized := store.NormalizeSSHHost(server.Host)
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		candidates = append(candidates, fallback)
	}
	notice := ""
	switch plan.FallbackOnlyReason {
	case sshbroker.FallbackOnlyNoCommonNetwork:
		notice = "本机与这台设备没有共同网络，只能使用兜底主机；建议补登记共同网络。"
	case sshbroker.FallbackOnlyDialerUnknown:
		notice = unknownDialerNotice(dialer.Reason, len(targetMemberships))
	}
	if notice == "" && len(targetMemberships) == 0 && len(servers) > 0 {
		notice = "这台设备还没有登记网络成员，只能使用兜底主机。"
	}
	return candidates, notice
}

func fallbackCandidate(server webui.TopologyDeviceServer) webui.TopologyCandidate {
	return webui.TopologyCandidate{Source: "fallback", ServerName: server.Name, Address: server.Host}
}

// serverHost 返回服务器条目的兜底主机地址。ListServersForDevice 的摘要不带
// 主机字段，这里按内部标识读取一次详情；找不到时返回空串，页面跳过这一条。
func serverHost(st *store.Store, id string) string {
	server, err := st.GetServer(id)
	if err != nil || server == nil {
		return ""
	}
	return server.Host
}

func unknownDialerNotice(reason dialdevice.Reason, membershipCount int) string {
	switch reason {
	case dialdevice.ReasonLocalDeviceNotSet:
		return "本机设备尚未登记，无法按共同网络排序；以下只列出兜底主机。"
	case dialdevice.ReasonLocalDeviceDeleted:
		return "本机设备已从拓扑删除，无法按共同网络排序；以下只列出兜底主机。"
	case dialdevice.ReasonDeviceMissingFromSnapshot:
		return "本机设备不在当前快照里，无法按共同网络排序；以下只列出兜底主机。"
	case dialdevice.ReasonClientDeviceNotRecorded:
		return "本机设备还没有随快照记录，无法按共同网络排序；以下只列出兜底主机。"
	default:
		return "本机设备无法识别，无法按共同网络排序；以下只列出兜底主机。"
	}
}
