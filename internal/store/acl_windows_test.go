//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestHardenACL_RemovesBroadGroups asserts HardenACL produces a restrictive
// DACL: SE_DACL_PROTECTED (inheritance disabled), SYSTEM + Administrators
// present with full control, and BUILTIN\Users / Authenticated Users /
// Everyone absent. Spec §5.2 (xcheck consensus E, codex P5/pi #3): the ACL is
// the ONLY protection layer for the plaintext master.key under L1+ — a
// leftover Authenticated Users ACE would let any same-machine logged-in user
// read every SSH credential.
//
// The test reads back the REAL DACL via GetNamedSecurityInfo (advapi32) — NOT
// a mock — so a miscompiled syscall or wrong flag fails here.
func TestHardenACL_RemovesBroadGroups(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "master.key.plain")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HardenACL(f); err != nil {
		t.Fatalf("HardenACL: %v", err)
	}

	dacl, sd, err := readDACL(f)
	if err != nil {
		t.Fatalf("read DACL: %v", err)
	}

	// Build the SIDs once (well-known SIDs are machine-independent constants).
	systemSID := mustWellKnownSID(t, windows.WinLocalSystemSid, "SYSTEM")
	adminsSID := mustWellKnownSID(t, windows.WinBuiltinAdministratorsSid, "Administrators")
	builtinUsersSID := mustWellKnownSID(t, windows.WinBuiltinUsersSid, "BUILTIN\\Users")
	authUsersSID := mustWellKnownSID(t, windows.WinAuthenticatedUserSid, "Authenticated Users")
	worldSID := mustWellKnownSID(t, windows.WinWorldSid, "Everyone")
	userSID := currentUserSIDForTest(t)
	if userSID == nil {
		t.Fatal("could not resolve current user SID")
	}

	// 1. Inheritance MUST be disabled (SE_DACL_PROTECTED). Without it the
	//    inherited Authenticated Users:modify ACE from C:\ProgramData\ would
	//    still apply and the file is world-readable to same-machine users.
	if !isDaclProtected(sd) {
		t.Error("DACL not protected (inheritance enabled) — file inherits broad ACEs from parent")
	}

	// 2. No broad groups. These are exactly the principals spec §5.2 names.
	for _, banned := range []*windows.SID{builtinUsersSID, authUsersSID, worldSID} {
		if trusteeInACL(dacl, banned) {
			t.Errorf("DACL still contains banned trustee")
		}
	}

	// 3. SYSTEM + Administrators MUST be present (else we've locked out the
	//    recovery principals — admin/LocalSystem can no longer service the file).
	if !trusteeInACL(dacl, systemSID) {
		t.Error("DACL missing SYSTEM (full control) — admin recovery lost")
	}
	if !trusteeInACL(dacl, adminsSID) {
		t.Error("DACL missing Administrators (full control) — admin recovery lost")
	}

	// 4. Current user MUST be present (else we've locked the caller out of its
	//    own master.key — the next Get would EACCES).
	if !trusteeInACL(dacl, userSID) {
		t.Error("DACL missing current user — caller locked out of own master.key")
	}

	// Diagnostic: log the SDDL string so a failure shows the live DACL.
	t.Logf("post-HardenACL SDDL: %s", sd.String())
}

// TestHardenACL_OnDir asserts HardenACL also works on a directory (the vault
// dir itself is hardened by future wiring + this is the same code path
// store.db's parent dir will use).
func TestHardenACL_OnDir(t *testing.T) {
	dir := t.TempDir()
	if err := HardenACL(dir); err != nil {
		t.Fatalf("HardenACL on dir: %v", err)
	}
	dacl, sd, err := readDACL(dir)
	if err != nil {
		t.Fatalf("read DACL: %v", err)
	}
	if !isDaclProtected(sd) {
		t.Error("dir DACL not protected")
	}
	authUsersSID := mustWellKnownSID(t, windows.WinAuthenticatedUserSid, "Authenticated Users")
	worldSID := mustWellKnownSID(t, windows.WinWorldSid, "Everyone")
	if trusteeInACL(dacl, authUsersSID) {
		t.Error("dir DACL contains Authenticated Users")
	}
	if trusteeInACL(dacl, worldSID) {
		t.Error("dir DACL contains Everyone")
	}
	t.Logf("post-HardenACL dir SDDL: %s", sd.String())
}

