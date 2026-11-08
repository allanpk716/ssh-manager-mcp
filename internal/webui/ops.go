package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OpsAuditQuery 是 Web UI 处理器传给运维窄接口的审计查询条件。处理器只负
// 责解析表单与保留链接，不知道数据库结构。
type OpsAuditQuery struct {
	Since     int64
	Servers   []string
	Projects  []string
	OwnerOnly bool
	Actions   []string
	Statuses  []string
	Source    string
	SourceIP  string
	BeforeID  int64
	Limit     int
}

// OpsAuditRow 是审计页的一行展示数据。
type OpsAuditRow struct {
	ID          int64
	TS          time.Time
	ProjectID   string
	ProjectName string
	ServerID    string
	ServerName  string
	Action      string
	Command     string
	Sudo        bool
	Status      string
	ExitCode    int
	DurationMS  int64
	Source      string
	SourceIP    string
}

// ProjectDisplay 返回项目显示名；owner 行显示「owner」。
func (r OpsAuditRow) ProjectDisplay() string {
	if r.ProjectName != "" {
		return r.ProjectName
	}
	if r.ProjectID != "" {
		return r.ProjectID
	}
	return "owner"
}

// ServerDisplay 返回服务器显示名；没有服务器上下文时显示「无」。
func (r OpsAuditRow) ServerDisplay() string {
	if r.ServerName != "" {
		return r.ServerName
	}
	if r.ServerID != "" {
		return r.ServerID
	}
	return "无"
}

// SourceText 返回来源的中文显示名；旧数据没有来源时显示「未知」。
func (r OpsAuditRow) SourceText() string {
	switch r.Source {
	case "cli":
		return "命令行"
	case "web_ui":
		return "Web UI"
	case "agent":
		return "agent"
	case "":
		return "未知"
	default:
		return r.Source
	}
}

// CommandDisplay 返回审计摘要；空摘要显示破折号，避免空表格单元。
func (r OpsAuditRow) CommandDisplay() string {
	if r.Command == "" {
		return "—"
	}
	return r.Command
}

// OpsStore 是 Web UI 读取运维能力的唯一入口。单机与权威端由运维模块适配
// 实现；处理器测试用桩，本包不接触数据库结构和文件布局。
type OpsStore interface {
	QueryAudit(query OpsAuditQuery) ([]OpsAuditRow, error)
	BackupStatus() (OpsBackupStatus, error)
	ReadBackupConfig() (OpsBackupConfig, error)
	SaveBackupConfig(config OpsBackupConfig, source OpsWriteSource) error
	CreateBackup(source OpsWriteSource) (OpsBackupCreateResult, error)
	VerifyLatestBackup(source OpsWriteSource) (OpsBackupVerifyResult, error)
	ListSettings() ([]OpsSetting, error)
	ValidateSetting(key, value string) error
	SetSetting(key, value string, source OpsWriteSource) error
	UnsetSetting(key string, source OpsWriteSource) error
	ReadRole() (OpsRole, error)
	// QueryClientAudit 读取客户端实例目录中的本机审计日志文件。instance 空串
	// 表示默认实例，处理器按当前浏览器会话传入。
	QueryClientAudit(instance string, limit int) ([]OpsAuditRow, error)
	// CheckUpdate 与 ApplyUpdate 的 instance 语义同上；升级服务按传入实例读
	// 取推荐版本。
	CheckUpdate(instance string) (OpsUpdateStatus, error)
	ApplyUpdate(instance, sourceIP string) (OpsUpdateApplyResult, error)
	// ListClientInstances 返回本机客户端实例及材料是否齐全。
	ListClientInstances() ([]OpsClientInstance, error)
	// ClientStatus 读取指定客户端实例的连接状态与本地快照摘要。
	ClientStatus(instance string) (OpsClientStatus, error)
	// SwitchClientInstance 校验目标实例可切换并写实例本机审计。
	SwitchClientInstance(from, instance, sourceIP string) error
	// SyncClient 立即同步指定客户端实例并写实例本机审计。
	SyncClient(instance, sourceIP string) (OpsClientStatus, error)
}

// OpsBackupConfig 是备份配置表单的三个字段。
type OpsBackupConfig struct {
	Dir            string
	Keep           int
	PassphraseFile string
}

// OpsBackupConfigError 标明备份配置的哪个字段有误。
type OpsBackupConfigError struct {
	Field string
	Err   error
}

func (e *OpsBackupConfigError) Error() string { return e.Err.Error() }
func (e *OpsBackupConfigError) Unwrap() error { return e.Err }

