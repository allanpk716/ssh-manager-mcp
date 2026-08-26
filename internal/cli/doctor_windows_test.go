//go:build windows

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"ssh-manager-mcp/internal/roles"
)

// seedBroadReadACE plants an explicit Everyone-allow read ACE on path
// (replacing its DACL, PROTECTED) — the signal-3 WARN trigger, through the
// REAL InspectFileACL (no seam).
func seedBroadReadACE(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("build DACL: %v", err)
	}
	si := windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, nil, nil, dacl, nil); err != nil {
		t.Fatalf("seed DACL: %v", err)
	}
}

// TestDoctorVaultKeyWindowsRealPath drives checkVaultKey against the REAL
// on-disk ACL (no seam): hardened seed → PASS; a planted broad read ACE →
// WARN with the frozen clause and unchanged exit code.
func TestDoctorVaultKeyWindowsRealPath(t *testing.T) {
	stubServeServiceState(t, "Running")
	vd, _ := withDoctorDirs(t)
	seedDoctorVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	// Hardened seed → PASS (proves seedDoctorVault's Set-based seeding).
	out, err := driveDoctor(t)
	if err != nil {
		t.Fatalf("hardened seed must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "masterkey:  PASS") || !strings.Contains(out, "overall: 0 WARN, 0 FAIL") {
		t.Fatalf("hardened seed must be fully quiet:\n%s", out)
	}

	// Planted broad ACE → WARN through the real InspectFileACL.
	seedBroadReadACE(t, filepath.Join(vd, "master.key.plain"))
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("loose-ACL WARN must not change the exit code: %v\n%s", err, out)
	}
	for _, want := range []string{
		"masterkey:  WARN",
		"grants access to unexpected principals: S-1-1-0",
		"overall: 1 WARN, 0 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
