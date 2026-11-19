package webui

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AccessProfileSummary 是 Profile 页的一行展示数据：profile 本身、授权服
// 务器，以及引用它的项目令牌与设备码。
type AccessProfileSummary struct {
	ID          string
	Name        string
	Servers     []AccessServerRef
	Projects    []AccessReference
	DeviceCodes []AccessReference
}

// AccessServerRef 是访问管理结果里的一台服务器：标识符加名字。
type AccessServerRef struct {
	ID   string
	Name string
}

// AccessReference 是引用某个 profile 的项目令牌或设备码。
type AccessReference struct {
	Name   string
	Status string
}

// ClientSummary 是客户端列表的一行。设备码本体绝不出现；令牌前缀只在详
// 情页作为凭据摘要展示。
type ClientSummary struct {
	ID            string
	Name          string
	ProfileID     string
	ProfileName   string
	ServerCount   int
	LastPullAt    time.Time
	ClientVersion string
	// RecommendedVersion 是权威端推荐客户端使用的版本。
	RecommendedVersion string
	MachineName        string
	// Paired 表示实例来自配对流程；否则是 owner 手动签发。
	Paired bool
	// Revoked 表示客户端已移除，只在历史数据需要回查时由来源给出。
	Revoked bool
	// Stale 与 UpgradeAvailable 由页面按当前时间和推荐版本计算。
	Stale            bool
	UpgradeAvailable bool
}

// ClientMachine 是客户端所在机器的展示数据。
type ClientMachine struct {
	ID       string
	Name     string
	SitePath string
}

// ClientSoftwareSummary 是客户端所在机器的软件清单摘要。
type ClientSoftwareSummary struct {
	LastCollectedAt      time.Time
	ItemCount            int
	PendingExposureCount int
	TopItems             []string
}

// ClientResource 是客户端经 profile 服务器能拿到的一项资源。
type ClientResource struct {
	ID         string
	Name       string
	Note       string
	ViaServers []string
}

// ClientDetail 是一个客户端实例的详情。
type ClientDetail struct {
	ClientSummary
	ProfileID          string
	RecommendedVersion string
	TokenPrefix        string
	Servers            []AccessServerRef
	Machine            *ClientMachine
	Software           *ClientSoftwareSummary
	Secrets            []ClientResource
	Shares             []ClientResource
}

type ClientProfilePreview struct {
	ClientName         string
	CurrentProfileID   string
	CurrentProfileName string
	DesiredProfileID   string
	DesiredProfileName string
	AddedServers       []AccessServerRef
	LostServers        []AccessServerRef
	AddedSecrets       []ClientResource
	LostSecrets        []ClientResource
	AddedShares        []ClientResource
	LostShares         []ClientResource
}

type ClientMachineChoice struct {
	ID       string
	Name     string
	SitePath string
	// Available 为 false 表示这台机器已经关联另一个客户端实例。
	Available bool
}

// AccessProfileInUse 是删除 profile 被拒绝时的引用者清单。
type AccessProfileInUse struct {
	ProfileName string
	Projects    []string
	DeviceCodes []string
}

// AccessGrantChoice 是授权表单里的一台可选服务器。
type AccessGrantChoice struct {
	ID      string
	Name    string
	Checked bool
}

// AccessGrantView 是授权表单的渲染数据。
type AccessGrantView struct {
	ProfileID   string
	ProfileName string
	Servers     []AccessGrantChoice
	ExpectedIDs []string
	// NeedsReverify 表示权威端会话超过 5 分钟没有验证管理员口令。
	NeedsReverify bool
	// ReverifyPath 是保留当前勾选状态后返回本表单的重新验证地址。
	ReverifyPath string
}

// AccessGrantChange 是按完整集合调整授权后的差异结果。
type AccessGrantChange struct {
	Added   []AccessServerRef
	Removed []AccessServerRef
}

// AccessProfileRef 是反向授权结果里对一个 profile 的引用。
type AccessProfileRef struct {
	ID   string
	Name string
}

// ServerProfileAuthorizationPreview 是服务器详情页 profile 授权组的差异。
type ServerProfileAuthorizationPreview struct {
	ServerID   string
	ServerName string
	Current    []AccessProfileRef
	Desired    []AccessProfileRef
	Added      []AccessProfileRef
	Removed    []AccessProfileRef
	Summary    string
}

// ServerProfileAuthorizationApplyResult 表示一次服务器详情页 profile 授权确
// 认的结果。
type ServerProfileAuthorizationApplyResult struct {
	Preview ServerProfileAuthorizationPreview
	Applied bool
}

// ServerProfileAuthorizationConflict 表示预览之后相关授权被其他入口修改。
type ServerProfileAuthorizationConflict struct {
	Latest ServerProfileAuthorizationPreview
}

