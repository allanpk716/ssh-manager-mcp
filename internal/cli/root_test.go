package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionCmdPrintsVersionVariable verifies that `ssh-manager version`
// prints the cli.Version package variable — so an ldflags -X override at
// release time is exactly what the user sees. Uses the same SetOut capture
// pattern as cli_smoke_test.go (which is why versionCmd must write to
// cmd.OutOrStdout(), not fmt.Println).
func TestVersionCmdPrintsVersionVariable(t *testing.T) {
	saved := Version
	Version = "9.9.9-test"
	t.Cleanup(func() { Version = saved })

	root := NewRootCmd()
	root.SetArgs([]string{"version"})
	out := &bytes.Buffer{}
	root.SetOut(out)

	if err := root.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "9.9.9-test" {
		t.Fatalf("version output = %q, want %q", got, "9.9.9-test")
	}
}
