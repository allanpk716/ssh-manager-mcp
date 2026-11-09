package webui

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ServerListEntry 是服务器页列表与过滤视图的一行；AttentionReasons 携带注
// 意原因代号（no-credential、no-role、needs-passphrase），模板负责换成人
// 能读懂的文案。
type ServerListEntry struct {
	ID               string
	Name             string
	User             string
	Host             string
	Port             int
	AttentionReasons []string
}

// ServerDetailView 是服务器详情页的展示数据，只含凭据状态，绝不含凭据内
// 容；AttentionReasons 携带注意原因代号，模板负责换成文案。
type ServerDetailView struct {
	ID               string
	Name             string
	User             string
	Host             string
	Port             int
	Shell            string
	CredentialType   string
	HasKeyPassphrase bool
	HasSudoPassword  bool
	Description      string
	Location         string
	Hardware         string
	Services         string
	Role             string
	Caveats          string
	Tags             []string
	ExposeHost       bool
	DeviceName       string
	HasDevice        bool
	// AttentionReasons 是注意原因代号列表，取值见 models 包的注意原因常
	// 量；页面判断不得依赖界面文案。
	AttentionReasons []string
	Revision         int64
}

// ServerWriteSource 描述一次服务器写操作的来源地址；适配器把它写进审计行
// 的 Web UI 来源列。
type ServerWriteSource struct {
	RemoteAddress string
}

// ServerEditForm 是编辑提交。指针字段为 nil 表示这个字段没有变化；凭据动
// 作是 keep、password、key、key-passphrase、clear 之一。
type ServerEditForm struct {
	Name        *string
	Host        *string
	Port        *int
	User        *string
	Shell       *string
	Description *string
	Location    *string
	Hardware    *string
	Services    *string
	Role        *string
	Caveats     *string
	// Tags 是解析后的标签列表；nil 表示这个字段没有变化。
	Tags       []string
	ExposeHost *bool

	CredentialAction string
	Password         string
	KeyContent       string
	KeyPassphrase    string
	SudoPassword     string
	ExpectedRevision int64
}

// NameValue 等取值方法给测试与模板读取可选字段，空值返回空串。
func (f ServerEditForm) NameValue() string        { return stringValue(f.Name) }
func (f ServerEditForm) DescriptionValue() string { return stringValue(f.Description) }
func (f ServerEditForm) RoleValue() string        { return stringValue(f.Role) }
func (f ServerEditForm) PasswordValue() string    { return f.Password }

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ServerEditResultView 是一次成功编辑后的结果；受影响服务器是共用凭据的
// 那批条目名字。
type ServerEditResultView struct {
	Detail          *ServerDetailView
	AffectedServers []string
}

// ServerFormError 表示服务器表单的提交内容校验失败；Message 是可直接显示
// 的中文提示，不含命令行参数名，也不含任何凭据内容。
type ServerFormError struct {
	Message string
}

func (e *ServerFormError) Error() string { return e.Message }

// ServerEditConflictView 表示编辑表单打开后服务器条目已被别的入口修改。
type ServerEditConflictView struct {
	Current *ServerDetailView
}

// ServerDeletePreviewView 是删除影响清单。
type ServerDeletePreviewView struct {
	ServerID           string
	ServerName         string
	HasDevice          bool
	DeviceName         string
	AnchorCount        int
	ProfileNames       []string
	SecretNames        []string
	DeletedCredentials []string
	KeptCredentials    []string
	Digest             string
}

// ErrServerNotFound 表示要操作的服务器条目不存在，页面按 404 处理。
var ErrServerNotFound = errors.New("服务器不存在")

// ServerDeletePreviewChanged 表示 owner 确认的删除预览与执行时的最新预览不
// 一致；Preview 是执行时重新组装的最新预览，页面直接渲染它供再次确认。
type ServerDeletePreviewChanged struct {
	Preview *ServerDeletePreviewView
}

func (e *ServerDeletePreviewChanged) Error() string {
	return "服务器删除预览已变化，未删除任何数据"
}

// ServerImportCandidateView 是导入候选清单里的一台机器。
type ServerImportCandidateView struct {
	Name string
	User string
	Host string
	Port int
	Note string
}

