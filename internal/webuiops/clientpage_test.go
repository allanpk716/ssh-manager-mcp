package webuiops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClientAdapterListsAndSwitchesInstances(t *testing.T) {
	userDir := t.TempDir()
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	cacheDir := filepath.Join(userDir, "ssh-manager")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"cache.auth.json": `{"url":"https://authority","token":"device-code","pin":"sha256:00"}`,
		"cache.bin":       "snapshot",
		"cache.meta.json": `{"pulled_at":100}`,
	} {
		if err := os.WriteFile(filepath.Join(cacheDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dekDir, "cache-dek.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := ClientPageStoreWithUpdates(nil, nil)
	instances, err := source.ListClientInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].DisplayName != "默认实例" || !instances[0].Complete {
		t.Fatalf("适配器返回的实例列表不符：%+v", instances)
	}
	if err := source.SwitchClientInstance("", "", "127.0.0.1"); err != nil {
		t.Fatalf("适配器切换默认实例失败：%v", err)
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, "cache-audit.log"))
	if err != nil || len(data) == 0 {
		t.Fatalf("适配器应写实例审计日志：%v %s", err, data)
	}
}
