package dialplan

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// TestStoreSourceResolvesDialObserver: 生产拓扑来源能解析出探测观察者，让
// 统一拨号入口无需每个调用方各自接线。
func TestStoreSourceResolvesDialObserver(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))

	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	device, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(device.ID); err != nil {
		t.Fatal(err)
	}

	source := NewStoreSource(st, "")
	observer, err := source.ResolveDialObserver()
	if err != nil {
		t.Fatal(err)
	}
	if observer == nil {
		t.Fatal("standalone role must resolve a dial observer")
	}
	if _, ok := observer.(sshbroker.DialAttemptObserver); !ok {
		t.Fatalf("observer %T must implement sshbroker.DialAttemptObserver", observer)
	}
}
