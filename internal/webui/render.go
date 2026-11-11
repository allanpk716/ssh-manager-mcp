package webui

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// parsedPage 保存一个页面预先解析好的整页模板与内容区片段模板。
type parsedPage struct {
	full     *template.Template
	fragment *template.Template
}

// pageData 是所有页面模板共享的渲染数据；新增页面时在这里扩展字段，
// 不另建一套传递结构。
type pageData struct {
	Title                            string
	Assets                           assetURLs
	Caps                             capabilities
	VersionHint                      string
	CSRFToken                        string
	Authenticated                    bool
	Servers                          []ServerRow
	ServerList                       []ServerListEntry
	NextSteps                        []NextStep
	AttentionFilter                  bool
	ServerDetail                     *ServerDetailView
	ServerEdit                       *serverEditView
	ServerDeletePreview              *ServerDeletePreviewView
	ServerDeleteNotice               string
	ServerImportError                string
	ServerImportPlan                 *ServerImportPlanView
	ServerImportRun                  *ServerImportRunView
	ServerCompleteQueue              string
	ServerComplete                   *serverCompleteView
	Secrets                          []SecretRow
	SecretFilterQuery                string
	SecretFilterServer               string
	SecretServerOptions              []ServerRow
	SecretForm                       secretFormView
	SecretDetail                     *SecretDetailView
	SecretDetailError                string
	SecretDetailNotice               string
	SecretDetailNote                 string
	SecretDetailEnvironmentName      string
	SecretMatrix                     *SecretMatrixView
	SecretAuthorizationPreview       SecretAuthorizationPreview
	SecretAuthorizationTargets       []SecretAuthorizationCell
	SecretAuthorizationReturnTo      string
	SecretAuthorizationNotice        string
	SoftwareInventoryDevices         []SoftwareInventoryDeviceSummary
	SoftwareInventoryDevice          *SoftwareInventoryDevice
	SoftwareInventorySearchRows      []SoftwareInventorySearchRow
	SoftwareInventoryQuery           string
	SoftwareInventoryTodos           SoftwareInventoryTodos
	SoftwareInventoryDeclarationForm *SoftwareInventoryDeclarationForm
	SoftwareInventoryDeclaration     *SoftwareInventoryDeclaration
	SoftwareInventoryDeclarations    []SoftwareInventoryDeclaration
	SoftwareInventoryDrafts          []SoftwareInventoryDraft
	SoftwareInventorySettings        *SoftwareInventorySettings
	SoftwareInventoryCollectorForm   *SoftwareInventoryCollectorForm
	SoftwareInventorySudoServer      string
	SoftwareInventorySudoEnabled     bool
	SoftwareInventoryResumeNotice    string
	SoftwareInventoryNotice          string
	SoftwareInventoryError           string
	ImportedCount                    int
	SkippedCount                     int
	FailedCount                      int
	PendingCount                     int
	AccessProfiles                   []AccessProfileSummary
	AccessProjects                   []AccessProjectSummary
	AccessProjectToken               *AccessProjectToken
	AccessProjectIncludeRevoked      bool
	AccessProjectNotice              string
	AccessProjectFormName            string
	AccessProjectFormProfileID       string
	AccessDeleteRefusal              *AccessProfileInUse
	AccessGrant                      *AccessGrantView
	AccessGrantNotice                string
	AccessFlash                      *AccessFlash
	AccessDeleteReverifyID           string
	AccessDeviceCodes                []AccessDeviceCodeSummary
	AccessDeviceIssue                *AccessDeviceIssueView
	AccessDeviceIssued               *AccessDeviceCodeResult
	AccessDeviceRevoke               *AccessDeviceCodeSummary
	AccessPairings                   []AccessPairingSummary
	AccessPairing                    *AccessPairingSummary
	AccessPairingForm                AccessPairingForm
	AccessPairingNotice              string
	OpsAuditRows                     []OpsAuditRow
	OpsAuditFilter                   OpsAuditFilterView
	OpsAuditHasMore                  bool
	OpsAuditOlderPath                string
	OpsBackupStatus                  *OpsBackupStatus
	OpsBackupFlash                   *OpsBackupFlash
	OpsBackupConfigForm              *OpsBackupConfigForm
	OpsSettings                      []OpsSetting
	OpsSettingForm                   OpsSettingForm
	OpsRole                          OpsRole
	OpsClientAuditRows               []OpsAuditRow
	OpsClientInstance                string
	OpsClientStatus                  *OpsClientStatus
	OpsClientInstances               []OpsClientInstance
	OpsClientNotice                  string
	OpsClientError                   string
	OpsUpdate                        *OpsUpdateStatus
	OpsUpdateResult                  *OpsUpdateApplyResult
	OpsUpdateError                   *OpsUpdateError
	OpsUpdateRestarting              bool
	OpsUpgradeServerError            *InitializationErrorView
	OpsUpgradeServerSuccess          bool
	ErrorTitle                       string
	ErrorMessage                     string
	AdminPasswordSet                 bool
	CertificateFingerprint           string
	LoginError                       string
	LockRemaining                    time.Duration
	NextPath                         string
	InitializationError              *InitializationErrorView
	InitializationMasterKeyPath      string
	InitializationSteps              []InitializationStepView
	InitializationInstallNotice      string
	InitializationInstallCommand     string
	InitializationServeAddresses     []InitializationServeAddressView
	InitializationNeedsManualListen  bool
	ClientGuideLines                 []string
	// TopologyPage 控制页面外壳是否加载拓扑图专用的脚本与样式；只有
	// 网络拓扑页为真。
	TopologyPage bool
}