// TestFileKeyProvider_SetHardensACLOnWindows is the integration guard that
// HardenACL is ACTUALLY wired into FileKeyProvider.Set on Windows (spec §5.2
// requires it — without it the plaintext master.key is world-readable). A
// future refactor that drops the HardenACL call from Set fails here, not in
// production.
func TestFileKeyProvider_SetHardensACLOnWindows(t *testing.T) {
	dir := t.TempDir()
	p := FileKeyProvider{Path: filepath.Join(dir, "mk.plain")}
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	if err := p.Set(mk); err != nil {
		t.Fatalf("Set: %v", err)
	}
	dacl, sd, err := readDACL(p.Path)
	if err != nil {
		t.Fatalf("read DACL: %v", err)
	}
	if !isDaclProtected(sd) {
		t.Error("FileKeyProvider.Set did not protect DACL (HardenACL not wired)")
	}
	authUsersSID := mustWellKnownSID(t, windows.WinAuthenticatedUserSid, "Authenticated Users")
	if trusteeInACL(dacl, authUsersSID) {
		t.Error("master.key written by FileKeyProvider.Set contains Authenticated Users — ACL not hardened")
	}
	t.Logf("post-Set SDDL: %s", sd.String())
}

// TestOpen_HardensStoreDBACL is the integration guard that store.Open ACL-
// hardens the store.db FILE it creates (spec §5.2 xcheck codex P6: "master.key
// + store.db + cache-dek.key 同 ACL"). store.db holds plaintext server metadata
// (host/user/name/tags/description) + audit logs; without HardenACL the fresh
// file inherits the creating token's default DACL (Users / Authenticated Users
// read on Windows), contradicting spec §6.1's "same-machine non-privileged
// processes cannot read". A future refactor that drops the HardenACL call
// from Open fails here, not in production.
//
// The test reads back the REAL on-disk DACL of the store.db that Open created
// (NOT a mock) and asserts the same shape HardenACL produces elsewhere:
// inheritance disabled + broad groups absent.
func TestOpen_HardensStoreDBACL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	st, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// store.db (and its -wal/-shm sidecars) must exist on disk by now.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("store.db not created: %v", err)
	}

	dacl, sd, err := readDACL(dbPath)
	if err != nil {
		t.Fatalf("read DACL on store.db: %v", err)
	}

	// 1. Inheritance MUST be disabled — else the inherited Authenticated
	//    Users:read ACE from the temp/vault dir still applies.
	if !isDaclProtected(sd) {
		t.Error("store.db DACL not protected (inheritance enabled) — plaintext metadata world-readable to same-machine users")
	}

	// 2. No broad groups (spec §5.2 names exactly these principals).
	authUsersSID := mustWellKnownSID(t, windows.WinAuthenticatedUserSid, "Authenticated Users")
	builtinUsersSID := mustWellKnownSID(t, windows.WinBuiltinUsersSid, "BUILTIN\\Users")
	worldSID := mustWellKnownSID(t, windows.WinWorldSid, "Everyone")
	for _, banned := range []*windows.SID{authUsersSID, builtinUsersSID, worldSID} {
		if trusteeInACL(dacl, banned) {
			t.Errorf("store.db DACL still contains banned trustee — ACL not hardened by Open")
		}
	}

	// 3. The *sql.DB handle must still be usable AFTER the ACL change (Open
	//    opened the handle before HardenACL ran; on Windows a DACL change does
	//    not invalidate an existing handle — prove it by running a DML round-
	//    trip against audit_log, a table with no FK so a bare INSERT succeeds).
	if _, err := st.db.Exec(`INSERT INTO audit_log(ts,action) VALUES(0,'acl-probe')`); err != nil {
		t.Fatalf("write after HardenACL failed (handle invalidated?): %v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("read after HardenACL failed (handle invalidated?): %v", err)
	}
	if n < 1 {
		t.Fatalf("read after HardenACL returned %d rows, expected >=1", n)
	}

	t.Logf("post-Open store.db SDDL: %s", sd.String())
}

