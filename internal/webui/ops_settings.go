package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// OpsWriteSource 描述一次运维写请求的来源；适配器把它换算成审计行里的
// Web UI 来源和来源地址。
type OpsWriteSource struct {
	RemoteAddress string
}

// OpsSettingSource 是设置项当前生效值的来源层次。
type OpsSettingSource string

const (
	OpsSettingSourceDefault     OpsSettingSource = "default"
	OpsSettingSourceStore       OpsSettingSource = "store"
	OpsSettingSourceFlag        OpsSettingSource = "flag"
	OpsSettingSourceEnvironment OpsSettingSource = "env"
)

// OpsSettingType 驱动设置页选择哪种表单控件。
type OpsSettingType string

const (
	OpsSettingTypeSwitch          OpsSettingType = "switch"
	OpsSettingTypeListenAddresses OpsSettingType = "listen_addresses"
	OpsSettingTypePort            OpsSettingType = "port"
	OpsSettingTypeDiscoveryName   OpsSettingType = "discovery_name"
	OpsSettingTypeProfile         OpsSettingType = "profile"
	OpsSettingTypeDuration        OpsSettingType = "duration"
)

// OpsSettingChoice 是有限候选值的值与显示文字。
type OpsSettingChoice struct {
	Value string
	Label string
}

// OpsSetting 是设置页的一行数据，由运维模块的设置项表适配而来。
type OpsSetting struct {
	Key             string
	Type            OpsSettingType
	StoredValue     string
	CurrentValue    string
	DefaultValue    string
	Source          OpsSettingSource
	Overridden      bool
	CanBeOverridden bool
	Effect          string
	Choices         []OpsSettingChoice
}

// SourceText 返回取值来源的中文显示。
func (s OpsSetting) SourceText() string {
	switch s.Source {
	case OpsSettingSourceStore:
		return "设置存储"
	case OpsSettingSourceFlag:
		return "命令行旗标"
	case OpsSettingSourceEnvironment:
		return "环境变量"
	case OpsSettingSourceDefault:
		return "默认值"
	default:
		return string(s.Source)
	}
}

// InputValue 返回表单回填值：优先显示设置存储里的原始值，未设置时显示默
// 认值；实际生效值单独展示，避免覆盖状态下误导 owner。
func (s OpsSetting) InputValue() string {
	if s.StoredValue != "" {
		return s.StoredValue
	}
	return s.DefaultValue
}

// OpsRole 是本机角色文件的只读展示。
type OpsRole struct {
	RoleName      string
	SetupComplete bool
	Path          string
}

// OpsSettingForm 保存一次提交回填后的字段值与校验错误。
type OpsSettingForm struct {
	Key   string
	Value string
	Error string
}

// RoleText 返回角色的中文显示；空角色表示尚未初始化。
func (r OpsRole) RoleText() string {
	switch r.RoleName {
	case "standalone":
		return "单机"
	case "server":
		return "权威端"
	case "client":
		return "客户端"
	case "":
		return "尚未初始化"
	default:
		return r.RoleName
	}
}

const opsSettingsPath = "/ui/ops/settings"

func (h *handler) handleOpsSettings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsSettingsPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method == http.MethodPost {
		h.handleOpsSettingsPost(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "设置页只接受 GET 与 POST 请求。")
		return
	}
	form := OpsSettingForm{}
	values := r.URL.Query()
	if values.Get("reverify") == "1" {
		form.Key = values.Get("key")
		form.Value = values.Get("value")
	}
	h.renderOpsSettings(w, r, http.StatusOK, form)
}

func (h *handler) handleOpsSettingsPost(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PostFormValue("key"))
	if key == "" {
		h.renderError(w, r, http.StatusBadRequest, "保存设置失败", "缺少设置项键名。")
		return
	}
	value := r.PostFormValue("value")
	next := url.Values{"reverify": {"1"}, "key": {key}, "value": {value}}
	if !h.requireRecentReverify(w, r, opsSettingsPath+"?"+next.Encode()) {
		return
	}
	source := OpsWriteSource{RemoteAddress: remoteIP(r)}
	if r.PostFormValue("action") == "restore_default" {
		if err := h.cfg.Ops.UnsetSetting(key, source); err != nil {
			h.renderOpsSettings(w, r, http.StatusBadRequest, OpsSettingForm{
				Key: key, Error: err.Error(),
			})
			return
		}
		http.Redirect(w, r, opsSettingsPath, http.StatusSeeOther)
		return
	}
	if err := h.cfg.Ops.ValidateSetting(key, value); err != nil {
		h.renderOpsSettings(w, r, http.StatusBadRequest, OpsSettingForm{
			Key: key, Value: value, Error: err.Error(),
		})
		return
	}
	if err := h.cfg.Ops.SetSetting(key, value, source); err != nil {
		h.renderOpsSettings(w, r, http.StatusBadRequest, OpsSettingForm{
			Key: key, Value: value, Error: err.Error(),
		})
		return
	}
	http.Redirect(w, r, opsSettingsPath, http.StatusSeeOther)
}

func (h *handler) renderOpsSettings(w http.ResponseWriter, r *http.Request, status int, form OpsSettingForm) {
	settings, err := h.cfg.Ops.ListSettings()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取设置失败", "读取设置项失败，请稍后重试。")
		return
	}
	role, err := h.cfg.Ops.ReadRole()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取本机角色失败", "读取本机角色文件失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, status, "settings.tmpl", pageData{
		Title: "设置", OpsSettings: settings, OpsRole: role, OpsSettingForm: form,
	})
}
