// Package webui 提供 sshmgr 的 Web 管理界面。回环模式由命令行把一次性启动
// 令牌换成进程内会话，再进入只读服务器列表；本包不直接读取 vault 或客户端
// 快照，展示数据一律经配置注入的来源接口取得。
package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Role 是 Web UI 需要区分的本机角色子集：本包只服务单机与客户端两种回环
// 模式；权威端的独立监听属于后续票，不在本包构造参数里出现。
type Role int

const (
	// RoleStandalone 表示单机角色：自持 vault，页面能力标记为全功能可写。
	RoleStandalone Role = iota
	// RoleClient 表示客户端角色：只读查看本地快照，修改入口一律不出现。
	RoleClient
)

// ServerRow 是服务器列表的展示行：名称、用户、主机、端口与「需要注意」
// 标记；其余字段留给将来的服务器详情页使用。
type ServerRow struct {
	ID             string
	Name           string
	User           string
	Host           string
	Port           int
	NeedsAttention bool
	Role           string
	Description    string
}

// ServerSource 是 Web UI 读取服务器列表的唯一入口，由命令行按本机角色提供
// 单机 vault 来源或客户端快照来源。
type ServerSource interface {
	Servers() ([]ServerRow, error)
}

// Config 是 Web UI Handler 的构造参数。Port 同时用于会话 cookie 命名与
// Host、Origin 校验；Now 是可选时钟，缺省使用标准时间。
type Config struct {
	Role        Role
	Servers     ServerSource
	VersionHint func() string
	Port        int
	Now         func() time.Time
}

func (c Config) validate() error {
	if c.Role != RoleStandalone && c.Role != RoleClient {
		return fmt.Errorf("未知的 Web UI 角色：%d", c.Role)
	}
	if c.Servers == nil {
		return errors.New("Web UI 必须提供服务器列表来源")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("Web UI 监听端口非法：%d", c.Port)
	}
	return nil
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// versionHint 读取版本提示来源；空串表示不显示。
func (c Config) versionHint() string {
	if c.VersionHint == nil {
		return ""
	}
	return c.VersionHint()
}

// sortNeedsAttentionFirst 稳定地把需要注意的服务器排到最前，与终端图形
// 界面的排序口径一致。
func sortNeedsAttentionFirst(rows []ServerRow) []ServerRow {
	sorted := append([]ServerRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].NeedsAttention && !sorted[j].NeedsAttention
	})
	return sorted
}

// capabilities 是页面能力开关：模板按它决定显示哪些标记与入口。后续票新增
// 写操作页面时扩展这个结构，不另造机制。
type capabilities struct {
	// RoleLabel 是页头展示的本机角色标记。
	RoleLabel string
	// CanWrite 表示本机角色是否可写 vault；客户端恒为 false。
	CanWrite bool
	// ReadOnlyNotice 非空时在页头下方显示只读指引；单机角色为空串。
	ReadOnlyNotice string
}

// capabilitiesForRole 按构造时声明的本机角色推导页面能力开关。
func capabilitiesForRole(role Role) capabilities {
	if role == RoleClient {
		return capabilities{
			RoleLabel:      "客户端 · 只读",
			CanWrite:       false,
			ReadOnlyNotice: "这里只读，修改需到权威端 Web UI 去做。",
		}
	}
	return capabilities{RoleLabel: "单机 · 全功能可写", CanWrite: true}
}

// Startup 携带一次性启动令牌，并按监听地址拼出浏览器要打开的网址。
type Startup struct {
	token string
}

// Token 返回一次性启动令牌本身。
func (s Startup) Token() string { return s.token }

// URL 按传入的「主机:端口」拼出带令牌的启动网址。
func (s Startup) URL(hostPort string) string {
	return "http://" + hostPort + startPath + "?token=" + url.QueryEscape(s.token)
}

// session 是回环模式的进程内会话：标识与防跨站请求伪造令牌都只在内存里，
// 进程退出即全部失效。
type session struct {
	id   string
	csrf string
}