// mustWellKnownSID builds a well-known SID, fataling on error (these are
// constant enums — failure means the x/sys/windows version doesn't know that
// SID type, which is a compile-time/version problem, not a runtime condition).
func mustWellKnownSID(t *testing.T, sidType windows.WELL_KNOWN_SID_TYPE, label string) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(sidType)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(%s=%d): %v", label, sidType, err)
	}
	return sid
}

// currentUserSIDForTest returns the current process user's SID for the
// "current user must be in the DACL" assertion.
func currentUserSIDForTest(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Logf("currentUserSID: %v", err)
		return nil
	}
	return sid
}

// TestOpen_DoesNotRewriteExistingStoreDBACL is the NUC10 F1 regression test.
// HardenACL must run ONLY on first creation, not on every Open. The bug: serve
// (LocalSystem) re-opens a store.db created by the interactive user, and
// HardenACL re-ran under the service token — currentUserSID() returned
// LocalSystem, which collided with the SYSTEM ACE (SET_ACCESS dedup), silently
// dropping the user's ACE. Fix: Open snapshots existence before sql.Open and
// skips HardenACL when the file already exists.
//
// This test proves the contract: after Open #1 sets the ACL, manually corrupt
// it to a DIFFERENT SDDL. Open #2 must NOT restore it (no HardenACL re-run).
// If Open #2 rewrote the ACL, the corrupted SDDL would be overwritten.
func TestOpen_DoesNotRewriteExistingStoreDBACL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	mk := make([]byte, 32)

	// Open #1 — creates store.db + HardenACL (3-ACE: SYSTEM+Admins+user).
	st1, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	st1.Close()
	originalSDDL := getDACLForTestOrFatal(t, dbPath).String()
	t.Logf("post-Open#1 SDDL: %s", originalSDDL)

	// Manually replace the DACL with a deliberately-different one: grant
	// Everyone FullControl (a clearly-wrong, broad ACE). If Open #2 re-runs
	// HardenACL, it would overwrite this; if it correctly skips (file exists),
	// this broad ACE survives Open #2.
	everyoneSID := mustWellKnownSID(t, windows.WinWorldSid, "Everyone")
	broadEntries := []windows.EXPLICIT_ACCESS{
		buildExplicitAccess(everyoneSID, windows.GENERIC_ALL, windows.SET_ACCESS, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}
	broadDACL, err := windows.ACLFromEntries(broadEntries, nil)
	if err != nil {
		t.Fatalf("build broad DACL: %v", err)
	}
	const si = windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	if err := windows.SetNamedSecurityInfo(dbPath, windows.SE_FILE_OBJECT, si, nil, nil, broadDACL, nil); err != nil {
		t.Fatalf("manually corrupt ACL: %v", err)
	}
	corruptSDDL := getDACLForTestOrFatal(t, dbPath).String()
	t.Logf("post-corrupt SDDL: %s", corruptSDDL)

	// Open #2 — file exists, HardenACL must NOT run.
	st2, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	st2.Close()
	afterSecondSDDL := getDACLForTestOrFatal(t, dbPath).String()
	t.Logf("post-Open#2 SDDL: %s", afterSecondSDDL)

	// The ACL after Open #2 must equal the corrupt one (untouched), NOT the
	// original hardened one. If they're equal to original, HardenACL re-ran
	// (the bug).
	if afterSecondSDDL == originalSDDL {
		t.Fatalf("Open #2 rewrote the ACL (HardenACL re-ran on existing file) — F1 regression. got original SDDL back: %s", afterSecondSDDL)
	}
	if afterSecondSDDL != corruptSDDL {
		t.Fatalf("Open #2 changed the ACL to something unexpected (neither corrupt nor original): %s", afterSecondSDDL)
	}
	// Sanity: the Everyone ACE (our corruption marker) must still be present.
	dacl2, _, _ := readDACL(dbPath)
	if !trusteeInACL(dacl2, everyoneSID) {
		t.Fatalf("Everyone ACE (corruption marker) missing after Open #2 — ACL was rewritten")
	}
}

