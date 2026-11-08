package webui

import (
	"net/http"
)

const (
	opsBackupPath       = "/ui/ops/backup"
	opsBackupCreatePath = "/ui/ops/backup/create"
	opsBackupVerifyPath = "/ui/ops/backup/verify"
)

// OpsBackupFlash 是一次备份写操作后只显示一次的页面提示。
type OpsBackupFlash struct {
	Action      string // create 表示立即备份，verify 表示校验最新备份
	File        string
	OK          bool
	Skipped     bool
	FailureKind string
	Message     string
}

// handleOpsBackup 渲染备份页状态区；页面加载时不解密备份。
func (h *handler) handleOpsBackup(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsBackupPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "备份页只接受 GET 请求。")
		return
	}
	status, err := h.cfg.Ops.BackupStatus()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取备份状态失败", "读取备份状态失败，请稍后重试。")
		return
	}
	h.renderOpsBackupPage(w, r, http.StatusOK, backupConfigFormFromStatus(status))
}

// handleOpsBackupCreate 处理「立即备份」。权威端要求最近 5 分钟内验证过
// 管理员口令；单机回环不要求。
func (h *handler) handleOpsBackupCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "立即备份只接受 POST 请求。")
		return
	}
	if !h.requireRecentReverify(w, r, opsBackupPath) {
		return
	}
	result, err := h.cfg.Ops.CreateBackup(OpsWriteSource{RemoteAddress: remoteIP(r)})
	flash := &OpsBackupFlash{Action: "create", File: result.File}
	switch {
	case err != nil:
		flash.Message = err.Error()
	case result.Skipped:
		flash.Skipped = true
	default:
		flash.OK = true
	}
	h.setOpsBackupFlash(r, flash)
	http.Redirect(w, r, opsBackupPath, http.StatusSeeOther)
}

// handleOpsBackupVerify 处理「校验最新备份」；它不解密 vault，不要求重新
// 验证管理员口令。
func (h *handler) handleOpsBackupVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "校验最新备份只接受 POST 请求。")
		return
	}
	result, err := h.cfg.Ops.VerifyLatestBackup(OpsWriteSource{RemoteAddress: remoteIP(r)})
	flash := &OpsBackupFlash{Action: "verify", File: result.File}
	if err != nil {
		flash.FailureKind = result.FailureKind
		flash.Message = err.Error()
	} else {
		flash.OK = true
	}
	h.setOpsBackupFlash(r, flash)
	http.Redirect(w, r, opsBackupPath, http.StatusSeeOther)
}

func (h *handler) setOpsBackupFlash(r *http.Request, flash *OpsBackupFlash) {
	if sess := h.currentSession(r); sess != nil {
		h.mu.Lock()
		sess.opsBackupFlash = flash
		h.mu.Unlock()
	}
}

func (h *handler) takeOpsBackupFlash(r *http.Request) *OpsBackupFlash {
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	flash := sess.opsBackupFlash
	sess.opsBackupFlash = nil
	return flash
}

// TaskText 返回计划任务探测结果的中文显示。
func (s OpsBackupStatus) TaskText() string {
	switch {
	case s.TaskInstalled:
		return "已安装"
	case s.TaskProbed:
		return "未安装"
	default:
		return "未探测（仅支持 Windows 计划任务探测）"
	}
}

// FailureKindText 返回校验失败类别的中文显示。
func (s OpsBackupVerifyState) FailureKindText() string {
	switch s.FailureKind {
	case "passphrase":
		return "口令不符（或备份文件已损坏）"
	case "corrupt":
		return "文件损坏"
	case "incomplete":
		return "文件不完整"
	case "":
		return "未知原因"
	default:
		return s.FailureKind
	}
}
