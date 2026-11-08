package webui

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AccessProjectSummary 是项目页列表的一行：令牌本体从不出现，只有前缀。
type AccessProjectSummary struct {
	ID          string
	Name        string
	TokenPrefix string
	Status      string
	ProfileID   string
	ProfileName string
	CreatedAt   time.Time
}

// ProfileDisplay 返回项目绑定的 profile 展示名；profile 已删除时退回原始
// 绑定标识符，与命令行列表的口径一致。
func (p AccessProjectSummary) ProfileDisplay() string {
	if p.ProfileName != "" {
		return p.ProfileName
	}
	return p.ProfileID
}

// StatusText 返回项目生命周期状态的中文展示。
func (p AccessProjectSummary) StatusText() string {
	switch p.Status {
	case "active":
		return "启用中"
	case "disabled":
		return "已停用"
	case "revoked":
		return "已吊销"
	default:
		return p.Status
	}
}

// IsRevoked 表示项目已吊销：吊销不可逆，只剩删除入口。
func (p AccessProjectSummary) IsRevoked() bool { return p.Status == "revoked" }

// CanRotate 表示项目还能轮换令牌；已吊销项目轮换会被拒绝，不显示入口。
func (p AccessProjectSummary) CanRotate() bool { return !p.IsRevoked() }

// CanDisable 表示项目当前启用中，可以停用。
func (p AccessProjectSummary) CanDisable() bool { return p.Status == "active" }

// CanEnable 表示项目当前已停用，可以重新启用。
func (p AccessProjectSummary) CanEnable() bool { return p.Status == "disabled" }

// CanRevoke 表示项目还未吊销，可以执行吊销。
func (p AccessProjectSummary) CanRevoke() bool { return !p.IsRevoked() }

// AccessProjectToken 是签发或轮换项目令牌的一次性结果：令牌与已代入令牌
// 的 `.mcp.json` 片段只在同一次提交响应里展示。
type AccessProjectToken struct {
	ID      string
	Name    string
	Token   string
	Snippet string
}

const (
	accessProjectsPath = "/ui/access/projects"

	projectRotatePath  = "/ui/access/projects/rotate"
	projectDisablePath = "/ui/access/projects/disable"
	projectEnablePath  = "/ui/access/projects/enable"
	projectRevokePath  = "/ui/access/projects/revoke"
	projectDeletePath  = "/ui/access/projects/delete"
)

// handleAccessProjects 渲染项目令牌列表；include_revoked=1 时把已吊销的
// 项目也显示出来。
func (h *handler) handleAccessProjects(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != accessProjectsPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleAccessProjectAdd(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "项目页只接受 GET 与 POST 请求。")
		return
	}
	includeRevoked := r.URL.Query().Get("include_revoked") == "1"
	projects, err := h.cfg.Access.ListProjects(includeRevoked)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取项目令牌列表失败", "读取项目令牌列表失败，请稍后重试。")
		return
	}
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取 profile 列表失败", "读取 profile 列表失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "projects.tmpl", pageData{
		Title: "项目令牌", AccessProjects: projects, AccessProfiles: profiles,
		AccessProjectIncludeRevoked: includeRevoked,
		AccessProjectNotice:         projectReverifyNotice(r.URL.Query()),
		AccessProjectFormName:       r.URL.Query().Get("add_name"),
		AccessProjectFormProfileID:  r.URL.Query().Get("add_profile"),
	})
}

// projectReverifyNotice 返回重新验证回跳后的页面提示：签发表单或操作按钮
// 仍在页面上，提示告诉 owner 验证完成后重新提交哪一步。
func projectReverifyNotice(query url.Values) string {
	switch {
	case query.Get("reverify_add") == "1":
		return "签发项目令牌前需要重新验证管理员口令，验证后请重新提交签发表单。"
	case query.Get("reverify_rotate") != "":
		return "轮换项目令牌前需要重新验证管理员口令，验证后请重新提交轮换。"
	case query.Get("reverify_enable") != "":
		return "重新启用项目前需要重新验证管理员口令，验证后请重新提交。"
	default:
		return ""
	}
}

// handleAccessProjectAdd 处理项目令牌签发表单。令牌只在这一个响应里出现：
// 成功时直接渲染一次性页面（带 no-store），不重定向，也不把令牌写进任何
// 链接、日志或会话。
func (h *handler) handleAccessProjectAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	profileID := r.PostFormValue("profile_id")
	if name == "" || profileID == "" {
		h.renderError(w, r, http.StatusBadRequest, "签发项目令牌失败", "项目名称和 profile 都不能为空。")
		return
	}
	if !h.projectSensitiveActionAllowed(w, r, url.Values{
		"reverify_add": {"1"}, "add_name": {name}, "add_profile": {profileID},
	}) {
		return
	}
	token, err := h.cfg.Access.AddProject(name, profileID, AccessWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "签发项目令牌失败", "签发项目令牌失败，请稍后重试。")
		return
	}
	h.renderProjectToken(w, r, "已签发项目令牌", token)
}

