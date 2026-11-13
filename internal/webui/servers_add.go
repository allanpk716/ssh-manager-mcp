package webui

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// serverAddView 是新增服务器表单页的渲染数据。表单值用于校验失败后保留
// owner 刚输入的非凭据字段；凭据值绝不写入模板输出。
type serverAddView struct {
	Form                *ServerAddForm
	Sites               []TopologySite
	Networks            []TopologyNetwork
	Devices             []TopologyDevice
	Profiles            []AccessProfileSummary
	Notice              string
	NeedsReverify       bool
	ReverifyPath        string
	HasCredential       bool
	HasStagedCredential bool
	VirtualMembers      []int
	DeviceOnly          bool
	ReturnTo            string
}

const serverAddDraftTTL = 5 * time.Minute

type serverAddDraft struct {
	form      ServerAddForm
	createdAt time.Time
}

// addTypeChoiceView 是新增类型选择页的两个入口地址。拓扑画布带上下文进入
// 时，两个地址都保留网络与返回位置，保证选择服务器或其他设备后仍使用同一
// 份预选内容。
type addTypeChoiceView struct {
	ServerHref      string
	OtherDeviceHref string
}

const topologyReturnPath = "/ui/topology/"

func normalizeAddReturnPath(value string) string {
	if value == topologyReturnPath || value == "/ui/topology" {
		return topologyReturnPath
	}
	return ""
}

func addFormHref(path, network, returnTo string) string {
	query := url.Values{}
	if network != "" {
		query.Set("network", network)
	}
	if returnTo != "" {
		query.Set("return_to", returnTo)
	}
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

func (h *handler) handleServerAddType(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增类型选择页只接受 GET 请求。")
		return
	}
	network := strings.TrimSpace(r.URL.Query().Get("network"))
	returnTo := normalizeAddReturnPath(r.URL.Query().Get("return_to"))
	h.renderPage(w, r, http.StatusOK, "servers-add-type.tmpl", pageData{
		Title: "新增", AddTypeChoice: &addTypeChoiceView{
			ServerHref:      addFormHref(serverAddFormPath, network, returnTo),
			OtherDeviceHref: addFormHref(deviceAddFormPath, network, returnTo),
		},
	})
}

func (h *handler) handleServerAddForm(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		h.handleServerAddFormPost(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增服务器页只接受 GET 与 POST 请求。")
		return
	}
	var submitted *ServerAddForm
	notice := ""
	resume := r.URL.Query().Get("resume") == "1"
	if resume {
		if draft := h.peekServerAddDraft(r); draft != nil {
			draft.StagedCredential = true
			submitted = draft
			notice = "已保留表单状态，请检查后再次提交。"
		} else {
			notice = "暂存的登录凭据已超过五分钟，请重新填写。"
		}
	} else if sess := h.currentSession(r); sess != nil {
		h.setServerAddDraft(sess, nil)
	}
	h.renderAddForm(w, r, submitted, notice, http.StatusOK, false)
}

func (h *handler) handleOtherDeviceAddForm(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		h.handleOtherDeviceAddFormPost(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增其他设备页只接受 GET 与 POST 请求。")
		return
	}
	h.renderAddForm(w, r, nil, "", http.StatusOK, true)
}

