package cli

// 命令策略·规则随备份恢复与 cache pull 下发(ticket #15 验收):
//   - 备份与恢复携带规则:`sshmgr export` → `sshmgr import` 后,规则与停用
//     记录原样到达恢复库,试算按它们判定;
//   - 规则随 cache pull 到达客户端并生效:权威库导出的裁剪快照经真实
//     HTTP 拉取落成 cache.bin,再按 `mcp --cache` 的水合形态(导入临时
//     只读库)驱动执行工具——自定义规则拦下、停用的默认规则放行。

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

// TestPolicyBackupRestore_KeepsRules:验收用例——备份与恢复后规则保持。
// 环境钉住文件主键路径(开发者本机的真实 master.key 不得串进解密,
// 同仓库既有的导出往返用例在这台机器上正因没钉而失败)。
func TestPolicyBackupRestore_KeepsRules(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	outFile := filepath.Join(dir, "vault.export")
	mkA, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbA,
		"SSHMGRKEY_UNUSED":     "",
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mkA),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
		"SSHMGR_CACHE_DIR":     t.TempDir(),
	})

	// 权威库 A:一条自定义规则 + 一条停用记录(种子里放一台服务器,快照
	// 不是空壳)。
	stA, err := store.Open(dbA, mkA)
	if err != nil {
		t.Fatal(err)
	}
	if err := stA.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []policy.ShellType{policy.ShellPOSIX},
		Commands: []string{"reboot"}, Disposition: policy.DispositionBlock,
		Explanation: "reboots are scheduled by the owner", Alternative: "ask the owner",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stA.DisableDefaultRule("POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	srvID, _ := stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"})
	pid, _ := stA.AddProfile("team-a")
	_ = stA.GrantServers(pid, []string{srvID})
	if err := stA.Close(); err != nil {
		t.Fatal(err)
	}

	// 口令缝换成固定值(export 读两遍、import 读一遍)。
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	t.Cleanup(func() {
		passphrasePrompt = orig
		passphraseConfirmPrompt = origConfirm
	})

	run := func(args ...string) {
		t.Helper()
		root := NewRootCmd()
		root.SetArgs(args)
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, out.String())
		}
	}
	run("export", "--out", outFile)
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("export file not written: %v", err)
	}

	// 恢复进另一个主键的空库 B。
	mkB, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mkB),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key-2"),
	})
	run("import", outFile)

	// 恢复库上规则仍在、仍生效:列出可见,试算按规则判定。
	out, err := runCLI(t, "policy", "ls")
	if err != nil {
		t.Fatalf("policy ls after restore: %v", err)
	}
	if !strings.Contains(out.String(), "NO-REBOOT") || !strings.Contains(out.String(), "已停用") {
		t.Fatalf("restored vault must still list the rule and the disabled default, got:\n%s", out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "posix", "reboot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NO-REBOOT") {
		t.Fatalf("restored vault must still judge by the rule, got:\n%s", out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "posix", ":(){ :|:& };:")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "放行") {
		t.Fatalf("restored vault must keep the default rule disabled, got:\n%s", out.String())
	}
}

// TestCachePull_DeliversPolicyRules_AndTheyTakeEffect:验收用例——规则与
// 停用记录随 `cache pull` 到达客户端并生效。权威侧用真实导出路径
// (ExportSnapshotForProfile)出快照,经 HTTP 拉取落成 cache.bin,再按
// `mcp --cache` 的水合形态(快照 → 临时只读库)驱动执行工具判定。
func TestCachePull_DeliversPolicyRules_AndTheyTakeEffect(t *testing.T) {
	// 权威库:服务器 + profile + 授权 + 一条自定义规则 + 一条停用记录。
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	srvID, _ := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"})
	pid, _ := src.AddProfile("team-a")
	_ = src.GrantServers(pid, []string{srvID})
	if err := src.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []policy.ShellType{policy.ShellPOSIX},
		Commands: []string{"reboot"}, Disposition: policy.DispositionBlock,
		Explanation: "reboots are scheduled by the owner, never run by the agent",
		Alternative: "ask the owner to schedule a reboot window",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.DisableDefaultRule("POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	snap, err := src.ExportSnapshotForProfile(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.PolicyRules) != 1 || len(snap.PolicyDisabledDefaults) != 1 {
		t.Fatalf("scoped snapshot must carry the policy sections, got %+v / %v", snap.PolicyRules, snap.PolicyDisabledDefaults)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	// serve 面:设备码鉴权后回裁剪快照(明文 HTTP + --allow-plaintext,
	// 沿既有拉取测试的形态;策略段的到达与生效与传输加密无关)。
	const deviceCode = "code-xyz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+deviceCode {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// 客户端机器:DEK 缝换成内存钥匙(拉取创建的 DEK 落在那里,后面的
	// 读取经同一把钥匙解密),缓存目录钉到临时目录。
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":     filepath.Join(t.TempDir(), "no-vault.db"),
		"SSHMGR_CACHE_DIR": cacheDir,
	})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", srv.URL, "--token", deviceCode, "--allow-plaintext"})
	pullOut := &bytes.Buffer{}
	root.SetOut(pullOut)
	root.SetErr(pullOut)
	if err := root.Execute(); err != nil {
		t.Fatalf("cache pull: %v\n%s", err, pullOut.String())
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err != nil {
		t.Fatalf("cache.bin not written: %v", err)
	}

	// 到达:读回缓存快照,策略两段都在。
	loaded := loadCacheSnapshotDefault(t)
	if len(loaded.PolicyRules) != 1 || loaded.PolicyRules[0].ID != "NO-REBOOT" {
		t.Fatalf("pulled snapshot policy rules = %+v, want NO-REBOOT", loaded.PolicyRules)
	}
	if len(loaded.PolicyDisabledDefaults) != 1 || loaded.PolicyDisabledDefaults[0] != "POSIX-FORK-BOMB" {
		t.Fatalf("pulled snapshot disabled defaults = %v", loaded.PolicyDisabledDefaults)
	}

	// 生效:按 `mcp --cache` 的水合形态(导入临时只读库)驱动执行工具。
	hyd := newPolicyHydratedStore(t, loaded)
	var gotSrvID, gotPid string
	for _, sv := range loaded.Servers {
		gotSrvID = sv.ID
	}
	for _, p := range loaded.Profiles {
		gotPid = p.ID
	}
	_, err = mcpserver.ExecCommandForProfile(t.Context(), hyd, "proj-pull", gotPid, gotSrvID, "reboot", false, 5*time.Second, "", "")
	if err == nil || !strings.Contains(err.Error(), "[NO-REBOOT]") {
		t.Fatalf("pulled custom rule must block on the client execution path, got %v", err)
	}
	_, err = mcpserver.ExecCommandForProfile(t.Context(), hyd, "proj-pull", gotPid, gotSrvID, ":(){ :|:& };:", false, 5*time.Second, "", "")
	if err == nil || strings.Contains(err.Error(), "POSIX-FORK-BOMB") {
		t.Fatalf("pulled disable record must reach the client execution path, got %v", err)
	}
}

// loadCacheSnapshotDefault 读默认实例的缓存快照(clientops 的既有读取面)。
func loadCacheSnapshotDefault(t *testing.T) *store.Snapshot {
	t.Helper()
	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// newPolicyHydratedStore 按 `mcp --cache` 的水合形态造客户端只读库:
// 临时库 + 丢弃型主键 + ImportSnapshot + SetReadOnly。
func newPolicyHydratedStore(t *testing.T, snap *store.Snapshot) *store.Store {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	st.SetReadOnly(nil)
	return st
}
