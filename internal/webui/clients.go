package webui

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"ssh-manager-mcp/internal/updater"
)

const (
	clientsPath       = "/ui/clients/"
	clientPairingPath = "/ui/clients/pairing"
)

// LastPullText 返回设备码最近拉取时间的展示文字；从未同步过时明确显示
// 「从未同步」。客户端实例页与采集轮次进度页共用这一口径。
func LastPullText(lastPullAt time.Time) string {
	if lastPullAt.IsZero() {
		return "从未同步"
	}
	return lastPullAt.Format("2006-01-02 15:04:05")
}

// LastPullText 返回最后同步时间；从未同步过时明确显示「从未同步」。
func (s ClientSummary) LastPullText() string {
	return LastPullText(s.LastPullAt)
}

// SourceText 区分经配对入网与 owner 手动签发的客户端实例。
func (s ClientSummary) SourceText() string {
	if s.Paired {
		return "经配对接入"
	}
	return "手动签发"
}

// MachineText 返回所在机器；未关联时用统一文案提示。
func (s ClientSummary) MachineText() string {
	if s.MachineName == "" {
		return "机器未关联"
	}
	return "机器 " + s.MachineName
}

// VersionComparisonText 比较客户端声明版本与权威端推荐版本。版本无法解
// 析时保持原文，不做臆测。
func (d ClientDetail) VersionComparisonText() string {
	if d.RecommendedVersion == "" {
		return "推荐版本未知"
	}
	if d.ClientVersion == "" {
		return "未声明版本，可升级"
	}
	client, clientErr := updater.ParseVersion(d.ClientVersion)
	recommended, recommendedErr := updater.ParseVersion(d.RecommendedVersion)
	if clientErr != nil || recommendedErr != nil {
		return "版本 " + d.ClientVersion + "，推荐版本 " + d.RecommendedVersion
	}
	if updater.CompareVersions(client, recommended) < 0 {
		return "低于推荐版本，可升级"
	}
	return "与推荐版本一致"
}

func (p ClientProfilePreview) AddedServerNames() string { return clientServerNameList(p.AddedServers) }
func (p ClientProfilePreview) LostServerNames() string  { return clientServerNameList(p.LostServers) }
func (p ClientProfilePreview) AddedSecretNames() string {
	return clientResourceNameList(p.AddedSecrets)
}
func (p ClientProfilePreview) LostSecretNames() string { return clientResourceNameList(p.LostSecrets) }
func (p ClientProfilePreview) AddedShareNames() string { return clientResourceNameList(p.AddedShares) }
func (p ClientProfilePreview) LostShareNames() string  { return clientResourceNameList(p.LostShares) }

func clientServerNameList(rows []AccessServerRef) string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return strings.Join(names, "、")
}

func clientResourceNameList(rows []ClientResource) string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return strings.Join(names, "、")
}

func (h *handler) registerClientRoutes() {
	h.mux.HandleFunc(clientsPath, h.requireSession(h.handleClients))
}

func (h *handler) handleClients(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != clientsPath {
		identifier, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, clientsPath), "/")
		switch action {
		case "":
			h.handleClientDetail(w, r, identifier)
		case "profile":
			h.handleClientProfile(w, r, identifier)
		case "machine":
			h.handleClientMachine(w, r, identifier)
		case "remove":
			h.handleClientRemove(w, r, identifier)
		default:
			h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		}
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "客户端列表只接受 GET 与 HEAD 请求。")
		return
	}
	clients, err := h.cfg.Access.ListClients()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取客户端列表失败", "读取客户端列表失败，请稍后重试。")
		return
	}
	pairings, err := h.cfg.Access.ListPairingRequests()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "读取配对请求失败，请稍后重试。")
		return
	}
	clients = activeClients(clients)
	now := h.cfg.now()
	for i := range clients {
		clients[i].Stale = clientStale(clients[i], now)
		clients[i].UpgradeAvailable = clientUpgradeAvailable(clients[i])
	}
	pending := 0
	for _, pairing := range pairings {
		if pairing.State == "pending" {
			pending++
		}
	}
	h.renderPage(w, r, http.StatusOK, "clients.tmpl", pageData{
		Title: "客户端", Clients: clients, ClientPendingPairingCount: pending,
	})
}

func (h *handler) handleClientDetail(w http.ResponseWriter, r *http.Request, identifier string) {
	if identifier == "" || strings.Contains(identifier, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "客户端详情只接受 GET 与 HEAD 请求。")
		return
	}
	detail, err := h.cfg.Access.ClientDetail(identifier)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取客户端详情失败", "读取客户端详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "客户端不存在", "这个客户端实例不存在或已移除。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "client-detail.tmpl", pageData{Title: detail.Name + " · 客户端", Client: detail})
}

func (h *handler) handleClientProfile(w http.ResponseWriter, r *http.Request, identifier string) {
	if identifier == "" || strings.Contains(identifier, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleClientProfileConfirm(w, r, identifier)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "更换 profile 页只接受 GET、HEAD 与 POST 请求。")
		return
	}
	detail, err := h.cfg.Access.ClientDetail(identifier)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取客户端详情失败", "读取客户端详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "客户端不存在", "这个客户端实例不存在或已移除。")
		return
	}
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 profile 列表失败", "读取 profile 列表失败，请稍后重试。")
		return
	}
	data := pageData{Title: "更换 profile", AccessProfiles: profiles, Client: detail}
	if desired := r.URL.Query().Get("profile"); desired != "" && desired != detail.ProfileID {
		preview, err := h.cfg.Access.PreviewClientProfileChange(identifier, desired)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "生成更换预览失败", "生成更换预览失败，请稍后重试。")
			return
		}
		data.ClientProfilePreview = preview
	}
	h.renderPage(w, r, http.StatusOK, "client-profile.tmpl", data)
}

