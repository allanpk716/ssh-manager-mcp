package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// withClearDirs isolates every filesystem location `clear` touches via env —
// mirror of roles' withDirs / tui's withRoleDirs (same rationale: the dev
// machine REALLY runs ssh-manager, so an unpinned probe/enumeration would see
// — and a teardown test could DELETE — the operator's live vault).
func withClearDirs(t *testing.T) (vaultDir, userDir string) {
	t.Helper()
	vaultDir = t.TempDir()
	userDir = t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, "master.key.plain"))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", filepath.Join(vaultDir, "cache-dek.key"))
	t.Setenv("SSHMGR_SERVE_LOG", filepath.Join(vaultDir, "serve.log")) // Plan 19 T7 lesson: pin the seam or clear's enumeration Stats the real vault dir
	t.Setenv("SSHMGR_SERVE_CERT", "")
	t.Setenv("SSHMGR_SERVE_KEY", "")
	t.Setenv("SSHMGR_SERVE_MARKER", "")
	t.Setenv("APPDATA", userDir) // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir)
	return vaultDir, userDir
}

// seedClearVault creates an unlocked (openable) vault at the pinned dirs —
// mirror of roles' seedVault.
func seedClearVault(t *testing.T, vaultDir string) {
	t.Helper()
	os.Remove(filepath.Join(vaultDir, "store.db"))
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(vaultDir, "master.key.plain"), mk, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stubClearExternals replaces every external-effect seam of `clear` with a
// hermetic fake: TTY detection, SCM probes (installed + uninstall), the
// legacy-timer query/delete, and the safety-net home dir. Tests that care
// pass recording pointers.
func stubClearExternals(t *testing.T, uninstallCalled, timerCalled *bool) {
	t.Helper()
	prevTTY, prevInst, prevUn, prevTimer, prevPresent, prevHome :=
		clearStdinIsTTY, serveInstalledFn, serveUninstallFn, deleteLegacyTimerFn, legacyTimerPresentFn, safetyNetHomeDir
	clearStdinIsTTY = func() bool { return true }
	serveInstalledFn = func() bool { return false }
	serveUninstallFn = func(io.Writer) error {
		if uninstallCalled != nil {
			*uninstallCalled = true
		}
		return nil
	}
	deleteLegacyTimerFn = func() error {
		if timerCalled != nil {
			*timerCalled = true
		}
		return nil
	}
	legacyTimerPresentFn = func() bool { return false }
	home := t.TempDir()
	safetyNetHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() {
		clearStdinIsTTY, serveInstalledFn, serveUninstallFn, deleteLegacyTimerFn, legacyTimerPresentFn, safetyNetHomeDir =
			prevTTY, prevInst, prevUn, prevTimer, prevPresent, prevHome
	})
}

func driveClear(t *testing.T, input string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(input))
	root.SetArgs([]string{"clear"})
	err := root.Execute()
	return out.String(), err
}

func assertExists(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("expected intact: %s (%v)", p, err)
	}
}

func assertGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err == nil {
		t.Errorf("expected deleted: %s", p)
	}
}

// backupFilesIn lists the safety-net files written under dir ("" slice = none).
func backupFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "ssh-manager-backup-*.sme"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// ---------------------------------------------------------------------------
// enumClearTargets
// ---------------------------------------------------------------------------