// AccessGrantConflict 表示授权表单打开后，别的入口已经改过授权范围。
type AccessGrantConflict struct {
	ProfileName string
	Current     []AccessServerRef
}

// AccessFlash 是一次写操作成功后只显示一次的页面提示。
type AccessFlash struct {
	ProfileName string
	Added       []AccessServerRef
	Removed     []AccessServerRef
}

// AddedNameList 返回新增授权服务器名字清单，模板直接展示。
func (f *AccessFlash) AddedNameList() string {
	return accessServerNames(f.Added)
}

// RemovedNameList 返回撤销授权服务器名字清单，模板直接展示。
func (f *AccessFlash) RemovedNameList() string {
	return accessServerNames(f.Removed)
}

func accessServerNames(servers []AccessServerRef) string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	return strings.Join(names, ",")
}

// AccessStore 是 Web UI 读取和修改访问管理的唯一入口。单机与权威端由
// 访问管理模块适配实现；处理器测试用桩，本包不知道数据库结构。配对与
// 设备码部分只在权威端注册路由，其余角色不需要实现。
type AccessStore interface {
	ListProfiles() ([]AccessProfileSummary, error)
	AddProfile(name string, source AccessWriteSource) error
	DeleteProfile(id string, source AccessWriteSource) (*AccessProfileInUse, error)
	SetServerGrants(profileID string, expected, desired []string, source AccessWriteSource) (AccessGrantChange, *AccessGrantConflict, error)
	PreviewServerProfileAuthorization(serverID string, desired []string) (ServerProfileAuthorizationPreview, error)
	ApplyServerProfileAuthorization(serverID string, desired []string, summary string, source AccessWriteSource) (ServerProfileAuthorizationApplyResult, *ServerProfileAuthorizationConflict, error)
	ListProjects(includeRevoked bool) ([]AccessProjectSummary, error)
	AddProject(name, profileID string, source AccessWriteSource) (AccessProjectToken, error)
	RotateProject(id string, source AccessWriteSource) (AccessProjectToken, error)
	DisableProject(id string, source AccessWriteSource) error
	EnableProject(id string, source AccessWriteSource) error
	RevokeProject(id string, source AccessWriteSource) error
	DeleteProject(id string, source AccessWriteSource) error
	AddDeviceCode(name, profile, serveURL string, source AccessWriteSource) (AccessDeviceCodeResult, error)
	ListClients() ([]ClientSummary, error)
	ClientDetail(id string) (*ClientDetail, error)
	PreviewClientProfileChange(id, profileID string) (*ClientProfilePreview, error)
	ChangeClientProfile(id, expectedProfileID, desiredProfileID string, source AccessWriteSource) error
	ListClientMachineChoices(id string) ([]ClientMachineChoice, error)
	LinkClientMachine(id, machineID string, source AccessWriteSource) error
	CreateAndLinkClientMachine(id, machineName, sitePath string, source AccessWriteSource) error
	RemoveClient(id string, source AccessWriteSource) error
	ListPairingRequests() ([]AccessPairingSummary, error)
	ApprovePairing(id, profile string, compared bool, allowForeignTarget bool, source AccessWriteSource) error
	RejectPairing(id string, source AccessWriteSource) error
}

// AccessWriteSource 描述一次 Web UI 写请求的来源；访问管理适配器把它换算
// 成审计行里的 Web UI 来源和来源地址。
type AccessWriteSource struct {
	RemoteAddress string
}

const (
	// accessRootPath 是「访问」区域的入口；客户端只有这里的提示页。
	accessRootPath              = "/ui/access/"
	accessProfilesPath          = "/ui/access/profiles"
	accessProfileDeletePath     = "/ui/access/profiles/delete"
	accessProfilesPrefix        = "/ui/access/profiles/"
	serverProfilePreviewPath    = "/ui/servers/profile-authorization/preview"
	serverProfileConfirmPath    = "/ui/servers/profile-authorization/confirm"
	accessDeviceCodesPath       = "/ui/access/device-codes"
	clientPairingListPath       = "/ui/clients/pairing/list"
	clientPairingPrefix         = "/ui/clients/pairing/"
	legacyAccessPairingPath     = "/ui/access/pairing"
	legacyAccessPairingListPath = "/ui/access/pairing/list"
	legacyAccessPairingPrefix   = "/ui/access/pairing/"
)

