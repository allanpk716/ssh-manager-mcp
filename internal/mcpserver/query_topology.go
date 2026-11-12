package mcpserver

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/dialplan"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/shareaddress"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// QueryTopologyInput 是 query_topology 工具的输入。From 是出发点：self 表
// 示当前 sshmgr 实例所在设备，服务器标识符表示该服务器条目所属设备。
// To 为空表示输出邻居分组；非空表示只输出出发点到这些目标的连通详情；
// 唯一例外是 To 只有一个共享地址时，输出在 From 服务器上的实际路径。
// Detail 为真时展开地址、网络属性与设备角色。
type QueryTopologyInput struct {
	From   string    `json:"from" jsonschema:"origin: \"self\" (this sshmgr instance) or a server id from list_servers"`
	To     *[]string `json:"to,omitempty" jsonschema:"optional target list; each item is a server id for a managed device, or a device name for an unmanaged device or client instance; alternatively, when from is a server id, put exactly one SMB shared address here to translate it to that server's local path"`
	Detail bool      `json:"detail,omitempty" jsonschema:"optional; when true, expand addresses, network bandwidth, metering, and device roles"`
}

// QueryTopologyForProfile 返回出发点所属网络与四类邻居分组，或在给出目
// 标时返回逐对连通详情。查询只读当前存储、成功地址缓存和本实例的本地探
// 测结果，不发起网络连接。
func QueryTopologyForProfile(st *store.Store, profileID, from string, to []string, detail bool) (string, error) {
	if st == nil {
		return "", errors.New("query_topology 没有绑定存储")
	}
	if strings.TrimSpace(from) == "" {
		return "", errors.New("from 不能为空")
	}
	if st.IsReadOnly() && st.CacheRecommendedVersion() == "" {
		return "", errTopologyAndSharedStorageAuthorityTooOld
	}

	origin, err := resolveQueryTopologyOrigin(st, profileID, from)
	if err != nil {
		return "", err
	}
	if to != nil {
		shareAddress, ordinaryTargetCount, err := queryTopologyShareAddressTarget(to)
		if err != nil {
			return "", err
		}
		if shareAddress != "" {
			if ordinaryTargetCount != 0 {
				return "", errors.New("to 不能同时包含共享地址与普通目标")
			}
			return translateQueryTopologyShareAddress(st, profileID, origin, shareAddress)
		}
		view, err := buildQueryTopologyPairView(st, profileID, origin.Device, to, detail)
		if err != nil {
			return "", err
		}
		return renderQueryTopologyPairView(view), nil
	}
	view, err := buildQueryTopologyView(st, profileID, origin.Device, detail)
	if err != nil {
		return "", err
	}
	return renderQueryTopologyView(view), nil
}

type queryTopologyDevice struct {
	device       *models.Device
	entries      []*models.DeviceServer
	networks     map[string]bool
	networkTypes map[string]models.NetworkType
}

type queryTopologyView struct {
	origin       *models.Device
	originNets   []string
	physical     map[string][]queryTopologyDevice
	virtual      map[string][]queryTopologyDevice
	unreachable  []string
	unregistered []string
	now          time.Time
	observations map[queryTopologyObservationKey]proberesults.Observation
	networks     map[string]*models.Network
	detail       bool
}

type queryTopologyPairTarget struct {
	label        string
	device       *models.Device
	entries      []*models.DeviceServer
	server       *models.Server
	unregistered bool
	selected     string
	// dialFailureReason 记录这一个目标按服务器条目规划拨号失败的简短原
	// 因；查询仍继续输出其他目标。
	dialFailureReason string
}

type queryTopologyPairView struct {
	origin *models.Device
	// originUsesLocalSuccessCache 表示出发点就是本实例所在设备；只有这
	// 时，标记 sshmgr 真正拨号时会优先尝试的网络才读本实例的成功地址
	// 缓存。
	originUsesLocalSuccessCache bool
	targets                     []queryTopologyPairTarget
	now                         time.Time
	observations                map[queryTopologyObservationKey]proberesults.Observation
	networks                    map[string]*models.Network
	detail                      bool
}

