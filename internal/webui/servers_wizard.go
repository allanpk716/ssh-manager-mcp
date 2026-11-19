package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type serverWizardMachineAddress struct {
	Address    string
	DeviceName string
}

type serverWizardSiteNode struct {
	ID       string
	Name     string
	Path     string
	Depth    int
	Selected bool
}

type serverWizardProfile struct {
	AccessProfileSummary
	ClientIDs []string
	Checked   bool
}

type serverWizardView struct {
	Notice           string
	Form             *ServerAddForm
	Networks         []TopologyNetwork
	MachineAddresses []serverWizardMachineAddress
	SiteNodes        []serverWizardSiteNode
	Profiles         []serverWizardProfile
	Detail           *ServerDetailView
	ConnectionTest   *ServerConnectionTestStatus
	TestNotice       string
	TestPollPath     string
	NextPath         string
	ServerID         string
	TaskID           string
	SameMachine      bool
	SiteAction       string
	SiteID           string
	NewSiteParent    string
	NewSiteName      string
	NewSiteNote      string
	ReturnTo         string
	NeedsReverify    bool
	ReverifyPath     string
}

func (h *handler) handleServerWizardStartGet(w http.ResponseWriter, r *http.Request) {
	if sess := h.currentSession(r); sess != nil {
		h.setServerAddDraft(sess, nil)
	}
	form := defaultServerAddForm()
	form.AddressNetworks = []string{"other"}
	form.AddressValues = []string{""}
	var topology *TopologyData
	if h.cfg.Topology != nil {
		var err error
		if topology, err = h.cfg.Topology.Topology(h.requestClientInstance(r), ""); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取新增选项失败", "读取地点与网络失败，请稍后重试。")
			return
		}
	}
	if title, message := applyServerWizardContext(form, r.URL.Query(), topology); title != "" {
		h.renderError(w, r, http.StatusNotFound, title, message)
		return
	}
	h.renderServerWizardStart(w, r, form, "", http.StatusOK, topology)
}

// applyServerWizardContext 把拓扑页带来的预选参数写进第 1 步表单；网络或
// 设备不存在时返回 404 文案，避免静默丢弃上下文。
func applyServerWizardContext(form *ServerAddForm, query url.Values, topology *TopologyData) (string, string) {
	form.ReturnTo = normalizeAddReturnPath(query.Get("return_to"))
	if network := strings.TrimSpace(query.Get("network")); network != "" {
		found := false
		if topology != nil {
			for _, item := range topology.Networks {
				if item.Name == network {
					found = true
					break
				}
			}
		}
		if !found {
			return "网络不存在", "选择的物理局域网或虚拟局域网不存在，请回到网络拓扑后重新进入。"
		}
		form.AddressNetworks = []string{network}
		form.AddressValues = []string{""}
	}
	if device := strings.TrimSpace(query.Get("device")); device != "" {
		found := false
		if topology != nil {
			for _, item := range topology.Devices {
				if item.Name == device {
					form.DeviceChoice = "existing"
					form.ExistingDeviceName = device
					form.AddressNetworks = []string{"other"}
					form.AddressValues = []string{""}
					found = true
					break
				}
			}
		}
		if !found {
			return "设备不存在", "选择的设备不存在，请回到网络拓扑后重新进入。"
		}
	}
	return "", ""
}

func (h *handler) renderServerWizardStart(
	w http.ResponseWriter, r *http.Request, form *ServerAddForm, notice string, status int, topology *TopologyData,
) {
	view := &serverWizardView{Form: form, ReturnTo: form.ReturnTo}
	if topology != nil {
		view.Networks = topology.Networks
		view.MachineAddresses = wizardMachineAddresses(topology)
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		view.NeedsReverify = true
		view.ReverifyPath = reverifyPath(serverAddFormPath + "?resume=1")
	}
	view.Notice = notice
	h.renderPage(w, r, status, "servers-wizard.tmpl", pageData{Title: "新增服务器 · 连接与地址", ServerWizard: view})
}

