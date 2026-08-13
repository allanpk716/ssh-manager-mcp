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
