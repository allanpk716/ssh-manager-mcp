package webui

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

const (
	sharesPath    = "/ui/shares/"
	sharesNewPath = "/ui/shares/new"

	// sharesSMBPrerequisites 与命令行 shares add 的新增前提提示保持同一
	// 份文案口径。
	sharesSMBPrerequisites = "前提：首版只支持 SMB；提供方设备需关闭『禁止同一 IP 多个连接』类限制。"
)

// ShareRow 是共享存储列表的一行展示数据，只含可见元数据；挂载密码的值
// 永远不出现在这里。
type ShareRow struct {
	ID               string
	Name             string
	DeviceName       string
	Drive            string
	Version          int64
	MountableServers []ShareServerView
	MountableCount   int
	PasswordSecret   string
	MountUser        string
	Directories      []ShareDirectoryView
	Note             string
	ChangeNote       string
}

// ShareDirectoryView 是一条目录约定的展示行。
type ShareDirectoryView struct {
	Purpose string
	Path    string
}

// ShareServerView 是可挂载服务器清单里的一台服务器条目。
type ShareServerView struct {
	ID         string
	Name       string
	DeviceName string
	ShellType  string
	MountPoint string
}

// ShareAuthorizationChoice 是可挂载范围勾选清单中的一台服务器条目。
// Checked 直接来自挂载密码机密项的可写入服务器列表。
type ShareAuthorizationChoice struct {
	ID                           string
	Name                         string
	DeviceName                   string
	ShellType                    string
	Checked                      bool
	SharesDeviceWithOtherEntries bool
}

// ShareAuthorizationView 是共享存储详情页的可挂载范围编辑区。
type ShareAuthorizationView struct {
	SecretID string
	Servers  []ShareAuthorizationChoice
}

// ShareSource 是 Web UI 读取共享存储可见数据的唯一入口。单机与权威端由
// 存储层适配；客户端由本机加密快照的无值视图适配。
type ShareSource interface {
	ListShares(instance, device, server string) ([]ShareRow, error)
	ShareDetail(instance, identifier string) (*ShareRow, error)
}

// ShareNotFoundError 表示路径里的共享存储不存在。
type ShareNotFoundError struct {
	Identifier string
}

func (e *ShareNotFoundError) Error() string {
	return "共享存储 " + e.Identifier + " 不存在"
}

// ShareWriteSource 描述一次共享存储写请求的来源地址；适配层把它写进审计行。
type ShareWriteSource struct {
	RemoteAddress string
}

// ShareAddForm 是新增共享存储表单的提交值，字段与 `sshmgr shares add` 的
// 参数一一对应；Directories 是逐条「用途=子路径」的原始输入。
type ShareAddForm struct {
	Name           string
	Device         string
	User           string
	PasswordSecret string
	Drive          string
	Directories    []string
	Note           string
}

// ShareEditForm 是一次共享存储部分更新；空指针表示该字段保持不变，与
// `sshmgr shares edit` 的旗标语义一致。
type ShareEditForm struct {
	Name           *string
	Device         *string
	User           *string
	PasswordSecret *string
	Drive          *string
	Directories    *[]models.SharedStorageDirectory
	Note           *string
	ChangeNote     *string
}

// ShareFormError 表示共享存储表单的提交内容校验失败。Message 是可直接展示
// 的存储层校验文案；适配层负责把内部错误与本类型区分开。
type ShareFormError struct {
	Message string
}

func (e *ShareFormError) Error() string { return e.Message }

// shareFormMessage 提取可直接展示的共享存储校验文案。只有适配层标记过的
// 表单错误保留原文；其余内部错误返回 false，调用方必须改用通用文案。
func shareFormMessage(err error) (string, bool) {
	var formErr *ShareFormError
	if errors.As(err, &formErr) {
		return formErr.Message, true
	}
	return "", false
}

// ShareWriter 是共享存储页写操作来源。单机与权威端必须提供；客户端不提
// 供，写路由不存在。
type ShareWriter interface {
	AddShare(form ShareAddForm, source ShareWriteSource) (*ShareRow, error)
	EditShare(identifier string, form ShareEditForm, source ShareWriteSource) (*ShareRow, error)
	DeleteShare(identifier string, source ShareWriteSource) error
}

// registerShareRoutes 注册共享存储页路由。客户端没有写来源，只注册只读
// 页面，写路由不存在。
func (h *handler) registerShareRoutes() {
	if h.cfg.Shares == nil {
		return
	}
	if h.cfg.SharesWriter != nil {
		h.mux.HandleFunc(sharesNewPath, h.requireSession(h.handleShareNew))
	}
	h.mux.HandleFunc(sharesPath, h.requireSession(h.handleShareDetailRoutes))
}

