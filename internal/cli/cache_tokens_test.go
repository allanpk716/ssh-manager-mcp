package cli

import (
	"bytes"
	"encoding/hex"
	"os"
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

// TestCacheTokensAddCertFailZeroOrphans pins the cert-first ordering in
// `cache-tokens add` (cache_tokens.go: "cert first: a failing cert load must
// not mint an orphan device code"): with a corrupt PEM at the serve-cert seam,
// the add must ERROR and leave the cache_tokens table EMPTY — no token row may
// be minted alongside enrollment instructions that could never carry a
// trustworthy fingerprint. LoadOrCreateServeCert's state-1 path (cert present →
// parse) refuses to regenerate, so a corrupt file stays an error, never a
// silent fresh cert.
func TestCacheTokensAddCertFailZeroOrphans(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "test.db")
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         storePath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		// All three serve seams pinned into the tempdir: cert+key hold corrupt
		// PEMs, and the marker is isolated away from any real vault dir even
		// though the present-cert path never consults it.
		"SSHMGR_SERVE_CERT":   filepath.Join(dir, "serve-cert.pem"),
		"SSHMGR_SERVE_KEY":    filepath.Join(dir, "serve-key.pem"),
		"SSHMGR_SERVE_MARKER": filepath.Join(dir, "serve-cert.init"),
	})
	if err := os.WriteFile(filepath.Join(dir, "serve-cert.pem"),
		[]byte("-----BEGIN CERTIFICATE-----\ngarbage-not-a-cert\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve-key.pem"),
		[]byte("-----BEGIN PRIVATE KEY-----\ngarbage-not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"cache-tokens", "add", "--name", "laptop"})
	err := root.Execute()
	if err == nil {
		t.Fatal("cache-tokens add must error when the serve cert is corrupt")
	}
	for _, want := range []string{"load serve cert", "corrupt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}

	// Zero orphans: reopen the SAME store and count every cache_tokens row
	// (ListCacheTokens is a full-table SELECT, all statuses).
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	tokens, err := st.ListCacheTokens()
	if err != nil {
		t.Fatalf("list cache tokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("cache_tokens must hold ZERO rows after a failed add (no orphan device codes), got %d: %+v", len(tokens), tokens)
	}
}
