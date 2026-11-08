package webui

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// LoginLimit 是权威端登录请求在验码前需要遵守的限制状态。
type LoginLimit struct {
	Locked        bool
	LockRemaining time.Duration
	Delay         time.Duration
}

// AuditEvent 是 Web UI 写入审计行的窄接口载荷。Detail 只放调用方显式给出
// 的非敏感摘要字段。
type AuditEvent struct {
	Action   string
	Status   string
	SourceIP string
	Detail   map[string]string
}

// AuthStore 是权威端 Web UI 访问 vault 鉴权状态的唯一入口。命令行与 serve
// 进程用同一个实现适配这个接口；本包不知道数据库结构。
type AuthStore interface {
	AdminPasswordIsSet() (bool, error)
	VerifyAdminPassword(password []byte) (bool, error)
	SetAdminPassword(password []byte) (int64, error)
	SessionGeneration() (int64, error)
	LogoutAllSessions() (int64, error)
	LoginLimit(ip string, now time.Time) (LoginLimit, error)
	RecordLoginFailure(ip string, now time.Time) (LoginLimit, error)
	ClearLoginLock(ip string) error
	WriteAudit(event AuditEvent) error
}

// AuthorityController 暴露监听管理者需要同步的运行期状态。
type AuthorityController interface {
	SetAllowedHosts(addresses []string)
}

const (
	authoritySessionCookieName = "__Host-sshmgr_ui"
	authorityCSRFCookieName    = "__Host-sshmgr_csrf"
)

// NewAuthority 构造权威端 Web UI。它不经一次性启动令牌；页面会话只能由
// 管理员口令登录换取。
func NewAuthority(cfg Config) (http.Handler, AuthorityController, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}
	assets, assetFiles, err := loadAssets()
	if err != nil {
		return nil, nil, err
	}
	pages, err := loadPageTemplates()
	if err != nil {
		return nil, nil, err
	}
	h := &handler{
		cfg:          cfg,
		caps:         capabilitiesForRole(cfg.Role),
		assets:       assets,
		assetFiles:   assetFiles,
		pages:        pages,
		allowedHosts: canonicalHosts(cfg.AllowedHosts),
		mux:          http.NewServeMux(),
		sessions:     map[string]*session{},
	}
	h.mux.HandleFunc("/ui/assets/", h.handleAsset)
	h.mux.HandleFunc("/ui/login", h.handleAuthorityLogin)
	h.mux.HandleFunc("/ui/", h.handleAuthorityRoot)
	return h, h, nil
}

// SetAllowedHosts 由监听管理者在地址集合变化时调用，只接受新的集合。
func (h *handler) SetAllowedHosts(addresses []string) {
	hosts := canonicalHosts(addresses)
	h.mu.Lock()
	h.allowedHosts = hosts
	h.mu.Unlock()
}

func canonicalHosts(addresses []string) map[string]struct{} {
	hosts := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil {
			hosts[ip.String()] = struct{}{}
		}
	}
	return hosts
}

func (h *handler) handleAuthorityLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.handleAuthorityLoginPage(w, r)
	case http.MethodPost:
		h.handleAuthorityLoginPost(w, r)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "登录页只接受 GET 请求。")
	}
}

func (h *handler) handleAuthorityLoginPage(w http.ResponseWriter, r *http.Request) {
	set, err := h.cfg.Auth.AdminPasswordIsSet()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取管理员口令状态失败", "读取管理员口令状态失败，请稍后重试。")
		return
	}
	csrfToken, err := generateSessionToken()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "无法建立登录表单", "生成防跨站请求伪造令牌失败，请刷新页面重试。")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: authorityCSRFCookieName, Value: csrfToken, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	h.renderPage(w, r, http.StatusOK, "login.tmpl", pageData{
		Title:                  "管理员登录",
		AdminPasswordSet:       set,
		CertificateFingerprint: h.cfg.CertificateFingerprint,
		CSRFToken:              csrfToken,
	})
}

func (h *handler) handleAuthorityRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != listPath {
		if h.currentAuthoritySession(r) == nil {
			http.Redirect(w, r, loginPath, http.StatusSeeOther)
			return
		}
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if h.currentAuthoritySession(r) == nil {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	rows, err := h.cfg.Servers.Servers()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers.tmpl", pageData{
		Title:   "服务器",
		Servers: sortNeedsAttentionFirst(rows),
	})
}

