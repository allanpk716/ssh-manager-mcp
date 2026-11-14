package webui

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	secretsPath        = "/ui/secrets/"
	secretsNewPath     = "/ui/secrets/new"
	secretsMatrixPath  = "/ui/secrets/matrix"
	secretsPreviewPath = "/ui/secrets/authorization/preview"
	secretsConfirmPath = "/ui/secrets/authorization/confirm"
	secretsValueSuffix = "/value"

	// secretValueLimit 是机密项值本身的上限。请求体另留多部分表单的边界、
	// 其他字段与文件名开销，超过即在读入值之前拒绝。解析多部分表单时的
	// 内存阈值使用同一个请求体上限，保证文件部分永远不写磁盘临时文件。
	secretValueLimit           = 64 * 1024
	secretUploadFormOverhead   = 16 * 1024
	secretUploadMaxRequestBody = secretValueLimit + secretUploadFormOverhead
)

func isSecretValueUpload(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == secretsNewPath ||
		(strings.HasPrefix(r.URL.Path, secretsPath) && strings.HasSuffix(r.URL.Path, secretsValueSuffix))
}

// SecretAuthorizationCell 是页面提交的一个目标授权格：可见范围内的一个机
// 密项与一台服务器条目的最终状态。
type SecretAuthorizationCell struct {
	SecretID string
	ServerID string
	Allow    bool
}

// SecretAuthorizationServer 是授权差异里的一台服务器条目。
type SecretAuthorizationServer struct {
	ID   string
	Name string
}

// SecretAuthorizationChange 是一个机密项的差异。
type SecretAuthorizationChange struct {
	SecretID   string
	SecretName string
	// DisplayName 覆盖差异表第一列的默认机密项名；共享存储反向编辑用它
	// 逐行显示共享名，避免 owner 必须记忆挂载密码机密项名字。
	DisplayName string
	Added       []SecretAuthorizationServer
	Removed     []SecretAuthorizationServer
}

// SecretAuthorizationPreview 是授权预览与确认摘要。
type SecretAuthorizationPreview struct {
	Summary      string
	Changes      []SecretAuthorizationChange
	TotalAdded   int
	TotalRemoved int
}

// SecretAuthorizationConflict 表示预览之后相关授权被其他入口修改。
type SecretAuthorizationConflict struct {
	Latest SecretAuthorizationPreview
}

// SecretApplyResult 表示一次授权确认的结果。
type SecretApplyResult struct {
	Preview SecretAuthorizationPreview
	Applied bool
}

// ErrSecretMetadataNoChange 表示 Web 表单提交的备注与环境变量名都与当前
// 值相同；适配层不写数据也不写审计，页面提示没有变化。
var ErrSecretMetadataNoChange = errors.New("机密项备注与环境变量名都没有变化")

// SecretFormError 表示机密项表单的提交内容校验失败。Message 是可直接展示
// 的存储层校验文案；适配层负责把内部错误与本类型区分开。
type SecretFormError struct {
	Message string
}

func (e *SecretFormError) Error() string { return e.Message }

// secretFormMessage 提取可直接展示的机密项校验文案。只有适配层标记过的表
// 单错误保留原文；其余内部错误返回 false，调用方必须改用通用文案。
func secretFormMessage(err error) (string, bool) {
	var formErr *SecretFormError
	if errors.As(err, &formErr) {
		return formErr.Message, true
	}
	return "", false
}

// SecretDetailView 是机密项详情页展示数据。
type SecretDetailView struct {
	Row             SecretRow
	Servers         []AccessGrantChoice
	DeliveryRecords []SecretDeliveryRecordRow
}

// SecretMatrixView 是机密项与服务器条目的授权矩阵。
type SecretMatrixView struct {
	Query   string
	Server  string
	Rows    []SecretMatrixRow
	Servers []ServerRow
}

// SecretMatrixRow 是一个机密项在矩阵中的整行。
type SecretMatrixRow struct {
	Secret SecretRow
	Cells  []AccessGrantChoice
}

