//go:build windows

package store

import (
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

	dacl, sd, err := getDACLForTest(f)
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
	dacl, sd, err := getDACLForTest(dir)
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
	dacl, sd, err := getDACLForTest(p.Path)
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

	dacl, sd, err := getDACLForTest(dbPath)
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
	dacl2, _, _ := getDACLForTest(dbPath)
	if !trusteeInACL(dacl2, everyoneSID) {
		t.Fatalf("Everyone ACE (corruption marker) missing after Open #2 — ACL was rewritten")
	}
}

// getDACLForTestOrFatal reads the DACL+SD for path, fataling on error.
func getDACLForTestOrFatal(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	_, sd, err := getDACLForTest(path)
	if err != nil {
		t.Fatalf("getDACLForTest(%s): %v", path, err)
	}
	return sd
}