// TestEnumClearTargets_ServerMachine pins the brief's full-list contract: a
// server machine with wal/marker/cert files AND same-machine client residue
// enumerates all of them (Stat-gated), with the category prefixes present.
func TestEnumClearTargets_ServerMachine(t *testing.T) {
	vd, ud := withClearDirs(t)
	seedClearVault(t, vd)
	for _, f := range []string{"store.db-wal", "store.db.meta.json", "master.key.plain", "serve-cert.pem", ".serve-cert-initialized"} {
		os.WriteFile(filepath.Join(vd, f), []byte("x"), 0o600)
	}
	// 同机 client 残留
	os.MkdirAll(filepath.Join(ud, "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(ud, "ssh-manager", "cache.meta.json"), []byte("{}"), 0o600)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	stubClearExternals(t, nil, nil) // service/timer probes hermetic

	got := enumClearTargets(roles.RoleServer)
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"vault:", "serve:", "client:", "role:",
		"store.db", "store.db-wal", "store.db.meta.json", "master.key.plain",
		"serve-cert.pem", ".serve-cert-initialized",
		"cache.meta.json", "role.json",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	// Stat-gated: candidates that do NOT exist must not appear.
	for _, absent := range []string{"store.db-shm", "serve-key.pem", "cache.bin", "cache.auth.json", "cache-audit.log"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("non-existent %q enumerated:\n%s", absent, joined)
		}
	}
}

// TestEnumClearTargets_DualRoleMachine: a machine holding BOTH role.json
// locations (e.g. mid-migration) enumerates BOTH — the scan is deliberately
// role-blind (clear.go's scanClearTargets contract: catch every leftover
// regardless of what role.json claims).
func TestEnumClearTargets_DualRoleMachine(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedClearVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	// Second role.json at the CLIENT location (roles.Save writes per-role
	// paths; write the client one by hand via a second Save + move, or call
	// roles.Save(RoleClient) if RolePath differs — see roles.RolePath).
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	stubClearExternals(t, nil, nil)

	got := enumClearTargets(roles.RoleServer)
	if n := strings.Count(strings.Join(got, "\n"), "role.json"); n != 2 {
		t.Fatalf("dual-role machine must enumerate BOTH role.json locations, got %d:\n%s", n, strings.Join(got, "\n"))
	}
}

// TestEnumClearTargets_EmptyMachine: nothing on disk → nothing enumerated
// (clear on a fresh machine is a no-op list, never a fabricated one).
func TestEnumClearTargets_EmptyMachine(t *testing.T) {
	withClearDirs(t)
	stubClearExternals(t, nil, nil)
	if got := enumClearTargets(roles.RoleClient); len(got) != 0 {
		t.Fatalf("expected empty list, got:\n%s", strings.Join(got, "\n"))
	}
}

// ---------------------------------------------------------------------------
// makeSafetyNet
// ---------------------------------------------------------------------------

// TestMakeSafetyNet pins the verified-write contract: backup lands under the
// home dir with the agreed name shape, the passphrase is non-empty, and the
// on-disk blob round-trips Decrypt + json.Unmarshal into store.Snapshot.
func TestMakeSafetyNet(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedClearVault(t, vd)
	stubClearExternals(t, nil, nil)

	path, pass, err := makeSafetyNet()
	if err != nil {
		t.Fatal(err)
	}
	if pass == "" {
		t.Fatal("empty passphrase")
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "ssh-manager-backup-") || !strings.HasSuffix(base, ".sme") {
		t.Fatalf("bad backup name: %s", path)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vaultio.Decrypt([]byte(pass), blob)
	if err != nil {
		t.Fatalf("decrypt backup: %v", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		t.Fatalf("unmarshal decrypted snapshot: %v", err)
	}
}

// ---------------------------------------------------------------------------
// interactive flow
// ---------------------------------------------------------------------------

// TestClear_CancelZeroMutation: wrong confirmation word → 已取消, exit 0, and
// EVERY seeded file (vault, serve, client residue, role.json) still present;
// no safety-net file was written either.
func TestClear_CancelZeroMutation(t *testing.T) {
	vd, ud := withClearDirs(t)
	seedClearVault(t, vd)
	for _, f := range []string{"store.db-wal", "serve-cert.pem"} {
		os.WriteFile(filepath.Join(vd, f), []byte("x"), 0o600)
	}
	os.MkdirAll(filepath.Join(ud, "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(ud, "ssh-manager", "cache.meta.json"), []byte("{}"), 0o600)
	roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true})
	stubClearExternals(t, nil, nil)
	home := safetyNetHomeDirHome(t)

	out, err := driveClear(t, "nope\n")
	if err != nil {
		t.Fatalf("cancel must exit 0, got: %v", err)
	}
	if !strings.Contains(out, "已取消") {
		t.Fatalf("missing cancel message in:\n%s", out)
	}
	for _, p := range []string{
		filepath.Join(vd, "store.db"),
		filepath.Join(vd, "store.db-wal"),
		filepath.Join(vd, "master.key.plain"),
		filepath.Join(vd, "serve-cert.pem"),
		filepath.Join(vd, "role.json"),
		filepath.Join(ud, "ssh-manager", "cache.meta.json"),
	} {
		assertExists(t, p)
	}
	if n := len(backupFilesIn(t, home)); n != 0 {
		t.Fatalf("cancel wrote %d safety-net file(s) — zero mutation violated", n)
	}
}

// safetyNetHomeDirHome reads the temp home the stub installed (helper kept
// tiny so the stub's capture stays in one place).
func safetyNetHomeDirHome(t *testing.T) string {
	t.Helper()
	home, err := safetyNetHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

// TestClear_YConfirmAbortsAfterSafetyNet pins the spec §3 v2 ORDER: for vault
// roles the safety net is written and the passphrase shown BEFORE the y
// confirmation; answering anything but y still mutates nothing original.
func TestClear_YConfirmAbortsAfterSafetyNet(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedClearVault(t, vd)
	roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true})
	stubClearExternals(t, nil, nil)
	home := safetyNetHomeDirHome(t)

	out, err := driveClear(t, "DELETE\nn\n")
	if err != nil {
		t.Fatalf("y-abort must exit 0, got: %v", err)
	}
	if !strings.Contains(out, "已取消") || !strings.Contains(out, "口令") {
		t.Fatalf("missing cancel/passphrase output:\n%s", out)
	}
	if n := len(backupFilesIn(t, home)); n != 1 {
		t.Fatalf("expected exactly 1 safety-net file before y-confirm, got %d", n)
	}
	assertExists(t, filepath.Join(vd, "store.db"))
	assertExists(t, filepath.Join(vd, "role.json"))
}

// TestClear_ServerFullFlow: DELETE + y on a server machine → verified safety
// net written (decryptable with the passphrase shown in the output), every
// enumerated file deleted, service uninstall + legacy timer attempted,
// role.json gone, wizard-return message printed.
func TestClear_ServerFullFlow(t *testing.T) {
	vd, ud := withClearDirs(t)
	seedClearVault(t, vd)
	for _, f := range []string{"store.db-wal", "serve-cert.pem", ".serve-cert-initialized", "cache-dek.key"} {
		os.WriteFile(filepath.Join(vd, f), []byte("x"), 0o600)
	}
	os.MkdirAll(filepath.Join(ud, "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(ud, "ssh-manager", "cache.meta.json"), []byte("{}"), 0o600)
	roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true})
	var uninstallCalled, timerCalled bool
	stubClearExternals(t, &uninstallCalled, &timerCalled)
	home := safetyNetHomeDirHome(t)

	out, err := driveClear(t, "DELETE\ny\n")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "已清理") || !strings.Contains(out, "首次向导") {
		t.Fatalf("missing ceremony output:\n%s", out)
	}
	if !uninstallCalled || !timerCalled {
		t.Fatal("service uninstall / legacy timer not attempted")
	}
	files := backupFilesIn(t, home)
	if len(files) != 1 {
		t.Fatalf("expected 1 safety-net file, got %d", len(files))
	}
	// The passphrase shown in the output must decrypt the backup.
	var pass string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "口令：") {
			pass = strings.TrimPrefix(line, "口令：")
		}
	}
	if pass == "" {
		t.Fatalf("no passphrase line in output:\n%s", out)
	}
	blob, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vaultio.Decrypt([]byte(pass), blob)
	if err != nil {
		t.Fatalf("backup not decryptable with shown passphrase: %v", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		t.Fatalf("backup snapshot unmarshal: %v", err)
	}
	for _, p := range []string{
		filepath.Join(vd, "store.db"),
		filepath.Join(vd, "store.db-wal"),
		filepath.Join(vd, "master.key.plain"),
		filepath.Join(vd, "serve-cert.pem"),
		filepath.Join(vd, ".serve-cert-initialized"),
		filepath.Join(vd, "cache-dek.key"),
		filepath.Join(vd, "role.json"),
		filepath.Join(ud, "ssh-manager", "cache.meta.json"),
	} {
		assertGone(t, p)
	}
}