// SecretWriter 是机密项页写操作来源。接口只接受写入值，不提供任何读值
// 方法。
type SecretWriter interface {
	AddSecret(name, note, environmentName string, value []byte, source SecretWriteSource) error
	SetSecretValue(identifier string, value []byte, source SecretWriteSource) error
	EditSecretMetadata(identifier, note, environmentName string, source SecretWriteSource) error
	DeleteSecret(identifier string, source SecretWriteSource) error
	PreviewAuthorization(target []SecretAuthorizationCell) (SecretAuthorizationPreview, error)
	ApplyAuthorization(target []SecretAuthorizationCell, summary string, source SecretWriteSource) (SecretApplyResult, *SecretAuthorizationConflict, error)
}

// registerSecretRoutes 注册机密项页路由。客户端没有写来源，只注册列表等
// 只读页面。
func (h *handler) registerSecretRoutes() {
	if h.cfg.Secrets == nil {
		return
	}
	if h.cfg.SecretsWriter == nil {
		h.mux.HandleFunc(secretsPath, h.requireSession(h.handleSecretReadRoutes))
		return
	}
	h.mux.HandleFunc(secretsNewPath, h.requireSession(h.handleSecretNew))
	h.mux.HandleFunc(secretsMatrixPath, h.requireSession(h.handleSecretMatrix))
	h.mux.HandleFunc(secretsPreviewPath, h.requireSession(h.handleSecretAuthorizationPreview))
	h.mux.HandleFunc(secretsConfirmPath, h.requireSession(h.handleSecretAuthorizationConfirm))
	h.mux.HandleFunc(secretsPath, h.requireSession(h.handleSecretDetailRoutes))
}

func (h *handler) handleSecretReadRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == secretsPath {
		h.handleSecretsRoot(w, r)
		return
	}
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleSecretsRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != secretsPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "机密项列表只接受 GET 请求。")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	server := strings.TrimSpace(r.URL.Query().Get("server"))
	rows, err := h.cfg.Secrets.ListSecrets(h.requestClientInstance(r), query, server)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取机密项列表失败", "读取机密项列表失败，请稍后重试。")
		return
	}
	var servers []ServerRow
	if h.cfg.Role == RoleClient {
		servers = secretServersFromRows(rows)
	} else {
		servers, err = h.cfg.Servers.Servers(h.requestClientInstance(r))
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请稍后重试。")
			return
		}
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	h.renderPage(w, r, http.StatusOK, "secrets.tmpl", pageData{
		Title: "机密项", Secrets: rows, SecretFilterQuery: query,
		SecretFilterServer: server, SecretServerOptions: servers,
	})
}

func secretServersFromRows(rows []SecretRow) []ServerRow {
	seen := make(map[string]bool)
	servers := make([]ServerRow, 0)
	for _, row := range rows {
		for _, name := range row.AllowedServers {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			servers = append(servers, ServerRow{Name: name})
		}
	}
	return servers
}

func (h *handler) handleSecretNew(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != secretsNewPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		view := secretFormView{
			Name: r.URL.Query().Get("name"), Note: r.URL.Query().Get("note"),
			EnvironmentName: r.URL.Query().Get("environment_name"),
		}
		if r.URL.Query().Get("resume") == "1" && h.cfg.Role == RoleServer {
			view.NeedsReverify = !h.recentlyVerified(h.currentSession(r))
			view.ReverifyPath = reverifyPath(secretNewResumePath(view))
		}
		h.renderPage(w, r, http.StatusOK, "secret-new.tmpl", pageData{
			Title: "新增机密项", SecretForm: view,
		})
	case http.MethodPost:
		h.handleSecretNewPost(w, r)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增机密项页只接受 GET 与 POST 请求。")
	}
}

type secretFormView struct {
	Name            string
	Note            string
	EnvironmentName string
	Error           string
	NeedsReverify   bool
	ReverifyPath    string
}

