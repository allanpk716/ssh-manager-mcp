package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestCacheTokens_AddLsRevoke(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	mustCli := func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		return out
	}

	// add prints the one-time code
	addOut := mustCli("cache-tokens", "add", "--name", "laptop")
	if !strings.Contains(addOut.String(), "Authorization code") || !strings.Contains(addOut.String(), "laptop") {
		t.Fatalf("add output missing code/name: %s", addOut.String())
	}

	// ls shows it (prefix only, never the full code)
	lsOut := mustCli("cache-tokens", "ls")
	if !strings.Contains(lsOut.String(), "laptop") || !strings.Contains(lsOut.String(), "active") {
		t.Fatalf("ls missing laptop/active: %s", lsOut.String())
	}

	// revoke → ls shows revoked
	mustCli("cache-tokens", "revoke", "laptop")
	lsOut2 := mustCli("cache-tokens", "ls")
	if !strings.Contains(lsOut2.String(), "revoked") {
		t.Fatalf("ls after revoke missing revoked: %s", lsOut2.String())
	}

	// revoke of unknown errors
	root := NewRootCmd()
	root.SetArgs([]string{"cache-tokens", "revoke", "nope"})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("revoke unknown must error")
	}
}
