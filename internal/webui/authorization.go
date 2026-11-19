package webui

import (
	"net/http"
	"sort"
	"strings"
)

const (
	authorizationRootPath             = "/ui/authorization/"
	authorizationProfilesPath         = authorizationRootPath + "profiles"
	authorizationSecretsPath          = authorizationRootPath + "secrets"
	authorizationSharesPath           = authorizationRootPath + "shares"
	authorizationSelectionPreviewPath = authorizationRootPath + "selection/preview"
	authorizationSecretsNewPath       = authorizationSecretsPath + "/new"
	authorizationServersSuffix        = "/servers"

	authorizationResourceKindProfile     = "profile"
	authorizationResourceKindSecret      = "secret"
	authorizationResourceKindSharedStore = "share"
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
	CancelPath       string
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
	desired := resource.DesiredServerIDs
	if desired == nil {
		desired = resource.CurrentServerIDs
	}
	selected := stringSet(desired)
	view := &ServerSelectionView{Resource: resource}

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

func registerAuthorizationRoutes(h *handler) {
	h.mux.HandleFunc(authorizationRootPath, h.requireSession(h.handleAuthorizationRoutes))
}

func (h *handler) handleAuthorizationRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == authorizationSelectionPreviewPath {
		if r.Method != http.MethodPost {
			h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "生成授权确认页只接受 POST 请求。")
			return
		}
		h.handleAuthorizationSelectionPreview(w, r)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, authorizationRootPath), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		h.redirectAuthorizationRoot(w, r)
		return
	}
	switch parts[0] {
	case "profiles":
		h.handleAuthorizationProfileRoutes(w, r, parts)
	case "secrets":
		h.handleAuthorizationSecretRoutes(w, r, parts)
	case "shares":
		h.handleAuthorizationShareRoutes(w, r, parts)
	default:
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	}
}

func (h *handler) redirectAuthorizationRoot(w http.ResponseWriter, r *http.Request) {
	target := authorizationRootPath
	switch {
	case h.caps.CanManageProfiles && h.cfg.Access != nil:
		target = authorizationProfilesPath
	case h.cfg.Secrets != nil:
		target = authorizationSecretsPath
	case h.cfg.Shares != nil:
		target = authorizationSharesPath
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *handler) handleAuthorizationProfileRoutes(w http.ResponseWriter, r *http.Request, parts []string) {
	if h.cfg.Access == nil || !h.caps.CanManageProfiles {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if len(parts) == 1 {
		h.handleAuthorizationProfiles(w, r)
		return
	}
	id := parts[1]
	if len(parts) == 2 {
		h.handleAuthorizationProfileDetail(w, r, id)
		return
	}
	if len(parts) == 3 && parts[2] == "servers" {
		h.handleAuthorizationServerSelection(w, r, authorizationResourceKindProfile, id)
		return
	}
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleAuthorizationSecretRoutes(w http.ResponseWriter, r *http.Request, parts []string) {
	if h.cfg.Secrets == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if len(parts) == 1 {
		h.handleSecretsRoot(w, r)
		return
	}
	if parts[1] == "new" && len(parts) == 2 {
		if !h.caps.CanManageSecrets {
			h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
			return
		}
		h.handleSecretNew(w, r)
		return
	}
	if len(parts) == 2 {
		h.handleSecretDetail(w, r, parts[1])
		return
	}
	if len(parts) == 3 && parts[2] == "servers" {
		if !h.caps.CanManageSecrets || h.cfg.SecretsWriter == nil {
			h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
			return
		}
		h.handleAuthorizationServerSelection(w, r, authorizationResourceKindSecret, parts[1])
		return
	}
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleAuthorizationShareRoutes(w http.ResponseWriter, r *http.Request, parts []string) {
	if h.cfg.Shares == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if len(parts) == 1 {
		h.handleSharesRoot(w, r)
		return
	}
	if len(parts) == 2 {
		h.handleShareDetail(w, r, parts[1])
		return
	}
	if len(parts) == 3 && parts[2] == "servers" {
		if !h.caps.CanManageShares || h.cfg.SecretsWriter == nil || h.cfg.Secrets == nil {
			h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
			return
		}
		h.handleAuthorizationServerSelection(w, r, authorizationResourceKindSharedStore, parts[1])
		return
	}
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleAuthorizationProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "Profile 子页只接受 GET 请求。")
		return
	}
	profiles, clients, err := h.authorizationProfilesAndClients()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 Profile 失败", "读取 Profile 与客户端失败，请稍后重试。")
		return
	}
	cards := make([]authorizationProfileCard, 0, len(profiles))
	for _, profile := range profiles {
		cards = append(cards, authorizationProfileCard{
			Profile:       profile,
			Clients:       clientsForProfile(clients, profile.ID),
			DetailPath:    authorizationProfilesPath + "/" + profile.ID,
			SelectionPath: authorizationProfilesPath + "/" + profile.ID + authorizationServersSuffix,
		})
	}
	h.renderAuthorizationPage(w, r, http.StatusOK, "authorization-profiles.tmpl", "Profile", &authorizationPage{
		Subpage: "profiles", Profiles: cards,
		Flash: h.takeAccessFlash(r), DeleteRefusal: h.takeAuthorizationRefusal(r),
		DeleteReverifyID: strings.TrimSpace(r.URL.Query().Get("reverify_delete")),
	})
}

func (h *handler) handleAuthorizationProfileDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "Profile 详情只接受 GET 请求。")
		return
	}
	profile, err := h.accessProfileByID(id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 Profile 失败", "读取 Profile 失败，请稍后重试。")
		return
	}
	if profile == nil {
		h.renderError(w, r, http.StatusNotFound, "Profile 不存在", "请求的 Profile 不存在。")
		return
	}
	_, clients, err := h.authorizationProfilesAndClients()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取客户端失败", "读取客户端失败，请稍后重试。")
		return
	}
	detail := &authorizationDetail{
		Kind: authorizationResourceKindProfile, KindLabel: "Profile", ID: profile.ID, Name: profile.Name,
		Servers:       profile.Servers,
		SelectionPath: authorizationProfilesPath + "/" + profile.ID + authorizationServersSuffix,
		CanAdjust:     true,
	}
	for _, client := range clientsForProfile(clients, profile.ID) {
		detail.Clients = append(detail.Clients, AuthorizationClientAccess{
			ClientID: client.ID, ClientName: client.Name, Profile: client.ProfileName,
			ViaServers: accessServerNameList(profile.Servers),
		})
	}
	h.renderAuthorizationPage(w, r, http.StatusOK, "authorization-profile-detail.tmpl", profile.Name, &authorizationPage{
		Subpage: "profiles", Detail: detail,
	})
}