// ServerImportSkipView 是计划阶段跳过的一条记录。
type ServerImportSkipView struct {
	Name   string
	Reason string
}

// ServerProfileOption 是导入表单里的一个可选 profile。
type ServerProfileOption struct {
	ID   string
	Name string
}

// ServerImportPlanView 是导入候选页数据。
type ServerImportPlanView struct {
	Path         string
	MatchWarning bool
	ProfileID    string
	Candidates   []ServerImportCandidateView
	VaultSkipped []ServerImportSkipView
	ParseSkipped []ServerImportSkipView
	// Failed 是计划阶段已能确定会失败的候选。当前导入实现里失败都发生在
	// 执行阶段（插入写入），计划阶段无法判定任何失败，因此恒为空；页面
	// 仍显示这一组，计数为 0。
	Failed   []ServerImportSkipView
	Profiles []ServerProfileOption
}

// ServerImportEntryView 是导入结果里的一台机器状态。
type ServerImportEntryView struct {
	Name   string
	Status string
}

// ServerImportRunView 是导入执行结果：四类计数与补全队列。
type ServerImportRunView struct {
	Imported    int
	Skipped     int
	Failed      int
	Pending     int
	Entries     []ServerImportEntryView
	ImportedIDs []string
}

// ServerCompleteForm 是补全提交；凭据与 sudo 输入为空表示不变。
type ServerCompleteForm struct {
	Description   string
	Location      string
	Hardware      string
	Services      string
	Role          string
	Caveats       string
	SudoPassword  string
	Password      string
	KeyPassphrase string
}

func (f ServerCompleteForm) RoleValue() string { return f.Role }

// ServerCompleteResultView 是补全保存结果。
type ServerCompleteResultView struct {
	Detail          *ServerDetailView
	AffectedServers []string
}

// ServerWriter 是 Web UI 修改服务器数据的唯一入口。单机与权威端由服务器管
// 理模块适配实现；客户端不注册写路由，也不实现这个接口。
type ServerWriter interface {
	EditServer(id string, form ServerEditForm, source ServerWriteSource) (*ServerEditResultView, *ServerEditConflictView, error)
	DeleteServerPreview(identifier string) (*ServerDeletePreviewView, error)
	DeleteServer(identifier, digest string, source ServerWriteSource) error
	// SharedCredentialServers 返回与这台服务器共用同一份登录凭据的服务器
	// 名字，补私钥口令前的提示需要它；列表含目标服务器本身。
	SharedCredentialServers(identifier string) ([]string, error)
	ImportPlan(path, profile string) (*ServerImportPlanView, error)
	ImportRun(path string, selected []string, profile string, source ServerWriteSource) (*ServerImportRunView, error)
	CompleteServer(id string, form ServerCompleteForm, source ServerWriteSource) (*ServerCompleteResultView, error)
}

const (
	serversPrefix       = "/ui/servers/"
	serversImportPath   = "/ui/servers/import"
	serversPlanPath     = "/ui/servers/import/plan"
	serversRunPath      = "/ui/servers/import/run"
	serversCompletePath = "/ui/servers/complete"
	serversResultPath   = "/ui/servers/complete/result"
)

// registerServerRoutes 注册服务器页路由。列表与详情所有角色可用；写路由只
// 在提供了写来源的角色注册，客户端的写路由不存在。
func (h *handler) registerServerRoutes() {
	h.mux.HandleFunc(serversPrefix, h.requireSession(h.handleServerEntry))
	if h.cfg.ServerWriter == nil {
		return
	}
	h.mux.HandleFunc(serversImportPath, h.requireSession(h.handleServerImport))
	h.mux.HandleFunc(serversPlanPath, h.requireSession(h.handleServerImportPlan))
	h.mux.HandleFunc(serversRunPath, h.requireSession(h.handleServerImportRun))
	h.mux.HandleFunc(serversCompletePath, h.requireSession(h.handleServerComplete))
	h.mux.HandleFunc(serversResultPath, h.requireSession(h.handleServerCompleteResult))
}

