package cli

import (
	"bytes"
	"database/sql"
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

	// Plan 39: the code binds to a profile — create one first, then add.
	mustCli("profiles", "add", "team-a")
	addOut := mustCli("cache-tokens", "add", "--name", "laptop", "--profile", "team-a")
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

	mustCli("profiles", "add", "team-a")
	out := mustCli("cache-tokens", "add", "--name", "laptop", "--profile", "team-a")
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

	// Plan 39: a profile must exist for the add to reach the cert step.
	if err := func() error {
		r := NewRootCmd()
		r.SetOut(&bytes.Buffer{})
		r.SetArgs([]string{"profiles", "add", "team-a"})
		return r.Execute()
	}(); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"cache-tokens", "add", "--name", "laptop", "--profile", "team-a"})
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

// TestCacheTokensAdd_ProfileRequired (Plan 39): add without --profile is
// rejected up front — an unbound code would be refused at its first pull (403),
// so minting one is always a misstep the CLI should block.
func TestCacheTokensAdd_ProfileRequired(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_SERVE_CERT":    filepath.Join(dir, "serve-cert.pem"),
		"SSHMGR_SERVE_KEY":     filepath.Join(dir, "serve-key.pem"),
	})
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"cache-tokens", "add", "--name", "laptop"})
	if err := root.Execute(); err == nil {
		t.Fatal("add without --profile must error")
	}
	// Unknown profile is equally loud.
	root2 := NewRootCmd()
	root2.SetOut(&bytes.Buffer{})
	root2.SetArgs([]string{"cache-tokens", "add", "--name", "laptop", "--profile", "ghost"})
	if err := root2.Execute(); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("add with unknown profile must error naming it: %v", err)
	}
}

// TestCacheTokensBind (Plan 39): the legacy repair path — an unbound row (the
// pre-Plan-39 migration state) is bound in place; ls then shows the profile;
// unknown names/profiles error.
func TestCacheTokensBind(t *testing.T) {
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
	// Pre-create the store in its PRE-Plan-39 shape: one profile + one
	// UNBOUND cache_tokens row (exactly what a legacy fleet migrates into —
	// the state this command exists to repair).
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE profiles (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE cache_tokens (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, token_hash BLOB NOT NULL, token_salt BLOB NOT NULL, token_prefix TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', last_pull_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO profiles VALUES ('p1','team-a',1,1)`,
		`INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,last_pull_at,created_at,updated_at) VALUES ('ct1','laptop-legacy',x'00',x'00','legacyXXX','active',NULL,1,1)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	// ls renders unbound as profile=-
	lsOut := mustCli("cache-tokens", "ls")
	if !strings.Contains(lsOut.String(), "profile=-") {
		t.Fatalf("unbound code must render profile=-, got: %s", lsOut.String())
	}
	// bind repairs it in place.
	out := mustCli("cache-tokens", "bind", "laptop-legacy", "team-a")
	if !strings.Contains(out.String(), "laptop-legacy") || !strings.Contains(out.String(), "team-a") {
		t.Fatalf("bind output must name device + profile: %s", out.String())
	}
	lsOut2 := mustCli("cache-tokens", "ls")
	if !strings.Contains(lsOut2.String(), "profile=team-a") {
		t.Fatalf("bound code must render its profile, got: %s", lsOut2.String())
	}
	// unknown device / unknown profile both error
	for _, args := range [][]string{
		{"cache-tokens", "bind", "ghost", "team-a"},
		{"cache-tokens", "bind", "laptop-legacy", "ghost"},
	} {
		root := NewRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("bind %v must error", args)
		}
	}
}