func (h *handler) authorizationProfilesAndClients() ([]AccessProfileSummary, []ClientSummary, error) {
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		return nil, nil, err
	}
	clients, err := h.cfg.Access.ListClients()
	if err != nil {
		return nil, nil, err
	}
	active := make([]ClientSummary, 0, len(clients))
	for _, client := range clients {
		if !client.Revoked {
			active = append(active, client)
		}
	}
	return profiles, active, nil
}

func clientsForProfile(clients []ClientSummary, profileID string) []ClientSummary {
	out := make([]ClientSummary, 0)
	for _, client := range clients {
		if client.ProfileID == profileID {
			out = append(out, client)
		}
	}
	return out
}

func accessServerIDs(rows []AccessServerRef) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func checkedServerIDs(rows []AccessGrantChoice) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Checked {
			ids = append(ids, row.ID)
		}
	}
	return ids
}

func (h *handler) handleAuthorizationServerSelection(w http.ResponseWriter, r *http.Request, kind, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "服务器勾选页只接受 GET 请求。")
		return
	}
	resource, servers, profiles, topology, err := h.serverSelectionSources(r, kind, id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取授权资源失败", "读取授权资源失败，请稍后重试。")
		return
	}
	desired := trimmedFormValues(r.URL.Query()["server"])
	if len(desired) == 0 {
		desired = nil
	}
	resource.DesiredServerIDs = desired
	view := BuildServerSelection(resource, servers, profiles, topology)
	view.Query = strings.TrimSpace(r.URL.Query().Get("query"))
	view.SiteFilter = strings.TrimSpace(r.URL.Query().Get("site"))
	view.NetworkFilter = strings.TrimSpace(r.URL.Query().Get("network"))
	view.ProfileFilter = strings.TrimSpace(r.URL.Query().Get("profile"))
	notice := ""
	if r.URL.Query().Get("conflict") == "1" {
		notice = "授权已被其他入口修改，本页已重新读取最新集合，请重新核对。"
	}
	h.renderAuthorizationPage(w, r, http.StatusOK, "server-selection.tmpl", "调整 "+resource.Name, &authorizationPage{
		Subpage: authorizationSubpage(kind), Selection: view, Notice: notice,
	})
}