// handleServerList 渲染服务器列表；attention=1 时只显示需要注意的服务器并
// 置顶。
func (h *handler) handleServerList(w http.ResponseWriter, r *http.Request) {
	attentionOnly := r.URL.Query().Get("attention") == "1"
	rows, err := h.cfg.Servers.ServerList(h.requestClientInstance(r), attentionOnly)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请稍后重试。")
		return
	}
	nextSteps, err := h.standaloneNextSteps(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取下一步引导失败", "读取服务器、Profile 与项目令牌失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers.tmpl", pageData{
		Title: "服务器", ServerList: sortServerAttentionFirst(rows), NextSteps: nextSteps,
		AttentionFilter: attentionOnly,
	})
}

// sortServerAttentionFirst 稳定地把需要注意的服务器排到最前。
func sortServerAttentionFirst(rows []ServerListEntry) []ServerListEntry {
	sorted := append([]ServerListEntry(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].AttentionReasons) > 0 && len(sorted[j].AttentionReasons) == 0
	})
	return sorted
}

// handleServerEntry 分发 /ui/servers/{id} 与 /ui/servers/{id}/edit|delete。
func (h *handler) handleServerEntry(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, serversPrefix)
	id, action, hasAction := strings.Cut(rest, "/")
	if id == "" || strings.Contains(id, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if !hasAction {
		h.handleServerDetail(w, r, id)
		return
	}
	if h.cfg.ServerWriter == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch action {
	case "edit":
		h.handleServerEdit(w, r, id)
	case "delete":
		h.handleServerDelete(w, r, id)
	default:
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	}
}

func (h *handler) handleServerDetail(w http.ResponseWriter, r *http.Request, id string) {
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers-detail.tmpl", pageData{Title: detail.Name, ServerDetail: detail})
}

// handleServerEdit 渲染编辑表单或处理编辑提交。权威端未在 5 分钟内验证过
// 管理员口令时，凭据区域显示重新验证提示；提交凭据时再检查一次。
func (h *handler) handleServerEdit(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == http.MethodPost {
		h.handleServerEditPost(w, r, id)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "编辑页只接受 GET 与 POST 请求。")
		return
	}
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
		return
	}
	view := h.serverEditView(w, r, detail, "")
	h.renderPage(w, r, http.StatusOK, "servers-edit.tmpl", pageData{Title: "编辑 " + detail.Name, ServerEdit: view})
}

type serverEditView struct {
	Detail             *ServerDetailView
	Notice             string
	NeedsReverify      bool
	ReverifyPath       string
	SharedKeyServers   []string
	HasSharedKeyNotice bool
}

func (h *handler) serverEditView(w http.ResponseWriter, r *http.Request, detail *ServerDetailView, notice string) *serverEditView {
	view := &serverEditView{Detail: detail, Notice: notice}
	if detail.CredentialType == string(models.CredPrivateKey) {
		if names, err := h.cfg.ServerWriter.SharedCredentialServers(detail.ID); err == nil {
			view.SharedKeyServers = names
			view.HasSharedKeyNotice = len(names) > 0
		}
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		view.NeedsReverify = true
		view.ReverifyPath = reverifyPath("/ui/servers/" + url.PathEscape(detail.ID) + "/edit")
	}
	return view
}

func (h *handler) handleServerEditPost(w http.ResponseWriter, r *http.Request, id string) {
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
	if err != nil || detail == nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "保存前读取服务器详情失败，请稍后重试。")
		return
	}
	form := h.parseServerEditForm(r, detail)
	if serverFormWritesCredential(form) && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath("/ui/servers/"+url.PathEscape(id)+"/edit"), http.StatusSeeOther)
		return
	}
	result, conflict, err := h.cfg.ServerWriter.EditServer(id, form, ServerWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		var formErr *ServerFormError
		if errors.As(err, &formErr) {
			view := h.serverEditView(w, r, editErrorDetail(r, detail), formErr.Message)
			h.renderPage(w, r, http.StatusBadRequest, "servers-edit.tmpl", pageData{
				Title: "编辑 " + view.Detail.Name, ServerEdit: view,
			})
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "保存服务器失败", "保存服务器失败，请稍后重试。")
		return
	}
	if conflict != nil {
		view := h.serverEditView(w, r, conflict.Current, "服务器条目已被其他入口修改，以下为最新值；请核对后重新提交。")
		h.renderPage(w, r, http.StatusConflict, "servers-edit.tmpl", pageData{
			Title: "编辑 " + conflict.Current.Name, ServerEdit: view,
		})
		return
	}
	http.Redirect(w, r, "/ui/servers/"+url.PathEscape(result.Detail.ID), http.StatusSeeOther)
}

