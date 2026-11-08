package ops

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/listenplan"
	"ssh-manager-mcp/internal/offlinelimit"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/switchstate"
)

// SettingSource 表示当前生效值的来源层次。
type SettingSource string

const (
	SettingSourceDefault     SettingSource = "default"
	SettingSourceStore       SettingSource = "store"
	SettingSourceFlag        SettingSource = "flag"
	SettingSourceEnvironment SettingSource = "env"
)

// SettingType 驱动 Web 表单选择哪种输入控件；适配层会把它原样转换给页面。
type SettingType string

const (
	SettingTypeSwitch          SettingType = "switch"
	SettingTypeListenAddresses SettingType = "listen_addresses"
	SettingTypePort            SettingType = "port"
	SettingTypeDiscoveryName   SettingType = "discovery_name"
	SettingTypeProfile         SettingType = "profile"
	SettingTypeDuration        SettingType = "duration"
)

// SettingInputs 是 serve 进程显式注入的环境变量与启动旗标。指针为空表示
// 那一层没有显式设置；非法环境变量由调用方按现有规则视为未设置。
type SettingInputs struct {
	PairingEnv       *bool
	PairingFlag      *bool
	DiscoveryEnv     *bool
	DiscoveryFlag    *bool
	MetadataEditEnv  *bool
	MetadataEditFlag *bool
}

// SettingChoice 是只能从有限集合里选择的设置项候选值。
type SettingChoice struct {
	Value string
	Label string
}

// Setting 是设置项表的一行。CurrentValue 是实际生效值；StoredValue 保留设
// 置存储里的原始写法，页面据此回填表单。
type Setting struct {
	Key             string
	Type            SettingType
	StoredValue     string
	CurrentValue    string
	DefaultValue    string
	Source          SettingSource
	Overridden      bool
	CanBeOverridden bool
	Effect          string
	Choices         []SettingChoice
}

type settingDefinition struct {
	key          string
	settingType  SettingType
	defaultValue string
	effect       string
	overridable  bool
}

var settingDefinitions = []settingDefinition{
	{key: "serve.pairing", settingType: SettingTypeSwitch, defaultValue: "true", effect: "约 5 秒内生效", overridable: true},
	{key: "serve.discovery", settingType: SettingTypeSwitch, defaultValue: "true", effect: "约 5 秒内生效", overridable: true},
	{key: "serve.metadata_edit", settingType: SettingTypeSwitch, defaultValue: "true", effect: "约 5 秒内生效", overridable: true},
	{key: "serve.ui_listen", settingType: SettingTypeListenAddresses, defaultValue: "", effect: "约 5 秒内生效"},
	{key: "serve.ui_port", settingType: SettingTypePort, defaultValue: "7879", effect: "重启服务后生效"},
	{key: "serve.discovery_name", settingType: SettingTypeDiscoveryName, defaultValue: "", effect: "重启服务后生效"},
	{key: "pair.default_profile", settingType: SettingTypeProfile, defaultValue: "", effect: "下次配对时生效"},
	{key: "pair.default_max_offline", settingType: SettingTypeDuration, defaultValue: "0", effect: "下次配对时生效"},
}

var discoveryNamePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)

// SettingInputsFromEnv 只把精确的 true/false 环境变量视为显式设置；空值与
// 非法写法按现有 serve 开关规则继续看下一层。
func SettingInputsFromEnv() SettingInputs {
	parse := func(name string) *bool {
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		switch switchstate.Parse(value) {
		case switchstate.On:
			result := true
			return &result
		case switchstate.Off:
			result := false
			return &result
		default:
			return nil
		}
	}
	return SettingInputs{
		PairingEnv:      parse("SSHMGR_SERVE_PAIRING"),
		DiscoveryEnv:    parse("SSHMGR_SERVE_DISCOVERY"),
		MetadataEditEnv: parse("SSHMGR_SERVE_METADATA_EDIT"),
	}
}

// NewWithSettingInputs 返回带 serve 进程显式输入的运维模块，供权威端 Web
// UI 判断设置项是否被环境变量或启动旗标覆盖。
func NewWithSettingInputs(st *store.Store, inputs SettingInputs) *Manager {
	manager := newManager(st)
	manager.settingInputs = cloneSettingInputs(inputs)
	return manager
}

