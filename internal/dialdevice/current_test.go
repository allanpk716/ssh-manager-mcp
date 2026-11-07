package dialdevice

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/clientops"
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
	dir, _, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"url":"https://authority","pulled_at":1,"server_anchored":true,` +
		`"scoped":true,"device_name":"` + instance + `","device_id":"` + deviceID + `"}`
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
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