func (h *handler) renderAddForm(
	w http.ResponseWriter, r *http.Request, submitted *ServerAddForm, notice string, status int, deviceOnly bool,
) {
	view := &serverAddView{Notice: notice, DeviceOnly: deviceOnly}
	var topology *TopologyData
	if h.cfg.Topology != nil {
		var err error
		topology, err = h.cfg.Topology.Topology(h.requestClientInstance(r), "")
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取新增选项失败", "读取站点、网络与设备失败，请稍后重试。")
			return
		}
	}
	if submitted == nil {
		if deviceOnly {
			view.Form = defaultOtherDeviceAddForm()
		} else {
			view.Form = defaultServerAddForm()
		}
		if title, message := applyAddFormContext(view.Form, r.URL.Query(), topology, deviceOnly); title != "" {
			h.renderError(w, r, http.StatusNotFound, title, message)
			return
		}
		view.ReturnTo = view.Form.ReturnTo
	} else {
		view.Form = submitted
		view.HasCredential = serverAddFormWritesCredential(*submitted)
		view.HasStagedCredential = submitted.StagedCredential
		view.ReturnTo = submitted.ReturnTo
	}
	if topology != nil {
		view.Sites, view.Networks, view.Devices = topology.Sites, topology.Networks, topology.Devices
	}
	view.VirtualMembers = virtualMemberIndexes(submitted)
	if !deviceOnly && h.cfg.Access != nil {
		profiles, err := h.cfg.Access.ListProfiles()
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取 profile 失败", "读取 profile 授权选项失败，请稍后重试。")
			return
		}
		view.Profiles = profiles
	}
	if !deviceOnly && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		view.NeedsReverify = true
		view.ReverifyPath = reverifyPath(serverAddFormPath + "?resume=1")
	}
	title := "新增服务器"
	if deviceOnly {
		title = "新增其他设备"
	}
	h.renderPage(w, r, status, "servers-add.tmpl", pageData{Title: title, ServerAdd: view})
}

// applyAddFormContext 把拓扑画布带来的网络或设备上下文写进表单默认值。
// 名称不存在时返回错误页标题与说明，避免把无效名称带进保存请求。
func applyAddFormContext(form *ServerAddForm, query url.Values, topology *TopologyData, deviceOnly bool) (string, string) {
	networkName := strings.TrimSpace(query.Get("network"))
	deviceName := strings.TrimSpace(query.Get("device"))
	form.ReturnTo = normalizeAddReturnPath(query.Get("return_to"))
	if networkName == "" && deviceName == "" {
		return "", ""
	}
	if topology == nil {
		return "新增上下文不存在", "读取不到选择的网络或设备，请回到网络拓扑后重新进入。"
	}
	if networkName != "" && deviceName != "" {
		return "新增上下文无效", "不能同时预选网络与设备，请回到网络拓扑后重新进入。"
	}
	if networkName != "" {
		for _, network := range topology.Networks {
			if network.Name != networkName {
				continue
			}
			switch network.Type {
			case "physical":
				form.SiteName = network.SiteName
				form.PhysicalNetwork = network.Name
			case "virtual":
				form.VirtualNetworkNames = []string{network.Name}
				form.SiteName = network.SiteName
			default:
				return "新增上下文无效", "选择的网络类型不支持预选，请回到网络拓扑后重新进入。"
			}
			return "", ""
		}
		return "网络不存在", "选择的物理局域网或虚拟局域网不存在，请回到网络拓扑后重新进入。"
	}
	if deviceOnly {
		return "新增上下文无效", "设备详情入口只能新增服务器条目，请从类型选择页新增其他设备。"
	}
	for _, device := range topology.Devices {
		if device.Name == deviceName {
			form.DeviceChoice = "existing"
			form.ExistingDeviceName = device.Name
			return "", ""
		}
	}
	return "设备不存在", "选择的设备不存在，请回到网络拓扑后重新进入。"
}

func defaultServerAddForm() *ServerAddForm {
	return &ServerAddForm{Port: 22, Shell: "auto", DeviceChoice: "new", CredentialAction: "none"}
}

func defaultOtherDeviceAddForm() *ServerAddForm {
	return &ServerAddForm{DeviceChoice: "new"}
}