func (h *handler) handleShareDetailRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == sharesPath {
		h.handleSharesRoot(w, r)
		return
	}
	identifier := strings.TrimPrefix(r.URL.Path, sharesPath)
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
		h.handleShareDetail(w, r, identifier)
	case "edit":
		if h.cfg.SharesWriter != nil {
			h.handleShareEdit(w, r, identifier)
			return
		}
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	case "delete":
		if h.cfg.SharesWriter != nil {
			h.handleShareDelete(w, r, identifier)
			return
		}
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	default:
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	}
}

func (h *handler) handleSharesRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != sharesPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "共享存储列表只接受 GET 请求。")
		return
	}
	device := strings.TrimSpace(r.URL.Query().Get("device"))
	server := strings.TrimSpace(r.URL.Query().Get("server"))
	rows, err := h.cfg.Shares.ListShares(h.requestClientInstance(r), device, server)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取共享存储列表失败", "读取共享存储列表失败，请稍后重试。")
		return
	}
	deviceOptions, err := h.shareDeviceOptions(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取共享存储筛选条件失败", "读取共享存储筛选条件失败，请稍后重试。")
		return
	}
	serverOptions, err := h.cfg.Servers.Servers(h.requestClientInstance(r))
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请稍后重试。")
		return
	}
	sort.SliceStable(serverOptions, func(i, j int) bool { return serverOptions[i].Name < serverOptions[j].Name })
	h.renderPage(w, r, http.StatusOK, "shares.tmpl", pageData{
		Title: "共享存储", Shares: rows,
		ShareFilterDevice: device, ShareFilterServer: server,
		ShareDeviceOptions: deviceOptions, ShareServerOptions: serverOptions,
	})
}

// shareDeviceOptions 汇总全部共享存储的提供方设备名，作为筛选下拉的去重
// 选项；不依赖拓扑来源，客户端快照同样可用。
func (h *handler) shareDeviceOptions(r *http.Request) ([]string, error) {
	rows, err := h.cfg.Shares.ListShares(h.requestClientInstance(r), "", "")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(rows))
	options := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.DeviceName == "" || seen[row.DeviceName] {
			continue
		}
		seen[row.DeviceName] = true
		options = append(options, row.DeviceName)
	}
	sort.Strings(options)
	return options, nil
}

func (h *handler) handleShareDetail(w http.ResponseWriter, r *http.Request, identifier string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if h.cfg.SharesWriter == nil {
			h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
			return
		}
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "共享存储详情只接受 GET 请求。")
		return
	}
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	data := pageData{
		Title: "共享存储 " + detail.Name, ShareDetail: detail,
		ShareDetailNotice: shareSavedNotice(r, detail),
	}
	if h.caps.CanManageShares && h.cfg.Secrets != nil && h.cfg.SecretsWriter != nil {
		scope, err := h.shareAuthorizationView(detail)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取可挂载范围失败", "读取可挂载范围失败，请稍后重试。")
			return
		}
		data.ShareAuthorization = scope
	}
	h.renderPage(w, r, http.StatusOK, "share-detail.tmpl", data)
}

// shareAuthorizationView 读取全部服务器条目与挂载密码机密项的当前授权，
// 组装成按服务器条目展示的勾选清单；差异计算仍由机密项授权模块负责。
func (h *handler) shareAuthorizationView(share *ShareRow) (*ShareAuthorizationView, error) {
	secrets, err := h.cfg.Secrets.ListSecrets("", "", "")
	if err != nil {
		return nil, err
	}
	secret, ok := FindSecretRow(secrets, share.PasswordSecret)
	if !ok {
		return nil, &SecretNotFoundError{Identifier: share.PasswordSecret}
	}
	servers, err := h.cfg.Servers.Servers("")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	allowed := secretAllowedServerSet(secret)
	scope := &ShareAuthorizationView{SecretID: secret.ID}
	for _, server := range servers {
		detail, err := h.cfg.Servers.ServerDetail("", server.ID)
		if err != nil {
			return nil, err
		}
		choice := ShareAuthorizationChoice{
			ID: server.ID, Name: server.Name, DeviceName: detail.DeviceName,
			ShellType: detail.Shell, Checked: allowed[server.Name],
		}
		scope.Servers = append(scope.Servers, choice)
	}
	markShareSameDeviceGroups(scope.Servers)
	return scope, nil
}

// markShareSameDeviceGroups 标出同设备还有其他服务器条目的行；页面脚本
// 再按当前勾选状态决定是否显示提示。文件权限由整个设备共享，不能按登
// 录用户隔离。
func markShareSameDeviceGroups(servers []ShareAuthorizationChoice) {
	for index := range servers {
		if servers[index].DeviceName == "" {
			continue
		}
		for otherIndex := range servers {
			if otherIndex != index && servers[otherIndex].DeviceName == servers[index].DeviceName {
				servers[index].SharesDeviceWithOtherEntries = true
				break
			}
		}
	}
}