func resolveQueryTopologyOrigin(st *store.Store, profileID, from string) (*dialdevice.ResolvedOrigin, error) {
	origin, err := dialdevice.ResolveOrigin(st, bridgeCacheInstance(), from, func(serverID string) error {
		return gateServer(st, profileID, serverID)
	})
	if err != nil {
		return nil, err
	}
	return origin, nil
}

// queryTopologyShareAddressTarget 从 to 中取出唯一的共享地址。返回值还
// 带有非共享目标数量，供调用方拒绝共享地址与普通目标混用。
func queryTopologyShareAddressTarget(targets []string) (string, int, error) {
	shareAddress := ""
	shareCount, ordinaryCount := 0, 0
	for _, target := range targets {
		if !queryTopologyLooksLikeShareAddress(target) {
			ordinaryCount++
			continue
		}
		shareCount++
		shareAddress = strings.TrimSpace(target)
	}
	if shareCount > 1 {
		return "", 0, errors.New("to 一次只能查询一个共享地址")
	}
	return shareAddress, ordinaryCount, nil
}

func queryTopologyLooksLikeShareAddress(target string) bool {
	value := strings.TrimSpace(target)
	return strings.HasPrefix(value, `\\`) ||
		strings.HasPrefix(strings.ToLower(value), "smb://")
}

func translateQueryTopologyShareAddress(
	st *store.Store, profileID string, origin *dialdevice.ResolvedOrigin, rawAddress string,
) (string, error) {
	if origin.Server == nil {
		return "", errors.New("共享地址翻译的 from 必须是授权范围内的服务器标识符")
	}
	visibleShares, err := sharedStoragesVisibleInSession(st, profileID)
	if err != nil {
		return "", err
	}
	resolved, err := shareaddress.Resolve(rawAddress, visibleShares)
	if err != nil {
		return "", err
	}
	for _, mountable := range resolved.Share.MountableServers {
		if mountable.ID != origin.Server.ID {
			continue
		}
		return strings.Join([]string{
			"服务器：" + origin.Server.Name,
			"共享存储：" + resolved.Share.Name + "（提供方设备：" + resolved.Share.DeviceName + "）",
			"可挂载：是",
			"实际路径：" + resolved.PathOnServer(origin.Server.ShellType),
			"本翻译只读登记数据；是否已现场挂载请用 list_shares 的现场检测确认。",
		}, "\n"), nil
	}
	return "", fmt.Errorf("服务器 %s 不在共享存储 %s 的可挂载范围内", origin.Server.Name, resolved.Share.Name)
}