func authorizationSubpage(kind string) string {
	switch kind {
	case authorizationResourceKindProfile:
		return "profiles"
	case authorizationResourceKindSharedStore:
		return "shares"
	default:
		return "secrets"
	}
}

func (h *handler) serverSelectionSources(
	r *http.Request, kind, id string,
) (ServerSelectionResource, []ServerRow, []AccessProfileSummary, *TopologyData, error) {
	var resource ServerSelectionResource
	servers, err := h.cfg.Servers.Servers(h.requestClientInstance(r))
	if err != nil {
		return resource, nil, nil, nil, err
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		return resource, nil, nil, nil, err
	}
	var topology *TopologyData
	if h.cfg.Topology != nil {
		topology, err = h.cfg.Topology.Topology(h.requestClientInstance(r), "")
		if err != nil {
			return resource, nil, nil, nil, err
		}
	}
	switch kind {
	case authorizationResourceKindProfile:
		profile, err := h.accessProfileByID(id)
		if err != nil {
			return resource, nil, nil, nil, err
		}
		if profile == nil {
			return resource, nil, nil, nil, &SecretNotFoundError{Identifier: id}
		}
		resource = ServerSelectionResource{
			Kind: kind, ID: profile.ID, Name: profile.Name,
			CurrentServerIDs: accessServerIDs(profile.Servers),
			CancelPath:       authorizationProfilesPath + "/" + profile.ID,
		}
	case authorizationResourceKindSecret:
		detail, err := h.secretDetail(id)
		if err != nil {
			return resource, nil, nil, nil, err
		}
		resource = ServerSelectionResource{
			Kind: kind, ID: detail.Row.ID, Name: detail.Row.Name, Note: detail.Row.Note,
			CurrentServerIDs: checkedServerIDs(detail.Servers),
			CancelPath:       authorizationSecretsPath + "/" + detail.Row.ID,
		}
	case authorizationResourceKindSharedStore:
		share, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), id)
		if err != nil {
			return resource, nil, nil, nil, err
		}
		scope, err := h.shareAuthorizationView(share)
		if err != nil {
			return resource, nil, nil, nil, err
		}
		current := make([]string, 0, len(scope.Servers))
		for _, choice := range scope.Servers {
			if choice.Checked {
				current = append(current, choice.ID)
			}
		}
		resource = ServerSelectionResource{
			Kind: kind, ID: share.ID, Name: share.Name, Note: share.Note,
			CurrentServerIDs: current, CancelPath: authorizationSharesPath + "/" + share.ID,
		}
	}
	return resource, servers, profiles, topology, nil
}

func (h *handler) handleAuthorizationSelectionPreview(w http.ResponseWriter, r *http.Request) {
	kind := r.PostFormValue("resource_kind")
	id := r.PostFormValue("resource_id")
	returnTo := safeAuthorizationReturnPath(r.PostFormValue("return_to"), kind)
	desired := trimmedFormValues(r.PostForm["server"])
	switch kind {
	case authorizationResourceKindProfile:
		h.renderProfileSelectionConfirm(w, r, id, desired, returnTo)
	case authorizationResourceKindSecret:
		h.renderSecretSelectionConfirm(w, r, id, desired, returnTo, "", "")
	case authorizationResourceKindSharedStore:
		h.renderSecretSelectionConfirm(w, r, id, desired, returnTo, "share-scope", id)
	default:
		h.renderError(w, r, http.StatusBadRequest, "生成授权确认页失败", "未知授权资源类型。")
	}
}

func safeAuthorizationReturnPath(path, kind string) string {
	prefix := ""
	switch kind {
	case authorizationResourceKindProfile:
		prefix = authorizationProfilesPath
	case authorizationResourceKindSecret:
		prefix = authorizationSecretsPath
	case authorizationResourceKindSharedStore:
		prefix = authorizationSharesPath
	}
	if prefix != "" && strings.HasPrefix(path, prefix) && !strings.HasPrefix(path, "//") {
		return path
	}
	return prefix
}