func wizardMachineAddresses(topology *TopologyData) []serverWizardMachineAddress {
	devices := map[string]string{}
	for _, device := range topology.Devices {
		devices[device.ID] = device.Name
	}
	out := make([]serverWizardMachineAddress, 0, len(topology.Memberships))
	for _, membership := range topology.Memberships {
		if membership.Address == "" {
			continue
		}
		out = append(out, serverWizardMachineAddress{
			Address: membership.Address, DeviceName: devices[membership.DeviceID],
		})
	}
	return out
}

func (h *handler) handleServerWizardStartPost(w http.ResponseWriter, r *http.Request) {
	form := h.parseServerAddForm(r)
	form.AddressNetworks = trimmedValues(r.PostForm["address_network"])
	form.AddressValues = trimmedValues(r.PostForm["address"])
	var fieldErrors []string
	if r.PostFormValue("resume_credentials") == "1" {
		draft := h.takeServerAddDraft(r)
		if draft == nil {
			fieldErrors = append(fieldErrors, "暂存的登录凭据已超过五分钟，请重新填写。")
		} else {
			form = resumeServerAddCredentialFields(form, *draft)
		}
	}
	for index, network := range form.AddressNetworks {
		if network != "other" || index >= len(form.AddressValues) {
			continue
		}
		form.Host = form.AddressValues[index]
	}
	if (r.PostFormValue("same_machine") == "1" || form.DeviceChoice == "existing") && form.ExistingDeviceName != "" {
		form.DeviceChoice = "existing"
	}
	var topology *TopologyData
	if h.cfg.Topology != nil {
		var err error
		if topology, err = h.cfg.Topology.Topology(h.requestClientInstance(r), ""); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取新增选项失败", "读取地点与网络失败，请稍后重试。")
			return
		}
	}
	if r.PostFormValue("add_address") == "1" {
		if serverAddFormWritesCredential(form) {
			if sess := h.currentSession(r); sess != nil {
				h.setServerAddDraft(sess, &form)
			}
			form.StagedCredential = true
		} else {
			form.StagedCredential = false
		}
		form.AddressNetworks = append(form.AddressNetworks, "other")
		form.AddressValues = append(form.AddressValues, "")
		h.renderServerWizardStart(w, r, &form, "已追加一个地址行，请继续填写。", http.StatusOK, topology)
		return
	}
	if r.PostFormValue("clear_staged_credentials") == "1" {
		if sess := h.currentSession(r); sess != nil {
			h.setServerAddDraft(sess, nil)
		}
		clearServerAddCredentialFields(&form)
		form.StagedCredential = false
		h.renderServerWizardStart(w, r, &form, "已清除暂存的登录凭据，请重新填写后再保存。", http.StatusOK, topology)
		return
	}
	fieldErrors = append(fieldErrors, serverWizardStartErrors(form)...)
	if len(fieldErrors) > 0 {
		h.renderServerWizardStart(w, r, &form, strings.Join(fieldErrors, " "), http.StatusBadRequest, topology)
		return
	}
	if serverAddFormWritesCredential(form) && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		if sess := h.currentSession(r); sess != nil {
			h.setServerAddDraft(sess, &form)
		}
		http.Redirect(w, r, reverifyPath(serverAddFormPath+"?resume=1"), http.StatusSeeOther)
		return
	}
	writeForm := form
	writeForm.AddressNetworks, writeForm.AddressValues = registeredWizardAddresses(form)
	result, err := h.cfg.ServerWriter.AddServer(writeForm, ServerWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		if isServerFormError(err) {
			h.renderServerWizardStart(w, r, &form, err.Error(), http.StatusBadRequest, topology)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "新增服务器失败", "新增服务器失败，请稍后重试。")
		return
	}
	if sess := h.currentSession(r); sess != nil {
		h.setServerAddDraft(sess, nil)
	}
	taskID := ""
	if h.cfg.ServerConnectionTester != nil {
		taskID, _ = h.cfg.ServerConnectionTester.StartServerTest(
			result.Detail.ID, form.Shell == "" || form.Shell == "auto",
			ServerWriteSource{RemoteAddress: remoteIP(r)},
			ServerConnectionAnchor{
				Copied: result.AnchorCopied, FromServerName: result.AnchorCopyFromServerName,
				Conflict: result.AnchorCopyConflict,
			},
		)
	}
	query := url.Values{
		"server": {result.Detail.ID}, "task": {taskID},
		"return_to": {form.ReturnTo}, "same_machine": {strconv.FormatBool(form.DeviceChoice == "existing")},
	}
	http.Redirect(w, r, serverWizardTestPath+"?"+query.Encode(), http.StatusSeeOther)
}