// renderPage 按请求渲染页面：带 HX-Request 请求头（htmx 局部刷新）时只渲染
// 内容区片段，普通请求渲染带布局外壳的整页。页头共用的角色标记与版本提示在
// 这里统一注入。
func (h *handler) renderPage(w http.ResponseWriter, r *http.Request, status int, page string, data pageData) {
	data.Assets = h.assets
	data.Caps = h.caps
	data.VersionHint = h.cfg.versionHint()
	if sess := h.currentSession(r); sess != nil {
		data.CSRFToken = sess.csrf
		data.Authenticated = true
	}
	h.writePage(w, r, status, page, data)
}

// writePage 选择整页或内容区片段模板并写出响应。
func (h *handler) writePage(w http.ResponseWriter, r *http.Request, status int, page string, data pageData) {
	parsed, ok := h.pages[page]
	if !ok {
		http.Error(w, "页面模板加载失败", http.StatusInternalServerError)
		return
	}
	if isFragmentRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_ = parsed.fragment.ExecuteTemplate(w, "content", data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := parsed.full.ExecuteTemplate(w, "layout", data); err != nil {
		// 写出头后无法再改状态码；模板执行失败时至少保留已写出的页面开头。
		return
	}
}

// pageFuncs 是页面模板共用的小工具函数，只做展示层转换。
var pageFuncs = template.FuncMap{
	// joinReasons 把注意原因代号换成中文文案后拼接；未知代号原样保留，
	// 便于发现新的原因代号没有配文案。
	"joinReasons": func(items []string) string {
		labels := make([]string, 0, len(items))
		for _, item := range items {
			labels = append(labels, attentionReasonLabel(item))
		}
		return strings.Join(labels, "、")
	},
	"joinTags":       func(items []string) string { return strings.Join(items, ",") },
	"formatUnixTime": formatUnixTime,
	"formatTime":     func(value time.Time) string { return value.Format("2006-01-02 15:04:05 Z07:00") },
	"formatOptionalTime": func(value *time.Time) string {
		if value == nil {
			return "-"
		}
		return value.Format("2006-01-02 15:04:05 Z07:00")
	},
	"declarationFormPath": declarationFormPath,
	"addOne":              func(v int) int { return v + 1 },
	"credentialLabel":     CredentialTypeLabel,
	// secretCellName 与 secretTargetValue 把授权格字段名、确认目标的编码
	// 格式固定在 Go 端一处，模板只负责展示。
	"secretCellName":    secretCellName,
	"secretTargetValue": encodeSecretTarget,
	// defaultConfigPath 是导入路径表单的默认值：运行 Web UI 进程那台机器上
	// 当前用户的 ~/.ssh/config。
	"defaultConfigPath": func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".ssh", "config")
	},
	// completeQueue 把当前列表里需要注意的服务器拼成补全队列参数。
	"completeQueue": func(rows []ServerListEntry) string {
		var ids []string
		for _, row := range rows {
			if len(row.AttentionReasons) > 0 {
				ids = append(ids, row.ID)
			}
		}
		return url.Values{"queue": {strings.Join(ids, ",")}}.Encode()
	},
}