func (h *handler) renderProfileSelectionConfirm(
	w http.ResponseWriter, r *http.Request, id string, desired []string, returnTo string,
) {
	resource, servers, profiles, _, err := h.serverSelectionSources(r, authorizationResourceKindProfile, id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "生成授权确认页失败", "读取 Profile 失败，请稍后重试。")
		return
	}
	current := resourceRefs(resource.CurrentServerIDs, servers)
	desiredRefs := resourceRefs(desired, servers)
	currentSet := stringSet(resource.CurrentServerIDs)
	added := make([]AccessServerRef, 0, len(desiredRefs))
	for _, server := range desiredRefs {
		if !currentSet[server.ID] {
			added = append(added, server)
		}
	}
	_, removed := serverSelectionDifference(current, desiredRefs)
	confirm := &ServerSelectionConfirm{
		KindLabel: resource.KindLabel(), ResourceID: resource.ID, ResourceName: resource.Name,
		Added: added, Removed: removed, ReturnPath: returnTo,
		ProfileID: resource.ID, ExpectedIDs: resource.CurrentServerIDs, DesiredIDs: desired,
		Clients: h.clientAccessChanges(current, desiredRefs, profiles),
	}
	h.renderAuthorizationPage(w, r, http.StatusOK, "server-selection-confirm.tmpl", "确认授权", &authorizationPage{
		Subpage: "profiles", Confirm: confirm,
	})
}

func (h *handler) renderSecretSelectionConfirm(
	w http.ResponseWriter, r *http.Request, id string, desired []string, returnTo, view, shareID string,
) {
	kind := authorizationResourceKindSecret
	if view == "share-scope" {
		kind = authorizationResourceKindSharedStore
	}
	resource, servers, profiles, _, err := h.serverSelectionSources(r, kind, id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "生成授权确认页失败", "读取授权资源失败，请稍后重试。")
		return
	}
	secretID := resource.ID
	if kind == authorizationResourceKindSharedStore {
		secretID = h.authorizationSecretID(r, id)
	}
	allowed := stringSet(desired)
	target := make([]SecretAuthorizationCell, 0, len(servers))
	for _, server := range servers {
		target = append(target, SecretAuthorizationCell{
			SecretID: secretID, ServerID: server.ID, Allow: allowed[server.ID],
		})
	}
	preview, err := h.cfg.SecretsWriter.PreviewAuthorization(target)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "生成授权预览失败", err.Error())
		return
	}
	current := resourceRefs(resource.CurrentServerIDs, servers)
	desiredRefs := resourceRefs(desired, servers)
	_, removed := serverSelectionDifference(current, desiredRefs)
	confirm := &ServerSelectionConfirm{
		KindLabel: resource.KindLabel(), ResourceID: resource.ID, ResourceName: resource.Name,
		Added: desiredRefs, Removed: removed,
		Clients: h.clientAccessChanges(current, desiredRefs, profiles),
	}
	data := pageData{
		Title: "机密项授权预览", SecretAuthorizationPreview: preview,
		SecretAuthorizationTargets: target, SecretAuthorizationReturnTo: returnTo,
		SecretAuthorizationView: view, SecretAuthorizationShareID: shareID,
		Authorization: &authorizationPage{Subpage: authorizationSubpage(kind), Confirm: confirm},
	}
	if view == "share-scope" {
		display, err := h.authorizationPreviewDisplay(r, preview, view, "", shareID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "生成授权预览失败", "生成授权预览失败，请稍后重试。")
			return
		}
		data.SecretAuthorizationPreview = display.Preview
		data.SecretAuthorizationShareImpact = display.ShareImpactRows
		data.SecretAuthorizationShareUnmounts = h.shareUnmountChoicesForAuthorization(r, returnTo, display.Preview, nil)
	}
	h.renderPage(w, r, http.StatusOK, "secret-authorization-preview.tmpl", data)
}

func (h *handler) authorizationSecretID(r *http.Request, shareID string) string {
	share, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), shareID)
	if err != nil || share == nil {
		return ""
	}
	scope, err := h.shareAuthorizationView(share)
	if err != nil {
		return ""
	}
	return scope.SecretID
}

