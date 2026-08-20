package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// driveProjects runs the CLI against the pinned vault dirs (mirror of clear's
// driveClear — the dev machine REALLY runs ssh-manager, so every path the
// command touches must come from withClearDirs' env seams).
func driveProjects(args ...string) error {
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// seedProjectsVault creates an openable vault holding profile "dev" with two
// active projects, "agent" and "agent2". Mirror of seedClearVault, plus the
// seeded rows; the CLI under test does every status transition from here.
func seedProjectsVault(t *testing.T, vaultDir string) {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.AddProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddProject("agent", pid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddProject("agent2", pid); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(vaultDir, "master.key.plain"), mk, 0o600); err != nil {
		t.Fatal(err)
	}
}

// v0.8.8: revoked is terminal at the CLI surface too — `projects enable` and
// `projects disable` on a revoked row must surface the store's refusal (enable
// resurrected a revoked token; disable→enable was a two-step bypass), while
// the reversible disable→enable pair on a live project keeps working.
func TestProjectsStatusRevokedTerminal(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedProjectsVault(t, vd)

	// revoke through the CLI under test — its own happy path
	if err := driveProjects("projects", "revoke", "agent"); err != nil {
		t.Fatalf("revoke (happy path) failed: %v", err)
	}

	// revoked → enable refused with the hint (the original gap)
	if err := driveProjects("projects", "enable", "agent"); err == nil || !strings.Contains(err.Error(), "不可逆") {
		t.Fatalf("enable on revoked must be refused with a hint, got %v", err)
	}
	// revoked → disable refused too (the two-step resurrect bypass)
	if err := driveProjects("projects", "disable", "agent"); err == nil {
		t.Fatal("disable on revoked must be refused (two-step resurrect bypass)")
	}

	// the reversible pair on a live project keeps working end-to-end
	if err := driveProjects("projects", "disable", "agent2"); err != nil {
		t.Fatalf("disable (happy path) failed: %v", err)
	}
	if err := driveProjects("projects", "enable", "agent2"); err != nil {
		t.Fatalf("enable on disabled (happy path) failed: %v", err)
	}
}

// v0.8.9: `projects rotate` on a revoked row must surface the store's refusal
// — the success path prints a one-time token, so the guard is what keeps a
// dead credential from being handed out looking fresh.
func TestProjectsRotateRevokedRefused(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedProjectsVault(t, vd)

	if err := driveProjects("projects", "revoke", "agent"); err != nil {
		t.Fatalf("revoke (happy path) failed: %v", err)
	}
	if err := driveProjects("projects", "rotate", "agent"); err == nil || !strings.Contains(err.Error(), "不可逆") {
		t.Fatalf("rotate on revoked must be refused with a hint, got %v", err)
	}
}