// OpsBackupStatus 是备份页状态区的展示数据。ConfigError 非空且 ConfigDir
// 为空串表示配置缺失；ConfigError 非空且 ConfigDir 非空表示配置有误。
type OpsBackupStatus struct {
	ConfigPath           string
	ConfigError          string
	ConfigMissing        bool
	ConfigDir            string
	ConfigKeep           int
	ConfigPassphraseFile string
	TaskInstalled        bool
	TaskProbed           bool
	TotalBackups         int
	LatestBackup         string
	LatestTime           time.Time
	HasLatest            bool
	Stale                bool
	Verify               *OpsBackupVerifyState
}

// OpsBackupVerifyState 是针对当前最新文件的最近一次校验结果。
type OpsBackupVerifyState struct {
	File        string
	OK          bool
	FailureKind string
	At          time.Time
}

// OpsBackupCreateResult 是立即备份的结果；Skipped 表示已有备份正在进行。
type OpsBackupCreateResult struct {
	File    string
	Skipped bool
}

// OpsBackupVerifyResult 是一键校验最新备份的结果；失败时 FailureKind 为
// 口令不符、文件损坏或文件不完整之一。
type OpsBackupVerifyResult struct {
	File        string
	OK          bool
	FailureKind string
}

// OpsAuditFilterView 保存过滤表单的当前值，服务器渲染时原样回填。
type OpsAuditFilterView struct {
	Since     string
	Action    string
	Server    string
	Project   string
	OwnerOnly bool
	Status    string
	Source    string
	SourceIP  string
	BeforeID  int64
}

const (
	opsRootPath  = "/ui/ops/"
	opsAuditPath = "/ui/ops/audit"
	opsAuditPage = 100
)

// registerOpsRoutes 按本机角色注册运维区域路由：单机与权威端注册备份、设
// 置与权威 vault 审计页；客户端注册客户端页、升级页与本机审计页。
func (h *handler) registerOpsRoutes() {
	h.mux.HandleFunc(opsRootPath, h.handleOpsRoot)
	if h.cfg.Ops == nil {
		h.caps.ShowOpsArea = false
		h.caps.CanQueryVaultAudit = false
		h.caps.CanManageBackups = false
		h.caps.CanQueryClientAudit = false
		h.caps.CanUpgradeBinary = false
		h.caps.CanUseClientOps = false
		return
	}
	if h.caps.CanUpgradeBinary {
		if h.cfg.Role == RoleServer {
			h.mux.HandleFunc(opsUpdatePath, h.handleAuthorityOpsUpdate)
		} else {
			h.mux.HandleFunc(opsUpdatePath, h.handleOpsUpdate)
		}
	}
	if h.cfg.Role == RoleServer && h.caps.CanRestartService {
		h.mux.HandleFunc(opsUpdateRestartPath, h.handleRestartAfterResponse(h.handleAuthorityOpsUpdateRestart))
	}
	if h.cfg.Role == RoleClient {
		if h.caps.CanUseClientOps {
			h.mux.HandleFunc(opsClientPath, h.handleOpsClient)
			h.mux.HandleFunc(opsClientSyncPath, h.handleOpsClientSync)
			h.mux.HandleFunc(opsClientInstancePath, h.handleOpsClientInstance)
		}
		if h.caps.CanQueryClientAudit {
			h.mux.HandleFunc(opsClientAuditPath, h.handleOpsClientAudit)
		}
		return
	}
	if h.caps.CanManageBackups {
		h.mux.HandleFunc(opsBackupPath, h.handleOpsBackup)
		h.mux.HandleFunc(opsBackupConfigPath, h.handleOpsBackupConfig)
		h.mux.HandleFunc(opsBackupCreatePath, h.handleOpsBackupCreate)
		h.mux.HandleFunc(opsBackupVerifyPath, h.handleOpsBackupVerify)
	}
	if h.caps.CanManageSettings {
		h.mux.HandleFunc(opsSettingsPath, h.handleOpsSettings)
	}
	if h.caps.CanQueryVaultAudit {
		h.mux.HandleFunc(opsAuditPath, h.handleOpsAudit)
	}
}