var errSecretUploadTooLarge = errors.New("上传文件超过 64 KiB")

func (h *handler) handleSecretNewPost(w http.ResponseWriter, r *http.Request) {
	value, valueErr := secretValueFromRequest(r)
	form := secretFormView{
		Name: strings.TrimSpace(r.PostFormValue("name")), Note: r.PostFormValue("note"),
		EnvironmentName: strings.TrimSpace(r.PostFormValue("environment_name")),
	}
	if valueErr != nil {
		form.Error = valueErr.Error()
		status := http.StatusBadRequest
		if valueErr == errSecretUploadTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		h.renderSecretNewForm(w, r, status, form)
		return
	}
	if h.redirectSecretWriteToReverify(w, r, secretNewResumePath(form)) {
		return
	}
	if err := h.cfg.SecretsWriter.AddSecret(
		form.Name, form.Note, form.EnvironmentName, value,
		SecretWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		if message, ok := secretFormMessage(err); ok {
			form.Error = message
			h.renderSecretNewForm(w, r, http.StatusBadRequest, form)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "新增机密项失败", "新增机密项失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, secretsPath, http.StatusSeeOther)
}

func (h *handler) renderSecretNewForm(w http.ResponseWriter, r *http.Request, status int, form secretFormView) {
	h.renderPage(w, r, status, "secret-new.tmpl", pageData{Title: "新增机密项", SecretForm: form})
}

func secretNewResumePath(form secretFormView) string {
	values := url.Values{
		"name": {form.Name}, "note": {form.Note},
		"environment_name": {form.EnvironmentName}, "resume": {"1"},
	}
	return secretsNewPath + "?" + values.Encode()
}

func secretValueFromRequest(r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(secretUploadMaxRequestBody); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, errSecretUploadTooLarge
			}
			return nil, errors.New("读取表单失败，请重试")
		}
		file, header, err := r.FormFile("value_file")
		if err == nil {
			defer file.Close()
			if r.PostFormValue("value") != "" {
				return nil, errors.New("隐藏输入框与上传文件不能同时提供")
			}
			if header.Size > secretValueLimit {
				return nil, errSecretUploadTooLarge
			}
			value, err := io.ReadAll(io.LimitReader(file, secretValueLimit+1))
			if err != nil {
				return nil, errors.New("读取上传文件失败，请重试")
			}
			if len(value) > secretValueLimit {
				return nil, errSecretUploadTooLarge
			}
			return value, nil
		}
	}
	return []byte(r.PostFormValue("value")), nil
}

func (h *handler) handleSecretMatrix(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != secretsMatrixPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "授权矩阵只接受 GET 请求。")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	serverFilter := strings.TrimSpace(r.URL.Query().Get("server"))
	secrets, err := h.cfg.Secrets.ListSecrets("", query, "")
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取机密项失败", "读取机密项失败，请稍后重试。")
		return
	}
	allServers, err := h.cfg.Servers.Servers("")
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请稍后重试。")
		return
	}
	servers := allServers
	if serverFilter != "" {
		servers = nil
		for _, server := range allServers {
			if server.ID == serverFilter || server.Name == serverFilter {
				servers = append(servers, server)
			}
		}
		if len(servers) == 0 {
			h.renderError(w, r, http.StatusBadRequest, "服务器不存在", "服务器条目 "+serverFilter+" 不存在")
			return
		}
	}
	sort.SliceStable(allServers, func(i, j int) bool { return allServers[i].Name < allServers[j].Name })
	rows := make([]SecretMatrixRow, 0, len(secrets))
	for _, secret := range secrets {
		allowed := secretAllowedServerSet(secret)
		row := SecretMatrixRow{Secret: secret}
		for _, server := range servers {
			row.Cells = append(row.Cells, AccessGrantChoice{
				ID: server.ID, Name: server.Name, Checked: allowed[server.Name],
			})
		}
		rows = append(rows, row)
	}
	h.renderPage(w, r, http.StatusOK, "secret-matrix.tmpl", pageData{
		Title: "机密项授权矩阵", SecretMatrix: &SecretMatrixView{
			Query: query, Server: serverFilter, Rows: rows, Servers: servers,
		}, SecretServerOptions: allServers,
	})
}