func buildQueryTopologyPairView(st *store.Store, profileID string, origin *models.Device, rawTargets []string, detail bool) (*queryTopologyPairView, error) {
	allowed, err := queryTopologyAllowedServers(st, profileID)
	if err != nil {
		return nil, err
	}
	allServers, err := st.ListServers()
	if err != nil {
		return nil, err
	}

	view := &queryTopologyPairView{
		origin: origin, now: time.Now(), detail: detail,
		originUsesLocalSuccessCache: queryTopologyOriginUsesLocalSuccessCache(st, bridgeCacheInstance(), origin),
	}
	seen := make(map[string]bool, len(rawTargets))
	for _, raw := range rawTargets {
		target := strings.TrimSpace(raw)
		if target == "" {
			return nil, errors.New("to 不能包含空目标")
		}
		if seen[target] {
			continue
		}
		seen[target] = true

		server, serverErr := st.GetServer(target)
		if serverErr != nil && !errors.Is(serverErr, sql.ErrNoRows) {
			return nil, serverErr
		}
		if server != nil {
			if !allowed[server.ID] {
				return nil, errors.New("目标不存在或不在授权范围内")
			}
			if server.DeviceID == "" {
				view.targets = append(view.targets, queryTopologyPairTarget{
					label: server.ID + "（" + server.Name + "）", server: server, unregistered: true,
				})
				continue
			}
			device, err := dialdevice.ByID(st, server.DeviceID)
			if err != nil {
				return nil, err
			}
			if device == nil {
				return nil, fmt.Errorf("目标服务器 %s 关联的拓扑设备已不存在，请 owner 补登记", target)
			}
			view.targets = append(view.targets, queryTopologyPairTarget{
				label: device.Name, device: device, server: server,
				entries: deviceServerEntries(allServers, allowed, device.ID),
			})
			if err := view.setActualNetwork(st, &view.targets[len(view.targets)-1]); err != nil {
				return nil, err
			}
			continue
		}

		device, deviceErr := st.GetDevice(target)
		if deviceErr != nil && !errors.Is(deviceErr, sql.ErrNoRows) && !errors.Is(deviceErr, store.ErrDeviceNotFound) {
			return nil, deviceErr
		}
		if device == nil {
			return nil, errors.New("目标不存在或不在授权范围内")
		}
		device.Memberships, err = st.ListNetworkMembershipsByRegistration(device.Name)
		if err != nil {
			return nil, err
		}
		entries := deviceServerEntries(allServers, allowed, device.ID)
		if len(entries) > 0 {
			return nil, fmt.Errorf("受管设备请用服务器编号指称：%s", device.Name)
		}
		// 设备上的服务器条目全部在授权外时，与不受管设备一样按设备名指
		// 称；授权外条目的名字、编号与地址都不进入输出。
		view.targets = append(view.targets, queryTopologyPairTarget{
			label: device.Name, device: device, entries: entries,
		})
	}

	observations, err := queryTopologyObservations(origin.ID)
	if err != nil {
		return nil, err
	}
	view.observations = observations
	if detail {
		view.networks, err = queryTopologyNetworks(st)
		if err != nil {
			return nil, err
		}
	}
	return view, nil
}

// setActualNetwork 按真实拨号会使用的最终候选计划，标记当前会先尝试的
// 共同网络。成功地址缓存读取失败时，真实拨号也会继续按登记顺序尝试；这
// 里同样不让辅助缓存把只读查询变成失败。
func (view *queryTopologyPairView) setActualNetwork(st *store.Store, target *queryTopologyPairTarget) error {
	if target == nil || target.server == nil || target.device == nil {
		return nil
	}
	var successCache sshbroker.DialSuccessCacheSource
	if view.originUsesLocalSuccessCache {
		successCache = dialplan.NewStoreSource(st, bridgeCacheInstance())
	}
	planned, err := sshbroker.PlanServerDialCandidates(sshbroker.ServerDialPlanInput{
		Server: target.server, HostKeys: st,
		Topology: sshbroker.DialTopology{
			TargetMemberships: target.device.Memberships,
			DialerMemberships: view.origin.Memberships,
			DialerKnown:       true,
		},
		SuccessCache: successCache,
		Now:          view.now,
	})
	if err != nil {
		if errors.Is(err, sshbroker.ErrFirstTrustNoCommonVirtualLan) {
			target.dialFailureReason = "真正拨号会失败：这个服务器条目还没有主机密钥锚，首次连接只能经虚拟局域网，而出发点与它没有共同虚拟局域网"
			return nil
		}
		return err
	}
	if len(planned.Plan.Candidates) > 0 {
		target.selected = planned.Plan.Candidates[0].NetworkName
	}
	return nil
}

// queryTopologyOriginUsesLocalSuccessCache 判断出发点是否就是本实例所在
// 设备。成功地址缓存记录的是本实例拨出的结果；出发点换成别的设备时不读
// 它，只按登记顺序与锚定规则规划。
func queryTopologyOriginUsesLocalSuccessCache(st *store.Store, instance string, origin *models.Device) bool {
	if origin == nil {
		return false
	}
	current, err := dialdevice.Current(st, instance)
	if err != nil || !current.Known || current.Device == nil {
		return false
	}
	return current.Device.ID == origin.ID
}

