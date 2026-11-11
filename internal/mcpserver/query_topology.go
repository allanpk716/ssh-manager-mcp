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
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// QueryTopologyInput 是 query_topology 工具的输入。From 是出发点：self 表
// 示当前 sshmgr 实例所在设备，服务器标识符表示该服务器条目所属设备。
// To 为空表示输出邻居分组；非空表示只输出出发点到这些目标的连通详情。
// Detail 为真时展开地址、网络属性与设备角色。
type QueryTopologyInput struct {
	From   string    `json:"from" jsonschema:"origin: \"self\" (this sshmgr instance) or a server id from list_servers"`
	To     *[]string `json:"to,omitempty" jsonschema:"optional target list; each item is a server id for a managed device, or a device name for an unmanaged device or client instance"`
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
		return "", errSharedStorageAuthorityTooOld
	}

	origin, err := resolveQueryTopologyOrigin(st, profileID, from)
	if err != nil {
		return "", err
	}
	if to != nil {
		view, err := buildQueryTopologyPairView(st, profileID, origin, to, detail)
		if err != nil {
			return "", err
		}
		return renderQueryTopologyPairView(view), nil
	}
	view, err := buildQueryTopologyView(st, profileID, origin, detail)
	if err != nil {
		return "", err
	}
	return renderQueryTopologyView(view), nil
}

type queryTopologyDevice struct {
	device       *models.Device
	entries      []*models.DeviceServer
	managed      bool
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
	observations map[string]proberesults.Observation
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
}

type queryTopologyPairView struct {
	origin       *models.Device
	targets      []queryTopologyPairTarget
	now          time.Time
	observations map[string]proberesults.Observation
	networks     map[string]*models.Network
	detail       bool
}

func resolveQueryTopologyOrigin(st *store.Store, profileID, from string) (*models.Device, error) {
	if from == "self" {
		current, err := dialdevice.Current(st, bridgeCacheInstance())
		if err != nil {
			return nil, err
		}
		if !current.Known || current.Device == nil {
			return nil, errors.New("本机未登记设备，请 owner 补登记（sshmgr devices local set）")
		}
		return current.Device, nil
	}

	if err := gateServer(st, profileID, from); err != nil {
		return nil, err
	}
	server, err := st.GetServer(from)
	if errors.Is(err, sql.ErrNoRows) || server == nil {
		return nil, fmt.Errorf("服务器 %s 不存在", from)
	}
	if err != nil {
		return nil, err
	}
	if server.DeviceID == "" {
		return nil, errors.New("起点服务器未关联拓扑设备，请 owner 补登记")
	}
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.ID == server.DeviceID {
			return device, nil
		}
	}
	return nil, fmt.Errorf("起点服务器 %s 关联的拓扑设备已不存在，请 owner 补登记", from)
}

func buildQueryTopologyPairView(st *store.Store, profileID string, origin *models.Device, rawTargets []string, detail bool) (*queryTopologyPairView, error) {
	allowedIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(allowedIDs))
	for _, id := range allowedIDs {
		allowed[id] = true
	}
	allServers, err := st.ListServers()
	if err != nil {
		return nil, err
	}

	view := &queryTopologyPairView{origin: origin, now: time.Now(), detail: detail}
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
			device, err := deviceByID(st, server.DeviceID)
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
			view.setActualNetwork(st, &view.targets[len(view.targets)-1])
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
		if hasDeviceServer(allServers, device.ID) {
			return nil, errors.New("目标不存在或不在授权范围内")
		}
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
		networks, err := st.ListNetworks(store.NetworkFilter{})
		if err != nil {
			return nil, err
		}
		view.networks = make(map[string]*models.Network, len(networks))
		for _, network := range networks {
			view.networks[network.Name] = network
		}
	}
	return view, nil
}

// setActualNetwork 按统一服务器条目拨号规划标记当前会先尝试的共同网络。
// 成功地址缓存读取失败时，统一拨号入口也会继续按登记顺序尝试；这里同样
// 不让辅助缓存把只读查询变成失败。
func (view *queryTopologyPairView) setActualNetwork(st *store.Store, target *queryTopologyPairTarget) {
	if target == nil || target.server == nil || target.device == nil {
		return
	}
	plan := sshbroker.PlanServerDialAddresses(
		target.device.Memberships, view.origin.Memberships, target.server.Host,
	)
	records, present, err := dialplan.NewStoreSource(st, bridgeCacheInstance()).ServerDialSuccess(target.server)
	if err == nil && present {
		plan = sshbroker.PlanServerDialAddressesWithSuccess(
			target.device.Memberships, view.origin.Memberships, target.server.Host,
			records, time.Now(),
		)
	}
	if len(plan.Candidates) > 0 {
		target.selected = plan.Candidates[0].NetworkName
	}
}

// deviceByID 从完整设备列表里按内部标识符取设备；完整列表带有网络成员。
func deviceByID(st *store.Store, id string) (*models.Device, error) {
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.ID == id {
			return device, nil
		}
	}
	return nil, nil
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

func hasDeviceServer(allServers []*models.Server, deviceID string) bool {
	for _, server := range allServers {
		if server.DeviceID == deviceID {
			return true
		}
	}
	return false
}