func (h *handler) handleSecretAuthorizationPreview(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != secretsPreviewPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		target := secretTargetFromCells(r)
		h.renderSecretPreview(w, r, target, safeSecretReturnPath(r.PostFormValue("return_to")), "",
			r.PostFormValue("authorization_view"), r.PostFormValue("server_id"),
			r.PostFormValue("share_id"), nil)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "授权预览只接受 GET 与 POST 请求。")
		return
	}
	target, err := parseSecretTargets(r.URL.Query()["target"])
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "授权预览失败", err.Error())
		return
	}
	h.renderSecretPreview(w, r, target, safeSecretReturnPath(r.URL.Query().Get("return_to")), "",
		r.URL.Query().Get("authorization_view"), r.URL.Query().Get("server_id"),
		r.URL.Query().Get("share_id"),
		stringSet(r.URL.Query()["unmount"]))
}

func (h *handler) handleSecretAuthorizationConfirm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != secretsConfirmPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "确认授权只接受 POST 请求。")
		return
	}
	target, err := parseSecretTargets(r.PostForm["target"])
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "确认授权失败", err.Error())
		return
	}
	view := r.PostFormValue("authorization_view")
	serverID := r.PostFormValue("server_id")
	shareID := r.PostFormValue("share_id")
	unmountServers := trimmedFormValues(r.PostForm["unmount_server"])
	selectedUnmount := stringSet(unmountServers)
	if h.redirectSecretWriteToReverify(w, r, secretAuthorizationResumePath(
		target, r.PostFormValue("return_to"), view, serverID, shareID, unmountServers,
	)) {
		return
	}
	unmountTargets := h.shareUnmountTargetsForAuthorization(
		r, safeSecretReturnPath(r.PostFormValue("return_to")), target, selectedUnmount,
	)
	result, conflict, err := h.cfg.SecretsWriter.ApplyAuthorization(
		target, r.PostFormValue("summary"), SecretWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "确认授权失败", "确认授权失败，请稍后重试。")
		return
	}
	if conflict != nil {
		latest, displayErr := h.authorizationPreviewDisplay(r, conflict.Latest, view, serverID, shareID)
		if displayErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, "确认授权失败", "确认授权失败，请稍后重试。")
			return
		}
		h.renderSecretPreviewPage(w, r, http.StatusConflict, latest, target,
			safeSecretReturnPath(r.PostFormValue("return_to")),
			view, serverID, shareID,
			"机密项授权状态已被其他入口修改，请核对最新预览后再确认。", selectedUnmount)
		return
	}
	if result.Applied && len(unmountTargets) > 0 {
		taskIDs := h.startAuthorizationUnmountTasks(
			r, safeSecretReturnPath(r.PostFormValue("return_to")), unmountTargets,
		)
		if len(taskIDs) > 0 {
			http.Redirect(w, r, shareUnmountBatchPath(unmountTargets[0].ShareID, taskIDs), http.StatusSeeOther)
			return
		}
	}
	notice := "没有变化，未写入数据或审计。"
	if result.Applied {
		notice = "已保存授权变更。"
	}
	preview, displayErr := h.authorizationPreviewDisplay(r, result.Preview, view, serverID, shareID)
	if displayErr != nil {
		h.renderError(w, r, http.StatusInternalServerError, "确认授权失败", "确认授权失败，请稍后重试。")
		return
	}
	h.renderSecretPreviewPage(w, r, http.StatusOK, preview, target,
		safeSecretReturnPath(r.PostFormValue("return_to")), view, serverID, shareID, notice, nil)
}

