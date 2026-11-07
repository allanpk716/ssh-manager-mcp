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
	startDigest [sha256.Size]byte
	mu          sync.Mutex
	startUsed   bool
	session     *session
	mux         *http.ServeMux
}

const (
	startPath = "/ui/start"
	listPath  = "/ui/"
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
	h := &handler{
		cfg:         cfg,
		startDigest: sha256.Sum256([]byte(token)),
		mux:         http.NewServeMux(),
	}
	h.mux.HandleFunc(startPath, h.handleStart)
	h.mux.HandleFunc(listPath, h.handleUIRoot)
	return h, Startup{token: token}, nil
}

// ServeHTTP 让 handler 直接作为 http.Server 的根处理器使用。所有请求（包括
// 启动入口与静态资源）先校验回环 Host，拒绝域名重绑定后再进入路由。
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hostAllowed(r.Host, h.cfg.Port) {
		renderError(w, http.StatusMisdirectedRequest, "请求来源不被接受", "Web UI 只允许从本机回环地址访问。请重新运行 `sshmgr ui`。")
		return
	}
	h.mux.ServeHTTP(w, r)
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "只允许 GET 访问启动入口", http.StatusMethodNotAllowed)
		return
	}
	digest := sha256.Sum256([]byte(r.URL.Query().Get("token")))
	h.mu.Lock()
	valid := !h.startUsed && subtle.ConstantTimeCompare(digest[:], h.startDigest[:]) == 1
	if valid {
		h.startUsed = true
	}
	h.mu.Unlock()
	if !valid {
		renderError(w, http.StatusForbidden, "启动链接无效", "启动链接无效或已使用。请关闭本页，重新运行 `sshmgr ui` 获取新的启动链接。")
		return
	}
	sess, err := newSession()
	if err != nil {
		renderError(w, http.StatusInternalServerError, "无法建立会话", "生成会话标识失败，请重新运行 `sshmgr ui`。")
		return
	}
	h.mu.Lock()
	h.session = sess
	h.mu.Unlock()
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
			renderError(w, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
			return
		}
		renderError(w, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if sess := h.currentSession(r); sess == nil {
		renderError(w, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
		return
	}
	rows, err := h.cfg.Servers.Servers()
	if err != nil {
		renderError(w, http.StatusInternalServerError, "读取服务器列表失败", "读取服务器列表失败，请重新运行 `sshmgr ui`。")
		return
	}
	renderFullPage(w, http.StatusOK, "servers.tmpl", pageData{Title: "服务器", Servers: rows})
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
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &session{id: id, csrf: csrf}, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// writePlainPage 写出无外部依赖的中文错误页/占位页。
func writePlainPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>", title, title, message)
}
