package store

import (
	"errors"
	"testing"
)

// TestSettings_RoundTripAndDelete pins the settings kv contract (Plan 42 批1
// T2): absent → absent, set → readable, set-empty → deleted. The empty-value
// delete is the tri-state "explicitly unset" form the switch resolver reads.
func TestSettings_RoundTripAndDelete(t *testing.T) {
	s := newTestStore(t)
	if _, ok, _ := s.GetSetting("pair.default_profile"); ok {
		t.Fatal("want absent")
	}
	if err := s.SetSetting("pair.default_max_offline", "24h"); err != nil {
		t.Fatal(err)
	}
	v, ok, _ := s.GetSetting("pair.default_max_offline")
	if !ok || v != "24h" {
		t.Fatalf("got %q %v", v, ok)
	}
	if err := s.SetSetting("pair.default_max_offline", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetSetting("pair.default_max_offline"); ok {
		t.Fatal("want deleted")
	}
}

// TestSettings_OverwriteUpdates pins the upsert half of SetSetting: a second
// set on an existing key must replace the value in place, not duplicate or
// error (settings(key) is the PRIMARY KEY — INSERT OR REPLACE path).
func TestSettings_OverwriteUpdates(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("serve.pairing", "true"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("serve.pairing", "false"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetSetting("serve.pairing")
	if err != nil || !ok || v != "false" {
		t.Fatalf("after overwrite got %q ok=%v err=%v, want \"false\" ok=true", v, ok, err)
	}
}

// TestSettings_ReadOnlyRejectsMutation pins that a read-only (offline cache)
// store refuses SetSetting with ErrReadOnly while GetSetting still reads —
// switches resolve on the cache too, but the cache never writes preferences.
func TestSettings_ReadOnlyRejectsMutation(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("serve.pairing", "true"); err != nil {
		t.Fatal(err)
	}
	s.SetReadOnly(nil)
	if err := s.SetSetting("serve.pairing", "false"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
	if _, ok, err := s.GetSetting("serve.pairing"); err != nil || !ok {
		t.Fatalf("read must stay allowed on read-only store: ok=%v err=%v", ok, err)
	}
}

// TestSnapshotSettingsImportRules pins both sides of the format boundary: a
// current owner snapshot replaces the settings table, while an old snapshot
// without the section leaves every target setting untouched.
func TestSnapshotSettingsImportRules(t *testing.T) {
	source := newTestStore(t)
	if err := source.SetSetting("serve.pairing", "false"); err != nil {
		t.Fatal(err)
	}
	if err := source.SetSetting("serve.ui_port", "18790"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatalf("export source snapshot: %v", err)
	}
	if len(snapshot.Settings) != 2 {
		t.Fatalf("snapshot settings = %+v, want two rows", snapshot.Settings)
	}

	target := newTestStore(t)
	if err := target.SetSetting("serve.pairing", "true"); err != nil {
		t.Fatal(err)
	}
	if err := target.SetSetting("local.only", "target-value"); err != nil {
		t.Fatal(err)
	}
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatalf("import current snapshot: %v", err)
	}
	if value, exists, err := target.GetSetting("serve.pairing"); err != nil || !exists || value != "false" {
		t.Fatalf("serve.pairing after current import = %q exists=%v err=%v, want false", value, exists, err)
	}
	if _, exists, err := target.GetSetting("local.only"); err != nil || exists {
		t.Fatalf("local.only after current import exists=%v err=%v, want removed", exists, err)
	}
	if value, exists, err := target.GetSetting(serverDeviceMigrationSetting); err != nil || !exists || value != "done" {
		t.Fatalf("migration marker after current import = %q exists=%v err=%v, want done",
			value, exists, err)
	}

	oldTarget := newTestStore(t)
	if err := oldTarget.SetSetting("serve.pairing", "true"); err != nil {
		t.Fatal(err)
	}
	if err := oldTarget.SetSetting("local.only", "old-target-value"); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := *snapshot
	oldSnapshot.Settings = nil
	if err := oldTarget.ImportSnapshot(&oldSnapshot); err != nil {
		t.Fatalf("import old snapshot: %v", err)
	}
	for key, want := range map[string]string{
		"serve.pairing": "true",
		"local.only":    "old-target-value",
	} {
		got, exists, err := oldTarget.GetSetting(key)
		if err != nil || !exists || got != want {
			t.Fatalf("old snapshot setting %s = %q exists=%v err=%v, want %q",
				key, got, exists, err, want)
		}
	}
}