// shareUnmountTargetsForAuthorization 计算确认授权时允许联动卸载的服务
// 器：必须来自共享存储详情页的授权编辑、确实在目标集合里被移除、最近
// 现场探测已挂载或状态未知，且不是 Windows 服务器。伪造的勾选值不在计
// 算结果里。
func (h *handler) shareUnmountTargetsForAuthorization(
	r *http.Request, returnTo string, target []SecretAuthorizationCell, selected map[string]bool,
) []SecretAuthorizationShareUnmount {
	if !h.caps.CanUnmountShares || len(selected) == 0 {
		return nil
	}
	detail := h.shareFromAuthorizationReturnTo(r, returnTo)
	if detail == nil {
		return nil
	}
	preview, err := h.cfg.SecretsWriter.PreviewAuthorization(target)
	if err != nil {
		return nil
	}
	choices := h.shareUnmountChoicesForAuthorization(r, returnTo, preview, selected)
	targets := make([]SecretAuthorizationShareUnmount, 0, len(choices))
	for _, choice := range choices {
		if choice.Selected && (choice.Mounted || choice.Unknown) {
			targets = append(targets, choice)
		}
	}
	return targets
}

// startAuthorizationUnmountTasks 对确认联动卸载的服务器逐台启动单台卸载
// 任务，返回任务标识列表。卸载全部复用单台卸载任务机制。
func (h *handler) startAuthorizationUnmountTasks(
	r *http.Request, returnTo string, targets []SecretAuthorizationShareUnmount,
) []string {
	taskIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		taskID, err := h.cfg.ShareUnmountRunner.StartShareUnmount(ShareUnmountRequest{
			ShareID: target.ShareID, ShareName: target.ShareName,
			ServerID: target.ServerID, ServerName: target.ServerName,
			Source: ShareWriteSource{RemoteAddress: remoteIP(r)},
		})
		if err != nil {
			h.logError(r, "启动可挂载范围联动卸载任务", err)
			continue
		}
		taskIDs = append(taskIDs, taskID)
	}
	return taskIDs
}

// redirectSecretWriteToReverify 在权威端且最近未重新验证时，把机密项写操作
// 跳到重新验证入口并保留 resumePath 描述的提交状态；其他角色直接放行。
func (h *handler) redirectSecretWriteToReverify(w http.ResponseWriter, r *http.Request, resumePath string) bool {
	if h.cfg.Role != RoleServer || h.recentlyVerified(h.currentSession(r)) {
		return false
	}
	http.Redirect(w, r, reverifyPath(resumePath), http.StatusSeeOther)
	return true
}

// secretAuthorizationResumePath 构造确认授权跳转重新验证后要回到的预览地
// 址，保留返回路径、全部目标授权格、服务器详情视图参数与已勾选的同时卸
// 载服务器。
func secretAuthorizationResumePath(
	target []SecretAuthorizationCell, returnTo, view, serverID, shareID string, unmountServers []string,
) string {
	next := url.Values{"return_to": {safeSecretReturnPath(returnTo)}}
	if view != "" {
		next.Set("authorization_view", view)
	}
	if serverID != "" {
		next.Set("server_id", serverID)
	}
	if shareID != "" {
		next.Set("share_id", shareID)
	}
	for _, cell := range target {
		next.Add("target", encodeSecretTarget(cell))
	}
	for _, unmountServerID := range unmountServers {
		if unmountServerID != "" {
			next.Add("unmount", unmountServerID)
		}
	}
	return secretsPreviewPath + "?" + next.Encode()
}

func (h *handler) renderSecretPreview(
	w http.ResponseWriter, r *http.Request, target []SecretAuthorizationCell,
	returnTo, notice, view, serverID, shareID string, selectedUnmount map[string]bool,
) {
	preview, err := h.cfg.SecretsWriter.PreviewAuthorization(target)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "生成授权预览失败", err.Error())
		return
	}
	display, err := h.authorizationPreviewDisplay(r, preview, view, serverID, shareID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "生成授权预览失败", "生成授权预览失败，请稍后重试。")
		return
	}
	h.renderSecretPreviewPage(w, r, http.StatusOK, display, target, returnTo, view, serverID, shareID,
		notice, selectedUnmount)
}

