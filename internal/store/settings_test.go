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
