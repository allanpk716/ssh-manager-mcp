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
		// Keep LoadOrCreateServeCert (called by `cache-tokens add` for the
		// fingerprint) from touching the developer's real vault dir.
		"SSHMGR_SERVE_CERT": filepath.Join(dir, "serve-cert.pem"),
		"SSHMGR_SERVE_KEY":  filepath.Join(dir, "serve-key.pem"),
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

	// Core safety: ls must NEVER emit the one-time code (only the prefix).
	// Extract the code from the add output (it trails `--token ` on the last line)
	// and assert it does not appear in ls output.
	addLines := strings.Split(strings.TrimSpace(addOut.String()), "\n")
	code := strings.TrimSpace(strings.Split(addLines[len(addLines)-1], "--token ")[1])
	if strings.Contains(lsOut.String(), code) {
		t.Fatalf("ls leaked the one-time code: %s", lsOut.String())
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

// TestCacheTokensAdd_EmitsFingerprint asserts `cache-tokens add` prints the
// server's SPKI fingerprint next to the one-time code, plus a cache-pull
// invocation showing how to pass the pin (--pin or SSHMGR_SERVE_PIN).
func TestCacheTokensAdd_EmitsFingerprint(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_SERVE_CERT":    filepath.Join(dir, "serve-cert.pem"),
		"SSHMGR_SERVE_KEY":     filepath.Join(dir, "serve-key.pem"),
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

	out := mustCli("cache-tokens", "add", "--name", "laptop")
	s := out.String()
	if !strings.Contains(s, "Authorization code") {
		t.Fatalf("missing code line: %s", s)
	}
	if !strings.Contains(s, "sha256:") {
		t.Fatalf("missing server fingerprint in output: %s", s)
	}
	if !strings.Contains(s, "--pin") && !strings.Contains(s, "SSHMGR_SERVE_PIN") {
		t.Fatalf("output should show how to pass the pin: %s", s)
	}
}