func (h *handler) renderSecretPreviewPage(
	w http.ResponseWriter, r *http.Request, status int, display secretAuthorizationDisplay,
	target []SecretAuthorizationCell, returnTo, view, serverID, shareID, notice string,
	selectedUnmount map[string]bool,
) {
	data := pageData{
		Title: "机密项授权预览", SecretAuthorizationPreview: display.Preview,
		SecretAuthorizationShareImpact: display.ShareImpactRows,
		SecretAuthorizationTargets:     target,
		SecretAuthorizationReturnTo:    returnTo,
		SecretAuthorizationView:        view,
		SecretAuthorizationServerID:    serverID,
		SecretAuthorizationShareID:     shareID,
		SecretAuthorizationNotice:      notice,
	}
	data.SecretAuthorizationShareUnmounts = h.shareUnmountChoicesForAuthorization(
		r, returnTo, display.Preview, selectedUnmount,
	)
	h.renderPage(w, r, status, "secret-authorization-preview.tmpl", data)
}

// stringSet 把表单里的多个值整理成集合，便于保留勾选状态。
func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return set
}

// trimmedFormValues 去掉表单多值字段的空白并过滤空值。
func trimmedFormValues(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			trimmed = append(trimmed, strings.TrimSpace(value))
		}
	}
	return trimmed
}

func secretTargetFromCells(r *http.Request) []SecretAuthorizationCell {
	secretIDs := r.PostForm["secret_id"]
	serverIDs := r.PostForm["server_id"]
	cells := make([]SecretAuthorizationCell, 0, len(secretIDs)*len(serverIDs))
	for _, secretID := range secretIDs {
		for _, serverID := range serverIDs {
			cells = append(cells, SecretAuthorizationCell{
				SecretID: secretID, ServerID: serverID,
				Allow: r.PostFormValue(secretCellName(secretID, serverID)) == "1",
			})
		}
	}
	return cells
}

func secretCellName(secretID, serverID string) string {
	return "cell__" + secretID + "__" + serverID
}

func encodeSecretTarget(cell SecretAuthorizationCell) string {
	allow := "0"
	if cell.Allow {
		allow = "1"
	}
	return cell.SecretID + " " + cell.ServerID + " " + allow
}

func parseSecretTargets(values []string) ([]SecretAuthorizationCell, error) {
	cells := make([]SecretAuthorizationCell, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, " ")
		if len(parts) != 3 || (parts[2] != "0" && parts[2] != "1") || parts[0] == "" || parts[1] == "" {
			return nil, errors.New("授权目标格式不正确")
		}
		cells = append(cells, SecretAuthorizationCell{
			SecretID: parts[0], ServerID: parts[1], Allow: parts[2] == "1",
		})
	}
	return cells, nil
}

func safeSecretReturnPath(path string) string {
	if strings.HasPrefix(path, "/ui/") && !strings.HasPrefix(path, "//") {
		return path
	}
	return secretsMatrixPath
}

func (h *handler) handleSecretDetailRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == secretsPath {
		h.handleSecretsRoot(w, r)
		return
	}
	identifier := strings.TrimPrefix(r.URL.Path, secretsPath)
	suffix := ""
	if index := strings.LastIndex(identifier, "/"); index >= 0 {
		suffix = identifier[index+1:]
		identifier = identifier[:index]
	}
	if identifier == "" || strings.Contains(identifier, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch suffix {
	case "":
		h.handleSecretDetail(w, r, identifier)
	case "edit":
		h.handleSecretMetadataEdit(w, r, identifier)
	case "value":
		h.handleSecretValue(w, r, identifier)
	case "delete":
		h.handleSecretDelete(w, r, identifier)
	default:
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	}
}

