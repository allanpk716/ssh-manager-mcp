package webuiops

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestManagerAdapterSettingsAndRole(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(storePath, key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t.Setenv("SSHMGR_STORE", storePath)
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}

	var source webui.OpsStore = FromManager(ops.New(st))
	if err := source.ValidateSetting("serve.pairing", "banana"); err == nil {
		t.Fatal("适配器应保留设置校验错误")
	}
	if err := source.SetSetting("serve.pairing", "false", webui.OpsWriteSource{
		RemoteAddress: "192.0.2.23",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := source.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	var pairing webui.OpsSetting
	for _, setting := range settings {
		if setting.Key == "serve.pairing" {
			pairing = setting
		}
	}
	if pairing.StoredValue != "false" || pairing.CurrentValue != "false" ||
		pairing.Source != webui.OpsSettingSourceStore || !pairing.CanBeOverridden {
		t.Fatalf("适配后的设置行不符：%+v", pairing)
	}
	role, err := source.ReadRole()
	if err != nil {
		t.Fatal(err)
	}
	if role.RoleName != "" || role.Path == "" {
		t.Fatalf("适配后的角色行不符：%+v", role)
	}
	if err := source.UnsetSetting("serve.pairing", webui.OpsWriteSource{}); err != nil {
		t.Fatal(err)
	}

	opens := 0
	fromOpener := FromOpener(func() (*ops.Manager, func(), error) {
		opens++
		return ops.New(st), func() {}, nil
	})
	if _, err := fromOpener.ListSettings(); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("按需打开适配器应每次请求打开存储，打开次数=%d", opens)
	}
}