// handler 是回环模式 Web UI 的根处理器，所有路由都在 /ui/ 前缀下。
type handler struct {
	cfg         Config
	caps        capabilities
	assets      assetURLs
	assetFiles  map[string]string
	pages       map[string]parsedPage
	startDigest [sha256.Size]byte
	mu          sync.Mutex
	startUsed   bool
	session     *session
	mux         *http.ServeMux
}

const (
	startPath = "/ui/start"
	listPath  = "/ui/"

	// contentSecurityPolicy 只允许同源脚本、样式、连接、图片与字体，禁止
	// 内联脚本、内联事件属性、内联样式以及任何被嵌入、跳转的可能。
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
)

// New 构造 Web UI 的根处理器与一次性启动令牌。构造完成后令牌只在第一次
// 有效使用时换取会话，之后立即作废。
func New(cfg Config) (http.Handler, Startup, error) {
	if err := cfg.validate(); err != nil {
		return nil, Startup{}, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, Startup{}, fmt.Errorf("生成启动令牌失败：%w", err)
	}
	assets, assetFiles, err := loadAssets()
	if err != nil {
		return nil, Startup{}, err
	}
	pages, err := loadPageTemplates()
	if err != nil {
		return nil, Startup{}, err
	}
	h := &handler{
		cfg:         cfg,
		caps:        capabilitiesForRole(cfg.Role),
		assets:      assets,
		assetFiles:  assetFiles,
		pages:       pages,
		startDigest: sha256.Sum256([]byte(token)),
		mux:         http.NewServeMux(),
	}
	h.mux.HandleFunc(startPath, h.handleStart)
	h.mux.HandleFunc("/ui/assets/", h.handleAsset)
	h.mux.HandleFunc(listPath, h.handleUIRoot)
	return h, Startup{token: token}, nil
}

// ServeHTTP 让 handler 直接作为 http.Server 的根处理器使用。所有请求（包括
// 启动入口与静态资源）先校验回环 Host，拒绝域名重绑定后再进入路由。
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 安全头对所有响应生效（静态资源随后覆盖缓存策略）；HTML 默认不缓存，
	// 静态资源在自身处理器里改成长期缓存。
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if !hostAllowed(r.Host, h.cfg.Port) {
		h.renderError(w, r, http.StatusMisdirectedRequest, "请求来源不被接受", "Web UI 只允许从本机回环地址访问。请重新运行 `sshmgr ui`。")
		return
	}
	if !h.writeRequestAllowed(w, r) {
		return
	}
	h.mux.ServeHTTP(w, r)
}

// writeRequestAllowed 统一校验所有写请求：非 GET、HEAD、OPTIONS 都必须来自
// 本次监听的回环来源，并携带当前会话绑定的防跨站请求伪造令牌。
func (h *handler) writeRequestAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if !originAllowed(r.Header.Get("Origin"), h.cfg.Port) {
		h.renderError(w, r, http.StatusForbidden, "请求来源校验失败", "请求来源不是本次启动的 Web UI，已拒绝。")
		return false
	}
	sess := h.currentSession(r)
	if sess == nil {
		h.renderError(w, r, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
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

// hostAllowed 判断 Host 是否为本次监听端口对应的回环地址：127.0.0.1、
// localhost 或 [::1]，端口必须与构造时声明的监听端口一致。
func hostAllowed(host string, port int) bool {
	name, hostPort, err := net.SplitHostPort(host)
	if err != nil || hostPort != strconv.Itoa(port) {
		return false
	}
	if name == "127.0.0.1" || name == "::1" || strings.EqualFold(name, "localhost") {
		return true
	}
	return false
}

// handleStart 是启动入口：有效令牌首次使用即作废、签发会话并 303 重定向到
// 不带令牌的服务器列表页；令牌缺失、错误或已用都返回 403 中文错误页。
func (h *handler) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "只允许 GET 访问启动入口", http.StatusMethodNotAllowed)
		return
	}
	digest := sha256.Sum256([]byte(r.URL.Query().Get("token")))
	sess, err := newSession()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "无法建立会话", "生成会话标识失败，请重新运行 `sshmgr ui`。")
		return
	}
	h.mu.Lock()
	valid := !h.startUsed && subtle.ConstantTimeCompare(digest[:], h.startDigest[:]) == 1
	if valid {
		h.startUsed = true
		h.session = sess
	}
	h.mu.Unlock()
	if !valid {
		h.renderError(w, r, http.StatusForbidden, "启动链接无效", "启动链接无效或已使用。请关闭本页，重新运行 `sshmgr ui` 获取新的启动链接。")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(h.cfg.Port),
		Value:    sess.id,
		Path:     "/ui/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, listPath, http.StatusSeeOther)
}