func (h *handler) handleServerAddFormPost(w http.ResponseWriter, r *http.Request) {
	form := h.parseServerAddForm(r)
	if r.PostFormValue("add_virtual_member") == "1" {
		if serverAddFormWritesCredential(form) {
			if sess := h.currentSession(r); sess != nil {
				h.setServerAddDraft(sess, &form)
			}
			form.StagedCredential = true
		} else if draft := h.peekServerAddDraft(r); draft != nil {
			form.StagedCredential = true
		}
		h.renderAddForm(w, r, &form, "已追加一个虚拟局域网成员，请继续填写。", http.StatusOK, false)
		return
	}
	if r.PostFormValue("clear_staged_credentials") == "1" {
		if sess := h.currentSession(r); sess != nil {
			h.setServerAddDraft(sess, nil)
		}
		clearServerAddCredentialFields(&form)
		h.renderAddForm(w, r, &form, "已清除暂存的登录凭据，请重新填写后再保存。", http.StatusOK, false)
		return
	}
	var fieldErrors []string
	resumeCredentials := r.PostFormValue("resume_credentials") == "1"
	if resumeCredentials {
		draft := h.takeServerAddDraft(r)
		if draft == nil {
			fieldErrors = append(fieldErrors, "暂存的登录凭据已超过五分钟，请重新填写。")
		} else {
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
		}
	}
	if form.KeyFileError != nil {
		h.logError(r, "读取新增服务器私钥文件", form.KeyFileError)
		fieldErrors = append(fieldErrors, "读取私钥文件失败，请重新选择文件。")
	}
	switch form.CredentialAction {
	case "none", "password", "key":
	default:
		fieldErrors = append(fieldErrors, "登录凭据类型无效。")
	}
	if form.Port < 1 || form.Port > 65535 {
		fieldErrors = append(fieldErrors, "端口必须在 1 到 65535 之间。")
	}
	fieldErrors = append(fieldErrors, deviceFieldErrors(form)...)
	if len(fieldErrors) > 0 {
		h.renderAddForm(w, r, &form, strings.Join(fieldErrors, " "), http.StatusBadRequest, false)
		return
	}
	if serverAddFormWritesCredential(form) && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		if sess := h.currentSession(r); sess != nil {
			h.setServerAddDraft(sess, &form)
		}
		http.Redirect(w, r, reverifyPath(serverAddFormPath+"?resume=1"), http.StatusSeeOther)
		return
	}
	result, err := h.cfg.ServerWriter.AddServer(form, ServerWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		if isServerFormError(err) {
			h.renderAddForm(w, r, &form, err.Error(), http.StatusBadRequest, false)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "新增服务器失败", "新增服务器失败，请稍后重试。")
		return
	}
	if sess := h.currentSession(r); sess != nil {
		h.setServerAddDraft(sess, nil)
	}
	taskID := ""
	probeShell := form.Shell == "" || form.Shell == "auto"
	if h.cfg.ServerConnectionTester != nil {
		taskID, _ = h.cfg.ServerConnectionTester.StartServerTest(
			result.Detail.ID, probeShell,
			ServerWriteSource{RemoteAddress: remoteIP(r)},
			ServerConnectionAnchor{
				Copied: result.AnchorCopied, FromServerName: result.AnchorCopyFromServerName,
				Conflict: result.AnchorCopyConflict,
			},
		)
	}
	result.ReturnTo = form.ReturnTo
	http.Redirect(w, r, serverAddResultPath+"?"+serverAddResultQuery(result, taskID).Encode(), http.StatusSeeOther)
}

func isServerFormError(err error) bool {
	var target *ServerFormError
	return errors.As(err, &target)
}

