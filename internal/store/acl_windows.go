//go:build windows

package store

import (
	"fmt"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HardenACL locks path (file or directory) to a restrictive DACL:
//
//   - SYSTEM (LocalSystem)          — full control
//   - BUILTIN\Administrators        — full control
//   - current user / service account — read + write + delete
//
// Inheritance is DISABLED (SE_DACL_PROTECTED / PROTECTED_DACL_SECURITY_INFORMATION)
// so the broad ACEs inherited from C:\ProgramData\ (which include
// Authenticated Users:modify) do NOT carry onto the file. BUILTIN\Users,
// Authenticated Users and Everyone are NOT in the freshly-built DACL.
//
// Spec §5.2 (Plan 16, xcheck consensus E): under the L1+ threat model the
// master.key is a PLAINTEXT file, so this ACL is the ONLY protection layer —
// HardenACL hard-fails on any error so a silent ACL gap never leaves
// credentials world-readable. Callers (FileKeyProvider.Set; future wiring for
// store.db + cache-dek.key) MUST propagate the error.
//
// Implementation: pure-Go golang.org/x/sys/windows security descriptor API.
// Each advapi32 entry point used here is wrapped by an exported x/sys/windows
// function — NO icacls.exe / no external process (spec §5.2 rejects icacls as
// a silently-failing external dependency).
//
// advapi32 functions transitively called (all via x/sys/windows wrappers):
//   - CreateWellKnownSid                       (SYSTEM, Administrators)
//   - GetTokenInformation(TokenUser)           (current user SID)
//   - SetEntriesInAclW  -> windows.ACLFromEntries (builds *ACL from EXPLICIT_ACCESS)
//   - SetNamedSecurityInfoW                    (windows.SetNamedSecurityInfo,
//     PROTECTED_DACL_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION —
//     the PROTECTED flag is what disables inheritance on the stored SD)
func HardenACL(path string) error {
	if path == "" {
		return fmt.Errorf("hardenACL: empty path")
	}

	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("hardenACL: build SYSTEM sid: %w", err)
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("hardenACL: build Administrators sid: %w", err)
	}
	userSID, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("hardenACL: resolve current user sid: %w", err)
	}

	// Build a fresh DACL. SET_ACCESS (not GRANT_ACCESS) means each entry
	// REPLACES any existing ACE for that trustee — combined with passing a
	// nil mergedACL into ACLFromEntries, the resulting DACL contains ONLY
	// these three entries; nothing inherited, nothing previously-granted.
	//
	// Full control for SYSTEM/Administrators is the recovery contract: an
	// admin shell or LocalSystem service can still service the file. The
	// current user gets read+write+delete+read-control (WRITE_DAC/WRITE_OWNER
	// deliberately withheld so a later compromise of the user process can't
	// relax the ACL — only an admin can take ownership and re-ACL).
	//
	// This grant set is mirrored by aclWhitelist() for InspectFileACL's
	// read-back comparison — change both together.
	const userMask = windows.READ_CONTROL | windows.DELETE |
		windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
	entries := []windows.EXPLICIT_ACCESS{
		buildExplicitAccess(systemSID, windows.GENERIC_ALL, windows.SET_ACCESS, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
		buildExplicitAccess(adminsSID, windows.GENERIC_ALL, windows.SET_ACCESS, windows.TRUSTEE_IS_GROUP),
		buildExplicitAccess(userSID, userMask, windows.SET_ACCESS, windows.TRUSTEE_IS_USER),
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("hardenACL: SetEntriesInAcl: %w", err)
	}

	// Apply to the on-disk object. PROTECTED_DACL_SECURITY_INFORMATION is what
	// actually disables inheritance on the file: it sets SE_DACL_PROTECTED in
	// the stored security descriptor, so the broad ACEs inherited from
	// C:\ProgramData\ (Authenticated Users:modify etc.) no longer apply.
	// DACL_SECURITY_INFORMATION says "write the supplied DACL". Owner/group
	// left untouched (nil = no change). The fresh DACL from ACLFromEntries is
	// allocated on the Go heap and outlives this call; SetNamedSecurityInfo
	// copies it into the object's security descriptor.
	const si = windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("hardenACL: SetNamedSecurityInfo: %w", err)
	}
	return nil
}