func (h *handler) handleClientProfileConfirm(w http.ResponseWriter, r *http.Request, identifier string) {
	desired := r.PostFormValue("profile")
	expected := r.PostFormValue("expected_profile")
	if desired == "" || expected == "" {
		h.renderError(w, r, http.StatusBadRequest, "更换 profile 失败", "请先选择一个新的 profile。")
		return
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		resume := url.Values{"profile": {desired}, "expected_profile": {expected}}
		http.Redirect(w, r, reverifyPath("/ui/clients/"+identifier+"/profile?"+resume.Encode()), http.StatusSeeOther)
		return
	}
	if err := h.cfg.Access.ChangeClientProfile(
		identifier, expected, desired, AccessWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "更换 profile 失败", "更换 profile 失败，请重新打开预览后再试。")
		return
	}
	http.Redirect(w, r, "/ui/clients/"+identifier+"?notice=profile", http.StatusSeeOther)
}

func (h *handler) handleClientMachine(w http.ResponseWriter, r *http.Request, identifier string) {
	if identifier == "" || strings.Contains(identifier, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleClientMachineConfirm(w, r, identifier)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "选择机器页只接受 GET、HEAD 与 POST 请求。")
		return
	}
	detail, err := h.cfg.Access.ClientDetail(identifier)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取客户端详情失败", "读取客户端详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "客户端不存在", "这个客户端实例不存在或已移除。")
		return
	}
	choices, err := h.cfg.Access.ListClientMachineChoices(identifier)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取机器列表失败", "读取机器列表失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "client-machine.tmpl", pageData{
		Title: "选择机器", Client: detail, ClientMachineChoices: choices,
	})
}

func (h *handler) handleClientMachineConfirm(w http.ResponseWriter, r *http.Request, identifier string) {
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath("/ui/clients/"+identifier+"/machine"), http.StatusSeeOther)
		return
	}
	source := AccessWriteSource{RemoteAddress: remoteIP(r)}
	machineID := r.PostFormValue("machine_id")
	newMachine := strings.TrimSpace(r.PostFormValue("new_machine"))
	switch {
	case machineID != "":
		if err := h.cfg.Access.LinkClientMachine(identifier, machineID, source); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "关联机器失败", "关联机器失败，请确认这台机器还没有关联其他客户端。")
			return
		}
	case newMachine != "":
		if err := h.cfg.Access.CreateAndLinkClientMachine(identifier, newMachine, r.PostFormValue("new_site"), source); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "新建并关联机器失败", "新建并关联机器失败，请检查机器名与地点。")
			return
		}
	default:
		h.renderError(w, r, http.StatusBadRequest, "关联机器失败", "请选择一台机器，或填写要新建的机器名。")
		return
	}
	http.Redirect(w, r, "/ui/clients/"+identifier+"?notice=machine", http.StatusSeeOther)
}

func (h *handler) handleClientRemove(w http.ResponseWriter, r *http.Request, identifier string) {
	if identifier == "" || strings.Contains(identifier, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		if r.PostFormValue("confirm") != "1" {
			h.renderError(w, r, http.StatusBadRequest, "移除客户端失败", "请先勾选确认移除。")
			return
		}
		if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
			http.Redirect(w, r, reverifyPath("/ui/clients/"+identifier+"/remove"), http.StatusSeeOther)
			return
		}
		if err := h.cfg.Access.RemoveClient(identifier, AccessWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "移除客户端失败", "移除客户端失败，请稍后重试。")
			return
		}
		http.Redirect(w, r, clientsPath+"?notice=removed", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "移除客户端页只接受 GET、HEAD 与 POST 请求。")
		return
	}
	detail, err := h.cfg.Access.ClientDetail(identifier)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取客户端详情失败", "读取客户端详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "客户端不存在", "这个客户端实例不存在或已移除。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "client-remove.tmpl", pageData{Title: "移除客户端", Client: detail})
}

// activeClients 只把仍在使用的客户端实例作为管理对象；已移除实例的历史
// 记录留在审计里，不再混入列表。
func activeClients(rows []ClientSummary) []ClientSummary {
	out := make([]ClientSummary, 0, len(rows))
	for _, row := range rows {
		if !row.Revoked {
			out = append(out, row)
		}
	}
	return out
}

// clientStale 判断客户端是否超过一天没有同步。
func clientStale(client ClientSummary, now time.Time) bool {
	return client.LastPullAt.IsZero() || client.LastPullAt.Before(now.Add(-24*time.Hour))
}

func clientUpgradeAvailable(client ClientSummary) bool {
	if client.RecommendedVersion == "" {
		return false
	}
	if client.ClientVersion == "" {
		return true
	}
	current, currentErr := updater.ParseVersion(client.ClientVersion)
	recommended, recommendedErr := updater.ParseVersion(client.RecommendedVersion)
	return currentErr == nil && recommendedErr == nil && updater.CompareVersions(current, recommended) < 0
}