// renderProjectToken 渲染签发或轮换后的令牌一次性页面。根处理器对全部响
// 应已设置 no-store；这里再显式设置一次，保证令牌页即使被单独复用也不缓
// 存。
func (h *handler) renderProjectToken(w http.ResponseWriter, r *http.Request, title string, token AccessProjectToken) {
	w.Header().Set("Cache-Control", "no-store")
	h.renderPage(w, r, http.StatusOK, "project-token.tmpl", pageData{
		Title: title, AccessProjectToken: &token,
	})
}

// handleAccessProjectRotate 轮换项目令牌并把新令牌按一次性页面直接返回。
func (h *handler) handleAccessProjectRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "轮换项目令牌只接受 POST 请求。")
		return
	}
	id := r.PostFormValue("project_id")
	if id == "" {
		h.renderError(w, r, http.StatusBadRequest, "轮换项目令牌失败", "缺少项目标识符。")
		return
	}
	if !h.projectSensitiveActionAllowed(w, r, url.Values{"reverify_rotate": {id}}) {
		return
	}
	token, err := h.cfg.Access.RotateProject(id, AccessWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "轮换项目令牌失败", "轮换项目令牌失败，请稍后重试。")
		return
	}
	h.renderProjectToken(w, r, "已轮换项目令牌", token)
}

// handleAccessProjectDisable 停用一个项目令牌，停用可逆。
func (h *handler) handleAccessProjectDisable(w http.ResponseWriter, r *http.Request) {
	h.handleAccessProjectStatus(w, r, "停用项目失败", "停用项目失败，请稍后重试。", false, "",
		func(id string, source AccessWriteSource) error {
			return h.cfg.Access.DisableProject(id, source)
		})
}

// handleAccessProjectEnable 重新启用一个已停用的项目令牌。
func (h *handler) handleAccessProjectEnable(w http.ResponseWriter, r *http.Request) {
	h.handleAccessProjectStatus(w, r, "重新启用项目失败", "重新启用项目失败，请稍后重试。", false, "reverify_enable",
		func(id string, source AccessWriteSource) error {
			return h.cfg.Access.EnableProject(id, source)
		})
}

// handleAccessProjectRevoke 永久吊销项目令牌；表单必须勾选确认，避免误触。
func (h *handler) handleAccessProjectRevoke(w http.ResponseWriter, r *http.Request) {
	h.handleAccessProjectStatus(w, r, "吊销项目失败", "吊销项目失败，请稍后重试。", true, "",
		func(id string, source AccessWriteSource) error {
			return h.cfg.Access.RevokeProject(id, source)
		})
}

// handleAccessProjectDelete 删除一个已吊销的项目行；是否已吊销由访问管理
// 模块判定，页面只对已吊销项目渲染入口。
func (h *handler) handleAccessProjectDelete(w http.ResponseWriter, r *http.Request) {
	h.handleAccessProjectStatus(w, r, "删除项目失败", "删除项目失败，请稍后重试。", false, "",
		func(id string, source AccessWriteSource) error {
			return h.cfg.Access.DeleteProject(id, source)
		})
}

// handleAccessProjectStatus 是项目生命周期动作的共用处理器：校验方法、
// 标识符与可选的确认勾选，调用访问管理来源后回到项目列表。
func (h *handler) handleAccessProjectStatus(
	w http.ResponseWriter, r *http.Request, title, message string, requireConfirm bool, reverifyParam string,
	apply func(id string, source AccessWriteSource) error,
) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "项目操作只接受 POST 请求。")
		return
	}
	id := r.PostFormValue("project_id")
	if id == "" {
		h.renderError(w, r, http.StatusBadRequest, title, "缺少项目标识符。")
		return
	}
	if requireConfirm && r.PostFormValue("confirm") == "" {
		h.renderError(w, r, http.StatusBadRequest, title, "请先勾选确认永久吊销。")
		return
	}
	if reverifyParam != "" {
		if !h.projectSensitiveActionAllowed(w, r, url.Values{reverifyParam: {id}}) {
			return
		}
	}
	if err := apply(id, AccessWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, title, message)
		return
	}
	http.Redirect(w, r, accessProjectsPath, http.StatusSeeOther)
}

// projectSensitiveActionAllowed 校验权威端敏感项目操作：签发、轮换与重新
// 启用会签发新令牌或恢复权限，必须最近 5 分钟内验证过管理员口令。未验证
// 时重定向到重新验证页；resume 保存验证后要回到的项目页查询参数。单机不
// 要求重新验证。
func (h *handler) projectSensitiveActionAllowed(w http.ResponseWriter, r *http.Request, resume url.Values) bool {
	if h.cfg.Role != RoleServer || h.recentlyVerified(h.currentSession(r)) {
		return true
	}
	http.Redirect(w, r, reverifyPath(accessProjectsPath+"?"+resume.Encode()), http.StatusSeeOther)
	return false
}