func (h *handler) parseServerAddForm(r *http.Request) ServerAddForm {
	form := *defaultServerAddForm()
	form.Name = strings.TrimSpace(r.PostFormValue("name"))
	form.User = strings.TrimSpace(r.PostFormValue("user"))
	form.Host = strings.TrimSpace(r.PostFormValue("host"))
	portText := strings.TrimSpace(r.PostFormValue("port"))
	if portText == "" {
		form.Port = 22
	} else if port, err := strconv.Atoi(portText); err == nil {
		form.Port = port
	} else {
		form.Port = -1
	}
	form.Shell = r.PostFormValue("shell")
	form.Description = strings.TrimSpace(r.PostFormValue("description"))
	form.Location = strings.TrimSpace(r.PostFormValue("location"))
	form.Hardware = strings.TrimSpace(r.PostFormValue("hardware"))
	form.Services = strings.TrimSpace(r.PostFormValue("services"))
	form.Role = strings.TrimSpace(r.PostFormValue("role"))
	form.Caveats = strings.TrimSpace(r.PostFormValue("special_handling"))
	form.Tags = parseTags(r.PostFormValue("tags"))
	form.ExposeHost = r.PostFormValue("expose_host") == "1"
	form.CredentialAction = r.PostFormValue("credential_action")
	form.Password = r.PostFormValue("password")
	form.KeySource = r.PostFormValue("key_source")
	form.KeyContent = r.PostFormValue("key")
	if form.CredentialAction == "key" && form.KeySource == "file" {
		file, _, err := r.FormFile("key_file")
		switch {
		case err == nil:
			form.KeyContent, form.KeyFileError = readPrivateKeyFile(file)
		case errors.Is(err, http.ErrMissingFile):
			// 没选择文件时沿用表单里的空值，由后续服务器管理校验给出提示。
		default:
			form.KeyFileError = err
		}
	}
	form.KeyPassphrase = r.PostFormValue("key_passphrase")
	form.SudoPassword = r.PostFormValue("sudo_password")
	parseDeviceFields(&form, r)
	if r.PostFormValue("add_virtual_member") == "1" {
		form.VirtualMemberRows = virtualMemberCount(&form) + 1
	}
	form.ReturnTo = normalizeAddReturnPath(r.PostFormValue("return_to"))
	form.ProfileIDs = trimmedValues(r.PostForm["profile_id"])
	return form
}