// getDACLForTestOrFatal reads the DACL+SD for path, fataling on error.
func getDACLForTestOrFatal(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	_, sd, err := readDACL(path)
	if err != nil {
		t.Fatalf("readDACL(%s): %v", path, err)
	}
	return sd
}

// TestHardenWALSidecars verifies the F2 fix: when -shm/-wal sidecars exist next
// to storePath, hardenWALSidecars applies HardenACL to them (disabling the broad
// inherited ACEs SQLite's creator-token would otherwise leave). These sidecars
// are created on demand by SQLite at first write under whatever process first
// writes — they inherit a too-broad / Admins-read-only DACL and block concurrent
// openers under WAL mode ("attempt to write a readonly database").
func TestHardenWALSidecars(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.db")

	// Create fake sidecar files with a deliberately-broad inherited ACL that
	// mimics what SQLite (LocalSystem) would leave: Everyone FullControl. If
	// hardenWALSidecars works, these get hardened (broad ACE gone, inheritance
	// disabled). Use icacls-equivalent via SetNamedSecurityInfo directly.
	everyoneSID := mustWellKnownSID(t, windows.WinWorldSid, "Everyone")
	for _, suffix := range []string{"-shm", "-wal"} {
		p := storePath + suffix
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("create %s: %v", suffix, err)
		}
		// Plant a broad DACL (Everyone FullControl, inheritance enabled) —
		// exactly the kind of thing SQLite's default-token creation leaves.
		broadDACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
			buildExplicitAccess(everyoneSID, windows.GENERIC_ALL, windows.SET_ACCESS, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
		}, nil)
		if err != nil {
			t.Fatalf("build broad DACL: %v", err)
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.UNPROTECTED_DACL_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
			nil, nil, broadDACL, nil); err != nil {
			t.Fatalf("plant broad DACL on %s: %v", suffix, err)
		}
	}

	// Run the fix.
	hardenWALSidecars(storePath)

	// Both sidecars must now be hardened: inheritance disabled, no Everyone.
	for _, suffix := range []string{"-shm", "-wal"} {
		p := storePath + suffix
		dacl, sd, err := readDACL(p)
		if err != nil {
			t.Fatalf("read DACL on %s: %v", suffix, err)
		}
		if !isDaclProtected(sd) {
			t.Errorf("%s DACL not protected after hardenWALSidecars", suffix)
		}
		if trusteeInACL(dacl, everyoneSID) {
			t.Errorf("%s still has Everyone ACE after hardenWALSidecars", suffix)
		}
	}

	// A missing storePath (no sidecars at all) must not error — normal for a
	// fresh vault with no writes yet.
	hardenWALSidecars(filepath.Join(dir, "nonexistent.db"))
}

// TestHardenWALSidecars_NoOpOnFreshStore confirms that on a freshly-created
// store.db (no -shm/-wal yet, because no writes), hardenWALSidecars does
// nothing and Open succeeds — the common path until the first write.
func TestHardenWALSidecars_NoOpOnFreshStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.db")
	mk := make([]byte, 32)
	st, err := Open(storePath, mk)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()
	// No -shm/-wal should exist (Open + schema does not create them; only
	// writes do). hardenWALSidecars must be a silent no-op here.
	for _, suffix := range []string{"-shm", "-wal"} {
		if _, err := os.Stat(storePath + suffix); err == nil {
			t.Errorf("unexpected %s on fresh store (Open should not create WAL sidecars)", suffix)
		}
	}
}

// seedLooseACE plants a single explicit allow ACE for sid with the given mask
// (PROTECTED — no inheritance), replacing the file's DACL. Test-only seeding,
// SetNamedSecurityInfo direct (TestOpen_DoesNotRewriteExistingStoreDBACL
// precedent).
func seedLooseACE(t *testing.T, path string, sid *windows.SID, mask uint32) {
	t.Helper()
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(mask),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("build seed DACL: %v", err)
	}
	si := windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, nil, nil, dacl, nil); err != nil {
		t.Fatalf("seed DACL: %v", err)
	}
}

