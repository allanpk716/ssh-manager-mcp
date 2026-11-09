package webui

import (
	"net/http"
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
)

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
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleSecretMatrix(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleSecretAuthorizationPreview(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleSecretAuthorizationConfirm(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func (h *handler) handleSecretDetailRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == secretsPath {
		h.handleSecretsRoot(w, r)
		return
	}
	h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
}

func formatUnixTime(value int64) string {
	if value == 0 {
		return "-"
	}
	return time.Unix(value, 0).Format("2006-01-02 15:04:05 Z07:00")
}