func cloneSettingInputs(inputs SettingInputs) SettingInputs {
	copyBool := func(value *bool) *bool {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	inputs.PairingEnv = copyBool(inputs.PairingEnv)
	inputs.PairingFlag = copyBool(inputs.PairingFlag)
	inputs.DiscoveryEnv = copyBool(inputs.DiscoveryEnv)
	inputs.DiscoveryFlag = copyBool(inputs.DiscoveryFlag)
	inputs.MetadataEditEnv = copyBool(inputs.MetadataEditEnv)
	inputs.MetadataEditFlag = copyBool(inputs.MetadataEditFlag)
	return inputs
}

// ListSettings 返回已知设置项表，并带上设置存储当前值与实际生效来源。
func (m *Manager) ListSettings() ([]Setting, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	choices := make([]SettingChoice, 0, len(profiles))
	for _, profile := range profiles {
		choices = append(choices, SettingChoice{Value: profile.Name, Label: profile.Name})
	}
	out := make([]Setting, 0, len(settingDefinitions))
	for _, definition := range settingDefinitions {
		stored, _, err := m.st.GetSetting(definition.key)
		if err != nil {
			return nil, err
		}
		setting := Setting{
			Key: definition.key, Type: definition.settingType,
			StoredValue: stored, DefaultValue: definition.defaultValue,
			CanBeOverridden: definition.overridable, Effect: definition.effect,
		}
		if definition.settingType == SettingTypeProfile {
			setting.Choices = append([]SettingChoice(nil), choices...)
		}
		m.resolveSetting(&setting)
		out = append(out, setting)
	}
	return out, nil
}

func (m *Manager) resolveSetting(setting *Setting) {
	switch setting.Key {
	case "serve.pairing":
		m.resolveSwitch(setting, m.settingInputs.PairingEnv, m.settingInputs.PairingFlag)
	case "serve.discovery":
		m.resolveSwitch(setting, m.settingInputs.DiscoveryEnv, m.settingInputs.DiscoveryFlag)
	case "serve.metadata_edit":
		m.resolveSwitch(setting, m.settingInputs.MetadataEditEnv, m.settingInputs.MetadataEditFlag)
	default:
		if setting.StoredValue != "" {
			setting.CurrentValue = setting.StoredValue
			setting.Source = SettingSourceStore
			return
		}
		setting.CurrentValue = setting.DefaultValue
		setting.Source = SettingSourceDefault
	}
}

func (m *Manager) resolveSwitch(setting *Setting, envValue, flagValue *bool) {
	switch {
	case envValue != nil:
		setting.Source = SettingSourceEnvironment
		setting.CurrentValue = boolText(*envValue)
	case flagValue != nil:
		setting.Source = SettingSourceFlag
		setting.CurrentValue = boolText(*flagValue)
	case switchstate.Parse(setting.StoredValue) != switchstate.Unset:
		setting.Source = SettingSourceStore
		setting.CurrentValue = setting.StoredValue
	default:
		setting.Source = SettingSourceDefault
		setting.CurrentValue = setting.DefaultValue
	}
	setting.Overridden = setting.Source == SettingSourceEnvironment || setting.Source == SettingSourceFlag
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// SetSetting 校验已知设置项并写入设置存储，然后写 setting.set 审计行。
func (m *Manager) SetSetting(key, value string, src Source) error {
	definition, err := settingDefinitionForKey(key)
	if err != nil {
		return err
	}
	normalized, err := m.validateSettingValue(definition, value)
	if err != nil {
		return err
	}
	oldValue, stored, err := m.st.GetSetting(key)
	if err != nil {
		return err
	}
	if !stored {
		oldValue = definition.defaultValue
	}
	if err := m.st.SetSetting(key, normalized); err != nil {
		return err
	}
	return m.writeSettingAudit("setting.set", key, oldValue, normalized, src)
}

// ValidateSetting 按同一张设置项表校验值，不写入设置存储。
func (m *Manager) ValidateSetting(key, value string) error {
	definition, err := settingDefinitionForKey(key)
	if err != nil {
		return err
	}
	_, err = m.validateSettingValue(definition, value)
	return err
}

// UnsetSetting 从设置存储删除已知键，使它恢复默认值，并写 setting.unset
// 审计行。
func (m *Manager) UnsetSetting(key string, src Source) error {
	definition, err := settingDefinitionForKey(key)
	if err != nil {
		return err
	}
	oldValue, stored, err := m.st.GetSetting(key)
	if err != nil {
		return err
	}
	if !stored {
		return nil
	}
	if err := m.st.SetSetting(key, ""); err != nil {
		return err
	}
	return m.writeSettingAudit("setting.unset", key, oldValue, definition.defaultValue, src)
}

func settingDefinitionForKey(key string) (settingDefinition, error) {
	for _, definition := range settingDefinitions {
		if definition.key == key {
			return definition, nil
		}
	}
	return settingDefinition{}, fmt.Errorf("未知设置项 %q，写入被拒绝", key)
}

func (m *Manager) validateSettingValue(definition settingDefinition, value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", fmt.Errorf("设置项 %s 不能为空；要恢复默认值请使用删除操作", definition.key)
	}
	switch definition.settingType {
	case SettingTypeSwitch:
		if normalized != "true" && normalized != "false" {
			return "", fmt.Errorf("设置项 %s 只接受 true 或 false，当前输入 %q", definition.key, value)
		}
	case SettingTypeListenAddresses:
		if _, err := listenplan.Derive(nil, nil, normalized); err != nil {
			return "", err
		}
	case SettingTypePort:
		port, err := strconv.Atoi(normalized)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("设置项 %s 必须是 1 到 65535 的端口，当前输入 %q", definition.key, value)
		}
	case SettingTypeDiscoveryName:
		if !discoveryNamePattern.MatchString(normalized) {
			return "", fmt.Errorf("设置项 %s 必须以字母或数字开头，只能包含字母、数字、空格、点、下划线与连字符，且不超过 32 个字符", definition.key)
		}
	case SettingTypeProfile:
		exists, err := m.profileExists(normalized)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", fmt.Errorf("设置项 %s 必须是已有 profile，当前输入 %q", definition.key, value)
		}
	case SettingTypeDuration:
		if _, err := offlinelimit.Parse(normalized, definition.key); err != nil {
			return "", fmt.Errorf("设置项 %s 必须是 0 或不小于 1 小时的时长（例如 168h），当前输入 %q", definition.key, value)
		}
	}
	return normalized, nil
}

func (m *Manager) profileExists(identifier string) (bool, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return false, err
	}
	for _, profile := range profiles {
		if profile.Name == identifier || profile.ID == identifier {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) writeSettingAudit(action, key, oldValue, newValue string, src Source) error {
	summary := map[string]any{
		"key": key, "old": oldValue, "new": newValue,
	}
	row := store.OwnerAuditRow(action, summary)
	row.Source = src.kind
	row.SourceIP = src.addr
	return m.st.WriteAudit(row)
}
