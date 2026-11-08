package ops

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func newSettingsTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func settingBool(value bool) *bool { return &value }

func TestListSettingsBuildsKnownTableAndReadsRole(t *testing.T) {
	st, storePath := newSettingsTestStore(t)
	t.Setenv("SSHMGR_STORE", storePath)
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("serve.pairing", "false"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("serve.discovery", "false"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("serve.metadata_edit", "false"); err != nil {
		t.Fatal(err)
	}
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	manager := NewWithSettingInputs(st, SettingInputs{
		PairingEnv:      settingBool(true),
		DiscoveryFlag:   settingBool(true),
		MetadataEditEnv: settingBool(false),
		Hostname:        "MacBook_Pro 2.local",
	})
	settings, err := manager.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 8 {
		t.Fatalf("已知设置项数量 = %d，应为 8：%+v", len(settings), settings)
	}
	byKey := make(map[string]Setting, len(settings))
	for _, setting := range settings {
		byKey[setting.Key] = setting
	}

	pairing := byKey["serve.pairing"]
	if pairing.CurrentValue != "true" || pairing.StoredValue != "false" ||
		pairing.Source != SettingSourceEnvironment || !pairing.Overridden ||
		!pairing.CanBeOverridden || pairing.Effect != "约 5 秒内生效" {
		t.Fatalf("配对开关的覆盖信息不符：%+v", pairing)
	}
	discovery := byKey["serve.discovery"]
	if discovery.CurrentValue != "true" || discovery.StoredValue != "false" ||
		discovery.Source != SettingSourceFlag || !discovery.Overridden {
		t.Fatalf("发现开关应显示启动旗标覆盖：%+v", discovery)
	}
	metadata := byKey["serve.metadata_edit"]
	if metadata.CurrentValue != "false" || metadata.Source != SettingSourceEnvironment ||
		metadata.StoredValue != "false" || !metadata.Overridden {
		t.Fatalf("客户端元数据写入开关应显示环境变量覆盖：%+v", metadata)
	}

	listen := byKey["serve.ui_listen"]
	if listen.Type != SettingTypeListenAddresses || listen.DefaultValue != "" ||
		listen.Source != SettingSourceDefault || listen.Overridden ||
		listen.Effect != "约 5 秒内生效" {
		t.Fatalf("监听地址设置项不符：%+v", listen)
	}
	port := byKey["serve.ui_port"]
	if port.Type != SettingTypePort || port.DefaultValue != "7879" ||
		port.Effect != "重启服务后生效" || port.CanBeOverridden {
		t.Fatalf("Web 端口设置项不符：%+v", port)
	}
	name := byKey["serve.discovery_name"]
	if name.Type != SettingTypeDiscoveryName || name.DefaultValue != "MacBook_Pro 2.local" ||
		name.Effect != "重启服务后生效" || name.Source != SettingSourceDefault {
		t.Fatalf("发现名称设置项不符：%+v", name)
	}
	profile := byKey["pair.default_profile"]
	if profile.Type != SettingTypeProfile || profile.Effect != "下次配对时生效" ||
		len(profile.Choices) != 1 || profile.Choices[0].Value != "ops" ||
		profile.Choices[0].Label != "ops" {
		t.Fatalf("默认 profile 设置项或候选不符：%+v", profile)
	}
	offline := byKey["pair.default_max_offline"]
	if offline.Type != SettingTypeDuration || offline.DefaultValue != "0" ||
		offline.Effect != "下次配对时生效" || offline.Source != SettingSourceDefault {
		t.Fatalf("默认离线时长设置项不符：%+v", offline)
	}

	role, err := ReadRole()
	if err != nil {
		t.Fatal(err)
	}
	if role.Role != roles.RoleStandalone || !role.SetupComplete || role.Path == "" {
		t.Fatalf("本机角色文件内容不符：%+v", role)
	}
}

func TestListSettingsShowsActualDiscoveryNameDefault(t *testing.T) {
	st, _ := newSettingsTestStore(t)
	manager := NewWithSettingInputs(st, SettingInputs{Hostname: "MacBook_Pro 2.local"})

	settings, err := manager.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range settings {
		if setting.Key == "serve.discovery_name" {
			if setting.DefaultValue != "MacBook_Pro 2.local" ||
				setting.Source != SettingSourceDefault || setting.Overridden {
				t.Fatalf("发现名称默认值应显示实际会使用的主机名：%+v", setting)
			}
			return
		}
	}
	t.Fatal("设置项表缺少 serve.discovery_name")
}