func registeredWizardAddresses(form ServerAddForm) ([]string, []string) {
	if form.DeviceChoice == "existing" {
		return []string{}, []string{}
	}
	networks, addresses := make([]string, 0, len(form.AddressNetworks)), make([]string, 0, len(form.AddressValues))
	for index, network := range form.AddressNetworks {
		if network == "" || network == "other" {
			continue
		}
		address := ""
		if index < len(form.AddressValues) {
			address = form.AddressValues[index]
		}
		if address != "" {
			networks = append(networks, network)
			addresses = append(addresses, address)
		}
	}
	return networks, addresses
}

func serverWizardStartErrors(form ServerAddForm) []string {
	var errors []string
	if form.Name == "" || form.User == "" {
		errors = append(errors, "名称与用户不能为空。")
	}
	if form.Port < 1 || form.Port > 65535 {
		errors = append(errors, "端口必须在 1 到 65535 之间。")
	}
	switch form.CredentialAction {
	case "none", "password", "key":
	default:
		errors = append(errors, "认证方式无效。")
	}
	if form.DeviceChoice == "existing" && form.ExistingDeviceName == "" {
		errors = append(errors, "选择同一台机器时必须指定机器。")
	}
	if addressRowError := wizardAddressRowError(form); addressRowError != "" {
		errors = append(errors, addressRowError)
	}
	if form.DeviceChoice != "existing" && wizardRegisteredAddressCount(form) == 0 && form.Host == "" {
		errors = append(errors, "至少填写一个地址。")
	}
	return errors
}

// wizardAddressRowError 拒绝只填了一半的地址行：网络与地址必须成对出现，
// 避免半行被静默丢弃或生成错误兜底地址。
func wizardAddressRowError(form ServerAddForm) string {
	for index, network := range form.AddressNetworks {
		address := ""
		if index < len(form.AddressValues) {
			address = form.AddressValues[index]
		}
		if (network == "") != (address == "") {
			return "每行网络地址必须同时选择网络并填写地址。"
		}
	}
	for _, address := range form.AddressValues {
		if strings.ContainsAny(address, " \t") {
			return "地址不能包含空白字符。"
		}
	}
	return ""
}

// resumeServerAddCredentialFields 把会话里暂存的凭据补回提交值；请求本
// 身携带的新凭据优先，模板与审计都不回显这些内容。
func resumeServerAddCredentialFields(form, draft ServerAddForm) ServerAddForm {
	if form.Password == "" {
		form.Password = draft.Password
	}
	if form.KeyContent == "" {
		form.KeyContent, form.KeySource = draft.KeyContent, draft.KeySource
	}
	if form.KeyPassphrase == "" {
		form.KeyPassphrase = draft.KeyPassphrase
	}
	if form.SudoPassword == "" {
		form.SudoPassword = draft.SudoPassword
	}
	form.StagedCredential = false
	return form
}

func wizardRegisteredAddressCount(form ServerAddForm) int {
	count := 0
	for index, network := range form.AddressNetworks {
		address := ""
		if index < len(form.AddressValues) {
			address = form.AddressValues[index]
		}
		if network != "" && network != "other" && address != "" {
			count++
		}
	}
	return count
}

