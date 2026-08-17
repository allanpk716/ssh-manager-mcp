package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// withDoctorDirs isolates every filesystem/env location doctor READS —
// withClearDirs discipline (the dev machine REALLY runs ssh-manager, so an
// unpinned check would inspect the operator's live vault). It adds the two
// cache-credential seams (SSHMGR_CACHE_URL / SSHMGR_CACHE_TOKEN) that clear
// never touched but doctor's env check enumerates.
func withDoctorDirs(t *testing.T) (vaultDir, userDir string) {
	t.Helper()
	vaultDir = t.TempDir()
	userDir = t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, "master.key.plain"))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", filepath.Join(vaultDir, "cache-dek.key"))
	t.Setenv("SSHMGR_SERVE_LOG", filepath.Join(vaultDir, "serve.log"))
	t.Setenv("SSHMGR_SERVE_CERT", "")
	t.Setenv("SSHMGR_SERVE_KEY", "")
	t.Setenv("SSHMGR_SERVE_MARKER", "")
	t.Setenv("SSHMGR_CACHE_URL", "")
	t.Setenv("SSHMGR_CACHE_TOKEN", "")
	t.Setenv("APPDATA", userDir) // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir)
	return vaultDir, userDir
}

// driveDoctor runs `doctor` through cobra with captured output — driveClear's
// out-buffer pattern (Plan 27 T1).
func driveDoctor(t *testing.T) (string, error) {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"doctor"})
	err := root.Execute()
	return out.String(), err
}