// buildExplicitAccess assembles an EXPLICIT_ACCESS entry for the given SID.
// TrusteeForm = TRUSTEE_IS_SID (we pass a *SID, not a name string). The
// Inheritance field is 0 (NO_INHERITANCE): for a file, container inheritance
// flags are meaningless; for a directory HardenACL callers want the ACE on the
// dir object itself (the dir's own DACL, not just inheritable ACEs), so a
// non-inheritable explicit ACE is correct for both. Callers that need children
// to inherit should harden each child file individually via the same helper
// (store.db / cache-dek.key).
func buildExplicitAccess(sid *windows.SID, mask windows.ACCESS_MASK, mode windows.ACCESS_MODE, ttype windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: mask,
		AccessMode:        mode,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			MultipleTrustee:          nil,
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              ttype,
			TrusteeValue:             windows.TrusteeValueFromSID(sid),
		},
	}
}

// currentUserSID returns the SID of the calling process's token (the account
// that owns the master.key file — the service account under serve, the
// interactive user under CLI). GetTokenInformation(TokenUser) via
// GetCurrentProcessToken: no handle to close (pseudo-token).
func currentUserSID() (*windows.SID, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("GetTokenUser: %w", err)
	}
	return tu.User.Sid, nil
}

// readDACL reads the security descriptor for path and returns the DACL plus
// the SECURITY_DESCRIPTOR. Renamed for production residency (Plan 38);
// behavior unchanged — its callers remain the tests (InspectFileACL performs
// its own GetNamedSecurityInfo including OWNER_SECURITY_INFORMATION).
func readDACL(path string) (dacl *windows.ACL, sd *windows.SECURITY_DESCRIPTOR, err error) {
	sd, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, nil, fmt.Errorf("GetNamedSecurityInfo: %w", err)
	}
	dacl, _, err = sd.DACL()
	if err != nil {
		return nil, sd, fmt.Errorf("DACL: %w", err)
	}
	return dacl, sd, nil
}

// aclDangerousBits is the signal-3 dangerous mask (spec §1.2): read data, or
// the ability to rewrite the DACL / take ownership (self-elevation then read).
// Read-back masks always carry expanded specific rights (measured 2026-08-26:
// a stored GENERIC_ALL comes back as 0x00120089), so GENERIC bits never appear.
const aclDangerousBits = windows.FILE_READ_DATA | windows.WRITE_DAC | windows.WRITE_OWNER

// accessAllowedCallbackAceType is ACCESS_ALLOWED_CALLBACK_ACE_TYPE (winnt.h;
// not exported by x/sys/windows v0.47.0) — a conditional-allow ACE that grants
// its Mask only when the embedded application data (an SDDL conditional
// expression, e.g. via the Authz API) evaluates true. It shares the exact
// ACCESS_ALLOWED_ACE prefix layout Header|Mask|SidStart — the condition blob
// sits AFTER the complete SID (MS-DTYP §2.4.4.6), so a *ACCESS_ALLOWED_ACE
// cast reads its Mask and SID correctly (Plan 38 final review, 2026-08-26).
const accessAllowedCallbackAceType = 0x9

// isWalkedAllowAceType reports whether aceType is a granting ACE type that
// InspectFileACL's grantor walk can soundly read through an
// ACCESS_ALLOWED_ACE cast — i.e. it grants access AND its SID starts
// immediately after Header|Mask.
//
// NOT walked, deliberately:
//   - deny/audit/alarm types tighten rather than expose (spec fact #9);
//   - object-form GRANTING types — ACCESS_ALLOWED_OBJECT_ACE_TYPE (0x5),
//     ACCESS_ALLOWED_CALLBACK_OBJECT_ACE_TYPE (0xB) — insert Flags and up to
//     two GUIDs between Mask and SidStart, so the cast would read garbage;
//   - the obsolete ACCESS_ALLOWED_COMPOUND_ACE_TYPE (0x4) likewise offsets
//     the SID past a compound-ace field.
//
// The object/compound forms are a known not-walked granting exposure,
// registered as a spec §7 residual.
func isWalkedAllowAceType(aceType uint8) bool {
	return aceType == windows.ACCESS_ALLOWED_ACE_TYPE || aceType == accessAllowedCallbackAceType
}