// handleUIRoot 是 /ui/ 前缀的总入口：精确命中服务器列表页时要求有效会话，
// 其余地址按会话状态返回 401 或中文 404 页。
func (h *handler) handleUIRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != listPath {
		if sess := h.currentSession(r); sess == nil {
			h.renderError(w, r, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
			return
		}
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if sess := h.currentSession(r); sess == nil {
		h.renderError(w, r, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
		return
	}
	rows, err := h.cfg.Servers.Servers()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请重新运行 `sshmgr ui`。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "servers.tmpl", pageData{Title: "服务器", Servers: sortNeedsAttentionFirst(rows)})
}

// currentSession 校验请求携带的会话 cookie，返回当前会话；无会话、cookie
// 缺失或值不符都返回 nil。
func (h *handler) currentSession(r *http.Request) *session {
	c, err := r.Cookie(sessionCookieName(h.cfg.Port))
	if err != nil || c.Value == "" {
		return nil
	}
	h.mu.Lock()
	sess := h.session
	h.mu.Unlock()
	if sess == nil {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(sess.id)) != 1 {
		return nil
	}
	return sess
}

// sessionCookieName 带上监听端口：浏览器的 cookie 不按端口隔离，同一台
// 机器同时开两个 Web UI 实例时才能互不覆盖。
func sessionCookieName(port int) string {
	return fmt.Sprintf("sshmgr_ui_%d", port)
}

func newSession() (*session, error) {
	id, err := generateSessionToken()
	if err != nil {
		return nil, err
	}
	csrf, err := generateSessionToken()
	if err != nil {
		return nil, err
	}
	return &session{id: id, csrf: csrf}, nil
}

// generateSessionToken 是会话随机数生成接缝，测试用它模拟系统随机数失败。
var generateSessionToken = randomToken

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// mountPost 在内部路由表上登记写操作路由：所有非 GET/HEAD/OPTIONS 的业务
// 路由都必须经它登记，统一获得会话校验、Origin 校验和与会话绑定的防跨站
// 请求伪造令牌校验。本票没有业务写操作，测试用它挂无副作用的写路由验证
// 中间件；后续票的写操作页面沿用同一入口。
func (h *handler) mountPost(pattern string, fn func(http.ResponseWriter, *http.Request, *session)) {
	h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "本地址只接受写请求。")
			return
		}
		sess := h.currentSession(r)
		if sess == nil {
			h.renderError(w, r, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
			return
		}
		if !originAllowed(r.Header.Get("Origin"), h.cfg.Port) {
			h.renderError(w, r, http.StatusForbidden, "请求来源校验失败", "请求来源不是本次启动的 Web UI，已拒绝。")
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			_ = r.ParseForm()
			token = r.PostFormValue("csrf_token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
			h.renderError(w, r, http.StatusForbidden, "防跨站请求伪造校验失败", "防跨站请求伪造令牌缺失或无效，已拒绝。请刷新页面后重试。")
			return
		}
		fn(w, r, sess)
	})
}

// originAllowed 校验写请求的 Origin：必须存在，且等于由配置端口生成的任一
// 回环来源，不信任请求自带的 Host。
func originAllowed(origin string, port int) bool {
	portText := strconv.Itoa(port)
	return origin == "http://"+net.JoinHostPort("127.0.0.1", portText) ||
		origin == "http://"+net.JoinHostPort("localhost", portText) ||
		origin == "http://"+net.JoinHostPort("::1", portText)
}