// TestClear_IdempotentRerun: full client-role flow with one target already
// missing (pre-deleted cache.meta.json) still succeeds and deletes the rest —
// the ENOENT-tolerant teardown contract.
func TestClear_IdempotentRerun(t *testing.T) {
	vd, ud := withClearDirs(t)
	cd := filepath.Join(ud, "ssh-manager")
	os.MkdirAll(cd, 0o700)
	for _, f := range []string{"cache.bin", "cache.auth.json", "cache-audit.log"} {
		os.WriteFile(filepath.Join(cd, f), []byte("x"), 0o600)
	}
	// pre-delete one enumerated target (cache.meta.json) → idempotency proof
	os.WriteFile(filepath.Join(cd, "cache.meta.json"), []byte("{}"), 0o600)
	os.Remove(filepath.Join(cd, "cache.meta.json"))
	os.WriteFile(filepath.Join(vd, "cache-dek.key"), []byte("k"), 0o600)
	roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true})
	var timerCalled bool
	stubClearExternals(t, nil, &timerCalled)
	home := safetyNetHomeDirHome(t)

	out, err := driveClear(t, "DELETE\n")
	if err != nil {
		t.Fatalf("clear (client): %v", err)
	}
	if !strings.Contains(out, "已清理") {
		t.Fatalf("missing done message:\n%s", out)
	}
	if !timerCalled {
		t.Fatal("legacy timer deletion not attempted")
	}
	for _, p := range []string{
		filepath.Join(cd, "cache.bin"),
		filepath.Join(cd, "cache.auth.json"),
		filepath.Join(cd, "cache-audit.log"),
		filepath.Join(cd, "role.json"),
		filepath.Join(vd, "cache-dek.key"),
	} {
		assertGone(t, p)
	}
	// client role: no vault → no safety net
	if n := len(backupFilesIn(t, home)); n != 0 {
		t.Fatalf("client clear must not write a safety net, got %d", n)
	}
}

