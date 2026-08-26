package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// 种一个实例槽（bin+meta 可解）——直接用 clientops.DoPull 太重，写最小材料:
// meta + withDEK seam + LoadCacheSnapshotFor 可载的 bin。此处只验证 VIEW,
// bin 用真加密太啰嗦:改为断言"列出了实例名 + 加载错误也成行不炸"。
func TestCacheStatus_ListsAllInstances(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	for _, n := range []string{"agentA", "agentB"} {
		dir := filepath.Join(userDir, "ssh-manager", "instances", n)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "cache.meta.json"),
			[]byte(fmt.Sprintf(`{"url":"https://s","pulled_at":1,"server_anchored":true,"scoped":false,"device_name":%q}`, n)), 0o600)
		os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("not-decryptable"), 0o600) // 触发行级错误
	}
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"status"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status list mode must not fail overall: %v", err)
	}
	got := out.String()
	for _, want := range []string{"default", "agentA", "agentB", "device:", "load:    ERROR"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

// seedInstanceSlot writes a REAL decryptable instance slot (true-encrypted bin
// under the withDEK seam + the given meta JSON) so `status --instance` can load
// it — the single view must be exercised against a snapshot that actually
// decrypts, not just a meta file.
func seedInstanceSlot(t *testing.T, userDir, name, metaJSON string, dek []byte) {
	t.Helper()
	dir := filepath.Join(userDir, "ssh-manager", "instances", name)
	os.MkdirAll(dir, 0o700)
	snap := store.Snapshot{
		Servers:     []store.SnapshotServer{{ID: "s1", Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"}},
		Credentials: []store.SnapshotCredential{{ID: "c1", Type: "password", Secret: []byte("pw")}},
		Profiles:    []store.SnapshotProfile{{ID: "p1", Name: "team-a"}},
	}
	plaintext, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.EncryptWithKey(dek, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(metaJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCacheStatus_InstanceSingleView (fix round 1): the `--instance` single
// view gained NEW code in T11/T12 (DeviceName meta parse + device line +
// preserved unverified/profile switch) — this pins it: a scoped slot with a
// recorded device identity renders device, the bound profile, and the counts.
func TestCacheStatus_InstanceSingleView(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	mem := withDEK(t)
	dek, _ := store.GenerateMasterKey()
	_ = mem.Set(dek)
	seedInstanceSlot(t, userDir, "agentA",
		`{"url":"https://s","pulled_at":1,"server_anchored":true,"scoped":true,"device_name":"agentA"}`, dek)

	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"status", "--instance", "agentA"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("single view must load the seeded slot: %v", err)
	}
	got := out.String()
	for _, want := range []string{"device:    agentA", "scope:    team-a", "servers:  1", "creds:    1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("single view missing %q:\n%s", want, got)
		}
	}
}

// TestCacheStatus_InstanceSingleView_UnscopedBlankDevice (fix round 1): an
// unscoped slot with NO device_name (legacy/plaintext pull shape) must keep
// the Plan-39 discipline (unverified scope, profile name never leaks) and
// render the device line BLANK — same rule as the list view; "(unknown)" is
// reserved for an unreadable meta, not for a recorded-blank identity.
func TestCacheStatus_InstanceSingleView_UnscopedBlankDevice(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	mem := withDEK(t)
	dek, _ := store.GenerateMasterKey()
	_ = mem.Set(dek)
	seedInstanceSlot(t, userDir, "agentB",
		`{"url":"https://s","pulled_at":1,"server_anchored":true,"scoped":false}`, dek)

	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"status", "--instance", "agentB"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("single view must load the seeded slot: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "unverified") {
		t.Fatalf("unscoped slot must show the unverified scope line:\n%s", got)
	}
	if strings.Contains(got, "team-a") {
		t.Fatalf("unscoped slot must NOT leak the profile name:\n%s", got)
	}
	if !strings.Contains(got, "device:    \n") || strings.Contains(got, "(unknown)") {
		t.Fatalf("blank device_name must render blank (not \"(unknown)\"):\n%s", got)
	}
}