// seedLooseOwner plants a non-whitelisted owner (Everyone). It reports the
// error instead of fataling: the assignment needs WRITE_OWNER on the object,
// which the hardened DACL deliberately withholds from the current user, and
// under a non-elevated (UAC-filtered) token even a fresh file rejects the
// assignment (Everyone/BUILTIN\Users → ERROR_INVALID_OWNER; Administrators is
// deny-only; probed 2026-08-26, Plan 38 T1). The caller decides between
// running the leg where the token permits it and skipping with the reason.
func seedLooseOwner(t *testing.T, path string) error {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	si := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION)
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, everyone, nil, nil, nil)
}

func mustEveryoneSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func TestInspectFileACL_Hardened(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "master.key.plain")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HardenACL(f); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectFileACL(f)
	if err != nil {
		t.Fatalf("InspectFileACL: %v", err)
	}
	if !rep.Supported || rep.TooLoose() {
		t.Fatalf("hardened file must not be loose: %+v", rep)
	}
	if !rep.Protected {
		t.Error("hardened file must report Protected")
	}
	if len(rep.UnexpectedReadGrantors) != 0 {
		t.Errorf("hardened file must have no unexpected grantors: %v", rep.UnexpectedReadGrantors)
	}
	if rep.OwnerUnexpected {
		t.Error("hardened file owner (creator) must be whitelisted")
	}
}

func TestInspectFileACL_TooLoose_Matrix(t *testing.T) {
	dir := t.TempDir()
	newFile := func() string {
		f := filepath.Join(dir, fmt.Sprintf("m%d.key", len(dirEntries(t, dir))))
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	everyone := mustEveryoneSID(t)
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}

	// ① null DACL — per the Step-1 probe outcome, ONE of the two legs:
	//   (probe said plantable) assert DaclNull && TooLoose
	//   (probe said NOT plantable) assert the fallback: explicit Everyone-allow
	//   + UNPROTECTED covers signal 3 only — and DELETE the DaclNull assertion,
	//   leaving the §7 residual as registered.
	// Probe outcome (2026-08-26): plantable — readback = SE_DACL_PRESENT=1 +
	// nil DACL pointer (the null-DACL form), so the mandatory leg runs.
	f := newFile()
	si := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, si, nil, nil, nil, nil); err != nil {
		t.Skipf("null-DACL not plantable via x/sys (probe branch): %v", err)
	}
	rep, err := InspectFileACL(f)
	if err != nil {
		t.Fatalf("①: %v", err)
	}
	if !rep.DaclNull || !rep.TooLoose() {
		t.Errorf("① null DACL must be DaclNull && TooLoose: %+v", rep)
	}

	// ② broad inheritable parent → child inherits the Everyone ACE.
	pdir := filepath.Join(dir, "broad-parent")
	if err := os.Mkdir(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	pdacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	psi := windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(pdir, windows.SE_FILE_OBJECT, psi, nil, nil, pdacl, nil); err != nil {
		t.Fatalf("seed broad parent: %v", err)
	}
	child := filepath.Join(pdir, "child.key")
	if err := os.WriteFile(child, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = InspectFileACL(child)
	if err != nil {
		t.Fatalf("②: %v", err)
	}
	if rep.TooLoose() != true || !containsSID(rep.UnexpectedReadGrantors, everyone.String()) {
		t.Errorf("② inherited broad ACE must be caught: %+v", rep)
	}

	// ③ explicit Everyone-allow read ACE.
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.FILE_GENERIC_READ))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("③: %v", err)
	}
	if !rep.TooLoose() || !containsSID(rep.UnexpectedReadGrantors, everyone.String()) {
		t.Errorf("③ explicit Everyone read must be caught: %+v", rep)
	}

	// ④ BUILTIN\Users read ACE (any non-whitelisted SID representative).
	f = newFile()
	seedLooseACE(t, f, users, uint32(windows.FILE_GENERIC_READ))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("④: %v", err)
	}
	if !rep.TooLoose() || !containsSID(rep.UnexpectedReadGrantors, users.String()) {
		t.Errorf("④ BUILTIN\\Users read must be caught: %+v", rep)
	}

	// ⑤ write-only mask → NOT dangerous (mask filter pin).
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.FILE_WRITE_DATA))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑤: %v", err)
	}
	if rep.TooLoose() {
		t.Errorf("⑤ write-only Everyone must NOT be flagged: %+v", rep)
	}

	// ⑥ deny ACE → NOT flagged (allow-only pin).
	f = newFile()
	denyDACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dsi := windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, dsi, nil, nil, denyDACL, nil); err != nil {
		t.Fatalf("seed deny: %v", err)
	}
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑥: %v", err)
	}
	if rep.TooLoose() {
		t.Errorf("⑥ Everyone-deny must NOT be flagged: %+v", rep)
	}

	// ⑦ WRITE_DAC-only → caught (elevation-bit positive).
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.WRITE_DAC))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑦: %v", err)
	}
	if !rep.TooLoose() {
		t.Errorf("⑦ WRITE_DAC-only must be caught: %+v", rep)
	}

	// ⑧ WRITE_OWNER-only → caught.
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.WRITE_OWNER))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑧: %v", err)
	}
	if !rep.TooLoose() {
		t.Errorf("⑧ WRITE_OWNER-only must be caught: %+v", rep)
	}

	// ⑨ raw GENERIC_ALL stored → read back expanded (fact #15 regression pin).
	f = newFile()
	seedLooseACE(t, f, everyone, 0x80000000)
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑨: %v", err)
	}
	if !rep.TooLoose() {
		t.Errorf("⑨ GENERIC_ALL must expand to dangerous specific rights and be caught: %+v", rep)
	}

	// ⑩ INHERIT_ONLY allow ACE → NOT flagged (no effect on the file itself).
	f = newFile()
	ioDACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.INHERIT_ONLY,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, dsi, nil, nil, ioDACL, nil); err != nil {
		t.Fatalf("seed inherit-only: %v", err)
	}
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑩: %v", err)
	}
	if rep.TooLoose() {
		t.Errorf("⑩ INHERIT_ONLY ACE must NOT be flagged: %+v", rep)
	}
}