// editErrorDetail 用提交值覆盖当前详情的非凭据字段，供校验失败后重新渲染
// 表单时保留 owner 刚输入的内容；凭据字段一律不回显。
func editErrorDetail(r *http.Request, current *ServerDetailView) *ServerDetailView {
	submitted := *current
	_ = r.ParseForm()
	if v := r.PostFormValue("name"); v != "" {
		submitted.Name = v
	}
	if v := r.PostFormValue("host"); v != "" {
		submitted.Host = v
	}
	if port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port"))); err == nil && port > 0 {
		submitted.Port = port
	}
	if v := r.PostFormValue("user"); v != "" {
		submitted.User = v
	}
	if v := r.PostFormValue("shell"); v != "" {
		submitted.Shell = v
	}
	submitted.Description = r.PostFormValue("description")
	submitted.Location = r.PostFormValue("location")
	submitted.Hardware = r.PostFormValue("hardware")
	submitted.Services = r.PostFormValue("services")
	submitted.Role = r.PostFormValue("role")
	submitted.Caveats = r.PostFormValue("special_handling")
	if tags := parseTags(r.PostFormValue("tags")); tags != nil {
		submitted.Tags = tags
	}
	submitted.ExposeHost = r.PostFormValue("expose_host") == "1"
	return &submitted
}

// parseServerEditForm 把提交值与当前值比对，只保留变化过的字段，避免把没
// 动过的字段清空。修订号取表单隐藏字段携带的值做乐观锁；与当前修订号不
// 一致时由写来源按冲突拒绝。
func (h *handler) parseServerEditForm(r *http.Request, current *ServerDetailView) ServerEditForm {
	form := ServerEditForm{CredentialAction: "keep"}
	if revision, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("revision")), 10, 64); err == nil {
		form.ExpectedRevision = revision
	}
	_ = r.ParseForm()
	changedString := func(submitted, current string) *string {
		if submitted != current {
			return &submitted
		}
		return nil
	}
	if v := r.PostFormValue("name"); v != "" {
		form.Name = changedString(v, current.Name)
	}
	if v := r.PostFormValue("host"); v != "" {
		form.Host = changedString(v, current.Host)
	}
	if v := r.PostFormValue("user"); v != "" {
		form.User = changedString(v, current.User)
	}
	if v := r.PostFormValue("shell"); v != "" {
		form.Shell = changedString(v, current.Shell)
	}
	form.Description = changedString(r.PostFormValue("description"), current.Description)
	form.Location = changedString(r.PostFormValue("location"), current.Location)
	form.Hardware = changedString(r.PostFormValue("hardware"), current.Hardware)
	form.Services = changedString(r.PostFormValue("services"), current.Services)
	form.Role = changedString(r.PostFormValue("role"), current.Role)
	form.Caveats = changedString(r.PostFormValue("special_handling"), current.Caveats)
	if tags := parseTags(r.PostFormValue("tags")); strings.Join(tags, ",") != strings.Join(current.Tags, ",") {
		form.Tags = tags
	}
	if port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port"))); err == nil && port > 0 && port != current.Port {
		form.Port = &port
	}
	expose := r.PostFormValue("expose_host") == "1"
	if expose != current.ExposeHost {
		form.ExposeHost = &expose
	}
	action := r.PostFormValue("credential_action")
	switch action {
	case "password", "key", "key-passphrase", "clear":
		form.CredentialAction = action
	}

	form.Password = r.PostFormValue("password")
	form.KeyContent = r.PostFormValue("key")
	form.KeyPassphrase = r.PostFormValue("key_passphrase")
	form.SudoPassword = r.PostFormValue("sudo_password")
	return form
}

// serverFormWritesCredential 判断一次编辑提交是否写入密码、私钥、私钥口令
// 或 sudo 密码。
func serverFormWritesCredential(form ServerEditForm) bool {
	if form.SudoPassword != "" {
		return true
	}
	switch form.CredentialAction {
	case "password":
		return form.Password != ""
	case "key":
		return form.KeyContent != ""
	case "key-passphrase":
		return form.KeyPassphrase != ""
	case "clear":
		return true
	}
	return false
}