// TestClear_NonTTYRefused: stdin not a TTY → hard refusal (script-proofing).
func TestClear_NonTTYRefused(t *testing.T) {
	vd, ud := withClearDirs(t)
	seedClearVault(t, vd)
	os.MkdirAll(filepath.Join(ud, "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(ud, "ssh-manager", "cache.bin"), []byte("x"), 0o600)
	stubClearExternals(t, nil, nil)
	prev := clearStdinIsTTY
	clearStdinIsTTY = func() bool { return false }
	t.Cleanup(func() { clearStdinIsTTY = prev })

	_, err := driveClear(t, "DELETE\ny\n")
	if err == nil || !strings.Contains(err.Error(), "交互式终端") {
		t.Fatalf("expected TTY refusal, got: %v", err)
	}
	assertExists(t, filepath.Join(vd, "store.db"))
	assertExists(t, filepath.Join(ud, "ssh-manager", "cache.bin"))
}

// TestClear_LockedVaultRefuses: vault role with an unopenable vault → clear
// aborts telling the user to unlock first (no backup-less deletion), files
// intact.
func TestClear_LockedVaultRefuses(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedClearVault(t, vd)
	os.Remove(filepath.Join(vd, "master.key.plain")) // locked: store present, key gone
	roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true})
	stubClearExternals(t, nil, nil)
	home := safetyNetHomeDirHome(t)

	_, err := driveClear(t, "DELETE\ny\n")
	if err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("expected unlock guidance, got: %v", err)
	}
	assertExists(t, filepath.Join(vd, "store.db"))
	assertExists(t, filepath.Join(vd, "role.json"))
	if n := len(backupFilesIn(t, home)); n != 0 {
		t.Fatalf("locked vault must not produce a backup, got %d", n)
	}
}

// TestClear_CorruptRoleJSONStillClears: clear must work when role.json itself
// is the broken state (its raison d'être) — the role resolver falls back to
// the vault probe, the safety net is made, everything is torn down.
func TestClear_CorruptRoleJSONStillClears(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedClearVault(t, vd)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte("{not json"), 0o600)
	stubClearExternals(t, nil, nil)
	home := safetyNetHomeDirHome(t)

	out, err := driveClear(t, "DELETE\ny\n")
	if err != nil {
		t.Fatalf("clear with corrupt role.json: %v", err)
	}
	if !strings.Contains(out, "已清理") {
		t.Fatalf("missing done message:\n%s", out)
	}
	if n := len(backupFilesIn(t, home)); n != 1 {
		t.Fatalf("fallback role must still make a safety net, got %d", n)
	}
	assertGone(t, filepath.Join(vd, "store.db"))
	assertGone(t, filepath.Join(vd, "role.json"))
}

// TestClear_EnumeratesInstancesAndDekVariants (Plan 40 §2.7): the whole
// instances/ tree and every per-instance DEK variant are enumerated — a
// residual instance dir IS residual credentials.
func TestClear_EnumeratesInstancesAndDekVariants(t *testing.T) {
	// 自包含重定向（clear_test.go 顶部已有同款 APPDATA/XDG 手法,形式一致）
	userDir, vault := t.TempDir(), t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":     "",
		"SSHMGR_CACHE_DEK":     "",
		"SSHMGR_STORE":         filepath.Join(vault, "store.db"), // clearVaultDir 从 store 路径派生
		"SSHMGR_CACHE_DEK_DIR": vault,                            // DEK 变体 glob 的 seam 基目录
	})
	instRoot := filepath.Join(userDir, "ssh-manager", "instances")
	os.MkdirAll(filepath.Join(instRoot, "agentA"), 0o700)
	os.WriteFile(filepath.Join(instRoot, "agentA", "cache.bin"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(vault, "cache-dek-agentA.key"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(vault, "cache-dek.key"), []byte("k"), 0o600)
	lines := enumClearTargets(roles.RoleClient)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, filepath.Join("instances")) || !strings.Contains(joined, "cache-dek-agentA.key") {
		t.Fatalf("enumeration missing instance artifacts:\n%s", joined)
	}
}
