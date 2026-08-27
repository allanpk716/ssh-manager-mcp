package tui

// Plan 40 批2 T5 §3.3: clientModel per-instance 核心的路由/丢弃语义测试。
//
// seed 形态仿 internal/cli/cache_status_instances_test.go 的 seedInstanceSlot
// （APPDATA/XDG_CONFIG_HOME 重定向 + DekProvider seam 换 MemKeyProvider +
// 真 DEK 加密的 cache.bin）。withDEK 在 cli/clientops 是 *_test.go 私有
// helper，tui 包跨包不可见，故此处自写同款——直接换导出的
// clientops.DekProvider seam。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// tuiWithDEK swaps the exported DekProvider seam to a fresh in-memory provider
// for the test, returning it so the test can store the DEK the seeded cache.bin
// was encrypted with.
func tuiWithDEK(t *testing.T) *store.MemKeyProvider {
	t.Helper()
	mem := &store.MemKeyProvider{}
	prev := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prev })
	return mem
}

// seedTUISlot writes a REAL decryptable slot into dir: a true-encrypted
// cache.bin (under the swapped DekProvider seam) + cache.meta.json +
// cache.auth.json. serverName is baked into the snapshot so two slots' replies
// are distinguishable — the per-instance routing assertion reads it back out.
func seedTUISlot(t *testing.T, dir, deviceName, serverName string, dek []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot{
		Servers:     []store.SnapshotServer{{ID: "s1", Name: serverName, Host: "192.0.2.10", Port: 22, User: "u"}},
		Credentials: []store.SnapshotCredential{{ID: "c1", Type: "password", Secret: []byte("pw")}},
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
	meta := fmt.Sprintf(`{"url":"https://s","pulled_at":1,"server_anchored":true,"scoped":false,"device_name":%q}`, deviceName)
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := fmt.Sprintf(`{"url":"https://s.example","token":"tok-%s","pin":"sha256:%s"}`, deviceName, strings.Repeat("a", 64))
	if err := os.WriteFile(filepath.Join(dir, "cache.auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshDataCmdFor_InstanceSlot: refreshDataCmdFor("agentA") must read THE
// NAMED SLOT's material (instances/agentA/ — its own cred + its own snapshot),
// not fall through to the default slot; refreshDataCmdFor("") stays the default
// slot view whose reply carries instance:"". Both slots are seeded with true-
// encrypted bins and DIFFERENT server names so a routing mix-up cannot pass.
func TestRefreshDataCmdFor_InstanceSlot(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	mem := tuiWithDEK(t)
	dek, _ := store.GenerateMasterKey()
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(userDir, "ssh-manager")
	seedTUISlot(t, base, "default-dev", "default-srv", dek)
	seedTUISlot(t, filepath.Join(base, "instances", "agentA"), "agentA", "agent-srv", dek)

	msg := refreshDataCmdFor("agentA")()
	ready, ok := msg.(dataReadyMsg)
	if !ok {
		t.Fatalf("named slot with full material must yield dataReadyMsg, got %T (%v)", msg, msg)
	}
	if ready.instance != "agentA" {
		t.Fatalf("reply must carry the named slot, got %q", ready.instance)
	}
	if ready.cred == nil || ready.snap == nil {
		t.Fatalf("named slot reply must carry non-nil cred+snap, got cred=%v snap=%v", ready.cred, ready.snap)
	}
	if ready.cred.Token != "tok-agentA" {
		t.Fatalf("cred must come from instances/agentA/cache.auth.json, got token %q", ready.cred.Token)
	}
	if len(ready.snap.Servers) != 1 || ready.snap.Servers[0].Name != "agent-srv" {
		t.Fatalf("snap must be agentA's snapshot, got %+v", ready.snap.Servers)
	}

	defMsg := refreshDataCmdFor("")()
	defReady, ok := defMsg.(dataReadyMsg)
	if !ok {
		t.Fatalf("default slot with full material must yield dataReadyMsg, got %T (%v)", defMsg, defMsg)
	}
	if defReady.instance != "" {
		t.Fatalf("default slot reply must carry instance \"\", got %q", defReady.instance)
	}
	if len(defReady.snap.Servers) != 1 || defReady.snap.Servers[0].Name != "default-srv" {
		t.Fatalf("default-slot refresh must read base/ssh-manager, got %+v", defReady.snap.Servers)
	}
}

// TestSyncCmdMode_PassesInstance: no fake serve here (too heavy for what this
// pins — the real Instance-to-PullOpts assertion is T11 e2e's job). With a
// valid pin against a dead loopback port the pull fails fast; the pinned
// contract is "syncCmdMode takes an instance argument, never panics on the
// named-slot path, and every failure still rides syncDoneMsg".
func TestSyncCmdMode_PassesInstance(t *testing.T) {
	cred := &clientops.CacheCred{URL: "https://127.0.0.1:1", Token: "t", Pin: "sha256:" + strings.Repeat("b", 64)}
	msg := syncCmdMode(cred, "agentA", false)()
	done, ok := msg.(syncDoneMsg)
	if !ok {
		t.Fatalf("want syncDoneMsg, got %T", msg)
	}
	if done.err == nil {
		t.Fatal("pull against a dead serve must fail (non-nil err), got nil")
	}
}

// TestDataReady_StaleSlotDropped: a dataReadyMsg belonging to another slot than
// the model's current m.instance is DROPPED wholesale — switching slots
// mid-flight must never paint the new header/list with the old slot's reply.
// The matching-slot counterpart must still apply (two-sided lock).
func TestDataReady_StaleSlotDropped(t *testing.T) {
	// Plan 40 批2 T6: the matching-slot branch now reaches the auto-picker
	// probe — isolate the config dir so a real machine's cache/instances state
	// can never decide the outcome (the dropped-branch half runs before any
	// message reaches the model's switch).
	isolatedConfigDir(t)
	m := newClientModel()
	m.instance = "agentA"

	stale := dataReadyMsg{
		instance: "",
		cred:     &clientops.CacheCred{URL: "https://x", Token: "t"},
		snap:     &store.Snapshot{Servers: []store.SnapshotServer{{ID: "s", Name: "stale-srv"}}},
	}
	nm, cmd := m.Update(stale)
	cm := nm.(clientModel)
	if cm.snap != nil || cm.cred != nil {
		t.Fatalf("stale slot reply must be dropped, got snap=%v cred=%v", cm.snap, cm.cred)
	}
	if cmd != nil {
		t.Fatalf("dropped reply must return no cmd, got %v", cmd)
	}

	match := stale
	match.instance = "agentA"
	nm2, _ := cm.Update(match)
	cm2 := nm2.(clientModel)
	if cm2.snap == nil || cm2.cred == nil || len(cm2.snap.Servers) != 1 || cm2.snap.Servers[0].Name != "stale-srv" {
		t.Fatalf("matching-slot reply must apply, got snap=%v cred=%v", cm2.snap, cm2.cred)
	}
}