func buildQueryTopologyView(st *store.Store, profileID string, origin *models.Device, detail bool) (*queryTopologyView, error) {
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	allowedIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(allowedIDs))
	for _, id := range allowedIDs {
		allowed[id] = true
	}
	allServers, err := st.ListServers()
	if err != nil {
		return nil, err
	}
	managedDevices := make(map[string]bool, len(allServers))
	for _, server := range allServers {
		if server.DeviceID != "" {
			managedDevices[server.DeviceID] = true
		}
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
		managed := managedDevices[device.ID]
		entries := deviceServerEntries(allServers, allowed, device.ID)
		// 受管设备只要没有任何当前授权条目就整台不显示；不受管设备与客
		// 户端实例没有服务器授权边界，按设备名照常显示。
		if managed && len(entries) == 0 {
			continue
		}
		candidates = append(candidates, queryTopologyDevice{
			device: device, entries: entries, managed: managed,
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
		view.unreachable = append(view.unreachable, queryDeviceLabel(candidate))
	}

	for _, id := range allowedIDs {
		server, err := st.GetServer(id)
		if err != nil || server == nil {
			continue
		}
		if server.DeviceID == "" {
			view.unregistered = append(view.unregistered, server.ID+"（"+server.Name+"）")
		}
	}
	sort.Slice(view.unregistered, func(i, j int) bool { return view.unregistered[i] < view.unregistered[j] })

	observations, err := queryTopologyObservations(origin.ID)
	if err != nil {
		return nil, err
	}
	view.observations = observations
	if detail {
		networks, err := st.ListNetworks(store.NetworkFilter{})
		if err != nil {
			return nil, err
		}
		view.networks = make(map[string]*models.Network, len(networks))
		for _, network := range networks {
			view.networks[network.Name] = network
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

func queryTopologyObservations(originDeviceID string) (map[string]proberesults.Observation, error) {
	out := make(map[string]proberesults.Observation)
	results, found, err := probeobserve.ResultsFor(bridgeCacheInstance())
	if err != nil || !found {
		return out, err
	}
	observations, err := results.List(originDeviceID, "")
	if err != nil {
		return nil, err
	}
	for _, observation := range observations {
		key := observation.TargetDeviceID + "\x1f" + observation.Network
		if current, exists := out[key]; exists && current.ObservedAt.After(observation.ObservedAt) {
			continue
		}
		out[key] = observation
	}
	return out, nil
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
			continue
		}
		selected := ""
		if target.server != nil {
			selected = target.selected
		}
		for _, networkName := range common {
			label := networkName
			if label == selected {
				label += "（实际会选）"
			}
			line := label
			if view.detail {
				line += " " + queryTopologyNetworkDetail(
					view, target, networkName,
				)
			}
			line += " " + queryTopologyObservationLabel(
				view.observations, view.now, target.device.ID, networkName,
			)
			lines = append(lines, "  "+strings.TrimSpace(line))
		}
	}
	lines = append(lines, "本工具不主动探测；需要现场结论请调用 probe_connectivity。")
	return strings.Join(lines, "\n") + "\n"
}

func queryTopologyPairTargetLabel(target queryTopologyPairTarget) string {
	if target.unregistered || target.device == nil {
		return target.label
	}
	if len(target.entries) == 0 {
		return target.device.Name
	}
	names := make([]string, 0, len(target.entries))
	for _, entry := range target.entries {
		names = append(names, entry.Name)
	}
	return target.device.Name + " [" + strings.Join(names, "、") + "]"
}

func commonQueryNetworks(originNetworks map[string]bool, device *models.Device) []string {
	var physical, virtual []string
	for _, membership := range device.Memberships {
		if membership == nil || !originNetworks[membership.NetworkName] {
			continue
		}
		switch membership.NetworkType {
		case models.NetworkPhysical:
			physical = append(physical, membership.NetworkName)
		case models.NetworkVirtual:
			virtual = append(virtual, membership.NetworkName)
		}
	}
	sort.Strings(physical)
	sort.Strings(virtual)
	return append(physical, virtual...)
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
		label := "隐藏"
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
		addresses = append(addresses, "隐藏")
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
			line := queryDeviceLabel(candidate) + " " + observationLabel(view, candidate.device.ID, networkName)
			if view.detail {
				line = queryDeviceLabel(candidate) + " 角色：" +
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

func queryDeviceLabel(candidate queryTopologyDevice) string {
	labels := make([]string, 0, len(candidate.entries)+1)
	if candidate.device.ClientName != "" {
		labels = append(labels, "客户端实例："+candidate.device.ClientName)
	}
	for _, entry := range candidate.entries {
		labels = append(labels, entry.Name)
	}
	if len(labels) == 0 {
		return candidate.device.Name
	}
	return candidate.device.Name + " [" + strings.Join(labels, "、") + "]"
}

func observationLabel(view *queryTopologyView, targetDeviceID, network string) string {
	return queryTopologyObservationLabel(view.observations, view.now, targetDeviceID, network)
}

func queryTopologyObservationLabel(
	observations map[string]proberesults.Observation, now time.Time, targetDeviceID, network string,
) string {
	observation, exists := observations[targetDeviceID+"\x1f"+network]
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
	age := relativeObservationAge(now, observation.ObservedAt)
	if now.Sub(observation.ObservedAt) > 10*time.Minute {
		age += " 旧"
	}
	parts := []string{marker + detail, age}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func relativeObservationAge(now, observedAt time.Time) string {
	age := now.Sub(observedAt)
	switch {
	case age < time.Minute:
		return "刚刚"
	case age < time.Hour:
		return strconv.Itoa(int(age/time.Minute)) + "m前"
	case age < 24*time.Hour:
		return strconv.Itoa(int(age/time.Hour)) + "h前"
	default:
		return strconv.Itoa(int(age/(24*time.Hour))) + "d前"
	}
}

func nonEmptyList(values []string, emptyLabel string) []string {
	if len(values) == 0 {
		return []string{emptyLabel}
	}
	return values
}