func (h *handler) handleServerWizardTest(w http.ResponseWriter, r *http.Request) {
	serverID := strings.TrimSpace(r.URL.Query().Get("server"))
	taskID := strings.TrimSpace(r.URL.Query().Get("task"))
	detail := h.serverWizardDetail(w, r, serverID)
	if detail == nil {
		return
	}
	view := &serverWizardView{
		Detail: detail, ServerID: serverID, TaskID: taskID,
		SameMachine:  r.URL.Query().Get("same_machine") == "true",
		ReturnTo:     normalizeAddReturnPath(r.URL.Query().Get("return_to")),
		TestPollPath: r.URL.RequestURI(),
	}
	if taskID != "" && h.cfg.ServerConnectionTester != nil {
		if status, ok := h.cfg.ServerConnectionTester.ServerTestStatus(taskID); ok && status != nil && status.ServerID == serverID {
			view.ConnectionTest = status
		}
	}
	if view.ConnectionTest == nil {
		view.TestNotice = "连接测试结果已失效。"
		view.ConnectionTest = &ServerConnectionTestStatus{State: ServerConnectionTestFailed, ServerID: serverID}
	}
	next := serverWizardSitePath + "?" + serverWizardStateQuery(serverID, taskID, view.SameMachine, view.ReturnTo).Encode()
	if view.SameMachine {
		next = serverWizardProfilePath + "?" + serverWizardStateQuery(serverID, taskID, view.SameMachine, view.ReturnTo).Encode()
	}
	view.NextPath = next
	title := "连接测试进行中"
	if view.ConnectionTest.State == ServerConnectionTestSucceeded {
		title = "连接测试完成"
	} else if view.ConnectionTest.State == ServerConnectionTestFailed {
		title = "连接测试未通过"
	}
	h.renderPage(w, r, http.StatusOK, "servers-wizard-test.tmpl", pageData{Title: title, ServerWizard: view})
}

func serverWizardStateQuery(serverID, taskID string, sameMachine bool, returnTo string) url.Values {
	query := url.Values{
		"server": {serverID}, "task": {taskID},
		"same_machine": {strconv.FormatBool(sameMachine)},
	}
	if returnTo != "" {
		query.Set("return_to", returnTo)
	}
	return query
}

func (h *handler) serverWizardDetail(w http.ResponseWriter, r *http.Request, serverID string) *ServerDetailView {
	if serverID == "" {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "分步新增对应的服务器不存在，请重新新增。")
		return nil
	}
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), serverID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器失败", "读取服务器详情失败，请稍后重试。")
		return nil
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "分步新增对应的服务器不存在，请重新新增。")
		return nil
	}
	return detail
}

func (h *handler) handleServerWizardSite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "地点步骤只接受 GET 请求。")
		return
	}
	serverID := strings.TrimSpace(r.URL.Query().Get("server"))
	detail := h.serverWizardDetail(w, r, serverID)
	if detail == nil {
		return
	}
	var topology *TopologyData
	if h.cfg.Topology != nil {
		var err error
		if topology, err = h.cfg.Topology.Topology(h.requestClientInstance(r), ""); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取地点失败", "读取地点失败，请稍后重试。")
			return
		}
	}
	selectedSite := strings.TrimSpace(r.URL.Query().Get("site"))
	view := &serverWizardView{
		Detail: detail, ServerID: serverID, TaskID: strings.TrimSpace(r.URL.Query().Get("task")),
		SameMachine: r.URL.Query().Get("same_machine") == "true",
		ReturnTo:    normalizeAddReturnPath(r.URL.Query().Get("return_to")),
		SiteNodes:   wizardSiteNodes(topology, selectedSite),
	}
	h.renderPage(w, r, http.StatusOK, "servers-wizard-site.tmpl", pageData{Title: "新增服务器 · 地点", ServerWizard: view})
}

func wizardSiteNodes(topology *TopologyData, selected string) []serverWizardSiteNode {
	if topology == nil {
		return nil
	}
	out := make([]serverWizardSiteNode, 0, len(topology.Sites))
	eachSiteTreeNode(topology.Sites, func(site TopologySite, depth int) {
		out = append(out, serverWizardSiteNode{
			ID: site.ID, Name: site.Name, Path: site.LocationPath, Depth: depth, Selected: site.ID == selected,
		})
	})
	return out
}

