package webui

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	SetAdminPasswordWithAudit(password []byte, event AuditEvent) (int64, error)
	SessionGeneration() (int64, error)
	LogoutAllSessionsWithAudit(event AuditEvent) (int64, error)
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
	caps := capabilitiesForConfig(cfg)
	if cfg.Role == RoleServer {
		caps.CanRestartService = cfg.ServiceRestart != nil
	}
	h := &handler{
		cfg:          cfg,
		caps:         caps,
		assets:       assets,
		assetFiles:   assetFiles,
		pages:        pages,
		allowedHosts: canonicalIPSet(cfg.AllowedHosts),
		mux:          http.NewServeMux(),
		sessions:     map[string]*session{},
	}
	h.mux.HandleFunc("/ui/assets/", h.handleAsset)
	h.mux.HandleFunc("/ui/login", h.handleAuthorityLogin)
	h.mux.HandleFunc("/ui/logout", h.handleAuthorityLogout)
	h.mux.HandleFunc("/ui/logout-all", h.handleAuthorityLogoutAll)
	h.mux.HandleFunc("/ui/password", h.requireSession(h.handleAuthorityPassword))
	h.mux.HandleFunc("/ui/reverify", h.requireSession(h.handleAuthorityReverify))
	h.mux.HandleFunc("/ui/", h.requireSession(h.handleAuthorityRoot))
	// 访问区域路由按权威端角色注册全部已实现的页面。
	h.registerAccessRoutes()
	h.registerServerRoutes()
	h.registerSecretRoutes()
	h.registerSoftwareInventoryRoutes()
	h.registerOpsRoutes()
	return h, h, nil
}

// SetAllowedHosts 由监听管理者在地址集合变化时调用，只接受新的集合。
func (h *handler) SetAllowedHosts(addresses []string) {
	hosts := canonicalIPSet(addresses)
	h.mu.Lock()
	h.allowedHosts = hosts
	h.mu.Unlock()
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
	setAuthorityCSRFCookie(w, csrfToken)
	h.renderPage(w, r, http.StatusOK, "login.tmpl", pageData{
		Title:                  "管理员登录",
		AdminPasswordSet:       set,
		CertificateFingerprint: h.cfg.CertificateFingerprint,
		CSRFToken:              csrfToken,
	})
}

func (h *handler) handleAuthorityRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != listPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	h.handleServerList(w, r)
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
	check, err := h.checkAuthorityPassword(ip, r.PostFormValue("password"), now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "处理登录请求失败", "读取或记录登录限制失败，请稍后重试。")
		return
	}
	if check.Limit.Locked {
		h.renderLoginPageWithError(w, r, http.StatusForbidden, "来源 IP 已锁定", "这个来源 IP 已被锁定，请等剩余时间结束后再试。", check.Limit.LockRemaining)
		return
	}
	if !check.Valid {
		h.renderLoginPageWithError(w, r, http.StatusUnauthorized, "口令不正确", "管理员口令不正确，请重试。", check.Limit.LockRemaining)
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
	h.clearExpiredAuthoritySessionsLocked(now)
	h.sessions[sess.id] = sess
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: authoritySessionCookieName, Value: sess.id, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	h.writeAuthorityAudit(ip, "admin.login", "ok", nil)
	http.Redirect(w, r, listPath, http.StatusSeeOther)
}

type authorityPasswordCheck struct {
	Limit LoginLimit
	Valid bool
}