// loadPageTemplates 在构造 Handler 时解析全部页面模板；解析失败时 New 直接
// 返回错误，请求过程不再读取或解析模板文件。
func loadPageTemplates() (map[string]parsedPage, error) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return nil, fmt.Errorf("读取内嵌页面模板失败：%w", err)
	}
	pages := make(map[string]parsedPage)
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "layout.tmpl" {
			continue
		}
		pagePath := "templates/" + entry.Name()
		fragment, err := template.New(filepath.Base(pagePath)).Funcs(pageFuncs).ParseFS(templateFS, pagePath)
		if err != nil {
			return nil, fmt.Errorf("解析页面片段模板 %s 失败：%w", entry.Name(), err)
		}
		full, err := template.New("layout").Funcs(pageFuncs).ParseFS(templateFS, "templates/layout.tmpl", pagePath)
		if err != nil {
			return nil, fmt.Errorf("解析整页模板 %s 失败：%w", entry.Name(), err)
		}
		pages[entry.Name()] = parsedPage{full: full, fragment: fragment}
	}
	return pages, nil
}

// CredentialTypeLabel 把登录凭据类型代号换成页面文案；未知代号原样返回。
func CredentialTypeLabel(kind string) string {
	switch kind {
	case "password":
		return "密码"
	case "private_key":
		return "私钥"
	case "":
		return "未挂凭据"
	default:
		return kind
	}
}

// attentionReasonLabel 把注意原因代号换成页面文案；未知代号原样返回。
func attentionReasonLabel(code string) string {
	switch code {
	case models.ReasonNoCredential:
		return "未挂凭据"
	case models.ReasonNoRole:
		return "未设角色"
	case models.ReasonNeedsPassphrase:
		return "待补私钥口令"
	default:
		return code
	}
}

// isFragmentRequest 判断是否为 htmx 发起的局部刷新请求。
func isFragmentRequest(r *http.Request) bool {
	return r != nil && r.Header.Get("HX-Request") == "true"
}

// templateContents 返回内嵌的全部模板文件内容，供模板静态扫描测试使用。
func templateContents() (map[string]string, error) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := templateFS.ReadFile("templates/" + entry.Name())
		if err != nil {
			return nil, err
		}
		out[entry.Name()] = string(content)
	}
	return out, nil
}

// templateStaticProblems 返回模板内容违反内容安全策略的静态扫描问题。
func templateStaticProblems(content string) []string {
	var problems []string
	styleAttr := regexp.MustCompile(`(?i)\sstyle\s*=`)
	eventAttr := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	if styleAttr.MatchString(content) {
		problems = append(problems, "含内联 style 属性")
	}
	if eventAttr.MatchString(content) {
		problems = append(problems, "含内联事件属性")
	}
	if strings.Contains(content, "hx-on") {
		problems = append(problems, "含 hx-on")
	}

	scriptTag := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	openTag := regexp.MustCompile(`(?is)<script\b[^>]*>`)
	matches := scriptTag.FindAllStringSubmatch(content, -1)
	if len(openTag.FindAllString(content, -1)) != len(matches) {
		problems = append(problems, "含未闭合的 script 标签")
	}
	srcAttr := regexp.MustCompile(`(?i)\bsrc\s*=`)
	jsonType := regexp.MustCompile(`(?i)\btype\s*=\s*["']?application/json["']?`)
	for _, match := range matches {
		attributes, code := match[1], match[2]
		if srcAttr.MatchString(attributes) {
			if strings.TrimSpace(code) != "" {
				problems = append(problems, "带 src 的 script 标签之间含代码")
			}
			continue
		}
		if !jsonType.MatchString(attributes) {
			problems = append(problems, "含非 JSON 类型的内联 script 代码")
		}
	}
	return problems
}

// renderError 渲染中文错误页；htmx 请求得到片段，普通请求得到整页。
func (h *handler) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	data := pageData{
		Title:        title,
		Assets:       h.assets,
		ErrorTitle:   title,
		ErrorMessage: message,
	}
	h.writePage(w, r, status, "error.tmpl", data)
}
