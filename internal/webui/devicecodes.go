package webui

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultServePort 是权威端主监听的默认端口；拉取命令按它兜底，serve 进
// 程会用真实监听端口覆盖。
const defaultServePort = 7878

// AccessDeviceCodeSummary 是设备码列表的一行展示数据：名称、令牌前缀、状
// 态、绑定的 profile、最近拉取时间与最近客户端版本；令牌本体绝不出现。
type AccessDeviceCodeSummary struct {
	ID            string
	Name          string
	TokenPrefix   string
	Status        string
	ProfileName   string
	LastPullAt    time.Time
	ClientVersion string
}

// Revoked 表示设备码是否已撤销，模板据此显示状态与操作入口。
func (s AccessDeviceCodeSummary) Revoked() bool {
	return s.Status == "revoked"
}

// LastPullText 返回最近拉取时间的展示文字；从未拉取过时显示「从未拉取」。
func (s AccessDeviceCodeSummary) LastPullText() string {
	if s.LastPullAt.IsZero() {
		return "从未拉取"
	}
	return s.LastPullAt.Format("2006-01-02 15:04:05")
}

// ClientVersionText 返回最近客户端版本的展示文字；空值表示未知或旧版客
// 户端没有声明版本。
func (s AccessDeviceCodeSummary) ClientVersionText() string {
	if s.ClientVersion == "" {
		return "未声明"
	}
	return s.ClientVersion
}

// AccessDeviceIssueView 是设备码签发表单的渲染数据；Name 与 ProfileID 在
// 重新验证回来时保留提交时的填写。
type AccessDeviceIssueView struct {
	Name          string
	ProfileID     string
	NeedsReverify bool
	ReverifyPath  string
}

// AccessDeviceCodeResult 是签发设备码的一次性结果：设备码、权威端证书公
// 钥指纹和可直接粘贴的拉取命令只在签发表单提交的这次响应里出现。
type AccessDeviceCodeResult struct {
	Name        string
	Code        string
	Fingerprint string
	PullCommand string
}

// registerDeviceCodeRoutes 只在权威端注册设备码路由：单机没有设备码，客
// 户端的修改一律在权威端做。
func (h *handler) registerDeviceCodeRoutes() {
	h.mux.HandleFunc(accessDeviceCodesPath, h.requireSession(h.handleAccessDeviceCodes))
	h.mux.HandleFunc(accessDeviceCodesPrefix, h.requireSession(h.handleAccessDeviceCodeRevoke))
}

func (h *handler) handleAccessDeviceCodes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != accessDeviceCodesPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleAccessDeviceCodeAdd(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "设备码页只接受 GET 与 POST 请求。")
		return
	}
	devices, err := h.cfg.Access.ListDeviceCodes()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取设备码列表失败", "读取设备码列表失败，请稍后重试。")
		return
	}
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 profile 列表失败", "读取 profile 列表失败，请稍后重试。")
		return
	}
	view := &AccessDeviceIssueView{
		Name:      strings.TrimSpace(r.URL.Query().Get("name")),
		ProfileID: r.URL.Query().Get("profile"),
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		view.NeedsReverify = true
		view.ReverifyPath = reverifyPath(deviceIssueResumePath(view.Name, view.ProfileID))
	}
	h.renderPage(w, r, http.StatusOK, "device-codes.tmpl", pageData{
		Title: "设备码", AccessDeviceCodes: devices, AccessProfiles: profiles, AccessDeviceIssue: view,
	})
}

func (h *handler) handleAccessDeviceCodeAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	profileID := r.PostFormValue("profile")
	if name == "" || profileID == "" {
		h.renderError(w, r, http.StatusBadRequest, "签发设备码失败", "设备名与 profile 都不能为空。")
		return
	}
	if h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		http.Redirect(w, r, reverifyPath(deviceIssueResumePath(name, profileID)), http.StatusSeeOther)
		return
	}
	result, err := h.cfg.Access.AddDeviceCode(
		name, profileID, h.devicePullServeURL(r),
		AccessWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "签发设备码失败", "签发设备码失败，请稍后重试。")
		return
	}
	// 一次性页面必须不可缓存；设备码只出现在这次提交响应里，没有重新查
	// 看的路由。
	w.Header().Set("Cache-Control", "no-store")
	h.renderPage(w, r, http.StatusOK, "device-code-issued.tmpl", pageData{
		Title: "设备码已签发", AccessDeviceIssued: &result,
	})
}

func (h *handler) handleAccessDeviceCodeRevoke(w http.ResponseWriter, r *http.Request) {
	id, action, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, accessDeviceCodesPrefix), "/")
	if !ok || action != "revoke" || id == "" {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	device, err := h.accessDeviceCodeByID(id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取设备码失败", "读取设备码失败，请稍后重试。")
		return
	}
	if device == nil {
		h.renderError(w, r, http.StatusNotFound, "设备码不存在", "请求的设备码不存在。")
		return
	}
	if r.Method == http.MethodPost {
		if err := h.cfg.Access.RevokeDeviceCode(id, AccessWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "撤销设备码失败", "撤销设备码失败，请稍后重试。")
			return
		}
		http.Redirect(w, r, accessDeviceCodesPath, http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "撤销设备码页面只接受 GET 与 POST 请求。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "device-code-revoke.tmpl", pageData{
		Title: "撤销设备码", AccessDeviceRevoke: device,
	})
}

// accessDeviceCodeByID 在设备码列表里按标识符查找一行。
func (h *handler) accessDeviceCodeByID(id string) (*AccessDeviceCodeSummary, error) {
	devices, err := h.cfg.Access.ListDeviceCodes()
	if err != nil {
		return nil, err
	}
	for i := range devices {
		if devices[i].ID == id {
			return &devices[i], nil
		}
	}
	return nil, nil
}

// devicePullServeURL 按浏览器访问权威端 Web UI 的主机名和权威端主监听端
// 口拼出拉取命令要用的地址；Web UI 自己的监听端口与主监听端口不同，不
// 能直接使用请求里的完整 Host。
func (h *handler) devicePullServeURL(r *http.Request) string {
	host := r.Host
	if name, _, err := net.SplitHostPort(r.Host); err == nil {
		host = name
	}
	port := h.cfg.ServePort
	if port <= 0 {
		port = defaultServePort
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// deviceIssueResumePath 保留签发表单填写，重新验证后回到同一份表单。
func deviceIssueResumePath(name, profileID string) string {
	query := url.Values{}
	if name != "" {
		query.Set("name", name)
	}
	if profileID != "" {
		query.Set("profile", profileID)
	}
	return accessDeviceCodesPath + "?" + query.Encode()
}