func (h *handler) checkAuthorityPassword(ip, password string, now time.Time) (authorityPasswordCheck, error) {
	// 全局延迟在串行锁外等待，否则其他设备排队的请求会把 owner 的登录拖到无限长。
	if pending, err := h.cfg.Auth.LoginLimit(ip, now); err == nil {
		h.loginSleep(pending.Delay)
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	limit, err := h.cfg.Auth.LoginLimit(ip, now)
	if err != nil {
		return authorityPasswordCheck{}, err
	}
	if limit.Locked {
		return authorityPasswordCheck{Limit: limit}, nil
	}
	valid, err := h.cfg.Auth.VerifyAdminPassword([]byte(password))
	if err != nil {
		return authorityPasswordCheck{}, err
	}
	if valid {
		return authorityPasswordCheck{Limit: limit, Valid: true}, nil
	}
	limit, err = h.cfg.Auth.RecordLoginFailure(ip, now)
	if err != nil {
		return authorityPasswordCheck{}, err
	}
	h.writeAuthorityAudit(ip, "admin.login_fail", "error", nil)
	if limit.Locked {
		h.writeAuthorityAudit(ip, "admin.lockout", "blocked", nil)
	}
	return authorityPasswordCheck{Limit: limit}, nil
}

func (h *handler) clearExpiredAuthoritySessionsLocked(now time.Time) {
	for id, sess := range h.sessions {
		if now.Sub(sess.lastActive) >= 12*time.Hour {
			delete(h.sessions, id)
		}
	}
}

func (h *handler) clearAuthoritySessions() {
	h.mu.Lock()
	h.sessions = map[string]*session{}
	h.mu.Unlock()
}

func setAuthorityCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: authorityCSRFCookieName, Value: token, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (h *handler) renderLoginPageWithError(w http.ResponseWriter, r *http.Request, status int, title, message string, remaining time.Duration) {
	set, err := h.cfg.Auth.AdminPasswordIsSet()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取管理员口令状态失败", "读取管理员口令状态失败，请稍后重试。")
		return
	}
	csrfToken, tokenErr := generateSessionToken()
	if tokenErr == nil {
		setAuthorityCSRFCookie(w, csrfToken)
	}
	h.renderPage(w, r, status, "login.tmpl", pageData{
		Title: title, AdminPasswordSet: set,
		CertificateFingerprint: h.cfg.CertificateFingerprint,
		LoginError:             message, LockRemaining: remaining, CSRFToken: csrfToken,
	})
}

func (h *handler) writeAuthorityAudit(ip, action, status string, detail map[string]string) {
	err := h.cfg.Auth.WriteAudit(AuditEvent{Action: action, Status: status, SourceIP: ip, Detail: detail})
	if err != nil && h.cfg.Log != nil {
		h.cfg.Log("sshmgr serve: 写入 Web UI 审计失败 action=%s ip=%s: %v", action, ip, err)
	}
}

func (h *handler) clearAuthoritySessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: authoritySessionCookieName, Value: "", Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

func (h *handler) handleAuthorityLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "注销只接受 POST 请求。")
		return
	}
	if cookie, err := r.Cookie(authoritySessionCookieName); err == nil {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
	}
	h.clearAuthoritySessionCookie(w)
	h.writeAuthorityAudit(remoteIP(r), "admin.logout", "ok", nil)
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

func (h *handler) handleAuthorityLogoutAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "注销全部会话只接受 POST 请求。")
		return
	}
	logoutEvent := AuditEvent{Action: "admin.logout_all", Status: "ok", SourceIP: remoteIP(r)}
	if _, err := h.cfg.Auth.LogoutAllSessionsWithAudit(logoutEvent); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "注销全部会话失败", "注销全部会话失败，请稍后重试。")
		return
	}
	h.clearAuthoritySessions()
	h.clearAuthoritySessionCookie(w)
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

func (h *handler) recentlyVerified(sess *session) bool {
	if sess == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.now().Sub(sess.lastVerified) <= 5*time.Minute
}

// requireRecentReverify 是运维写操作的统一重新验证守卫。单机与客户端的
// 回环界面没有管理员口令，直接放行；权威端 5 分钟内未重新验证时跳到重
// 新验证页，next 必须指向重新验证后可用 GET 打开的页面。
func (h *handler) requireRecentReverify(w http.ResponseWriter, r *http.Request, next string) bool {
	if h.cfg.Role != RoleServer {
		return true
	}
	if h.recentlyVerified(h.currentAuthoritySession(r)) {
		return true
	}
	http.Redirect(w, r, reverifyPath(next), http.StatusSeeOther)
	return false
}