func (h *handler) handleAuthorityLoginPost(w http.ResponseWriter, r *http.Request) {
	set, err := h.cfg.Auth.AdminPasswordIsSet()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取管理员口令状态失败", "读取管理员口令状态失败，请稍后重试。")
		return
	}
	if !set {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	ip := remoteIP(r)
	now := h.cfg.now()
	limit, err := h.cfg.Auth.LoginLimit(ip, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取登录限制失败", "读取登录限制失败，请稍后重试。")
		return
	}
	h.loginSleep(limit.Delay)
	if limit.Locked {
		h.writeAuthorityAudit(ip, "web.login.locked", "blocked", nil)
		h.renderLoginPageWithError(w, r, http.StatusForbidden, "来源 IP 已锁定", "这个来源 IP 已被锁定，请等剩余时间结束后再试。", limit.LockRemaining)
		return
	}
	password := r.PostFormValue("password")
	valid, err := h.cfg.Auth.VerifyAdminPassword([]byte(password))
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "校验管理员口令失败", "校验管理员口令失败，请稍后重试。")
		return
	}
	if !valid {
		limit, err = h.cfg.Auth.RecordLoginFailure(ip, now)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "记录登录失败失败", "记录登录失败失败，请稍后重试。")
			return
		}
		h.writeAuthorityAudit(ip, "web.login.failed", "error", nil)
		if limit.Locked {
			h.writeAuthorityAudit(ip, "web.login.locked", "blocked", nil)
		}
		h.renderLoginPageWithError(w, r, http.StatusUnauthorized, "口令不正确", "管理员口令不正确，请重试。", limit.LockRemaining)
		return
	}
	generation, err := h.cfg.Auth.SessionGeneration()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取会话代失败", "读取会话代失败，请稍后重试。")
		return
	}
	sess, err := newSession()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "无法建立会话", "生成会话标识失败，请刷新页面重试。")
		return
	}
	sess.generation = generation
	sess.lastActive = now
	sess.lastVerified = now
	h.mu.Lock()
	h.sessions[sess.id] = sess
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: authoritySessionCookieName, Value: sess.id, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	h.writeAuthorityAudit(ip, "web.login.success", "ok", nil)
	http.Redirect(w, r, listPath, http.StatusSeeOther)
}

func (h *handler) renderLoginPageWithError(w http.ResponseWriter, r *http.Request, status int, title, message string, remaining time.Duration) {
	set, err := h.cfg.Auth.AdminPasswordIsSet()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取管理员口令状态失败", "读取管理员口令状态失败，请稍后重试。")
		return
	}
	csrfToken, tokenErr := generateSessionToken()
	if tokenErr == nil {
		http.SetCookie(w, &http.Cookie{
			Name: authorityCSRFCookieName, Value: csrfToken, Path: "/",
			Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
	}
	h.renderPage(w, r, status, "login.tmpl", pageData{
		Title: title, AdminPasswordSet: set,
		CertificateFingerprint: h.cfg.CertificateFingerprint,
		LoginError:             message, LockRemaining: remaining, CSRFToken: csrfToken,
	})
}

func (h *handler) writeAuthorityAudit(ip, action, status string, detail map[string]string) {
	_ = h.cfg.Auth.WriteAudit(AuditEvent{Action: action, Status: status, SourceIP: ip, Detail: detail})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *handler) loginSleep(delay time.Duration) {
	if delay <= 0 {
		return
	}
	if h.cfg.Sleep != nil {
		h.cfg.Sleep(delay)
		return
	}
	time.Sleep(delay)
}

func (h *handler) currentAuthoritySession(r *http.Request) *session {
	cookie, err := r.Cookie(authoritySessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	h.mu.Lock()
	sess := h.sessions[cookie.Value]
	h.mu.Unlock()
	if sess == nil {
		return nil
	}
	generation, err := h.cfg.Auth.SessionGeneration()
	if err != nil || generation != sess.generation {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
		return nil
	}
	now := h.cfg.now()
	if now.Sub(sess.lastActive) >= 12*time.Hour {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
		return nil
	}
	h.mu.Lock()
	sess.lastActive = now
	h.mu.Unlock()
	return sess
}

func (h *handler) authorityWriteAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if !h.authorityOriginAllowed(r.Header.Get("Origin")) {
		h.renderError(w, r, http.StatusForbidden, "请求来源校验失败", "请求来源不是本次监听的 Web UI，已拒绝。")
		return false
	}
	if r.URL.Path == loginPath {
		cookie, cookieErr := r.Cookie(authorityCSRFCookieName)
		_ = r.ParseForm()
		token := r.PostFormValue("csrf_token")
		if cookieErr != nil || cookie.Value == "" || token == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(cookie.Value)) != 1 {
			h.renderError(w, r, http.StatusForbidden, "防跨站请求伪造校验失败", "防跨站请求伪造令牌缺失或无效，已拒绝。请刷新页面后重试。")
			return false
		}
		return true
	}
	sess := h.currentAuthoritySession(r)
	if sess == nil {
		h.renderError(w, r, http.StatusUnauthorized, "需要登录", "本页需要有效会话。请重新登录。")
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		_ = r.ParseForm()
		token = r.PostFormValue("csrf_token")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
		h.renderError(w, r, http.StatusForbidden, "防跨站请求伪造校验失败", "防跨站请求伪造令牌缺失或无效，已拒绝。请刷新页面后重试。")
		return false
	}
	return true
}

func (h *handler) authorityHostAllowed(host string) bool {
	name, portText, err := net.SplitHostPort(host)
	if err != nil || portText != strconv.Itoa(h.cfg.Port) {
		return false
	}
	ip := net.ParseIP(name)
	if ip == nil {
		return false
	}
	h.mu.Lock()
	_, ok := h.allowedHosts[ip.String()]
	h.mu.Unlock()
	return ok
}

func (h *handler) authorityOriginAllowed(origin string) bool {
	u, err := parseOrigin(origin)
	if err != nil || u.Scheme != "https" {
		return false
	}
	return h.authorityHostAllowed(u.Host)
}

func parseOrigin(origin string) (*url.URL, error) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	return parsed, nil
}
