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
	secretsPath         = "/ui/secrets/"
	secretsNewPath      = "/ui/secrets/new"
	secretsMatrixPath   = "/ui/secrets/matrix"
	secretsPreviewPath  = "/ui/secrets/authorization/preview"
	secretsConfirmPath  = "/ui/secrets/authorization/confirm"
	secretsPrefix       = "/ui/secrets/"
	secretsValueSuffix  = "/value"
	secretsDeleteSuffix = "/delete"

	// secretValueLimit 是机密项值本身的上限；请求体另留少量多部分表单
	// 开销，超过即在读入值之前拒绝。
	secretValueLimit           = 64 * 1024
	secretUploadMaxRequestBody = secretValueLimit + 512
)

func isSecretValueUpload(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == secretsNewPath || strings.HasSuffix(r.URL.Path, secretsValueSuffix)
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
	Added      []SecretAuthorizationServer
	Removed    []SecretAuthorizationServer
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

// SecretDetailView 是机密项详情页展示数据。
type SecretDetailView struct {
	Row     SecretRow
	Servers []AccessGrantChoice
}

// SecretMatrixView 是机密项与服务器条目的授权矩阵。
type SecretMatrixView struct {
	Query       string
	Server      string
	Rows        []SecretMatrixRow
	Servers     []ServerRow
	HasOverride bool
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
		h.mux.HandleFunc(secretsPrefix, h.requireSession(h.handleSecretReadRoutes))
		return
	}
	h.mux.HandleFunc(secretsNewPath, h.requireSession(h.handleSecretNew))
	h.mux.HandleFunc(secretsMatrixPath, h.requireSession(h.handleSecretMatrix))
	h.mux.HandleFunc(secretsPreviewPath, h.requireSession(h.handleSecretAuthorizationPreview))
	h.mux.HandleFunc(secretsConfirmPath, h.requireSession(h.handleSecretAuthorizationConfirm))
	h.mux.HandleFunc(secretsPrefix, h.requireSession(h.handleSecretDetailRoutes))
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
		h.renderError(w, r, http.StatusInternalServerError, "读取机密项列表失败", err.Error())
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
	value, valueErr := secretValueFromRequest(w, r)
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
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath(secretNewResumePath(form)), http.StatusSeeOther)
		return
	}
	if err := h.cfg.SecretsWriter.AddSecret(
		form.Name, form.Note, form.EnvironmentName, value,
		SecretWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		form.Error = err.Error()
		h.renderSecretNewForm(w, r, http.StatusBadRequest, form)
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

func secretValueFromRequest(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(secretValueLimit); err != nil {
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
	override := r.URL.Query()
	for _, secret := range secrets {
		allowed := make(map[string]bool, len(secret.AllowedServers))
		for _, name := range secret.AllowedServers {
			allowed[name] = true
		}
		row := SecretMatrixRow{Secret: secret}
		for _, server := range servers {
			checked := allowed[server.Name]
			if override.Get(secretCellName(secret.ID, server.ID)) != "" {
				checked = override.Get(secretCellName(secret.ID, server.ID)) == "1"
			}
			row.Cells = append(row.Cells, AccessGrantChoice{ID: server.ID, Name: server.Name, Checked: checked})
		}
		rows = append(rows, row)
	}
	h.renderPage(w, r, http.StatusOK, "secret-matrix.tmpl", pageData{
		Title: "机密项授权矩阵", SecretMatrix: &SecretMatrixView{
			Query: query, Server: serverFilter, Rows: rows, Servers: servers,
			HasOverride: len(override) > 0,
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
		h.renderSecretPreview(w, r, target, safeSecretReturnPath(r.PostFormValue("return_to")), "")
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
	h.renderSecretPreview(w, r, target, safeSecretReturnPath(r.URL.Query().Get("return_to")), "")
}

func (h *handler) handleSecretAuthorizationConfirm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != secretsConfirmPath || r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	target, err := parseSecretTargets(r.PostForm["target"])
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "确认授权失败", err.Error())
		return
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		next := url.Values{"return_to": {safeSecretReturnPath(r.PostFormValue("return_to"))}}
		for _, cell := range target {
			next.Add("target", encodeSecretTarget(cell))
		}
		http.Redirect(w, r, reverifyPath(secretsPreviewPath+"?"+next.Encode()), http.StatusSeeOther)
		return
	}
	result, conflict, err := h.cfg.SecretsWriter.ApplyAuthorization(
		target, r.PostFormValue("summary"), SecretWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "确认授权失败", "确认授权失败，请稍后重试。")
		return
	}
	if conflict != nil {
		h.renderSecretPreviewPage(w, r, http.StatusConflict, conflict.Latest, target,
			safeSecretReturnPath(r.PostFormValue("return_to")),
			"机密项授权状态已被其他入口修改，请核对最新预览后再确认。")
		return
	}
	notice := "没有变化，未写入数据或审计。"
	if result.Applied {
		notice = "已保存授权变更。"
	}
	h.renderSecretPreviewPage(w, r, http.StatusOK, result.Preview, target,
		safeSecretReturnPath(r.PostFormValue("return_to")), notice)
}

func (h *handler) renderSecretPreview(w http.ResponseWriter, r *http.Request, target []SecretAuthorizationCell, returnTo, notice string) {
	preview, err := h.cfg.SecretsWriter.PreviewAuthorization(target)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "生成授权预览失败", err.Error())
		return
	}
	h.renderSecretPreviewPage(w, r, http.StatusOK, preview, target, returnTo, notice)
}