func parseTags(raw string) []string {
	var tags []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			tags = append(tags, item)
		}
	}
	return tags
}

// handleServerDelete 渲染删除预览或处理确认提交。
func (h *handler) handleServerDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == http.MethodPost {
		if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
			http.Redirect(w, r, reverifyPath("/ui/servers/"+url.PathEscape(id)+"/delete"), http.StatusSeeOther)
			return
		}
		digest := r.PostFormValue("digest")
		if digest == "" {
			h.renderError(w, r, http.StatusBadRequest, "删除服务器失败", "缺少预览摘要，请重新查看删除预览。")
			return
		}
		if err := h.cfg.ServerWriter.DeleteServer(id, digest, ServerWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
			var changed *ServerDeletePreviewChanged
			if errors.As(err, &changed) && changed.Preview != nil {
				h.renderPage(w, r, http.StatusConflict, "servers-delete.tmpl", pageData{
					Title:               "删除 " + changed.Preview.ServerName,
					ServerDeletePreview: changed.Preview,
					ServerDeleteNotice:  "删除预览已变化，未删除任何数据。以下是最新的影响预览，请核对后重新确认。",
				})
				return
			}
			if errors.Is(err, ErrServerNotFound) {
				h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
				return
			}
			h.renderError(w, r, http.StatusInternalServerError, "删除服务器失败", "删除服务器失败，请稍后重试。")
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "删除确认页只接受 GET 与 POST 请求。")
		return
	}
	preview, err := h.cfg.ServerWriter.DeleteServerPreview(id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取删除预览失败", "读取删除预览失败，请稍后重试。")
		return
	}
	if preview == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers-delete.tmpl", pageData{
		Title: "删除 " + preview.ServerName, ServerDeletePreview: preview,
	})
}

// handleServerImport 是导入第一步：填写运行 Web UI 进程所在机器上的 SSH 配
// 置路径。
func (h *handler) handleServerImport(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != serversImportPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "导入页只接受 GET 请求。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers-import.tmpl", pageData{Title: "导入 SSH 配置"})
}

// handleServerImportPlan 是导入第二步：候选清单，默认全选。POST 提交路径；
// GET 携带路径与 profile 查询参数时恢复同一张候选页。
func (h *handler) handleServerImportPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		path := strings.TrimSpace(r.URL.Query().Get("path"))
		if path == "" {
			http.Redirect(w, r, serversImportPath, http.StatusSeeOther)
			return
		}
		h.renderServerImportPlan(w, r, path, r.URL.Query().Get("profile"))
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "候选清单只接受 GET 与 POST 请求。")
		return
	}
	h.renderServerImportPlan(w, r, strings.TrimSpace(r.PostFormValue("path")), r.PostFormValue("profile"))
}

// renderServerImportPlan 解析 SSH 配置并渲染候选页；GET 用于重新验证后按原
// 路径与 profile 恢复表单。
func (h *handler) renderServerImportPlan(w http.ResponseWriter, r *http.Request, path, profile string) {
	if path == "" {
		h.renderError(w, r, http.StatusBadRequest, "读取 SSH 配置失败", "SSH 配置路径不能为空。")
		return
	}
	plan, err := h.cfg.ServerWriter.ImportPlan(path, profile)
	if err != nil {
		h.renderPage(w, r, http.StatusBadRequest, "servers-import-plan.tmpl", pageData{
			Title: "导入 SSH 配置", ServerImportError: err.Error(),
		})
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers-import-plan.tmpl", pageData{
		Title: "导入候选", ServerImportPlan: plan,
	})
}

