package webui

import (
	"net/http"
)

// OpsUpdateStatus 是升级页展示的版本状态；字段含义与运维模块一致。
type OpsUpdateStatus struct {
	CurrentVersion        string
	RecommendedVersion    string
	LatestVersion         string
	LatestError           string
	TargetVersion         string
	AssetName             string
	CanUpgrade            bool
	Reason                string
	Downgrade             bool
	NewerThanAuthority    bool
	IsClient              bool
	PendingRestart        bool
	PendingRestartVersion string
}

// RecommendedDisplay 返回推荐版本展示值；空串显示为「读取不到」。
func (s OpsUpdateStatus) RecommendedDisplay() string {
	if s.RecommendedVersion == "" {
		return "读取不到"
	}
	return s.RecommendedVersion
}

// LatestDisplay 返回 GitHub 最新版本展示值；读取失败时显示读取不到。
func (s OpsUpdateStatus) LatestDisplay() string {
	if s.LatestVersion == "" {
		return "读取不到"
	}
	return s.LatestVersion
}

// TargetDisplay 返回目标版本展示值；没有目标时显示破折号。
func (s OpsUpdateStatus) TargetDisplay() string {
	if s.TargetVersion == "" {
		return "—"
	}
	return s.TargetVersion
}

// OpsUpdateApplyResult 是一次已提交的替换结果。
type OpsUpdateApplyResult struct {
	OldVersion string
	NewVersion string
	Warning    string
}

// OpsUpdateStep 标记升级失败在哪一步，或表示已有升级正在进行。
type OpsUpdateStep string

const (
	OpsUpdateStepCheck      OpsUpdateStep = "check"
	OpsUpdateStepDownload   OpsUpdateStep = "download"
	OpsUpdateStepVerify     OpsUpdateStep = "verify"
	OpsUpdateStepStaged     OpsUpdateStep = "staged"
	OpsUpdateStepReplace    OpsUpdateStep = "replace"
	OpsUpdateStepInProgress OpsUpdateStep = "in_progress"
)

// Text 返回升级阶段的中文显示名。
func (s OpsUpdateStep) Text() string {
	switch s {
	case OpsUpdateStepCheck:
		return "检查"
	case OpsUpdateStepDownload:
		return "下载"
	case OpsUpdateStepVerify:
		return "校验"
	case OpsUpdateStepStaged:
		return "试运行"
	case OpsUpdateStepReplace:
		return "替换"
	case OpsUpdateStepInProgress:
		return "进行中"
	default:
		return string(s)
	}
}

// OpsUpdateError 带失败阶段的升级错误。
type OpsUpdateError struct {
	Step    OpsUpdateStep
	Message string
}

func (e *OpsUpdateError) Error() string {
	return e.Message
}

const opsUpdatePath = "/ui/ops/update"

// handleOpsUpdate 渲染回环升级页并处理替换二进制的表单提交。单机与客户端替
// 换完成后提示重新运行 `sshmgr ui`；权威端的两步升级由后续票接入。
func (h *handler) handleOpsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsUpdatePath || h.cfg.Ops == nil || !h.caps.CanUpgradeBinary ||
		h.cfg.Role == RoleServer {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		instance := h.requestClientInstance(r)
		status, err := h.cfg.Ops.CheckUpdate(instance)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取升级状态失败", "读取升级状态失败，请稍后重试。")
			return
		}
		h.renderPage(w, r, http.StatusOK, "update.tmpl", pageData{Title: "升级", OpsUpdate: &status})
	case http.MethodPost:
		sourceIP := remoteIP(r)
		instance := h.requestClientInstance(r)
		result, err := h.cfg.Ops.ApplyUpdate(instance, sourceIP)
		status, statusErr := h.cfg.Ops.CheckUpdate(instance)
		data := pageData{Title: "升级"}
		if statusErr == nil {
			data.OpsUpdate = &status
		}
		if err != nil {
			var updateErr *OpsUpdateError
			if !asOpsUpdateError(err, &updateErr) {
				h.renderError(w, r, http.StatusInternalServerError, "升级失败", "升级失败，请稍后重试。")
				return
			}
			data.OpsUpdateError = updateErr
			h.renderPage(w, r, http.StatusBadRequest, "update.tmpl", data)
			return
		}
		data.OpsUpdateResult = &result
		h.renderPage(w, r, http.StatusOK, "update.tmpl", data)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "升级页只接受 GET 与 POST 请求。")
	}
}

// asOpsUpdateError 是 errors.As 的薄封装，避免处理器文件直接依赖错误包。
func asOpsUpdateError(err error, target **OpsUpdateError) bool {
	for err != nil {
		if cast, ok := err.(*OpsUpdateError); ok {
			*target = cast
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
