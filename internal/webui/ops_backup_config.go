package webui

import (
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

const opsBackupConfigPath = "/ui/ops/backup/config"

// OpsBackupConfigForm 保存一次配置表单提交后的回填值与逐字段错误。
type OpsBackupConfigForm struct {
	Dir                 string
	Keep                string
	PassphraseFile      string
	DirError            string
	KeepError           string
	PassphraseFileError string
	GeneralError        string
}

// handleOpsBackupConfig 处理备份配置保存。权威端要求最近 5 分钟内验证过
// 管理员口令；单机回环不要求。
func (h *handler) handleOpsBackupConfig(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsBackupConfigPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "保存备份配置只接受 POST 请求。")
		return
	}
	if !h.requireRecentReverify(w, r, opsBackupPath) {
		return
	}
	config, form := parseOpsBackupConfigForm(r)
	if form.hasFieldError() {
		h.renderOpsBackupPage(w, r, http.StatusBadRequest, &form)
		return
	}
	if err := h.cfg.Ops.SaveBackupConfig(config, OpsWriteSource{RemoteAddress: remoteIP(r)}); err != nil {
		applyOpsBackupConfigError(&form, err)
		h.renderOpsBackupPage(w, r, http.StatusBadRequest, &form)
		return
	}
	h.setOpsBackupFlash(r, &OpsBackupFlash{Action: "config", OK: true})
	http.Redirect(w, r, opsBackupPath, http.StatusSeeOther)
}

// parseOpsBackupConfigForm 解析三个字段，并尽量同时给出所有字段错误，方便
// owner 一次修正。
func parseOpsBackupConfigForm(r *http.Request) (OpsBackupConfig, OpsBackupConfigForm) {
	var config OpsBackupConfig
	var form OpsBackupConfigForm
	form.Dir = strings.TrimSpace(r.PostFormValue("dir"))
	form.PassphraseFile = strings.TrimSpace(r.PostFormValue("passphrase_file"))
	form.Keep = strings.TrimSpace(r.PostFormValue("keep"))

	if form.Dir == "" || !filepath.IsAbs(form.Dir) {
		form.DirError = "备份目录必须是绝对路径。"
	}
	keep, err := strconv.Atoi(form.Keep)
	if err != nil || keep < 0 {
		form.KeepError = "保留份数必须是 0 或正整数。"
	} else {
		config.Keep = keep
	}
	if form.PassphraseFile == "" || !filepath.IsAbs(form.PassphraseFile) {
		form.PassphraseFileError = "口令文件必须是绝对路径。"
	} else if form.DirError == "" && passphraseInsideBackupDir(form.PassphraseFile, form.Dir) {
		form.PassphraseFileError = "口令文件不能位于备份目录里。"
	}
	if form.DirError == "" {
		config.Dir = form.Dir
	}
	if form.PassphraseFileError == "" {
		config.PassphraseFile = form.PassphraseFile
	}
	return config, form
}

// passphraseInsideBackupDir 只做表单层的词法检查；符号链接等复杂位置仍由运
// 维模块在保存前统一校验。
func passphraseInsideBackupDir(passphraseFile, backupDir string) bool {
	if passphraseFile == "" || backupDir == "" {
		return false
	}
	passphrase := filepath.Clean(passphraseFile)
	dir := filepath.Clean(backupDir)
	return passphrase == dir || strings.HasPrefix(passphrase, dir+string(filepath.Separator))
}

// applyOpsBackupConfigError 把运维模块的字段错误放回对应表单项；其他错误
// 显示在表单顶部。
func applyOpsBackupConfigError(form *OpsBackupConfigForm, err error) {
	var configErr *OpsBackupConfigError
	if !errors.As(err, &configErr) {
		form.GeneralError = err.Error()
		return
	}
	switch configErr.Field {
	case "dir":
		form.DirError = configErr.Err.Error()
	case "keep":
		form.KeepError = configErr.Err.Error()
	case "passphrase_file":
		form.PassphraseFileError = configErr.Err.Error()
	default:
		form.GeneralError = configErr.Err.Error()
	}
}

func (f OpsBackupConfigForm) hasFieldError() bool {
	return f.DirError != "" || f.KeepError != "" || f.PassphraseFileError != "" || f.GeneralError != ""
}

// renderOpsBackupPage 统一读取状态并渲染备份页；配置表单出错时仍保留状态区，
// 方便对照当前备份情况。
func (h *handler) renderOpsBackupPage(w http.ResponseWriter, r *http.Request, status int, form *OpsBackupConfigForm) {
	backupStatus, err := h.cfg.Ops.BackupStatus()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取备份状态失败", "读取备份状态失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, status, "backup.tmpl", pageData{
		Title: "备份", OpsBackupStatus: &backupStatus,
		OpsBackupFlash: h.takeOpsBackupFlash(r), OpsBackupConfigForm: form,
	})
}

// backupConfigFormFromStatus 用当前配置回填表单；配置缺失或有误时给空值，
// 让 owner 直接填写修复。
func backupConfigFormFromStatus(status OpsBackupStatus) *OpsBackupConfigForm {
	keep := strconv.Itoa(status.ConfigKeep)
	if status.ConfigError != "" && status.ConfigKeep == 0 {
		keep = ""
	}
	return &OpsBackupConfigForm{
		Dir: status.ConfigDir, Keep: keep,
		PassphraseFile: status.ConfigPassphraseFile,
	}
}