// InspectFileACL reads the file's security descriptor back and reports whether
// its protection matches the hardened shape: whitelist = SYSTEM,
// BUILTIN\Administrators, and the current process user (exactly the three
// HardenACL grants). Only "looser than hardened" is reported — deny ACEs,
// INHERIT_ONLY ACEs, absent whitelist principals (over-tight) and whitelist
// principals holding extra rights are deliberately NOT flagged (spec §1.2
// asymmetry). Read-only: no SD is written.
func InspectFileACL(path string) (FileACLReport, error) {
	rep := FileACLReport{Supported: true}

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: %w", err)
	}

	whitelist, err := aclWhitelist()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: %w", err)
	}

	// Signal 4 — owner (conservative: OWNER_RIGHTS ACEs could limit the
	// owner's implicit WRITE_DAC, but we do not model that; anomaly advice).
	owner, _, err := sd.Owner()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: owner: %w", err)
	}
	rep.OwnerSID = owner.String()
	rep.OwnerUnexpected = true
	for _, w := range whitelist {
		if w.Equals(owner) {
			rep.OwnerUnexpected = false
			break
		}
	}

	// Signal 1 — DACL presence. The present bit is read from Control()
	// (SECURITY_DESCRIPTOR.DACL()'s second return measured ambiguous on
	// 2026-08-26; Control is authoritative).
	ctrl, _, err := sd.Control()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: control: %w", err)
	}
	if ctrl&windows.SE_DACL_PRESENT == 0 {
		rep.DaclNull = true
		return rep, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: dacl: %w", err)
	}
	if dacl == nil {
		rep.DaclNull = true
		return rep, nil
	}
	rep.Protected = ctrl&windows.SE_DACL_PROTECTED != 0

	// Signal 3 — grantor walk.
	seen := map[string]bool{}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			continue // unreadable ACE: skip, the structural signals above stand
		}
		if !isWalkedAllowAceType(ace.Header.AceType) {
			continue // deny/audit/alarm ACEs are tightening, not exposure (spec
			// fact #9); object-form allow ACEs are granting but not walked
			// here — different SID offset, see isWalkedAllowAceType + §7.
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies to children only, not to this file (spec §1.2)
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		whitelisted := false
		for _, w := range whitelist {
			if w.Equals(aceSID) {
				whitelisted = true
				break
			}
		}
		if whitelisted || ace.Mask&aclDangerousBits == 0 {
			continue
		}
		s := aceSID.String()
		if !seen[s] {
			seen[s] = true
			rep.UnexpectedReadGrantors = append(rep.UnexpectedReadGrantors, s)
		}
	}
	sort.Strings(rep.UnexpectedReadGrantors) // deterministic rendering
	return rep, nil
}

// aclWhitelist builds the trusted-principal set: the exact three HardenACL
// grants (well-known SIDs + the current process token user). It mirrors
// HardenACL's entries build — change both together.
func aclWhitelist() ([]*windows.SID, error) {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("build SYSTEM sid: %w", err)
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("build Administrators sid: %w", err)
	}
	userSID, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("resolve current user sid: %w", err)
	}
	return []*windows.SID{systemSID, adminsSID, userSID}, nil
}

// isDaclProtected reports whether the security descriptor has SE_DACL_PROTECTED
// set (inheritance from parent disabled). Production reader since Plan 38 —
// promoted out of the former test-helper block, behavior unchanged.
func isDaclProtected(sd *windows.SECURITY_DESCRIPTOR) bool {
	ctrl, _, err := sd.Control()
	if err != nil {
		return false
	}
	return ctrl&windows.SE_DACL_PROTECTED != 0
}

// trusteeInACL reports whether any ACE in dacl applies to sid. It walks the
// ACL via GetAce (advapi32) and compares each ACE's SID with SID.Equals —
// locale-independent and exact (LookupAccountName would localize names).
// Production reader since Plan 38 — promoted out of the former test-helper
// block, behavior unchanged.
func trusteeInACL(dacl *windows.ACL, sid *windows.SID) bool {
	if dacl == nil || sid == nil {
		return false
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			continue
		}
		// ACCESS_ALLOWED_ACE.SidStart is the first uint32 of the variable-length
		// SID that follows the ACE_HEADER + Mask. The SID starts immediately
		// after Mask in the on-disk ACE layout.
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if aceSID.Equals(sid) {
			return true
		}
	}
	return false
}