func readPrivateKeyFile(file multipart.File) (string, error) {
	content, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func (h *handler) handleOtherDeviceAddFormPost(w http.ResponseWriter, r *http.Request) {
	form := h.parseOtherDeviceAddForm(r)
	if r.PostFormValue("add_virtual_member") == "1" {
		h.renderAddForm(w, r, &form, "已追加一个虚拟局域网成员，请继续填写。", http.StatusOK, true)
		return
	}
	if fieldErrors := deviceFieldErrors(form); len(fieldErrors) > 0 {
		h.renderAddForm(w, r, &form, strings.Join(fieldErrors, " "), http.StatusBadRequest, true)
		return
	}
	result, err := h.cfg.ServerWriter.AddOtherDevice(otherDeviceAddForm(form), ServerWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		if isServerFormError(err) {
			h.renderAddForm(w, r, &form, err.Error(), http.StatusBadRequest, true)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "新增其他设备失败", "新增其他设备失败，请稍后重试。")
		return
	}
	result.ReturnTo = form.ReturnTo
	resultQuery := url.Values{"device": {result.DeviceName}}
	if result.ReturnTo != "" {
		resultQuery.Set("return_to", result.ReturnTo)
	}
	http.Redirect(w, r, deviceAddResultPath+"?"+resultQuery.Encode(), http.StatusSeeOther)
}

func (h *handler) parseOtherDeviceAddForm(r *http.Request) ServerAddForm {
	form := *defaultOtherDeviceAddForm()
	form.DeviceNote = strings.TrimSpace(r.PostFormValue("note"))
	parseDeviceFields(&form, r)
	if r.PostFormValue("add_virtual_member") == "1" {
		form.VirtualMemberRows = virtualMemberCount(&form) + 1
	}
	form.ReturnTo = normalizeAddReturnPath(r.PostFormValue("return_to"))
	return form
}

func parseDeviceFields(form *ServerAddForm, r *http.Request) {
	form.DeviceChoice = r.PostFormValue("device_choice")
	if form.DeviceChoice != "existing" {
		form.DeviceChoice = "new"
	}
	form.DeviceName = strings.TrimSpace(r.PostFormValue("device_name"))
	form.SiteName = strings.TrimSpace(r.PostFormValue("site"))
	form.ExistingDeviceName = strings.TrimSpace(r.PostFormValue("existing_device"))
	form.PhysicalNetwork = strings.TrimSpace(r.PostFormValue("physical_network"))
	form.PhysicalAddress = strings.TrimSpace(r.PostFormValue("physical_address"))
	form.VirtualNetworkNames = trimmedValues(r.PostForm["virtual_network"])
	form.VirtualAddresses = trimmedValues(r.PostForm["virtual_address"])
}

func otherDeviceAddForm(form ServerAddForm) OtherDeviceAddForm {
	return OtherDeviceAddForm{
		Name: form.DeviceName, Note: form.DeviceNote, SiteName: form.SiteName,
		PhysicalNetwork: form.PhysicalNetwork, PhysicalAddress: form.PhysicalAddress,
		VirtualNetworkNames: form.VirtualNetworkNames, VirtualAddresses: form.VirtualAddresses,
	}
}

func deviceFieldErrors(form ServerAddForm) []string {
	if form.DeviceChoice == "existing" {
		return nil
	}
	var fieldErrors []string
	if (form.PhysicalNetwork == "") != (form.PhysicalAddress == "") {
		fieldErrors = append(fieldErrors, "网络成员必须同时填写网络与地址。")
	}
	if form.SiteName != "" && (form.PhysicalNetwork == "" || form.PhysicalAddress == "") {
		fieldErrors = append(fieldErrors, "填写站点时必须同时填写物理局域网与地址。")
	}
	for index, network := range form.VirtualNetworkNames {
		address := ""
		if index < len(form.VirtualAddresses) {
			address = form.VirtualAddresses[index]
		}
		if network == "" && address == "" {
			continue
		}
		if network == "" || address == "" {
			fieldErrors = append(fieldErrors, "网络成员必须同时填写网络与地址。")
			break
		}
	}
	return fieldErrors
}

func trimmedValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

func serverAddFormWritesCredential(form ServerAddForm) bool {
	if form.SudoPassword != "" || form.KeyPassphrase != "" {
		return true
	}
	switch form.CredentialAction {
	case "password":
		return form.Password != ""
	case "key":
		return form.KeyContent != ""
	}
	return false
}

func clearServerAddCredentialFields(form *ServerAddForm) {
	form.CredentialAction = "none"
	form.Password = ""
	form.KeySource = ""
	form.KeyContent = ""
	form.KeyPassphrase = ""
	form.SudoPassword = ""
	form.KeyFileError = nil
	form.StagedCredential = false
}

func (f ServerAddForm) HasProfileID(id string) bool {
	for _, item := range f.ProfileIDs {
		if item == id {
			return true
		}
	}
	return false
}

func (f ServerAddForm) VirtualNetworkValue(index int) string {
	if index < len(f.VirtualNetworkNames) {
		return f.VirtualNetworkNames[index]
	}
	return ""
}

func virtualMemberCount(form *ServerAddForm) int {
	count := 3
	if form == nil {
		return count
	}
	if len(form.VirtualNetworkNames) > count {
		count = len(form.VirtualNetworkNames)
	}
	if len(form.VirtualAddresses) > count {
		count = len(form.VirtualAddresses)
	}
	return count
}

func virtualMemberIndexes(form *ServerAddForm) []int {
	count := virtualMemberCount(form)
	if form != nil && form.VirtualMemberRows > count {
		count = form.VirtualMemberRows
	}
	indexes := make([]int, count)
	for index := range indexes {
		indexes[index] = index
	}
	return indexes
}

func (f ServerAddForm) VirtualAddressValue(index int) string {
	if index < len(f.VirtualAddresses) {
		return f.VirtualAddresses[index]
	}
	return ""
}

func (h *handler) handleServerAddResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增结果页只接受 GET 请求。")
		return
	}
	identifier := strings.TrimSpace(r.URL.Query().Get("server"))
	if identifier == "" {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "新增结果对应的服务器不存在，请从服务器列表重新进入。")
		return
	}
	detail, err := h.serverAddResultDetail(r, identifier)
	if err != nil {
		h.logError(r, "读取新增结果服务器详情", err)
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "新增结果对应的服务器不存在，请从服务器列表重新进入。")
		return
	}
	view := &ServerAddResultView{
		Detail: detail, DeviceName: detail.DeviceName,
		ConnectionTestPollPath: r.URL.RequestURI(),
		ReturnTo:               normalizeAddReturnPath(r.URL.Query().Get("return_to")),
	}
	if h.cfg.Access == nil {
		view.ProfileNamesUnavailable = true
	} else {
		profiles, err := h.cfg.Access.ListProfiles()
		if err != nil {
			h.logError(r, "读取新增结果 profile 授权", err)
			h.renderError(w, r, http.StatusInternalServerError, "读取 profile 授权失败", "读取 profile 授权失败，请稍后重试。")
			return
		}
		view.ProfileNames = profileNamesForServer(profiles, detail.ID)
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskID != "" && h.cfg.ServerConnectionTester != nil {
		if status, ok := h.cfg.ServerConnectionTester.ServerTestStatus(taskID); ok &&
			status != nil && status.ServerID == identifier {
			view.ConnectionTest = status
			view.AnchorCopied = status.AnchorCopied
			view.AnchorCopyFromServerName = status.AnchorCopyFromServerName
			view.AnchorCopyConflict = status.AnchorCopyConflict
		} else {
			view.ConnectionTestNotice = "连接测试结果已失效，服务器条目仍保持未验证。"
			view.ConnectionTest = &ServerConnectionTestStatus{
				State: ServerConnectionTestFailed, ServerID: identifier,
			}
		}
	} else {
		view.ConnectionTestNotice = "连接测试未启动，服务器条目仍保持未验证。"
		view.ConnectionTest = &ServerConnectionTestStatus{
			State: ServerConnectionTestFailed, ServerID: identifier,
		}
	}
	h.renderPage(w, r, http.StatusOK, "servers-add-result.tmpl", pageData{
		Title: "新增结果 · " + detail.Name, ServerAddResult: view,
	})
}

