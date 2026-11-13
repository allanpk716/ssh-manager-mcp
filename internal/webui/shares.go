package webui

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
)

const (
	sharesPath    = "/ui/shares/"
	sharesNewPath = "/ui/shares/new"
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
	h.mux.HandleFunc(sharesPath, h.requireSession(h.handleShareReadRoutes))
}

func (h *handler) handleShareReadRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == sharesPath {
		h.handleSharesRoot(w, r)
		return
	}
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
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