func (h *handler) handleServerWizardProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		h.handleServerWizardProfilePost(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "Profile 步骤只接受 GET 与 POST 请求。")
		return
	}
	serverID := strings.TrimSpace(r.URL.Query().Get("server"))
	detail := h.serverWizardDetail(w, r, serverID)
	if detail == nil {
		return
	}
	profiles, clients, err := h.authorizationProfilesAndClients()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 Profile 失败", "读取 Profile 与客户端失败，请稍后重试。")
		return
	}
	checked := map[string]bool{}
	if requested := trimmedValues(r.URL.Query()["profile"]); len(requested) > 0 {
		for _, id := range requested {
			checked[id] = true
		}
	} else {
		for _, profile := range detail.Profiles {
			checked[profile.ID] = true
		}
	}
	view := &serverWizardView{
		Detail: detail, ServerID: serverID, TaskID: strings.TrimSpace(r.URL.Query().Get("task")),
		SameMachine:   r.URL.Query().Get("same_machine") == "true",
		SiteAction:    firstNonEmpty(strings.TrimSpace(r.URL.Query().Get("site_action")), "skip"),
		SiteID:        strings.TrimSpace(r.URL.Query().Get("site_id")),
		NewSiteParent: strings.TrimSpace(r.URL.Query().Get("new_site_parent")),
		NewSiteName:   strings.TrimSpace(r.URL.Query().Get("new_site_name")),
		NewSiteNote:   strings.TrimSpace(r.URL.Query().Get("new_site_note")),
		ReturnTo:      normalizeAddReturnPath(r.URL.Query().Get("return_to")),
	}
	clientIDsByProfile := map[string][]string{}
	for _, client := range clients {
		clientIDsByProfile[client.ProfileID] = append(clientIDsByProfile[client.ProfileID], client.ID)
	}
	for _, profile := range profiles {
		ids := clientIDsByProfile[profile.ID]
		view.Profiles = append(view.Profiles, serverWizardProfile{AccessProfileSummary: profile, ClientIDs: ids, Checked: checked[profile.ID]})
	}
	h.renderPage(w, r, http.StatusOK, "servers-wizard-profile.tmpl", pageData{Title: "新增服务器 · Profile", ServerWizard: view})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (h *handler) handleServerWizardProfilePost(w http.ResponseWriter, r *http.Request) {
	form := ServerAddFinishForm{
		ServerID:      strings.TrimSpace(r.PostFormValue("server_id")),
		SiteAction:    firstNonEmpty(strings.TrimSpace(r.PostFormValue("site_action")), "skip"),
		SiteID:        strings.TrimSpace(r.PostFormValue("site_id")),
		NewSiteParent: strings.TrimSpace(r.PostFormValue("new_site_parent")),
		NewSiteName:   strings.TrimSpace(r.PostFormValue("new_site_name")),
		NewSiteNote:   strings.TrimSpace(r.PostFormValue("new_site_note")),
		ProfileIDs:    trimmedValues(r.PostForm["profile_id"]),
		ReturnTo:      normalizeAddReturnPath(r.PostFormValue("return_to")),
	}
	resume := r.URL.RequestURI()
	if len(form.ProfileIDs) > 0 && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		resumeQuery := url.Values{
			"server": {form.ServerID}, "task": {r.PostFormValue("task_id")},
			"same_machine": {strconv.FormatBool(r.PostFormValue("same_machine") == "true")},
			"site_action":  {form.SiteAction}, "site_id": {form.SiteID},
			"new_site_parent": {form.NewSiteParent}, "new_site_name": {form.NewSiteName},
			"new_site_note": {form.NewSiteNote},
		}
		for _, id := range form.ProfileIDs {
			resumeQuery.Add("profile", id)
		}
		if form.ReturnTo != "" {
			resumeQuery.Set("return_to", form.ReturnTo)
		}
		resume = serverWizardProfilePath + "?" + resumeQuery.Encode()
		http.Redirect(w, r, reverifyPath(resume), http.StatusSeeOther)
		return
	}
	if err := h.cfg.ServerWriter.FinishServerAdd(form, ServerWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
		if isServerFormError(err) {
			h.renderError(w, r, http.StatusBadRequest, "保存新增结果失败", err.Error())
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "保存新增结果失败", "保存地点或 Profile 授权失败，请稍后重试。")
		return
	}
	query := url.Values{"server": {form.ServerID}, "task": {r.PostFormValue("task_id")}}
	if form.ReturnTo != "" {
		query.Set("return_to", form.ReturnTo)
	}
	http.Redirect(w, r, serverAddResultPath+"?"+query.Encode(), http.StatusSeeOther)
}
