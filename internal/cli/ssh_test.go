package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestOwnerSSHNoCommandErrors pins the arg contract: the owner ssh path is a
// SINGLE non-interactive command — host-only, empty-string, and whitespace-only
// command args all fail fast BEFORE any connection or audit row.
func TestOwnerSSHNoCommandErrors(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	cases := []struct {
		name string
		args []string
	}{
		{"host only", []string{"ssh", "t"}},
		{"empty string cmd", []string{"ssh", "t", ""}},
		{"whitespace cmd", []string{"ssh", "t", "   "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(c.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("args %v: expected error, got nil", c.args)
			}
			if !strings.Contains(err.Error(), "no command given") {
				t.Fatalf("args %v: error %q missing 'no command given'", c.args, err)
			}
		})
	}
}
