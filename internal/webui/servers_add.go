package webui

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// serverAddView 是新增服务器表单页的渲染数据。表单值用于校验失败后保留
// owner 刚输入的非凭据字段；凭据值绝不写入模板输出。
type serverAddView struct {
	Form           *ServerAddForm
	Sites          []TopologySite
	Networks       []TopologyNetwork
	Devices        []TopologyDevice
	Profiles       []AccessProfileSummary
	Notice         string
	NeedsReverify  bool
	ReverifyPath   string
	HasCredential  bool
	VirtualMembers []int
}

func (h *handler) handleServerAddType(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增类型选择页只接受 GET 请求。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers-add-type.tmpl", pageData{Title: "新增"})
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
		if draft := h.serverAddDraft(r); draft != nil {
			submitted = draft
			notice = "已保留表单状态，请检查后再次提交。"
		}
	} else if sess := h.currentSession(r); sess != nil {
		h.setServerAddDraft(sess, nil)
	}
	h.renderAddForm(w, r, submitted, notice, http.StatusOK)
}

func (h *handler) renderAddForm(w http.ResponseWriter, r *http.Request, submitted *ServerAddForm, notice string, status int) {
	view := &serverAddView{Notice: notice, VirtualMembers: []int{0, 1, 2}}
	if submitted == nil {
		view.Form = defaultServerAddForm()
	} else {
		view.Form = submitted
		view.HasCredential = serverAddFormWritesCredential(*submitted)
	}
	if h.cfg.Topology != nil {
		topology, err := h.cfg.Topology.Topology(h.requestClientInstance(r), "")
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取新增选项失败", "读取站点、网络与设备失败，请稍后重试。")
			return
		}
		view.Sites, view.Networks, view.Devices = topology.Sites, topology.Networks, topology.Devices
	}
	if h.cfg.Access != nil {
		profiles, err := h.cfg.Access.ListProfiles()
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取 profile 失败", "读取 profile 授权选项失败，请稍后重试。")
			return
		}
		view.Profiles = profiles
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		view.NeedsReverify = true
		view.ReverifyPath = reverifyPath(serverAddFormPath + "?resume=1")
	}
	h.renderPage(w, r, status, "servers-add.tmpl", pageData{Title: "新增服务器", ServerAdd: view})
}

func defaultServerAddForm() *ServerAddForm {
	return &ServerAddForm{Port: 22, Shell: "auto", DeviceChoice: "new", CredentialAction: "none"}
}

func (h *handler) handleServerAddFormPost(w http.ResponseWriter, r *http.Request) {
	form := h.parseServerAddForm(r)
	var fieldErrors []string
	switch form.CredentialAction {
	case "none", "password", "key":
	default:
		fieldErrors = append(fieldErrors, "登录凭据类型无效。")
	}
	if form.Port < 1 || form.Port > 65535 {
		fieldErrors = append(fieldErrors, "端口必须在 1 到 65535 之间。")
	}
	if len(fieldErrors) > 0 {
		h.renderAddForm(w, r, &form, strings.Join(fieldErrors, " "), http.StatusBadRequest)
		return
	}
	if r.PostFormValue("resume_credentials") == "1" {
		if draft := h.serverAddDraft(r); draft != nil {
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
			h.renderAddForm(w, r, &form, err.Error(), http.StatusBadRequest)
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
			h.requestClientInstance(r), result.Detail.ID, probeShell,
			ServerWriteSource{RemoteAddress: remoteIP(r)},
		)
	}
	http.Redirect(w, r, serverAddResultPath+"?"+serverAddResultQuery(result, taskID, probeShell).Encode(), http.StatusSeeOther)
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
		if file, _, err := r.FormFile("key_file"); err == nil {
			if content, readErr := io.ReadAll(file); readErr == nil {
				form.KeyContent = string(content)
			}
		}
	}
	form.KeyPassphrase = r.PostFormValue("key_passphrase")
	form.SudoPassword = r.PostFormValue("sudo_password")
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
	form.ProfileIDs = trimmedValues(r.PostForm["profile_id"])
	return form
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
	detail, err := h.serverAddResultDetail(r, identifier)
	if err != nil || detail == nil || identifier == "" {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "新增结果对应的服务器不存在，请从服务器列表重新进入。")
		return
	}
	view := &ServerAddResultView{
		Detail: detail, DeviceName: r.URL.Query().Get("device"),
		ProfileNames:             parseTags(r.URL.Query().Get("profiles")),
		ConnectionTestPollPath:   r.URL.RequestURI(),
		AnchorCopied:             r.URL.Query().Get("anchor") == "copy",
		AnchorCopyFromServerName: r.URL.Query().Get("anchor_from"),
		AnchorCopyConflict:       r.URL.Query().Get("anchor") == "conflict",
	}
	probeShell := r.URL.Query().Get("shell") == "auto"
	taskID := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskID != "" && h.cfg.ServerConnectionTester != nil {
		if status, ok := h.cfg.ServerConnectionTester.ServerTestStatus(taskID); ok &&
			status != nil && status.ServerID == identifier {
			view.ConnectionTest = status
		} else {
			view.ConnectionTestNotice = "连接测试结果已失效，服务器条目仍保持未验证。"
			view.ConnectionTest = &ServerConnectionTestStatus{
				State: ServerConnectionTestFailed, ServerID: identifier, ProbeShell: probeShell,
			}
		}
	} else {
		view.ConnectionTestNotice = "连接测试未启动，服务器条目仍保持未验证。"
		view.ConnectionTest = &ServerConnectionTestStatus{
			State: ServerConnectionTestFailed, ServerID: identifier, ProbeShell: probeShell,
		}
	}
	h.renderPage(w, r, http.StatusOK, "servers-add-result.tmpl", pageData{
		Title: "新增结果 · " + detail.Name, ServerAddResult: view,
	})
}

// serverAddResultDetail 读取刚保存条目的详情。保存、后台测试与结果页读取会
// 分别打开单机 vault；极短窗口内读到空结果时重试几次，避免刚提交的成功保
// 存被误报成不存在。真实不存在的条目最终仍返回 404。
func (h *handler) serverAddResultDetail(r *http.Request, identifier string) (*ServerDetailView, error) {
	var detail *ServerDetailView
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		detail, err = h.cfg.Servers.ServerDetail(h.requestClientInstance(r), identifier)
		if err == nil && detail != nil {
			return detail, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return detail, err
}

func serverAddResultQuery(result *ServerAddResultView, taskID string, probeShell bool) url.Values {
	query := url.Values{"server": {result.Detail.ID}}
	if taskID != "" {
		query.Set("task", taskID)
	}
	if probeShell {
		query.Set("shell", "auto")
	}
	if result.DeviceName != "" {
		query.Set("device", result.DeviceName)
	}
	if len(result.ProfileNames) > 0 {
		query.Set("profiles", strings.Join(result.ProfileNames, ","))
	}
	switch {
	case result.AnchorCopied:
		query.Set("anchor", "copy")
		if result.AnchorCopyFromServerName != "" {
			query.Set("anchor_from", result.AnchorCopyFromServerName)
		}
	case result.AnchorCopyConflict:
		query.Set("anchor", "conflict")
	}
	return query
}

func (h *handler) serverAddDraft(r *http.Request) *ServerAddForm {
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return sess.serverAddDraft
}

func (h *handler) setServerAddDraft(sess *session, form *ServerAddForm) {
	if sess == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sess.serverAddDraft = form
}
