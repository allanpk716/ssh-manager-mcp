package dialdevice

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func withSnapshotStore(t *testing.T, devices ...store.SnapshotDevice) *store.Store {
	t.Helper()
	snapshot := &store.Snapshot{Version: 1, Devices: devices}
	temporary, err := store.OpenTemporaryClientCacheStore(snapshot, "dial-device-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = temporary.Close() })
	return temporary.Store
}

func writeClientMeta(t *testing.T, instance, deviceID string) {
	t.Helper()
	base, err := paths.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "ssh-manager")
	if instance != "" {
		dir = filepath.Join(dir, "instances", instance)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"url":"https://authority","pulled_at":1,"server_anchored":true,` +
		`"scoped":true,"device_name":"` + instance + `","device_id":"` + deviceID + `"}`
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func redirectClientConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("APPDATA", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("SSHMGR_CONFIG_ROOT", root)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "absent-store.db"))
	return root
}

func TestCurrentClientDevice(t *testing.T) {
	clientRoot := redirectClientConfig(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st := withSnapshotStore(t,
		store.SnapshotDevice{ID: "device-old", Name: "old-laptop"},
		store.SnapshotDevice{ID: "device-new", Name: "new-laptop"},
	)
	writeClientMeta(t, "laptop", "device-old")

	current, err := Current(st, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Known || current.Device == nil || current.Device.ID != "device-old" {
		t.Fatalf("first current device = %+v, want device-old", current)
	}

	writeClientMeta(t, "laptop", "device-new")
	current, err = Current(st, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Known || current.Device == nil || current.Device.ID != "device-new" {
		t.Fatalf("refreshed current device = %+v, want device-new", current)
	}
	if _, err := os.Stat(filepath.Join(clientRoot, "ssh-manager", "role.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentClientUnknowns(t *testing.T) {
	redirectClientConfig(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st := withSnapshotStore(t, store.SnapshotDevice{ID: "device-laptop", Name: "laptop"})

	writeClientMeta(t, "laptop", "")
	current, err := Current(st, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if current.Known || current.Reason != ReasonClientDeviceNotRecorded {
		t.Fatalf("unassociated current device = %+v, want unrecorded unknown", current)
	}

	writeClientMeta(t, "laptop", "device-removed")
	current, err = Current(st, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if current.Known || current.Reason != ReasonDeviceMissingFromSnapshot {
		t.Fatalf("missing current device = %+v, want snapshot-missing unknown", current)
	}
}

// TestCurrentForClientCacheUsesUniqueSnapshotAssociation: mcp --cache 的生产
// 源可能拿不到实例名；裁剪快照会只给发起客户端保留设备码关联，因此唯一带
// 关联的设备就是本机设备。
func TestCurrentForClientCacheUsesUniqueSnapshotAssociation(t *testing.T) {
	redirectClientConfig(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st := withSnapshotStore(t,
		store.SnapshotDevice{ID: "device-target", Name: "target"},
		store.SnapshotDevice{ID: "device-laptop", Name: "laptop", ClientTokenID: "token-laptop"},
	)

	current, err := CurrentForClientCache(st)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Known || current.Device == nil || current.Device.ID != "device-laptop" {
		t.Fatalf("current device = %+v, want the uniquely associated device-laptop", current)
	}
}

// TestCurrentForClientCacheUsesMetadataBeforeAssociation: cache.meta.json 记录
// 的设备标识优先；即使快照里有多台设备仍带设备码关联，也按元数据精确选中
// 本机设备。
func TestCurrentForClientCacheUsesMetadataBeforeAssociation(t *testing.T) {
	redirectClientConfig(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st := withSnapshotStore(t,
		store.SnapshotDevice{ID: "device-default", Name: "default laptop"},
		store.SnapshotDevice{ID: "device-first", Name: "first laptop", ClientTokenID: "token-first"},
		store.SnapshotDevice{ID: "device-named", Name: "named laptop", ClientTokenID: "token-named"},
	)
	writeClientMeta(t, "", "device-default")

	current, err := CurrentForClientCache(st)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Known || current.Device == nil || current.Device.ID != "device-default" {
		t.Fatalf("current device = %+v, want the metadata-recorded device-default", current)
	}
}

// TestCurrentForClientCacheFallsBackWithoutMetadata: 旧缓存的元数据没有设备
// 标识时，才使用裁剪快照里唯一带设备码关联的设备。
func TestCurrentForClientCacheFallsBackWithoutMetadata(t *testing.T) {
	redirectClientConfig(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st := withSnapshotStore(t,
		store.SnapshotDevice{ID: "device-default", Name: "default laptop"},
		store.SnapshotDevice{ID: "device-named", Name: "named laptop", ClientTokenID: "token-named"},
	)
	writeClientMeta(t, "", "")

	current, err := CurrentForClientCache(st)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Known || current.Device == nil || current.Device.ID != "device-named" {
		t.Fatalf("current device = %+v, want the snapshot-associated device-named", current)
	}
}

// TestCurrentForClientCacheMetadataDeviceMissing: 元数据记录的设备已不在当前
// 快照代里时，按“记录的设备缺失”处理，而不是回退猜另一台设备。
func TestCurrentForClientCacheMetadataDeviceMissing(t *testing.T) {
	redirectClientConfig(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st := withSnapshotStore(t,
		store.SnapshotDevice{ID: "device-named", Name: "named laptop", ClientTokenID: "token-named"},
	)
	writeClientMeta(t, "", "device-removed")

	current, err := CurrentForClientCache(st)
	if err != nil {
		t.Fatal(err)
	}
	if current.Known || current.Reason != ReasonDeviceMissingFromSnapshot {
		t.Fatalf("current device = %+v, want the recorded device to be missing", current)
	}
}

func TestCurrentLocalDevice(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	st := withSnapshotStore(t, store.SnapshotDevice{ID: "device-local", Name: "workstation"})

	for _, role := range []roles.Role{roles.RoleStandalone, roles.RoleServer} {
		if err := roles.Save(roles.State{Role: role, SetupComplete: true}); err != nil {
			t.Fatal(err)
		}
		if err := roles.SetLocalDeviceID("device-local"); err != nil {
			t.Fatal(err)
		}
		current, err := Current(st, "")
		if err != nil {
			t.Fatal(err)
		}
		if !current.Known || current.Device == nil || current.Device.ID != "device-local" {
			t.Fatalf("role %s current device = %+v, want device-local", role, current)
		}
	}

	if err := roles.SetLocalDeviceID("device-deleted"); err != nil {
		t.Fatal(err)
	}
	current, err := Current(st, "")
	if err != nil {
		t.Fatal(err)
	}
	if current.Known || current.Reason != ReasonLocalDeviceDeleted {
		t.Fatalf("deleted local current device = %+v, want deleted unknown", current)
	}

	if _, err := roles.ClearLocalDevice(); err != nil {
		t.Fatal(err)
	}
	current, err = Current(st, "")
	if err != nil {
		t.Fatal(err)
	}
	if current.Known || current.Reason != ReasonLocalDeviceNotSet {
		t.Fatalf("cleared local current device = %+v, want unset unknown", current)
	}
}
