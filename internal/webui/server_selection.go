package webui

import (
	"sort"
	"strings"
)

// ServerSelectionResource 描述「给定资源、给定服务器集合变化」的编辑起点。
// #298 的服务器列表批量授权与 #299 的服务器详情授权开关只要组装这个结
// 构，并复用 BuildServerSelection 与 _server-selection.tmpl 模板片段，就
// 能得到搜索、按地点/网络/profile 筛选、三态全选与底部差异统计；不要在
// 各页面复制这套交互。
type ServerSelectionResource struct {
	Kind string
	ID   string
	Name string
	Note string
	// CurrentServerIDs 是打开页面时的授权集合，既做差异基准，也做并发修
	// 改时的期望值。
	CurrentServerIDs []string
	// DesiredServerIDs 为空时默认等于当前集合；从重新验证地址回到本页时
	// 会显式传入候选集合。
	DesiredServerIDs []string
	// SelectionMode 为 add 时目标集合是当前授权与提交值的并集，用于服
	// 务器列表的批量「加入」操作；空值表示提交值就是完整目标集合。
	SelectionMode string
	CancelPath    string
}

// KindLabel 返回资源类型的人能读懂名称。
func (r ServerSelectionResource) KindLabel() string {
	switch r.Kind {
	case authorizationResourceKindProfile:
		return "Profile"
	case authorizationResourceKindSharedStore:
		return "共享存储"
	default:
		return "机密项"
	}
}

// ServerSelectionChoice 是勾选组件里的一台服务器。搜索与三类筛选都在浏
// 览器内完成；隐藏行的勾选仍随表单提交，因此筛选不会丢失选择。
type ServerSelectionChoice struct {
	ID             string
	Name           string
	DeviceName     string
	SiteID         string
	SitePath       string
	NetworkIDs     []string
	NetworkNames   []string
	ProfileIDs     []string
	ProfileNames   []string
	Checked        bool
	CurrentChecked bool
	// SharesDeviceWithOtherEntries 表示同一台机器上还有别的服务器条目；
	// 共享存储勾选页用它提示同机条目可能读到同一挂载内容。
	SharesDeviceWithOtherEntries bool
	SearchText                   string
}

// ServerSelectionOption 是一个筛选下拉选项；Value 是标识符，Label 是完整
// 名称或完整地点路径。
type ServerSelectionOption struct {
	Value string
	Label string
}

// ServerSelectionView 是共用服务器勾选组件的渲染数据。
type ServerSelectionView struct {
	Resource          ServerSelectionResource
	Sites             []ServerSelectionOption
	Networks          []ServerSelectionOption
	Profiles          []ServerSelectionOption
	Servers           []ServerSelectionChoice
	Query             string
	SiteFilter        string
	NetworkFilter     string
	ProfileFilter     string
	ReturnPath        string
	InitialAddedCount int
	InitialRemoved    []AccessServerRef
}

// AuthorizationClientAccess 是一个客户端当前取得资源的途径。
type AuthorizationClientAccess struct {
	ClientID   string
	ClientName string
	Profile    string
	ViaServers []string
}

// ViaText 返回模板直接可用的途径文案；空集合明确表示无法拿到。
func (a AuthorizationClientAccess) ViaText() string {
	if len(a.ViaServers) == 0 {
		return "无法拿到"
	}
	return "经 " + strings.Join(a.ViaServers, "、")
}

// AuthorizationClientChange 是确认页里一个客户端的实际取得途径变化。
type AuthorizationClientChange struct {
	ClientID   string
	ClientName string
	Profile    string
	Before     []string
	After      []string
}

// BeforeText 返回提交前途径；空集合明确表示无法拿到。
func (c AuthorizationClientChange) BeforeText() string {
	if len(c.Before) == 0 {
		return "现在无法拿到"
	}
	return "现在经 " + strings.Join(c.Before, "、")
}

// AfterText 返回提交后途径；空集合明确表示无法拿到。
func (c AuthorizationClientChange) AfterText() string {
	if len(c.After) == 0 {
		return "提交后无法拿到"
	}
	return "提交后经 " + strings.Join(c.After, "、")
}