func (h *handler) handleAuthorityPassword(w http.ResponseWriter, r *http.Request) {
	sess := h.currentAuthoritySession(r)
	if !h.recentlyVerified(sess) {
		http.Redirect(w, r, "/ui/reverify?next=/ui/password", http.StatusSeeOther)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.renderPage(w, r, http.StatusOK, "password.tmpl", pageData{Title: "修改管理员口令"})
	case http.MethodPost:
		newPassword := r.PostFormValue("new_password")
		confirmPassword := r.PostFormValue("confirm_password")
		if newPassword == "" || newPassword != confirmPassword {
			h.renderPage(w, r, http.StatusBadRequest, "password.tmpl", pageData{
				Title: "修改管理员口令", LoginError: "两次输入的新口令不一致或为空，请重试。",
			})
			return
		}
		changeEvent := AuditEvent{
			Action: "admin.password_change", Status: "ok", SourceIP: remoteIP(r),
		}
		if _, err := h.cfg.Auth.SetAdminPasswordWithAudit([]byte(newPassword), changeEvent); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "修改管理员口令失败", "修改管理员口令失败，请稍后重试。")
			return
		}
		h.clearAuthoritySessions()
		h.clearAuthoritySessionCookie(w)
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "修改口令页面只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleAuthorityReverify(w http.ResponseWriter, r *http.Request) {
	sess := h.currentAuthoritySession(r)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.renderPage(w, r, http.StatusOK, "reverify.tmpl", pageData{
			Title: "重新验证", NextPath: safeNextPath(r.URL.Query().Get("next")),
		})
	case http.MethodPost:
		ip := remoteIP(r)
		now := h.cfg.now()
		check, err := h.checkAuthorityPassword(ip, r.PostFormValue("password"), now)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "处理重新验证请求失败", "读取或记录登录限制失败，请稍后重试。")
			return
		}
		if check.Limit.Locked {
			h.renderPage(w, r, http.StatusForbidden, "reverify.tmpl", pageData{
				Title: "重新验证", LoginError: "这个来源 IP 已被锁定，请等剩余时间结束后再试。",
				LockRemaining: check.Limit.LockRemaining,
				NextPath:      safeNextPath(r.PostFormValue("next")),
			})
			return
		}
		if !check.Valid {
			h.renderPage(w, r, http.StatusUnauthorized, "reverify.tmpl", pageData{
				Title: "重新验证", LoginError: "管理员口令不正确，请重试。", LockRemaining: check.Limit.LockRemaining,
				NextPath: safeNextPath(r.PostFormValue("next")),
			})
			return
		}
		h.mu.Lock()
		sess.lastVerified = now
		h.mu.Unlock()
		http.Redirect(w, r, safeNextPath(r.PostFormValue("next")), http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "重新验证页面只接受 GET 与 POST 请求。")
	}
}

func safeNextPath(next string) string {
	if strings.HasPrefix(next, "/ui/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return listPath
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
	if sess == nil {
		h.mu.Unlock()
		return nil
	}
	now := h.cfg.now()
	if now.Sub(sess.lastActive) >= 12*time.Hour {
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
		return nil
	}
	sess.lastActive = now
	h.mu.Unlock()
	generation, err := h.cfg.Auth.SessionGeneration()
	if err != nil || generation != sess.generation {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
		return nil
	}
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
	if err != nil {
		return false
	}
	ip := net.ParseIP(name)
	if strings.EqualFold(name, "localhost") || (ip != nil && ip.IsLoopback()) {
		return true
	}
	if portText != strconv.Itoa(h.cfg.Port) {
		return false
	}
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