// handleServerImportRun 是导入第三步：执行并显示四类计数与补全入口。
func (h *handler) handleServerImportRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "导入执行只接受 POST 请求。")
		return
	}
	path := strings.TrimSpace(r.PostFormValue("path"))
	if path == "" {
		h.renderError(w, r, http.StatusBadRequest, "导入失败", "SSH 配置路径不能为空。")
		return
	}
	profile := r.PostFormValue("profile")
	if profile != "" && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		resume := url.Values{"path": {path}, "profile": {profile}}
		http.Redirect(w, r, reverifyPath(serversPlanPath+"?"+resume.Encode()), http.StatusSeeOther)
		return
	}
	run, err := h.cfg.ServerWriter.ImportRun(path, r.PostForm["candidate"], profile, ServerWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "导入失败", err.Error())
		return
	}
	queue := strings.Join(run.ImportedIDs, ",")
	h.renderPage(w, r, http.StatusOK, "servers-import-run.tmpl", pageData{
		Title: "导入结果", ServerImportRun: run, ServerCompleteQueue: queue,
		ImportedCount: run.Imported, SkippedCount: run.Skipped, FailedCount: run.Failed,
	})
}

// handleServerComplete 是逐台补全表单；队列与当前位置都在链接和表单里，服
// 务端不保存补全会话。
func (h *handler) handleServerComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		h.handleServerCompletePost(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "补全页只接受 GET 与 POST 请求。")
		return
	}
	queue, position := completeQueueFromRequest(r)
	h.renderCompleteForm(w, r, queue, position, "", nil, http.StatusOK)
}

func completeQueueFromRequest(r *http.Request) (queue []string, position int) {
	queue = parseCompleteQueue(r.URL.Query().Get("queue"))
	position, _ = strconv.Atoi(r.URL.Query().Get("position"))
	if position < 0 {
		position = 0
	}
	return queue, position
}

// parseCompleteQueue 把链接或表单里的补全队列参数拆成服务器标识符列表。
func parseCompleteQueue(raw string) []string {
	var queue []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			queue = append(queue, item)
		}
	}
	return queue
}

// completeCounts 是随补全队列一起传递的导入计数；「待补」始终按当前数据重
// 新计算，另外三个数字来自刚完成的导入或上一次页面。
type completeCounts struct {
	Imported int
	Skipped  int
	Failed   int
}

// requestCounts 从 POST 表单或 GET 查询读取导入计数；补全队列是无状态的，
// 计数只能跟着链接与表单走。
func requestCounts(r *http.Request) completeCounts {
	value := func(name string) int {
		if v := r.PostFormValue(name); v != "" {
			n, _ := strconv.Atoi(v)
			return n
		}
		n, _ := strconv.Atoi(r.URL.Query().Get(name))
		return n
	}
	counts := completeCounts{Imported: value("imported"), Skipped: value("skipped"), Failed: value("failed")}
	if counts.Imported < 0 {
		counts.Imported = 0
	}
	if counts.Skipped < 0 {
		counts.Skipped = 0
	}
	if counts.Failed < 0 {
		counts.Failed = 0
	}
	return counts
}

type serverCompleteView struct {
	Detail            *ServerDetailView
	Queue             []string
	Position          int
	ShowPassword      bool
	ShowKeyPassphrase bool
	SharedKeyServers  []string
	NeedsReverify     bool
	ReverifyPath      string
	Notice            string
	Imported          int
	Skipped           int
	Failed            int
}

func (h *handler) renderCompleteForm(w http.ResponseWriter, r *http.Request, queue []string, position int, notice string, submitted *ServerCompleteForm, status int) {
	if position >= len(queue) {
		http.Redirect(w, r, completeResultPath(queue, requestCounts(r)), http.StatusSeeOther)
		return
	}
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), queue[position])
	if err != nil || detail == nil {
		h.renderError(w, r, http.StatusNotFound, "读取补全目标失败", "补全目标服务器不存在，请从服务器列表重新进入补全。")
		return
	}
	if submitted != nil {
		// 校验失败后重新渲染时保留 owner 刚输入的元数据；凭据输入一律不
		// 回显。
		shown := *detail
		shown.Description = submitted.Description
		shown.Location = submitted.Location
		shown.Hardware = submitted.Hardware
		shown.Services = submitted.Services
		shown.Role = submitted.Role
		shown.Caveats = submitted.Caveats
		detail = &shown
	}
	counts := requestCounts(r)
	view := &serverCompleteView{
		Detail: detail, Queue: queue, Position: position, Notice: notice,
		ShowPassword:      detail.CredentialType == "",
		ShowKeyPassphrase: containsString(detail.AttentionReasons, models.ReasonNeedsPassphrase),
		Imported:          counts.Imported, Skipped: counts.Skipped, Failed: counts.Failed,
	}
	if view.ShowKeyPassphrase {
		if names, err := h.cfg.ServerWriter.SharedCredentialServers(detail.ID); err == nil {
			view.SharedKeyServers = names
		}
		if view.SharedKeyServers == nil {
			view.SharedKeyServers = []string{detail.Name}
		}
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		view.NeedsReverify = true
		view.ReverifyPath = reverifyPath(completeResumePath(queue, position, counts))
	}
	h.renderPage(w, r, status, "servers-complete.tmpl", pageData{
		Title: "补全 " + detail.Name, ServerComplete: view, ServerCompleteQueue: strings.Join(queue, ","),
	})
}