type authorizationProfileCard struct {
	Profile       AccessProfileSummary
	Clients       []ClientSummary
	DetailPath    string
	SelectionPath string
}

type authorizationSecretCard struct {
	Secret      SecretRow
	ClientCount int
}

type authorizationShareCard struct {
	Share       ShareRow
	ClientCount int
}

type authorizationField struct {
	Label string
	Value string
}

type authorizationDetail struct {
	Kind          string
	KindLabel     string
	ID            string
	Name          string
	Note          string
	Fields        []authorizationField
	Servers       []AccessServerRef
	Clients       []AuthorizationClientAccess
	SelectionPath string
	CanAdjust     bool
}

// ServerSelectionConfirm 是 Profile 确认页的数据。机密项与共享存储的确认
// 复用既有授权预览页，只借 Clients 字段渲染客户端影响。
type ServerSelectionConfirm struct {
	KindLabel    string
	ResourceID   string
	ResourceName string
	Added        []AccessServerRef
	Removed      []AccessServerRef
	Clients      []AuthorizationClientChange
	ReturnPath   string
	ProfileID    string
	ExpectedIDs  []string
	DesiredIDs   []string
	Mode         string
}

type authorizationPage struct {
	Subpage            string
	Notice             string
	MissingSecretCount int
	MissingShareCount  int
	Profiles           []authorizationProfileCard
	Secrets            []authorizationSecretCard
	Shares             []authorizationShareCard
	Detail             *authorizationDetail
	Selection          *ServerSelectionView
	Confirm            *ServerSelectionConfirm
	Flash              *AccessFlash
	DeleteRefusal      *AccessProfileInUse
	DeleteReverifyID   string
	Query              string
	ServerFilter       string
	ServerOptions      []ServerRow
	DeviceFilter       string
	DeviceOptions      []string
}

// BuildServerSelection 是共用勾选组件的纯组装函数。复用方只需要给出资
// 源、全部服务器、全部 profile 与拓扑数据；它不读写任何来源，方便单元
// 测试与 #298、#299 在不同页面复用。拓扑为空时地点与网络筛选自然为空。
func BuildServerSelection(
	resource ServerSelectionResource,
	servers []ServerRow,
	profiles []AccessProfileSummary,
	topology *TopologyData,
) *ServerSelectionView {
	current := stringSet(resource.CurrentServerIDs)
	desired := mergeServerSelectionDesired(resource.CurrentServerIDs, resource.DesiredServerIDs, resource.SelectionMode)
	selected := stringSet(desired)
	view := &ServerSelectionView{Resource: resource}
	view.ReturnPath = resource.CancelPath

	deviceByServer := map[string]TopologyDevice{}
	siteByID := map[string]TopologySite{}
	networkByID := map[string]TopologyNetwork{}
	networksByDevice := map[string][]string{}
	if topology != nil {
		for _, site := range topology.Sites {
			siteByID[site.ID] = site
			if site.ID != "" {
				view.Sites = append(view.Sites, ServerSelectionOption{Value: site.ID, Label: site.LocationPath})
			}
		}
		for _, network := range topology.Networks {
			networkByID[network.ID] = network
			if network.ID != "" {
				view.Networks = append(view.Networks, ServerSelectionOption{Value: network.ID, Label: network.Name})
			}
		}
		for _, membership := range topology.Memberships {
			networksByDevice[membership.DeviceID] = append(networksByDevice[membership.DeviceID], membership.NetworkID)
		}
		for _, device := range topology.Devices {
			for _, server := range device.Servers {
				deviceByServer[server.ID] = device
			}
		}
	}
	sort.Slice(view.Sites, func(i, j int) bool { return view.Sites[i].Label < view.Sites[j].Label })
	sort.Slice(view.Networks, func(i, j int) bool { return view.Networks[i].Label < view.Networks[j].Label })

	for _, server := range servers {
		choice := ServerSelectionChoice{
			ID: server.ID, Name: server.Name,
			Checked: selected[server.ID], CurrentChecked: current[server.ID],
		}
		if device, ok := deviceByServer[server.ID]; ok {
			choice.DeviceName = device.Name
			choice.SiteID = device.SiteID
			choice.SitePath = device.LocationPath
			for _, networkID := range networksByDevice[device.ID] {
				if network, ok := networkByID[networkID]; ok {
					choice.NetworkIDs = append(choice.NetworkIDs, network.ID)
					choice.NetworkNames = append(choice.NetworkNames, network.Name)
				}
			}
		}
		if site, ok := siteByID[choice.SiteID]; ok && choice.SitePath == "" {
			choice.SitePath = site.LocationPath
		}
		view.Servers = append(view.Servers, choice)
	}

	profileIDsByServer := map[string][]string{}
	profileNamesByServer := map[string][]string{}
	for _, profile := range profiles {
		if profile.ID == "" {
			continue
		}
		view.Profiles = append(view.Profiles, ServerSelectionOption{Value: profile.ID, Label: profile.Name})
		for _, server := range profile.Servers {
			profileIDsByServer[server.ID] = append(profileIDsByServer[server.ID], profile.ID)
			profileNamesByServer[server.ID] = append(profileNamesByServer[server.ID], profile.Name)
		}
	}
	sort.Slice(view.Profiles, func(i, j int) bool { return view.Profiles[i].Label < view.Profiles[j].Label })
	deviceNameCount := map[string]int{}
	for index := range view.Servers {
		if view.Servers[index].DeviceName != "" {
			deviceNameCount[view.Servers[index].DeviceName]++
		}
	}
	for index := range view.Servers {
		choice := &view.Servers[index]
		choice.ProfileIDs = profileIDsByServer[choice.ID]
		choice.ProfileNames = profileNamesByServer[choice.ID]
		choice.SharesDeviceWithOtherEntries = choice.DeviceName != "" && deviceNameCount[choice.DeviceName] > 1
		search := []string{choice.Name, choice.DeviceName, choice.SitePath,
			strings.Join(choice.NetworkNames, " "), strings.Join(choice.ProfileNames, " ")}
		choice.SearchText = strings.ToLower(strings.Join(search, " "))
	}

	currentRefs := resourceRefs(resource.CurrentServerIDs, servers)
	desiredRefs := resourceRefs(desired, servers)
	view.InitialAddedCount, view.InitialRemoved = serverSelectionDifference(currentRefs, desiredRefs)
	return view
}