// serverAddResultDetail 读取刚保存条目的详情；读取错误交给调用方记录并返
// 回服务器错误，只有存储明确返回空详情才按不存在处理。
func (h *handler) serverAddResultDetail(r *http.Request, identifier string) (*ServerDetailView, error) {
	return h.cfg.Servers.ServerDetail(h.requestClientInstance(r), identifier)
}

func serverAddResultQuery(result *ServerAddResultView, taskID string) url.Values {
	query := url.Values{"server": {result.Detail.ID}}
	if taskID != "" {
		query.Set("task", taskID)
	}
	if result.ReturnTo != "" {
		query.Set("return_to", result.ReturnTo)
	}
	return query
}

func profileNamesForServer(profiles []AccessProfileSummary, serverID string) []string {
	names := make([]string, 0)
	for _, profile := range profiles {
		for _, server := range profile.Servers {
			if server.ID == serverID {
				names = append(names, profile.Name)
				break
			}
		}
	}
	return names
}

func (h *handler) handleOtherDeviceAddResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "其他设备新增结果页只接受 GET 请求。")
		return
	}
	deviceName := strings.TrimSpace(r.URL.Query().Get("device"))
	if deviceName == "" {
		h.renderError(w, r, http.StatusNotFound, "设备不存在", "新增结果对应的设备不存在，请重新新增。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "devices-add-result.tmpl", pageData{
		Title: "新增其他设备结果 · " + deviceName,
		OtherDeviceAddResult: &OtherDeviceAddResultView{
			DeviceName: deviceName, ReturnTo: normalizeAddReturnPath(r.URL.Query().Get("return_to")),
		},
	})
}

func (h *handler) peekServerAddDraft(r *http.Request) *ServerAddForm {
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	draft := sess.serverAddDraft
	if draft == nil {
		return nil
	}
	if h.draftExpiredLocked(draft) {
		sess.serverAddDraft = nil
		return nil
	}
	form := draft.form
	return &form
}

func (h *handler) takeServerAddDraft(r *http.Request) *ServerAddForm {
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	draft := sess.serverAddDraft
	sess.serverAddDraft = nil
	if draft == nil || h.draftExpiredLocked(draft) {
		return nil
	}
	form := draft.form
	return &form
}

func (h *handler) draftExpiredLocked(draft *serverAddDraft) bool {
	return !h.cfg.now().Before(draft.createdAt.Add(serverAddDraftTTL))
}

func (h *handler) setServerAddDraft(sess *session, form *ServerAddForm) {
	if sess == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if form == nil {
		sess.serverAddDraft = nil
		return
	}
	sess.serverAddDraft = &serverAddDraft{form: *form, createdAt: h.cfg.now()}
}