func (h *handler) handleServerCompletePost(w http.ResponseWriter, r *http.Request) {
	queue := parseCompleteQueue(r.PostFormValue("queue"))
	position, _ := strconv.Atoi(r.PostFormValue("position"))
	counts := requestCounts(r)
	if position < 0 || position >= len(queue) {
		http.Redirect(w, r, completeResultPath(queue, counts), http.StatusSeeOther)
		return
	}
	switch r.PostFormValue("action") {
	case "end":
		http.Redirect(w, r, completeResultPath(queue, counts), http.StatusSeeOther)
		return
	case "skip":
		http.Redirect(w, r, completeResumePath(queue, position+1, counts), http.StatusSeeOther)
		return
	}
	id := queue[position]
	form := ServerCompleteForm{
		Description: r.PostFormValue("description"), Location: r.PostFormValue("location"),
		Hardware: r.PostFormValue("hardware"), Services: r.PostFormValue("services"),
		Role: r.PostFormValue("role"), Caveats: r.PostFormValue("special_handling"),
		SudoPassword: r.PostFormValue("sudo_password"), Password: r.PostFormValue("password"),
		KeyPassphrase: r.PostFormValue("key_passphrase"),
	}
	if (form.Password != "" || form.KeyPassphrase != "" || form.SudoPassword != "") &&
		h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath(completeResumePath(queue, position, counts)), http.StatusSeeOther)
		return
	}
	if _, err := h.cfg.ServerWriter.CompleteServer(id, form, ServerWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
		var formErr *ServerFormError
		if errors.As(err, &formErr) {
			h.renderCompleteForm(w, r, queue, position, formErr.Message, &form, http.StatusBadRequest)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "保存补全失败", "保存补全失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, completeResumePath(queue, position+1, counts), http.StatusSeeOther)
}

// handleServerCompleteResult 是补全结果页；「待补」按当前数据重新计算。
func (h *handler) handleServerCompleteResult(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != serversResultPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "补全结果页只接受 GET 请求。")
		return
	}
	queue, _ := completeQueueFromRequest(r)
	pending := 0
	for _, id := range queue {
		if detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id); err == nil && detail != nil && len(detail.AttentionReasons) > 0 {
			pending++
		}
	}
	counts := requestCounts(r)
	h.renderPage(w, r, http.StatusOK, "servers-complete-result.tmpl", pageData{
		Title: "补全结果", ImportedCount: counts.Imported,
		SkippedCount: counts.Skipped, FailedCount: counts.Failed,
		PendingCount: pending,
	})
}

func completeResumePath(queue []string, position int, counts completeCounts) string {
	values := completeCountValues(counts)
	values.Set("queue", strings.Join(queue, ","))
	values.Set("position", strconv.Itoa(position))
	return serversCompletePath + "?" + values.Encode()
}

func completeResultPath(queue []string, counts completeCounts) string {
	values := completeCountValues(counts)
	values.Set("queue", strings.Join(queue, ","))
	return serversResultPath + "?" + values.Encode()
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// completeCountValues 把导入计数编成查询参数，补全队列的每一步都带着它。
func completeCountValues(counts completeCounts) url.Values {
	return url.Values{
		"imported": {strconv.Itoa(counts.Imported)},
		"skipped":  {strconv.Itoa(counts.Skipped)},
		"failed":   {strconv.Itoa(counts.Failed)},
	}
}