// shareSavedNotice 在保存跳回详情页时提示新的共享存储版本号；普通浏览返
// 回空串。
func shareSavedNotice(r *http.Request, detail *ShareRow) string {
	if r.URL.Query().Get("saved") != "1" {
		return ""
	}
	return "已保存共享存储登记；共享存储版本号：" + strconv.FormatInt(detail.Version, 10) + "。"
}

func (h *handler) renderShareReadError(w http.ResponseWriter, r *http.Request, err error) {
	var missing *ShareNotFoundError
	if errors.As(err, &missing) {
		h.renderError(w, r, http.StatusNotFound, "共享存储不存在", err.Error())
		return
	}
	h.renderError(w, r, http.StatusInternalServerError, "读取共享存储详情失败", "读取共享存储详情失败，请稍后重试。")
}

// shareFormView 是新增共享存储表单的展示数据；Directories 保持「用途=子
// 路径」逐行文本，与命令行 --dir 参数一一对应。
type shareFormView struct {
	Name           string
	Device         string
	User           string
	PasswordSecret string
	Drive          string
	Directories    string
	Note           string
	Error          string
}

func (h *handler) handleShareNew(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != sharesNewPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.renderPage(w, r, http.StatusOK, "share-form.tmpl", pageData{
			Title: "新增共享存储", ShareForm: shareFormView{
				Name: r.URL.Query().Get("name"), Device: r.URL.Query().Get("device"),
				User: r.URL.Query().Get("user"), PasswordSecret: r.URL.Query().Get("password_secret"),
				Drive: r.URL.Query().Get("drive"), Directories: r.URL.Query().Get("directories"),
				Note: r.URL.Query().Get("note"),
			},
		})
	case http.MethodPost:
		h.handleShareNewPost(w, r)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增共享存储页只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleShareNewPost(w http.ResponseWriter, r *http.Request) {
	form := shareAddFormFromRequest(r)
	added, err := h.cfg.SharesWriter.AddShare(ShareAddForm{
		Name: form.Name, Device: form.Device, User: form.User,
		PasswordSecret: form.PasswordSecret, Drive: form.Drive,
		Directories: shareDirectoryLines(form.Directories), Note: form.Note,
	}, ShareWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		if message, ok := shareFormMessage(err); ok {
			form.Error = message
			h.renderPage(w, r, http.StatusBadRequest, "share-form.tmpl", pageData{
				Title: "新增共享存储", ShareForm: form,
			})
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "新增共享存储失败", "新增共享存储失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, sharesPath+added.ID, http.StatusSeeOther)
}

func shareAddFormFromRequest(r *http.Request) shareFormView {
	_ = r.ParseForm()
	return shareFormView{
		Name: strings.TrimSpace(r.PostFormValue("name")), Device: strings.TrimSpace(r.PostFormValue("device")),
		User:           strings.TrimSpace(r.PostFormValue("user")),
		PasswordSecret: strings.TrimSpace(r.PostFormValue("password_secret")),
		Drive:          strings.TrimSpace(r.PostFormValue("drive")),
		Directories:    r.PostFormValue("directories"),
		Note:           r.PostFormValue("note"),
	}
}

// shareDirectoryLines 把目录约定输入拆成逐条「用途=子路径」的行；空行忽
// 略，其余内容原样交给存储层解析与校验。
func shareDirectoryLines(raw string) []string {
	parts := strings.Split(raw, "\n")
	out := make([]string, 0, len(parts))
	for _, line := range parts {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// shareEditView 是编辑共享存储表单的展示数据；Form 保留 owner 提交或当前
// 的值，ChangeNote 与命令行 --change-note 一一对应。
type shareEditView struct {
	Detail     *ShareRow
	Form       shareFormView
	ChangeNote string
	Error      string
}

func (h *handler) handleShareEdit(w http.ResponseWriter, r *http.Request, identifier string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
		if err != nil {
			h.renderShareReadError(w, r, err)
			return
		}
		view := shareEditViewFromDetail(detail)
		h.renderPage(w, r, http.StatusOK, "share-edit.tmpl", pageData{
			Title: "编辑 " + detail.Name, ShareEdit: &view,
		})
	case http.MethodPost:
		h.handleShareEditPost(w, r, identifier)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "编辑共享存储页只接受 GET 与 POST 请求。")
	}
}

func shareEditViewFromDetail(detail *ShareRow) shareEditView {
	return shareEditView{Detail: detail, Form: shareFormView{
		Name: detail.Name, Device: detail.DeviceName, User: detail.MountUser,
		PasswordSecret: detail.PasswordSecret, Drive: detail.Drive,
		Directories: shareDirectoryText(detail.Directories), Note: detail.Note,
	}, ChangeNote: detail.ChangeNote}
}

func shareDirectoryText(directories []ShareDirectoryView) string {
	lines := make([]string, 0, len(directories))
	for _, directory := range directories {
		lines = append(lines, directory.Purpose+"="+directory.Path)
	}
	return strings.Join(lines, "\n")
}

func (h *handler) handleShareEditPost(w http.ResponseWriter, r *http.Request, identifier string) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	view := shareEditViewFromDetail(detail)
	view.Form = shareAddFormFromRequest(r)
	view.ChangeNote = r.PostFormValue("change_note")
	changes, changed := shareEditChanges(view.Form, view.ChangeNote, detail)
	if !changed {
		h.renderPage(w, r, http.StatusOK, "share-detail.tmpl", pageData{
			Title: "共享存储 " + detail.Name, ShareDetail: detail,
			ShareDetailNotice: "没有变化，未写入数据或审计。",
		})
		return
	}
	edited, err := h.cfg.SharesWriter.EditShare(identifier, changes, ShareWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		if message, ok := shareFormMessage(err); ok {
			view.Error = message
			h.renderPage(w, r, http.StatusBadRequest, "share-edit.tmpl", pageData{
				Title: "编辑 " + detail.Name, ShareEdit: &view,
			})
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "保存共享存储失败", "保存共享存储失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, sharesPath+edited.ID+"?saved=1", http.StatusSeeOther)
}

// shareEditChanges 按命令行部分更新语义生成变更：只有与当前值不同的字
// 段进入变更；目录约定逐行解析后整体比较。
func shareEditChanges(form shareFormView, changeNote string, current *ShareRow) (ShareEditForm, bool) {
	changes := ShareEditForm{}
	changed := false
	if form.Name != current.Name {
		changes.Name = &form.Name
		changed = true
	}
	if form.Device != current.DeviceName {
		changes.Device = &form.Device
		changed = true
	}
	if form.User != current.MountUser {
		changes.User = &form.User
		changed = true
	}
	if form.PasswordSecret != current.PasswordSecret {
		changes.PasswordSecret = &form.PasswordSecret
		changed = true
	}
	if drive := strings.ToUpper(form.Drive); drive != current.Drive {
		changes.Drive = &drive
		changed = true
	}
	if directories := shareDirectoryLines(form.Directories); !sameDirectoryLines(directories, current.Directories) {
		parsed := make([]models.SharedStorageDirectory, 0, len(directories))
		for _, line := range directories {
			purpose, path, _ := strings.Cut(line, "=")
			parsed = append(parsed, models.SharedStorageDirectory{Purpose: purpose, Path: path})
		}
		changes.Directories = &parsed
		changed = true
	}
	if form.Note != current.Note {
		changes.Note = &form.Note
		changed = true
	}
	if changeNote != current.ChangeNote {
		changes.ChangeNote = &changeNote
		changed = true
	}
	return changes, changed
}

func sameDirectoryLines(lines []string, directories []ShareDirectoryView) bool {
	if len(lines) != len(directories) {
		return false
	}
	for index, line := range lines {
		purpose, path, _ := strings.Cut(line, "=")
		if purpose != directories[index].Purpose || path != directories[index].Path {
			return false
		}
	}
	return true
}

// handleShareDelete 渲染删除预览或处理确认提交。权威端要求最近 5 分钟内
// 重新输入过管理员口令；单机回环模式没有管理员口令，直接放行。
func (h *handler) handleShareDelete(w http.ResponseWriter, r *http.Request, identifier string) {
	target := sharesPath + identifier
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
		if err != nil {
			h.renderShareReadError(w, r, err)
			return
		}
		h.renderPage(w, r, http.StatusOK, "share-delete.tmpl", pageData{
			Title: "删除 " + detail.Name, ShareDeletePreview: detail,
		})
	case http.MethodPost:
		if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
			http.Redirect(w, r, reverifyPath(target+"/delete"), http.StatusSeeOther)
			return
		}
		if err := h.cfg.SharesWriter.DeleteShare(
			identifier, ShareWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			var missing *ShareNotFoundError
			if errors.As(err, &missing) {
				h.renderShareReadError(w, r, err)
				return
			}
			if message, ok := shareFormMessage(err); ok {
				h.renderError(w, r, http.StatusBadRequest, "删除共享存储失败", message)
				return
			}
			h.renderError(w, r, http.StatusInternalServerError, "删除共享存储失败", "删除共享存储失败，请稍后重试。")
			return
		}
		http.Redirect(w, r, sharesPath, http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "删除共享存储页只接受 GET 与 POST 请求。")
	}
}