// FindSecretRow 按标识符或名字在机密项展示行里查找目标；两条入口（页面详
// 情与 Web 写适配层）共用这一份匹配规则。
func FindSecretRow(rows []SecretRow, identifier string) (SecretRow, bool) {
	for i := range rows {
		if rows[i].ID == identifier || rows[i].Name == identifier {
			return rows[i], true
		}
	}
	return SecretRow{}, false
}

// secretAllowedServerSet 把一个机密项的可写入服务器条目名转成集合，供矩
// 阵与详情页勾选状态共用。
func secretAllowedServerSet(row SecretRow) map[string]bool {
	allowed := make(map[string]bool, len(row.AllowedServers))
	for _, name := range row.AllowedServers {
		allowed[name] = true
	}
	return allowed
}

func (h *handler) secretDetail(identifier string) (*SecretDetailView, error) {
	rows, err := h.cfg.Secrets.ListSecrets("", "", "")
	if err != nil {
		return nil, err
	}
	row, ok := FindSecretRow(rows, identifier)
	if !ok {
		return nil, &SecretNotFoundError{Identifier: identifier}
	}
	var deliveryRecords []SecretDeliveryRecordRow
	if source, ok := h.cfg.Secrets.(SecretDeliveryRecordSource); ok {
		deliveryRecords, err = source.SecretDeliveryRecords(row.Name)
		if err != nil {
			return nil, err
		}
	}
	servers, err := h.cfg.Servers.Servers("")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	allowed := secretAllowedServerSet(row)
	choices := make([]AccessGrantChoice, 0, len(servers))
	for _, server := range servers {
		choices = append(choices, AccessGrantChoice{
			ID: server.ID, Name: server.Name, Checked: allowed[server.Name],
		})
	}
	return &SecretDetailView{
		Row: row, Servers: choices, DeliveryRecords: deliveryRecords,
	}, nil
}

func (h *handler) handleSecretDetail(w http.ResponseWriter, r *http.Request, identifier string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "机密项详情只接受 GET 请求。")
		return
	}
	detail, err := h.secretDetail(identifier)
	if err != nil {
		h.renderSecretReadError(w, r, err)
		return
	}
	h.renderPage(w, r, http.StatusOK, "secret-detail.tmpl", pageData{
		Title: "机密项 " + detail.Row.Name, SecretDetail: detail,
	})
}

func (h *handler) renderSecretReadError(w http.ResponseWriter, r *http.Request, err error) {
	var missing *SecretNotFoundError
	if errors.As(err, &missing) {
		h.renderError(w, r, http.StatusNotFound, "机密项不存在", err.Error())
		return
	}
	h.renderError(w, r, http.StatusInternalServerError, "读取机密项失败", "读取机密项失败，请稍后重试。")
}