func resourceRefs(ids []string, servers []ServerRow) []AccessServerRef {
	byID := make(map[string]ServerRow, len(servers))
	for _, server := range servers {
		byID[server.ID] = server
	}
	out := make([]AccessServerRef, 0, len(ids))
	for _, id := range ids {
		if server, ok := byID[id]; ok {
			out = append(out, AccessServerRef{ID: server.ID, Name: server.Name})
		}
	}
	return out
}

func serverRefsByNames(servers []ServerRow, names []string) []AccessServerRef {
	byName := make(map[string]ServerRow, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	out := make([]AccessServerRef, 0, len(names))
	for _, name := range names {
		if server, ok := byName[name]; ok {
			out = append(out, AccessServerRef{ID: server.ID, Name: server.Name})
		}
	}
	return out
}

func shareServerRefs(rows []ShareServerView) []AccessServerRef {
	out := make([]AccessServerRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, AccessServerRef{ID: row.ID, Name: row.Name})
	}
	return out
}

// serverSelectionDifference 返回目标集合相对当前集合的新增数量与移除清单。
func serverSelectionDifference(current, desired []AccessServerRef) (int, []AccessServerRef) {
	currentSet := make(map[string]bool, len(current))
	for _, server := range current {
		currentSet[server.ID] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	added := 0
	for _, server := range desired {
		desiredSet[server.ID] = true
		if !currentSet[server.ID] {
			added++
		}
	}
	removed := make([]AccessServerRef, 0)
	for _, server := range current {
		if !desiredSet[server.ID] {
			removed = append(removed, server)
		}
	}
	return added, removed
}

func mergeServerSelectionDesired(current, submitted []string, mode string) []string {
	if mode == serverSelectionModeAdd {
		seen := make(map[string]bool, len(current)+len(submitted))
		desired := make([]string, 0, len(current)+len(submitted))
		for _, ids := range [][]string{current, submitted} {
			for _, id := range ids {
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				desired = append(desired, id)
			}
		}
		return desired
	}
	if submitted == nil {
		return current
	}
	return submitted
}