func (h *handler) clientAccessChanges(current, desired []AccessServerRef, profiles []AccessProfileSummary) []AuthorizationClientChange {
	if h.cfg.Access == nil {
		return nil
	}
	clients, err := h.cfg.Access.ListClients()
	if err != nil {
		return nil
	}
	profileServers := make(map[string][]AccessServerRef, len(profiles))
	for _, profile := range profiles {
		profileServers[profile.ID] = profile.Servers
	}
	currentSet := serverIDSet(current)
	desiredSet := serverIDSet(desired)
	out := make([]AuthorizationClientChange, 0)
	for _, client := range clients {
		if client.Revoked {
			continue
		}
		before := intersectServers(profileServers[client.ProfileID], currentSet)
		after := intersectServers(profileServers[client.ProfileID], desiredSet)
		if len(before) == 0 && len(after) == 0 {
			continue
		}
		out = append(out, AuthorizationClientChange{
			ClientID: client.ID, ClientName: client.Name, Profile: client.ProfileName,
			Before: before, After: after,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientName < out[j].ClientName })
	return out
}

func (h *handler) authorizationClientAccess(servers []AccessServerRef) []AuthorizationClientAccess {
	if h.cfg.Access == nil {
		return nil
	}
	profiles, clients, err := h.authorizationProfilesAndClients()
	if err != nil {
		return nil
	}
	profileByID := make(map[string]AccessProfileSummary, len(profiles))
	for _, profile := range profiles {
		profileByID[profile.ID] = profile
	}
	set := serverIDSet(servers)
	out := make([]AuthorizationClientAccess, 0)
	for _, client := range clients {
		profile, ok := profileByID[client.ProfileID]
		if !ok {
			continue
		}
		via := intersectServers(profile.Servers, set)
		if len(via) == 0 {
			continue
		}
		out = append(out, AuthorizationClientAccess{
			ClientID: client.ID, ClientName: client.Name,
			Profile: client.ProfileName, ViaServers: via,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientName < out[j].ClientName })
	return out
}

func (h *handler) authorizationClientsForServers(servers []AccessServerRef) int {
	return len(h.authorizationClientAccess(servers))
}

func serverIDSet(rows []AccessServerRef) map[string]bool {
	set := make(map[string]bool, len(rows))
	for _, row := range rows {
		set[row.ID] = true
	}
	return set
}

func intersectServers(servers []AccessServerRef, resourceSet map[string]bool) []string {
	out := make([]string, 0, len(servers))
	for _, server := range servers {
		if resourceSet[server.ID] {
			out = append(out, server.Name)
		}
	}
	sort.Strings(out)
	return out
}

func accessServerNameList(servers []AccessServerRef) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	sort.Strings(names)
	return names
}

func (h *handler) missingSecretNoteCount() int {
	if !h.caps.CanManageSecrets {
		return 0
	}
	source, ok := h.cfg.Secrets.(SecretMissingNoteSource)
	if !ok {
		return 0
	}
	rows, err := source.ListSecretsWithoutNote()
	if err != nil {
		return 0
	}
	return len(rows)
}

func (h *handler) missingShareNoteCount() int {
	if !h.caps.CanManageShares {
		return 0
	}
	source, ok := h.cfg.Shares.(ShareMissingNoteSource)
	if !ok {
		return 0
	}
	rows, err := source.ListSharedStoragesWithoutNote()
	if err != nil {
		return 0
	}
	return len(rows)
}

func (h *handler) renderAuthorizationPage(
	w http.ResponseWriter, r *http.Request, status int, page, title string, view *authorizationPage,
) {
	if view != nil {
		view.MissingSecretCount = h.missingSecretNoteCount()
		view.MissingShareCount = h.missingShareNoteCount()
	}
	h.renderPage(w, r, status, page, pageData{Title: title, Authorization: view})
}

// setAuthorizationRefusal 保存删除 Profile 被拒绝的一次性提示；重定向到
// 新列表后由 takeAuthorizationRefusal 取出显示。
func (h *handler) setAuthorizationRefusal(r *http.Request, refusal *AccessProfileInUse) {
	if sess := h.currentSession(r); sess != nil {
		h.mu.Lock()
		sess.authorizationRefusal = refusal
		h.mu.Unlock()
	}
}

func (h *handler) takeAuthorizationRefusal(r *http.Request) *AccessProfileInUse {
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	refusal := sess.authorizationRefusal
	sess.authorizationRefusal = nil
	h.mu.Unlock()
	return refusal
}