// queryTopologyAllowedServers 返回当前授权范围内的服务器编号集合。
func queryTopologyAllowedServers(st *store.Store, profileID string) (map[string]bool, error) {
	allowedIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(allowedIDs))
	for _, id := range allowedIDs {
		allowed[id] = true
	}
	return allowed, nil
}

// queryTopologyNetworks 按名字索引网络登记，供展开参数显示带宽与计费。
func queryTopologyNetworks(st *store.Store) (map[string]*models.Network, error) {
	networks, err := st.ListNetworks(store.NetworkFilter{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*models.Network, len(networks))
	for _, network := range networks {
		out[network.Name] = network
	}
	return out, nil
}

// deviceServerEntries 收集一台设备上当前授权范围内的服务器条目摘要。
func deviceServerEntries(allServers []*models.Server, allowed map[string]bool, deviceID string) []*models.DeviceServer {
	var entries []*models.DeviceServer
	for _, server := range allServers {
		if server.DeviceID != deviceID || !allowed[server.ID] {
			continue
		}
		entries = append(entries, &models.DeviceServer{
			ID: server.ID, Name: server.Name, User: server.User, Port: server.Port,
			ExposeHost: server.ExposeHost,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

func buildQueryTopologyView(st *store.Store, profileID string, origin *models.Device, detail bool) (*queryTopologyView, error) {
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	allowed, err := queryTopologyAllowedServers(st, profileID)
	if err != nil {
		return nil, err
	}
	allServers, err := st.ListServers()
	if err != nil {
		return nil, err
	}
	view := &queryTopologyView{
		origin: origin, now: time.Now(), detail: detail,
		physical: make(map[string][]queryTopologyDevice),
		virtual:  make(map[string][]queryTopologyDevice),
	}
	originNetworks := networkSet(origin)
	view.originNets = networkLabels(origin)

	var candidates []queryTopologyDevice
	for _, device := range devices {
		if device.ID == origin.ID {
			continue
		}
		entries := deviceServerEntries(allServers, allowed, device.ID)
		candidates = append(candidates, queryTopologyDevice{
			device: device, entries: entries,
			networks: networkSet(device), networkTypes: networkTypes(device),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].device.Name != candidates[j].device.Name {
			return candidates[i].device.Name < candidates[j].device.Name
		}
		return candidates[i].device.ID < candidates[j].device.ID
	})

	for _, candidate := range candidates {
		var physical, virtual []string
		for networkName := range candidate.networks {
			if !originNetworks[networkName] {
				continue
			}
			if candidate.networkTypes[networkName] == models.NetworkPhysical {
				physical = append(physical, networkName)
			} else if candidate.networkTypes[networkName] == models.NetworkVirtual {
				virtual = append(virtual, networkName)
			}
		}
		sort.Strings(physical)
		sort.Strings(virtual)
		if len(physical) > 0 {
			for _, networkName := range physical {
				view.physical[networkName] = append(view.physical[networkName], candidate)
			}
			continue
		}
		if len(virtual) > 0 {
			for _, networkName := range virtual {
				view.virtual[networkName] = append(view.virtual[networkName], candidate)
			}
			continue
		}
		view.unreachable = append(view.unreachable, queryTopologyDeviceLabel(candidate.device, candidate.entries))
	}

	for _, server := range allServers {
		if server.DeviceID != "" || !allowed[server.ID] {
			continue
		}
		view.unregistered = append(view.unregistered, server.ID+"（"+server.Name+"）")
	}
	sort.Slice(view.unregistered, func(i, j int) bool { return view.unregistered[i] < view.unregistered[j] })

	observations, err := queryTopologyObservations(origin.ID)
	if err != nil {
		return nil, err
	}
	view.observations = observations
	if detail {
		view.networks, err = queryTopologyNetworks(st)
		if err != nil {
			return nil, err
		}
	}
	return view, nil
}

func networkSet(device *models.Device) map[string]bool {
	out := make(map[string]bool, len(device.Memberships))
	for _, membership := range device.Memberships {
		out[membership.NetworkName] = true
	}
	return out
}

func networkTypes(device *models.Device) map[string]models.NetworkType {
	out := make(map[string]models.NetworkType, len(device.Memberships))
	for _, membership := range device.Memberships {
		out[membership.NetworkName] = membership.NetworkType
	}
	return out
}

func networkLabels(device *models.Device) []string {
	var physical, virtual []string
	for _, membership := range device.Memberships {
		switch membership.NetworkType {
		case models.NetworkPhysical:
			label := membership.NetworkName + "（物理局域网"
			if membership.SiteName != "" {
				label += "，站点：" + membership.SiteName
			}
			physical = append(physical, label+"）")
		case models.NetworkVirtual:
			virtual = append(virtual, membership.NetworkName+"（虚拟局域网）")
		}
	}
	sort.Strings(physical)
	sort.Strings(virtual)
	return append(physical, virtual...)
}

func queryTopologyObservations(originDeviceID string) (map[queryTopologyObservationKey]proberesults.Observation, error) {
	out := make(map[queryTopologyObservationKey]proberesults.Observation)
	results, found, err := probeobserve.ResultsFor(bridgeCacheInstance())
	if err != nil || !found {
		return out, err
	}
	observations, err := results.List(originDeviceID, "")
	if err != nil {
		return nil, err
	}
	for _, observation := range observations {
		key := queryTopologyObservationKey{TargetDeviceID: observation.TargetDeviceID, Network: observation.Network}
		if current, exists := out[key]; exists && current.ObservedAt.After(observation.ObservedAt) {
			continue
		}
		out[key] = observation
	}
	return out, nil
}

// queryTopologyObservationKey 按「目标设备 + 网络」索引本实例保存的观察。
type queryTopologyObservationKey struct {
	TargetDeviceID string
	Network        string
}

func renderQueryTopologyView(view *queryTopologyView) string {
	lines := []string{
		"出发点：" + view.origin.Name,
		"所属网络：" + strings.Join(nonEmptyList(view.originNets, "未登记"), "、"),
	}
	lines = appendGroup(lines, "同在物理局域网：", sortedNetworkNames(view.physical), view.physical, view)
	lines = appendGroup(lines, "只能经虚拟局域网到达：", sortedNetworkNames(view.virtual), view.virtual, view)
	lines = append(lines, "无共同网络不可达："+strings.Join(nonEmptyList(view.unreachable, "无"), "、"))
	lines = append(lines, "未登记服务器："+strings.Join(nonEmptyList(view.unregistered, "无"), "、"))
	lines = append(lines, "本工具不主动探测；需要现场结论请调用 probe_connectivity。")
	return strings.Join(lines, "\n") + "\n"
}

func renderQueryTopologyPairView(view *queryTopologyPairView) string {
	lines := []string{"出发点：" + view.origin.Name}
	originNetworks := networkSet(view.origin)
	for _, target := range view.targets {
		lines = append(lines, "目标："+queryTopologyPairTargetLabel(target))
		if target.unregistered || target.device == nil {
			lines = append(lines, "  服务器未关联拓扑设备，未登记")
			continue
		}
		if view.detail {
			lines = append(lines, "  角色："+queryTopologyDeviceRole(target.device, target.entries))
		}

		common := commonQueryNetworks(originNetworks, target.device)
		if len(common) == 0 {
			lines = append(lines, "  无共同网络，不可达")
			if target.dialFailureReason != "" {
				lines = append(lines, "  "+target.dialFailureReason)
			}
			continue
		}
		var virtualOnly []string
		hasPhysical := false
		for _, network := range common {
			if network.networkType == models.NetworkPhysical {
				hasPhysical = true
				continue
			}
			if network.networkType == models.NetworkVirtual {
				virtualOnly = append(virtualOnly, network.name)
			}
		}
		if !hasPhysical && len(virtualOnly) > 0 {
			lines = append(lines, "  只能经虚拟局域网到达："+strings.Join(virtualOnly, "、"))
		}
		selected := ""
		if target.server != nil {
			selected = target.selected
		}
		for _, network := range common {
			label := network.name + "（" + networkTypeLabel(network.networkType) + "）"
			if network.name == selected {
				label = network.name + "（" + networkTypeLabel(network.networkType) + "，实际会选）"
			}
			line := label
			if view.detail {
				line += " " + queryTopologyNetworkDetail(
					view, target, network.name,
				)
			}
			line += " " + queryTopologyObservationLabel(
				view.observations, view.now, target.device.ID, network.name,
			)
			lines = append(lines, "  "+strings.TrimSpace(line))
		}
		if target.dialFailureReason != "" {
			lines = append(lines, "  "+target.dialFailureReason)
		}
	}
	lines = append(lines, "本工具不主动探测；需要现场结论请调用 probe_connectivity。")
	return strings.Join(lines, "\n") + "\n"
}

func queryTopologyPairTargetLabel(target queryTopologyPairTarget) string {
	if target.unregistered || target.device == nil {
		return target.label
	}
	return queryTopologyDeviceLabel(target.device, target.entries)
}

type queryTopologyNetwork struct {
	name        string
	networkType models.NetworkType
}

func commonQueryNetworks(originNetworks map[string]bool, device *models.Device) []queryTopologyNetwork {
	var physical, virtual []queryTopologyNetwork
	for _, membership := range device.Memberships {
		if membership == nil || !originNetworks[membership.NetworkName] {
			continue
		}
		switch membership.NetworkType {
		case models.NetworkPhysical:
			physical = append(physical, queryTopologyNetwork{name: membership.NetworkName, networkType: models.NetworkPhysical})
		case models.NetworkVirtual:
			virtual = append(virtual, queryTopologyNetwork{name: membership.NetworkName, networkType: models.NetworkVirtual})
		}
	}
	sort.Slice(physical, func(i, j int) bool { return physical[i].name < physical[j].name })
	sort.Slice(virtual, func(i, j int) bool { return virtual[i].name < virtual[j].name })
	return append(physical, virtual...)
}

func networkTypeLabel(networkType models.NetworkType) string {
	if networkType == models.NetworkVirtual {
		return "虚拟局域网"
	}
	return "物理局域网"
}

func queryTopologyDeviceRole(device *models.Device, entries []*models.DeviceServer) string {
	var labels []string
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name)
		}
		labels = append(labels, "受管设备（"+strings.Join(names, "、")+"）")
	}
	if device.ClientName != "" {
		labels = append(labels, "客户端实例："+device.ClientName)
	}
	if len(labels) == 0 {
		return "不受管设备"
	}
	return strings.Join(labels, "、")
}

func queryTopologyNetworkDetail(
	view *queryTopologyPairView, target queryTopologyPairTarget, networkName string,
) string {
	expose := target.server != nil && target.server.ExposeHost
	return queryTopologyNetworkFields(
		view.networks, target.device, networkName, expose,
	)
}

func queryTopologyGroupNetworkDetail(
	view *queryTopologyView, candidate queryTopologyDevice, networkName string,
) string {
	expose := false
	for _, entry := range candidate.entries {
		if entry.ExposeHost {
			expose = true
			break
		}
	}
	return queryTopologyNetworkFields(view.networks, candidate.device, networkName, expose)
}

func queryTopologyNetworkFields(
	networks map[string]*models.Network, device *models.Device, networkName string, expose bool,
) string {
	var addresses []string
	seenAddresses := make(map[string]bool)
	for _, membership := range device.Memberships {
		if membership == nil || membership.NetworkName != networkName {
			continue
		}
		label := "hidden"
		if expose {
			label = "未登记"
			if membership.Address != "" {
				label = membership.Address
			}
		}
		if seenAddresses[label] {
			continue
		}
		seenAddresses[label] = true
		addresses = append(addresses, label)
	}
	if len(addresses) == 0 {
		addresses = append(addresses, "hidden")
	}

	bandwidth := "标称带宽：未登记"
	if network := networks[networkName]; network != nil && network.BandwidthMbps > 0 {
		bandwidth = "标称带宽：" + strconv.FormatInt(network.BandwidthMbps, 10) + " Mbit/s"
	}
	metered := "不按流量计费"
	if network := networks[networkName]; network != nil && network.Metered {
		metered = "按流量计费"
	}
	return "地址：" + strings.Join(addresses, "、") + "；" + bandwidth + "；" + metered
}

func appendGroup(lines []string, title string, networkNames []string,
	groups map[string][]queryTopologyDevice, view *queryTopologyView,
) []string {
	lines = append(lines, title)
	if len(networkNames) == 0 {
		return append(lines, "  无")
	}
	for _, networkName := range networkNames {
		lines = append(lines, "  "+networkName)
		for _, candidate := range groups[networkName] {
			line := queryTopologyDeviceLabel(candidate.device, candidate.entries) + " " + observationLabel(view, candidate.device.ID, networkName)
			if view.detail {
				line = queryTopologyDeviceLabel(candidate.device, candidate.entries) + " 角色：" +
					queryTopologyDeviceRole(candidate.device, candidate.entries) + " " + networkName +
					" " + queryTopologyGroupNetworkDetail(view, candidate, networkName) +
					" " + observationLabel(view, candidate.device.ID, networkName)
			}
			lines = append(lines, "    "+strings.TrimSpace(line))
		}
	}
	return lines
}

func sortedNetworkNames(groups map[string][]queryTopologyDevice) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// queryTopologyDeviceLabel 是分组视图与逐对视图共用的设备行标签：设备名
// 后跟已授权服务器条目名；客户端实例设备同时标出客户端实例名。
func queryTopologyDeviceLabel(device *models.Device, entries []*models.DeviceServer) string {
	labels := make([]string, 0, len(entries)+1)
	if device.ClientName != "" {
		labels = append(labels, "客户端实例："+device.ClientName)
	}
	for _, entry := range entries {
		labels = append(labels, entry.Name)
	}
	if len(labels) == 0 {
		return device.Name
	}
	return device.Name + " [" + strings.Join(labels, "、") + "]"
}

func observationLabel(view *queryTopologyView, targetDeviceID, network string) string {
	return queryTopologyObservationLabel(view.observations, view.now, targetDeviceID, network)
}

func queryTopologyObservationLabel(
	observations map[queryTopologyObservationKey]proberesults.Observation, now time.Time, targetDeviceID, network string,
) string {
	observation, exists := observations[queryTopologyObservationKey{TargetDeviceID: targetDeviceID, Network: network}]
	if !exists || observation.ObservedAt.IsZero() {
		return ""
	}
	detail := ""
	if observation.OK {
		if observation.Duration >= 0 {
			detail = strconv.FormatInt(observation.Duration.Milliseconds(), 10) + "ms"
		}
	} else {
		detail = models.ProbeErrorCategoryLabel(observation.Category)
	}
	marker := "✗"
	if observation.OK {
		marker = "✓"
	}
	age := probe.ObservationAgeLabel(now, observation.ObservedAt)
	parts := []string{marker + detail, age}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func nonEmptyList(values []string, emptyLabel string) []string {
	if len(values) == 0 {
		return []string{emptyLabel}
	}
	return values
}