func TestInspectFileACL_Owner(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "owner.key")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HardenACL(f); err != nil {
		t.Fatal(err)
	}
	// Planting a non-whitelisted owner requires a token that may assign this
	// owner: the hardened DACL withholds WRITE_OWNER from the current user by
	// design (only admins can re-owner), and a non-elevated token rejects
	// every candidate owner outright (probed 2026-08-26, Plan 38 T1 — see the
	// spec §7 residual). Same degrade pattern as the null-DACL leg ①: run
	// where seedable, skip with the reason elsewhere (whitelisted-owner
	// coverage stays via TestInspectFileACL_Hardened's creator-owner
	// assertion).
	if err := seedLooseOwner(t, f); err != nil {
		t.Skipf("non-whitelisted owner not seedable under this token (owner re-assignment needs an owner-capable token, probed 2026-08-26 — see spec §7 residual): %v", err)
	}
	rep, err := InspectFileACL(f)
	if err != nil {
		t.Fatalf("InspectFileACL: %v", err)
	}
	if !rep.OwnerUnexpected || !rep.TooLoose() {
		t.Fatalf("non-whitelisted owner must trigger: %+v", rep)
	}
	if rep.OwnerSID != mustEveryoneSID(t).String() {
		t.Errorf("OwnerSID must render the planted SID: %s", rep.OwnerSID)
	}
	// restore owner to Administrators → not triggered
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	si := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, si, admins, nil, nil, nil); err != nil {
		t.Fatalf("restore owner: %v", err)
	}
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("InspectFileACL: %v", err)
	}
	if rep.OwnerUnexpected {
		t.Errorf("Administrators owner must be whitelisted: %+v", rep)
	}
}

func TestInspectFileACL_MissingPath(t *testing.T) {
	if _, err := InspectFileACL(filepath.Join(t.TempDir(), "nope.key")); err == nil {
		t.Fatal("missing path must return an error")
	}
}

func containsSID(sids []string, sid string) bool {
	for _, s := range sids {
		if s == sid {
			return true
		}
	}
	return false
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}