// TestDoctorExitCodes pins the three-state exit-code convention:
// nil → 0 (no FAIL; WARN never changes the code), errDoctorFindings → 1
// (≥1 FAIL), anything else → 2 (doctor internal error).
func TestDoctorExitCodes(t *testing.T) {
	// State 1 — fresh machine: role.json missing is INFO (points at the
	// wizard), NOT a FAIL; no FAIL anywhere → runDoctor returns nil.
	withDoctorDirs(t)
	out, err := driveDoctor(t)
	if err != nil {
		t.Fatalf("fresh machine must have no FAIL (exit 0), got: %v\n%s", err, out)
	}
	for _, want := range []string{
		"ssh-manager doctor (", // header carries buildinfo.Version
		"no role.json — fresh machine, run the wizard",
		"overall: 0 WARN, 0 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fix:") {
		t.Fatalf("no fix lines expected without WARN/FAIL:\n%s", out)
	}

	// State 2 — a FAIL finding (corrupt role.json: the role check's FAIL
	// branch) → errDoctorFindings, rendered with a fix line and counted.
	vd, _ := withDoctorDirs(t)
	if err := os.WriteFile(filepath.Join(vd, "role.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = driveDoctor(t)
	if !errors.Is(err, errDoctorFindings) {
		t.Fatalf("≥1 FAIL must return errDoctorFindings, got: %v\n%s", err, out)
	}
	for _, want := range []string{
		"role:  FAIL",
		"fix:",
		"overall: 0 WARN, 1 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// State 3 — the mapping itself: nil → 0, findings (wrapped included) → 1,
	// any other error → 2.
	if got := doctorExitCode(nil); got != 0 {
		t.Fatalf("nil must map to 0, got %d", got)
	}
	if got := doctorExitCode(errDoctorFindings); got != 1 {
		t.Fatalf("errDoctorFindings must map to 1, got %d", got)
	}
	if got := doctorExitCode(fmt.Errorf("%w (1) — see report", errDoctorFindings)); got != 1 {
		t.Fatalf("wrapped findings must still map to 1, got %d", got)
	}
	if got := doctorExitCode(errors.New("boom")); got != 2 {
		t.Fatalf("internal error must map to 2, got %d", got)
	}
}

// TestDoctorEnvSeamsReported pins the env check's security contract: set
// seams are reported BY NAME ONLY (values may be keys/tokens), and the dev
// affordance SSHMGR_MASTERKEY_HEX is a WARN with remediation — never a FAIL.
func TestDoctorEnvSeamsReported(t *testing.T) {
	vd, _ := withDoctorDirs(t)
	t.Setenv("SSHMGR_MASTERKEY_HEX", strings.Repeat("41", 32))
	out, err := driveDoctor(t)
	if err != nil {
		t.Fatalf("WARN must not change the exit code, got: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SSHMGR_MASTERKEY_HEX is set") {
		t.Fatalf("dev-affordance env must be flagged:\n%s", out)
	}
	if !strings.Contains(out, "dev/test affordance") {
		t.Fatalf("the affordance warning text must be present:\n%s", out)
	}
	// Values NEVER reach the output — neither the hex key itself...
	if strings.Contains(out, strings.Repeat("41", 32)) {
		t.Fatalf("SSHMGR_MASTERKEY_HEX VALUE leaked into the report:\n%s", out)
	}
	// ...nor any other seam's value (withDoctorDirs points SSHMGR_STORE at a
	// temp path; only the NAME may appear).
	if strings.Contains(out, filepath.Join(vd, "store.db")) {
		t.Fatalf("SSHMGR_STORE VALUE leaked into the report:\n%s", out)
	}
	for _, name := range []string{"SSHMGR_STORE", "SSHMGR_FILEKEY_PATH", "SSHMGR_CACHE_DEK", "SSHMGR_SERVE_LOG"} {
		if !strings.Contains(out, name) {
			t.Fatalf("set seam %s must be listed by name:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "overall: 1 WARN, 0 FAIL") {
		t.Fatalf("masterkey WARN must be the only WARN:\n%s", out)
	}
}

// TestDoctorRoleStates pins the role check's remaining states: incomplete
// wizard → WARN, completed role → PASS with the role value in Detail, and
// dual-location role.json residue → WARN. The vault is seeded up front so the
// T2 structural checks stay quiet — this test isolates the ROLE row (a
// server-role machine without store.db is a T2 FAIL by design, tested in
// TestDoctorVaultStructural).
func TestDoctorRoleStates(t *testing.T) {
	// Incomplete wizard: role.json saved with setup_complete=false.
	vd, _ := withDoctorDirs(t)
	seedDoctorVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: false}); err != nil {
		t.Fatal(err)
	}
	out, err := driveDoctor(t)
	if err != nil {
		t.Fatalf("WARN must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wizard incomplete") || !strings.Contains(out, "role=server") {
		t.Fatalf("incomplete wizard must WARN with the role value in Detail:\n%s", out)
	}
	if !strings.Contains(out, "overall: 1 WARN, 0 FAIL") {
		t.Fatalf("expected exactly the role WARN:\n%s", out)
	}

	// Completed role → PASS with the role value in Detail.
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("complete role must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "role:  PASS") || !strings.Contains(out, "role=standalone") {
		t.Fatalf("complete role must PASS with the role value in Detail:\n%s", out)
	}
	if !strings.Contains(out, "overall: 0 WARN, 0 FAIL") {
		t.Fatalf("healthy role must contribute no WARN:\n%s", out)
	}

	// Dual-location residue: BOTH role.json locations present → WARN.
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err) // vault-dir role.json
	}
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err) // user-config role.json — vault one stays on disk
	}
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("dual-role residue must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dual-role residue") {
		t.Fatalf("both role.json locations present must WARN:\n%s", out)
	}
	if !strings.Contains(out, "overall: 1 WARN, 0 FAIL") {
		t.Fatalf("expected exactly the dual-role WARN:\n%s", out)
	}
}

// seedDoctorVault builds a REAL vault in the test's temp dir — seedClearVault
// precedent (clear_test.go): store.Open to create store.db (side effects are
// legal in TESTS; doctor itself only Stats/ReadFiles) + the 32-byte
// master.key.plain next to it.
func seedDoctorVault(t *testing.T, vaultDir string) {
	t.Helper()
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

// TestDoctorVaultStructural pins the T2 vault structural checks: store.db /
// master.key presence per role, the 32-byte key-length contract, and — Unix
// only, guarded by runtime.GOOS (mode bits are not a protection layer on
// Windows) — the group/world-readable permission WARN.
func TestDoctorVaultStructural(t *testing.T) {
	// Case 1 — healthy vault: real store.db + valid 32-byte master.key +
	// complete server role → both structural rows PASS with sizes in Detail.
	vd, _ := withDoctorDirs(t)
	seedDoctorVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	out, err := driveDoctor(t)
	if err != nil {
		t.Fatalf("healthy vault must not FAIL: %v\n%s", err, out)
	}
	for _, want := range []string{
		"store:  PASS",
		"store.db present (", // size in Detail
		"masterkey:  PASS",
		"master.key present (32 bytes)",
		"overall: 0 WARN, 0 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// Case 2 — wrong-length master.key (17 bytes, truncated/garbage) → FAIL
	// with the expected-32 contract in Detail; store.db itself stays PASS.
	vd, _ = withDoctorDirs(t)
	seedDoctorVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vd, "master.key.plain"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = driveDoctor(t)
	if !errors.Is(err, errDoctorFindings) {
		t.Fatalf("wrong-length master.key must FAIL (exit 1), got: %v\n%s", err, out)
	}
	for _, want := range []string{
		"store:  PASS",
		"masterkey:  FAIL",
		"master.key is 17 bytes, expected 32",
		"fix:",
		"overall: 0 WARN, 1 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// Case 3 — Unix only: a valid key with loose mode bits (0644) → WARN with
	// "group/world readable" in Detail, still exit 0. On Windows the plaintext
	// key is protected by ACLs, not mode bits — the branch is skipped there.
	if runtime.GOOS == "windows" {
		t.Log("skipping permission-bit WARN on Windows (ACLs, not mode bits, are the protection layer)")
	} else {
		vd, _ = withDoctorDirs(t)
		seedDoctorVault(t, vd)
		if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(vd, "master.key.plain"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err = driveDoctor(t)
		if err != nil {
			t.Fatalf("a permission WARN must not change the exit code: %v\n%s", err, out)
		}
		for _, want := range []string{
			"masterkey:  WARN",
			"group/world readable",
			"overall: 1 WARN, 0 FAIL",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q in:\n%s", want, out)
			}
		}
	}

	// Case 4 — the missing-vault matrix: a vault-holding role (server) with NO
	// vault on disk → both rows FAIL with unlock/wizard remediation; a client
	// role with no vault → both INFO (cache-only is legitimate), exit 0.
	withDoctorDirs(t)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	out, err = driveDoctor(t)
	if !errors.Is(err, errDoctorFindings) {
		t.Fatalf("missing vault on a server must FAIL (exit 1), got: %v\n%s", err, out)
	}
	for _, want := range []string{
		"store:  FAIL",
		"masterkey:  FAIL",
		"unlock",
		"overall: 0 WARN, 2 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	withDoctorDirs(t)
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("client without a local vault must stay exit 0: %v\n%s", err, out)
	}
	for _, want := range []string{
		"store:  INFO",
		"masterkey:  INFO",
		"overall: 0 WARN, 0 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