func (h *handler) handleOpsRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsRootPath || h.cfg.Ops == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	target := opsAuditPath
	if h.cfg.Role == RoleClient {
		switch {
		case h.caps.CanUseClientOps:
			target = opsClientPath
		case h.caps.CanQueryClientAudit:
			target = opsClientAuditPath
		case h.caps.CanUpgradeBinary:
			target = opsUpdatePath
		default:
			h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
			return
		}
	} else if !h.caps.CanQueryVaultAudit {
		if h.caps.CanUpgradeBinary && h.cfg.Role != RoleServer {
			http.Redirect(w, r, opsUpdatePath, http.StatusSeeOther)
			return
		}
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *handler) handleOpsAudit(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsAuditPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "审计页只接受 GET 请求。")
		return
	}
	view, query, err := parseOpsAuditFilter(r.URL.Query())
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "审计查询条件有误", err.Error())
		return
	}
	query.Limit = opsAuditPage + 1
	rows, err := h.cfg.Ops.QueryAudit(query)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取审计记录失败", "读取审计记录失败，请稍后重试。")
		return
	}
	hasMore := len(rows) > opsAuditPage
	if hasMore {
		rows = rows[:opsAuditPage]
	}
	olderPath := ""
	if hasMore && len(rows) > 0 {
		values := opsAuditFilterValues(view)
		values.Set("before", strconv.FormatInt(rows[len(rows)-1].ID, 10))
		olderPath = opsAuditPath + "?" + values.Encode()
	}
	data := pageData{
		Title: "审计", OpsAuditRows: rows, OpsAuditFilter: view,
		OpsAuditHasMore: hasMore, OpsAuditOlderPath: olderPath,
	}
	page := "audit.tmpl"
	if isFragmentRequest(r) {
		page = "audit-results.tmpl"
	}
	h.renderPage(w, r, http.StatusOK, page, data)
}

// parseOpsAuditFilter 解析过滤条件，并返回回填表单所需的视图。
func parseOpsAuditFilter(values url.Values) (OpsAuditFilterView, OpsAuditQuery, error) {
	view := OpsAuditFilterView{
		Since: strings.TrimSpace(values.Get("since")), Action: strings.TrimSpace(values.Get("action")),
		Server: strings.TrimSpace(values.Get("server")), Project: strings.TrimSpace(values.Get("project")),
		OwnerOnly: values.Get("owner") == "1", Status: strings.TrimSpace(values.Get("status")),
		Source: values.Get("source"), SourceIP: strings.TrimSpace(values.Get("source_ip")),
	}
	query := OpsAuditQuery{OwnerOnly: view.OwnerOnly, Source: view.Source, SourceIP: view.SourceIP}
	if view.Since != "" {
		since, err := parseOpsAuditSince(view.Since)
		if err != nil {
			return view, query, err
		}
		query.Since = since
	}
	if view.Action != "" {
		query.Actions = []string{view.Action}
	}
	if view.Server != "" {
		query.Servers = []string{view.Server}
	}
	if view.Project != "" {
		query.Projects = []string{view.Project}
	}
	if view.Status != "" {
		query.Statuses = []string{view.Status}
	}
	switch query.Source {
	case "", "cli", "web_ui", "agent":
	default:
		return view, query, errOpsAuditSource
	}
	before := strings.TrimSpace(values.Get("before"))
	if before != "" {
		id, err := strconv.ParseInt(before, 10, 64)
		if err != nil || id <= 0 {
			return view, query, errOpsAuditBefore
		}
		view.BeforeID = id
		query.BeforeID = id
	}
	return view, query, nil
}

var (
	errOpsAuditSource = errOpsAuditInput("来源只能选择命令行、Web UI 或 agent。")
	errOpsAuditBefore = errOpsAuditInput("翻页编号必须是正整数。")
)

type auditInputError struct{ message string }

func (e auditInputError) Error() string { return e.message }

func errOpsAuditInput(message string) error { return auditInputError{message: message} }

// parseOpsAuditSince 支持 datetime-local 表单值、日期、RFC3339 时间戳，以及
// 脚本直接传入的 Unix 秒数。
func parseOpsAuditSince(value string) (int64, error) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return seconds, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", time.RFC3339, "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed.Unix(), nil
		}
	}
	return 0, errOpsAuditInput("时间下限无法识别，请使用页面提供的日期时间格式。")
}

// opsAuditFilterValues 把当前过滤条件转回查询参数；before 由调用方按本页
// 最小编号单独追加。
func opsAuditFilterValues(view OpsAuditFilterView) url.Values {
	values := url.Values{}
	if view.Since != "" {
		values.Set("since", view.Since)
	}
	if view.Action != "" {
		values.Set("action", view.Action)
	}
	if view.Server != "" {
		values.Set("server", view.Server)
	}
	if view.Project != "" {
		values.Set("project", view.Project)
	}
	if view.OwnerOnly {
		values.Set("owner", "1")
	}
	if view.Status != "" {
		values.Set("status", view.Status)
	}
	if view.Source != "" {
		values.Set("source", view.Source)
	}
	if view.SourceIP != "" {
		values.Set("source_ip", view.SourceIP)
	}
	return values
}
