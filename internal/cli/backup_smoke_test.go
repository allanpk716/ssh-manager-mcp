package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestBackupCmd_RegisteredWithFlags is the T7 smoke test: confirm the backup
// command tree (create + verify) is wired into root and that create exposes
// its required flags. Mirrors the root.Find pattern in serve_smoke_test.go.
// This guards against regressions where newBackupCmd is dropped from root.go's
// AddCommand list (e.g. during a refactor).
func TestBackupCmd_RegisteredWithFlags(t *testing.T) {
	root := newRootForTest(t)
	backup, _, err := root.Find([]string{"backup"})
	if err != nil {
		t.Fatal("backup subcommand not registered:", err)
	}
	for _, sub := range []string{"create", "verify"} {
		if _, _, e := backup.Find([]string{sub}); e != nil {
			t.Errorf("backup missing %q subcommand: %v", sub, e)
		}
	}
	// create flags required by spec §3.x
	create, _, err := backup.Find([]string{"create"})
	if err != nil {
		t.Fatal("backup create missing:", err)
	}
	for _, flag := range []string{"dir", "keep", "prefix"} {
		if create.Flags().Lookup(flag) == nil {
			t.Errorf("backup create missing --%s flag", flag)
		}
	}
	// --dir must be marked required (cobra stores this as a flag annotation).
	if fl := create.Flags().Lookup("dir"); fl != nil {
		if v, ok := fl.Annotations[cobra.BashCompOneRequiredFlag]; !ok || len(v) == 0 || v[0] != "true" {
			t.Errorf("backup create --dir should be marked required")
		}
	}
}