// registerAccessRoutes 按本机角色注册访问区域路由。客户端只保留区域提示
// 页；后续配对、设备码、项目令牌页面在各自票据里接入这里的角色分支。
func (h *handler) registerAccessRoutes() {
	h.mux.HandleFunc(accessRootPath, h.requireSession(h.handleAccessRoot))
	if h.cfg.Role == RoleClient || h.cfg.Access == nil {
		return
	}
	h.mux.HandleFunc(accessProfilesPath, h.requireSession(h.handleAccessProfiles))
	h.mux.HandleFunc(accessProfileDeletePath, h.requireSession(h.handleAccessProfileDelete))
	h.mux.HandleFunc(accessProfilesPrefix, h.requireSession(h.handleAccessProfileGrants))
	h.mux.HandleFunc(serverProfilePreviewPath, h.requireSession(h.handleServerProfileAuthorizationPreview))
	h.mux.HandleFunc(serverProfileConfirmPath, h.requireSession(h.handleServerProfileAuthorizationConfirm))
	h.mux.HandleFunc(accessProjectsPath, h.requireSession(h.handleAccessProjects))
	for old, target := range map[string]string{
		"/ui/access/projects":         accessProjectsPath,
		"/ui/access/projects/rotate":  projectRotatePath,
		"/ui/access/projects/disable": projectDisablePath,
		"/ui/access/projects/enable":  projectEnablePath,
		"/ui/access/projects/revoke":  projectRevokePath,
		"/ui/access/projects/delete":  projectDeletePath,
	} {
		target := target
		h.mux.HandleFunc(old, h.requireSession(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
		}))
	}
	h.mux.HandleFunc(projectRotatePath, h.requireSession(h.handleAccessProjectRotate))
	h.mux.HandleFunc(projectDisablePath, h.requireSession(h.handleAccessProjectDisable))
	h.mux.HandleFunc(projectEnablePath, h.requireSession(h.handleAccessProjectEnable))
	h.mux.HandleFunc(projectRevokePath, h.requireSession(h.handleAccessProjectRevoke))
	h.mux.HandleFunc(projectDeletePath, h.requireSession(h.handleAccessProjectDelete))
	if h.cfg.Role == RoleServer {
		h.registerClientRoutes()
		h.registerPairingRoutes()
		h.registerDeviceCodeRoutes()
	}
}

func (h *handler) handleAccessRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != accessRootPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if h.cfg.Role != RoleClient {
		http.Redirect(w, r, accessProfilesPath, http.StatusSeeOther)
		return
	}
	h.renderPage(w, r, http.StatusOK, "access-notice.tmpl", pageData{Title: "访问"})
}

func (h *handler) handleAccessProfiles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != accessProfilesPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleAccessProfileAdd(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "Profile 页只接受 GET 与 POST 请求。")
		return
	}
	// 旧地址保持可用：列表读取一律进入新的授权子页。
	http.Redirect(w, r, authorizationProfilesPath, http.StatusSeeOther)
}

func (h *handler) handleAccessProfileAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		h.renderError(w, r, http.StatusBadRequest, "新增 profile 失败", "profile 名称不能为空。")
		return
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath(authorizationProfilesPath), http.StatusSeeOther)
		return
	}
	if err := h.cfg.Access.AddProfile(name, AccessWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "新增 profile 失败", "新增 profile 失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, authorizationProfilesPath, http.StatusSeeOther)
}

func (h *handler) handleAccessProfileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "删除 profile 只接受 POST 请求。")
		return
	}
	id := r.PostFormValue("profile_id")
	if id == "" {
		h.renderError(w, r, http.StatusBadRequest, "删除 profile 失败", "缺少 profile 标识符。")
		return
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		next := url.Values{"reverify_delete": {id}}
		http.Redirect(w, r, reverifyPath(authorizationProfilesPath+"?"+next.Encode()), http.StatusSeeOther)
		return
	}
	inUse, err := h.cfg.Access.DeleteProfile(id, AccessWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "删除 profile 失败", "删除 profile 失败，请稍后重试。")
		return
	}
	if inUse != nil {
		h.setAuthorizationRefusal(r, inUse)
		http.Redirect(w, r, authorizationProfilesPath, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, authorizationProfilesPath, http.StatusSeeOther)
}