func (h *handler) handleSecretMetadataEdit(w http.ResponseWriter, r *http.Request, identifier string) {
	target := secretsPath + identifier
	if r.Method == http.MethodGet {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "修改元数据请从详情页进入。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "修改机密项元数据只接受 POST 请求。")
		return
	}
	note := r.PostFormValue("note")
	environmentName := strings.TrimSpace(r.PostFormValue("environment_name"))
	if err := h.cfg.SecretsWriter.EditSecretMetadata(
		identifier, note, environmentName, SecretWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		if errors.Is(err, ErrSecretMetadataNoChange) {
			detail, detailErr := h.secretDetail(identifier)
			if detailErr == nil {
				h.renderPage(w, r, http.StatusOK, "secret-detail.tmpl", pageData{
					Title: "机密项 " + detail.Row.Name, SecretDetail: detail,
					SecretDetailNotice: "没有变化，未写入数据或审计。",
				})
				return
			}
		}
		detail, detailErr := h.secretDetail(identifier)
		if message, ok := secretFormMessage(err); ok && detailErr == nil {
			h.renderPage(w, r, http.StatusBadRequest, "secret-detail.tmpl", pageData{
				Title: "机密项 " + detail.Row.Name, SecretDetail: detail,
				SecretDetailError: message, SecretDetailNote: note,
				SecretDetailEnvironmentName: environmentName,
			})
			return
		}
		if detailErr != nil {
			h.renderSecretReadError(w, r, detailErr)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "修改机密项失败", "修改机密项失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *handler) handleSecretValue(w http.ResponseWriter, r *http.Request, identifier string) {
	target := secretsPath + identifier
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		detail, err := h.secretDetail(identifier)
		if err != nil {
			h.renderSecretReadError(w, r, err)
			return
		}
		h.renderPage(w, r, http.StatusOK, "secret-value.tmpl", pageData{
			Title: "换值 " + detail.Row.Name, SecretDetail: detail,
		})
	case http.MethodPost:
		value, valueErr := secretValueFromRequest(r)
		if valueErr != nil {
			h.renderSecretValueInputError(w, r, identifier, valueErr)
			return
		}
		if h.redirectSecretWriteToReverify(w, r, target+"/value?resume=1") {
			return
		}
		if err := h.cfg.SecretsWriter.SetSecretValue(
			identifier, value, SecretWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			h.renderSecretValueWriteError(w, r, identifier, err)
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "换值页只接受 GET 与 POST 请求。")
	}
}

// renderSecretValueInputError 渲染值录入本身的错误；这些文案由页面层构造，
// 不包含内部细节，超限使用 413。
func (h *handler) renderSecretValueInputError(w http.ResponseWriter, r *http.Request, identifier string, err error) {
	detail, detailErr := h.secretDetail(identifier)
	if detailErr != nil {
		h.renderSecretReadError(w, r, detailErr)
		return
	}
	status := http.StatusBadRequest
	if errors.Is(err, errSecretUploadTooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	h.renderPage(w, r, status, "secret-value.tmpl", pageData{
		Title: "换值 " + detail.Row.Name, SecretDetail: detail, SecretDetailError: err.Error(),
	})
}

// renderSecretValueWriteError 渲染换值写操作的错误；只有表单校验错误保留
// 原文，其余内部错误使用通用文案。
func (h *handler) renderSecretValueWriteError(w http.ResponseWriter, r *http.Request, identifier string, err error) {
	detail, detailErr := h.secretDetail(identifier)
	if detailErr != nil {
		h.renderSecretReadError(w, r, detailErr)
		return
	}
	if message, ok := secretFormMessage(err); ok {
		h.renderPage(w, r, http.StatusBadRequest, "secret-value.tmpl", pageData{
			Title: "换值 " + detail.Row.Name, SecretDetail: detail, SecretDetailError: message,
		})
		return
	}
	h.renderError(w, r, http.StatusInternalServerError, "换值失败", "换值失败，请稍后重试。")
}

func (h *handler) handleSecretDelete(w http.ResponseWriter, r *http.Request, identifier string) {
	target := secretsPath + identifier
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		detail, err := h.secretDetail(identifier)
		if err != nil {
			h.renderSecretReadError(w, r, err)
			return
		}
		h.renderPage(w, r, http.StatusOK, "secret-delete.tmpl", pageData{
			Title: "删除 " + detail.Row.Name, SecretDetail: detail,
		})
	case http.MethodPost:
		if h.redirectSecretWriteToReverify(w, r, target+"/delete?resume=1") {
			return
		}
		if err := h.cfg.SecretsWriter.DeleteSecret(
			identifier, SecretWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			var missing *SecretNotFoundError
			if errors.As(err, &missing) {
				h.renderSecretReadError(w, r, err)
				return
			}
			if message, ok := secretFormMessage(err); ok {
				h.renderError(w, r, http.StatusBadRequest, "删除机密项失败", message)
				return
			}
			h.renderError(w, r, http.StatusInternalServerError, "删除机密项失败", "删除机密项失败，请稍后重试。")
			return
		}
		http.Redirect(w, r, secretsPath, http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "删除机密项页只接受 GET 与 POST 请求。")
	}
}

func formatUnixTime(value int64) string {
	if value == 0 {
		return "-"
	}
	return time.Unix(value, 0).Format("2006-01-02 15:04:05 Z07:00")
}
