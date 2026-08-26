package roles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// withDirs isolates both role-file locations via env (SSHMGR_STORE pins the vault
// dir; XDG_CONFIG_HOME/APPDATA pins the user dir). The master-key path and cache
// dir are also pinned so a real vault / cache on the dev machine
// (C:\ProgramData\ssh-manager, %AppData%\ssh-manager) can never leak into the
// probe. SSHMGR_MASTERKEY_HEX / SSHMGR_CACHE_DIR / SSHMGR_SERVE_CERT are cleared
// so stray env never short-circuits the tiers being tested.
func withDirs(t *testing.T) (vaultDir, userDir string) {
	t.Helper()
	vaultDir = t.TempDir()
	userDir = t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, "master.key.plain"))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_SERVE_CERT", "")
	t.Setenv("APPDATA", userDir) // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir)
	return vaultDir, userDir
}

func seedVault(t *testing.T, vaultDir string) {
	t.Helper()
	// Drop any placeholder store.db from an earlier subtest (e.g. the locked
	// vault's "x" byte) — sqlite refuses to open a garbage file.
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
	// VaultUnlocked probes via store.FileKeyProvider, so the key must exist at
	// the pinned SSHMGR_FILEKEY_PATH for the vault to count as unlocked.
	if err := os.WriteFile(filepath.Join(vaultDir, "master.key.plain"), mk, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_Empty(t *testing.T) {
	withDirs(t)
	s, err := Load()
	if err != nil || s != nil {
		t.Fatalf("empty: (%v,%v)", s, err)
	}
}

func TestSaveLoad_ClientRoundTrip(t *testing.T) {
	withDirs(t)
	if err := Save(State{Role: RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil || s == nil || s.Role != RoleClient || !s.SetupComplete {
		t.Fatalf("roundtrip: %+v %v", s, err)
	}
	// client role.json must live in the USER dir, not the vault dir
	p, _ := RolePath(RoleClient)
	if !strings.Contains(p, "ssh-manager") {
		t.Fatalf("client role path not under user dir: %s", p)
	}
}

// TestRolePathPrecedenceVaultOverUserDir locks the two-location coexistence
// rule: when role.json exists in BOTH the vault dir (SSHMGR_STORE side) and
// the user config dir, Load/ResolveMode must resolve to the VAULT side — the
// production vaultRolePath derives from the store-path directory (see
// vaultRolePath), and the vault machine's role must never be shadowed by a
// stale client-side leftover file.
func TestRolePathPrecedenceVaultOverUserDir(t *testing.T) {
	vd, ud := withDirs(t)
	seedVault(t, vd) // a vault-side server/standalone role only resolves with a live vault
	if err := os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"server","setup_complete":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ud, "ssh-manager"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ud, "ssh-manager", "role.json"), []byte(`{"role":"client","setup_complete":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil || s == nil || s.Role != RoleServer {
		t.Fatalf("Load must resolve to the vault-side role.json: %+v %v", s, err)
	}
	l, err := ResolveMode("")
	if err != nil || l.Role != RoleServer || l.Kind != LaunchBroker || l.ResumeSetup {
		t.Fatalf("ResolveMode must resolve to the vault side: %+v %v", l, err)
	}
}

func TestResolve_FullMatrix(t *testing.T) {
	// wizard on empty
	withDirs(t)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchWizard {
		t.Fatalf("empty: %+v %v", l, err)
	}
	// locked vault never degrades to client (vault exists, no master key)
	vd, ud := withDirs(t)
	_ = ud
	os.WriteFile(filepath.Join(vd, "store.db"), []byte("x"), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("locked vault must fail-closed: %v", err)
	}
	// unlocked vault → broker (standalone heuristic: no serve cert)
	seedVault(t, vd)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchBroker || l.Role != RoleStandalone {
		t.Fatalf("vault: %+v %v", l, err)
	}
	// vault + serve cert → server heuristic
	os.WriteFile(filepath.Join(vd, "serve-cert.pem"), []byte("x"), 0o600)
	if l, _ := ResolveMode(""); l.Role != RoleServer {
		t.Fatalf("serve cert should hint server: %+v", l)
	}
	// cache cred only → client
	vd2, _ := withDirs(t)
	_ = vd2
	os.MkdirAll(filepath.Join(os.Getenv("APPDATA"), "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(os.Getenv("APPDATA"), "ssh-manager", "cache.auth.json"),
		[]byte(`{"url":"https://x","token":"t"}`), 0o600)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchClient {
		t.Fatalf("cache: %+v %v", l, err)
	}
	// force client on vault machine → guided error mentioning clear + 删除
	// (seed vd2 — the cache subtest's withDirs re-pinned SSHMGR_STORE there)
	seedVault(t, vd2)
	if _, err := ResolveMode("client"); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("force client on vault: %v", err)
	}
}

func TestResolve_IncompleteVaultRole_Resumes(t *testing.T) {
	vd, _ := withDirs(t)
	// setup_complete=false without vault → resume wizard
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"standalone","setup_complete":false}`), 0o600)
	l, err := ResolveMode("")
	if err != nil || l.Kind != LaunchWizard || !l.ResumeSetup || l.Role != RoleStandalone {
		t.Fatalf("want wizard resume, got %+v %v", l, err)
	}
	// setup_complete=true without vault → anomaly
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"standalone","setup_complete":true}`), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("complete+missing vault must stay anomaly: %v", err)
	}
}

func TestResolve_RoleFileAnomalies(t *testing.T) {
	// invalid value
	vd, _ := withDirs(t)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"clientx"}`), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("invalid role: %v", err)
	}
	// setup_complete=false without vault → resume wizard (not an error)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"standalone","setup_complete":false}`), 0o600)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchWizard || !l.ResumeSetup || l.Role != RoleStandalone {
		t.Fatalf("incomplete vault-role should resume wizard: got %+v %v", l, err)
	}
	// setup_complete=true without vault → genuine anomaly (error)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"standalone","setup_complete":true}`), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("complete+missing vault must be anomaly: %v", err)
	}
	// setup_complete=false with vault → resume broker setup (flag set)
	seedVault(t, vd)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"standalone","setup_complete":false}`), 0o600)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchBroker || !l.ResumeSetup {
		t.Fatalf("resume broker setup: %+v %v", l, err)
	}
}

// TestResolveMode_OnlyNamedInstances_IsClient (Plan 40 §2.7): a machine whose
// ONLY cache material is named-instance slots is still an enrolled client —
// never misrouted to the wizard.
func TestResolveMode_OnlyNamedInstances_IsClient(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "") // 默认槽 auth 也不在
	os.MkdirAll(filepath.Join(userDir, "ssh-manager", "instances", "agentA"), 0o700)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchClient {
		t.Fatalf("named-instance-only machine must be a client: %+v %v", l, err)
	}
}