func (h *handler) handleAccessProfileGrants(w http.ResponseWriter, r *http.Request) {
	profileID, action, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, accessProfilesPrefix), "/")
	if !ok || action != "grants" || profileID == "" {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleAccessProfileGrantPost(w, r, profileID)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "授权表单只接受 GET 与 POST 请求。")
		return
	}
	// 旧授权表单地址保持可用：读取一律进入新的服务器勾选组件，并保留
	// 重新验证跳转带回的候选集合。
	query := url.Values{}
	for _, id := range r.URL.Query()["server"] {
		if id != "" {
			query.Add("server", id)
		}
	}
	target := authorizationProfilesPath + "/" + profileID + authorizationServersSuffix
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *handler) handleAccessProfileGrantPost(w http.ResponseWriter, r *http.Request, profileID string) {
	profile, err := h.accessProfileByID(profileID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 profile 失败", "读取 profile 失败，请稍后重试。")
		return
	}
	if profile == nil {
		h.renderError(w, r, http.StatusNotFound, "profile 不存在", "请求的 profile 不存在。")
		return
	}
	expected := r.PostForm["expected"]
	desired := r.PostForm["server"]
	mode := serverSelectionMode(r.PostFormValue("mode"))
	desired = mergeServerSelectionDesired(accessServerIDs(profile.Servers), desired, mode)
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath(grantResumePath(
			profileID, desired, mode, r.PostFormValue("return_to"),
		)), http.StatusSeeOther)
		return
	}
	change, conflict, err := h.cfg.Access.SetServerGrants(
		profileID, expected, desired,
		AccessWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "保存授权失败", "保存授权失败，请稍后重试。")
		return
	}
	if conflict != nil {
		http.Redirect(w, r,
			authorizationProfilesPath+"/"+profileID+authorizationServersSuffix+"?conflict=1",
			http.StatusSeeOther)
		return
	}
	h.setAccessFlash(r, &AccessFlash{
		ProfileName: profile.Name, Added: change.Added, Removed: change.Removed,
	})
	http.Redirect(w, r, authorizationReturnPath(r.PostFormValue("return_to"), authorizationResourceKindProfile), http.StatusSeeOther)
}

func grantAddsServer(expected, desired []string) bool {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		expectedSet[id] = struct{}{}
	}
	for _, id := range desired {
		if _, ok := expectedSet[id]; !ok {
			return true
		}
	}
	return false
}

func grantResumePath(profileID string, desired []string, mode, returnTo string) string {
	query := url.Values{}
	for _, id := range desired {
		query.Add("server", id)
	}
	if mode != "" {
		query.Set("mode", mode)
	}
	if returnTo != "" {
		query.Set("return_to", returnTo)
	}
	return authorizationProfilesPath + "/" + profileID + authorizationServersSuffix + "?" + query.Encode()
}

func reverifyPath(next string) string {
	return "/ui/reverify?next=" + url.QueryEscape(next)
}

func selectedGrantIDs(view *AccessGrantView) []string {
	ids := make([]string, 0, len(view.Servers))
	for _, server := range view.Servers {
		if server.Checked {
			ids = append(ids, server.ID)
		}
	}
	return ids
}

func (h *handler) applyDesiredGrantSelection(r *http.Request, view *AccessGrantView) {
	selected := r.URL.Query()["server"]
	if selected == nil {
		return
	}
	checked := make(map[string]bool, len(selected))
	for _, id := range selected {
		checked[id] = true
	}
	for i := range view.Servers {
		view.Servers[i].Checked = checked[view.Servers[i].ID]
	}
}

func (h *handler) setAccessFlash(r *http.Request, flash *AccessFlash) {
	if sess := h.currentSession(r); sess != nil {
		h.mu.Lock()
		sess.accessFlash = flash
		h.mu.Unlock()
	}
}

func (h *handler) takeAccessFlash(r *http.Request) *AccessFlash {
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	flash := sess.accessFlash
	sess.accessFlash = nil
	return flash
}

func (h *handler) renderGrantConflict(w http.ResponseWriter, r *http.Request, status int, profileID string, conflict *AccessGrantConflict) {
	servers, err := h.cfg.Servers.Servers("")
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "授权冲突后读取服务器列表失败，请稍后重试。")
		return
	}
	view := accessGrantView(AccessProfileSummary{
		ID: profileID, Name: conflict.ProfileName, Servers: conflict.Current,
	}, servers)
	h.renderPage(w, r, status, "profile-grants.tmpl", pageData{
		Title: "调整 profile 授权", AccessGrant: view,
		AccessGrantNotice: "授权范围已被其他入口修改，已按最新授权范围重新勾选；请确认后重新提交。",
	})
}

func (h *handler) accessProfileByID(id string) (*AccessProfileSummary, error) {
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		return nil, err
	}
	for i := range profiles {
		if profiles[i].ID == id {
			return &profiles[i], nil
		}
	}
	return nil, nil
}

func accessGrantView(profile AccessProfileSummary, servers []ServerRow) *AccessGrantView {
	current := make(map[string]bool, len(profile.Servers))
	expected := make([]string, 0, len(profile.Servers))
	for _, server := range profile.Servers {
		current[server.ID] = true
		expected = append(expected, server.ID)
	}
	sorted := append([]ServerRow(nil), servers...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	view := &AccessGrantView{
		ProfileID: profile.ID, ProfileName: profile.Name,
		Servers: make([]AccessGrantChoice, 0, len(sorted)), ExpectedIDs: expected,
	}
	for _, server := range sorted {
		view.Servers = append(view.Servers, AccessGrantChoice{
			ID: server.ID, Name: server.Name, Checked: current[server.ID],
		})
	}
	return view
}
