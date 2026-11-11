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
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/store"
)

// QueryTopologyInput 是 query_topology 工具的输入。From 是出发点：self 表
// 示当前 sshmgr 实例所在设备，服务器标识符表示该服务器条目所属设备。
type QueryTopologyInput struct {
	From string `json:"from" jsonschema:"origin: \"self\" (this sshmgr instance) or a server id from list_servers"`
}

// QueryTopologyForProfile 返回出发点所属网络与四类邻居分组。查询只读当
// 前存储和本实例的本地探测结果，不发起网络连接。
func QueryTopologyForProfile(st *store.Store, profileID, from string) (string, error) {
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
	view, err := buildQueryTopologyView(st, profileID, origin)
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

func buildQueryTopologyView(st *store.Store, profileID string, origin *models.Device) (*queryTopologyView, error) {
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
		origin: origin, now: time.Now(),
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
		var entries []*models.DeviceServer
		for _, server := range allServers {
			if server.DeviceID == device.ID && allowed[server.ID] {
				entries = append(entries, &models.DeviceServer{
					ID: server.ID, Name: server.Name, User: server.User, Port: server.Port,
				})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Name != entries[j].Name {
				return entries[i].Name < entries[j].Name
			}
			return entries[i].ID < entries[j].ID
		})
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
	observation, exists := view.observations[targetDeviceID+"\x1f"+network]
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
	age := relativeObservationAge(view.now, observation.ObservedAt)
	if view.now.Sub(observation.ObservedAt) > 10*time.Minute {
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