func (h *handler) renderSecretPreviewPage(
	w http.ResponseWriter, r *http.Request, status int, preview SecretAuthorizationPreview,
	target []SecretAuthorizationCell, returnTo, notice string,
) {
	h.renderPage(w, r, status, "secret-authorization-preview.tmpl", pageData{
		Title: "机密项授权预览", SecretAuthorizationPreview: preview,
		SecretAuthorizationTargets: target, SecretAuthorizationReturnTo: returnTo,
		SecretAuthorizationNotice: notice,
	})
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
	identifier := strings.TrimPrefix(r.URL.Path, secretsPrefix)
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

func (h *handler) secretDetail(identifier string) (*SecretDetailView, error) {
	rows, err := h.cfg.Secrets.ListSecrets("", "", "")
	if err != nil {
		return nil, err
	}
	var row *SecretRow
	for i := range rows {
		if rows[i].ID == identifier || rows[i].Name == identifier {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return nil, &SecretNotFoundError{Identifier: identifier}
	}
	servers, err := h.cfg.Servers.Servers("")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	allowed := make(map[string]bool, len(row.AllowedServers))
	for _, name := range row.AllowedServers {
		allowed[name] = true
	}
	choices := make([]AccessGrantChoice, 0, len(servers))
	for _, server := range servers {
		choices = append(choices, AccessGrantChoice{
			ID: server.ID, Name: server.Name, Checked: allowed[server.Name],
		})
	}
	return &SecretDetailView{Row: *row, Servers: choices}, nil
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
	target := secretsPrefix + identifier
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
		detail, detailErr := h.secretDetail(identifier)
		if detailErr == nil {
			h.renderPage(w, r, http.StatusBadRequest, "secret-detail.tmpl", pageData{
				Title: "机密项 " + detail.Row.Name, SecretDetail: detail,
				SecretDetailError: err.Error(), SecretDetailNote: note,
				SecretDetailEnvironmentName: environmentName,
			})
			return
		}
		h.renderError(w, r, http.StatusBadRequest, "修改机密项失败", err.Error())
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *handler) handleSecretValue(w http.ResponseWriter, r *http.Request, identifier string) {
	target := secretsPrefix + identifier
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
		value, valueErr := secretValueFromRequest(w, r)
		if valueErr != nil {
			status := http.StatusBadRequest
			if valueErr == errSecretUploadTooLarge {
				status = http.StatusRequestEntityTooLarge
			}
			h.renderError(w, r, status, "换值失败", valueErr.Error())
			return
		}
		if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
			http.Redirect(w, r, reverifyPath(target+"/value?resume=1"), http.StatusSeeOther)
			return
		}
		if err := h.cfg.SecretsWriter.SetSecretValue(
			identifier, value, SecretWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			h.renderError(w, r, http.StatusBadRequest, "换值失败", err.Error())
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "换值页只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleSecretDelete(w http.ResponseWriter, r *http.Request, identifier string) {
	target := secretsPrefix + identifier
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
		if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
			http.Redirect(w, r, reverifyPath(target+"/delete?resume=1"), http.StatusSeeOther)
			return
		}
		if err := h.cfg.SecretsWriter.DeleteSecret(
			identifier, SecretWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			h.renderError(w, r, http.StatusBadRequest, "删除机密项失败", err.Error())
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
