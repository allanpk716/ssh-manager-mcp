package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// withCliStoreEnv pins SSHMGR_STORE + SSHMGR_MASTERKEY_HEX at a fresh temp
// vault — the env tier openUnlockedStore resolves through (vault.OpenStore →
// storePath/resolveMasterKey) — and returns the path + master key so tests can
// ALSO open a direct store handle for seeding registry rows / reading orders.
// Same injection pattern as cache_tokens_test.go.
func withCliStoreEnv(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	path := filepath.Join(dir, "test.db")
	// Serve-cert seams pinned into the tempdir too (cache_tokens_test.go
	// precedent): none of the bind/tunnels commands touch certs, but a typo'd
	// `serve`-subcommand test falls back to serve's RunE (cobra legacy args
	// fallback), which auto-loads/generates the serve cert — never in the
	// developer's real vault dir.
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         path,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_SERVE_CERT":    filepath.Join(dir, "serve-cert.pem"),
		"SSHMGR_SERVE_KEY":     filepath.Join(dir, "serve-key.pem"),
		"SSHMGR_SERVE_MARKER":  filepath.Join(dir, "serve-cert.init"),
	})
	return path, mk
}

// runCli drives the root command with args, requires success, returns stdout.
func runCli(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("cli %v: %v", args, err)
	}
	return out.String()
}

// runCliErr drives the root command EXPECTING failure and returns the error
// surface (cobra's "Error: …" stderr print + the returned error). House
// convention (cache_tokens_test.go): a refusal is asserted on Execute's error,
// not on stdout text.
func runCliErr(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		t.Fatalf("cli %v: expected an error, got success:\n%s", args, out.String())
	}
	return errOut.String() + "\n" + err.Error()
}

func TestServeBindCmd(t *testing.T) {
	withCliStoreEnv(t)

	// ls on a virgin vault: the empty-whitelist note
	ls := runCli(t, "serve", "bind", "ls")
	if !strings.Contains(ls, "only loopback binds allowed") {
		t.Fatalf("empty ls must carry the loopback-only note: %s", ls)
	}

	// 1. add rejects invalid values with the store's reason
	//    (loopback / wildcard / hostname — spec §2 owner-CLI gate).
	for _, tc := range []struct{ bad, want string }{
		{"127.0.0.1", "loopback"},
		{"0.0.0.0", "wildcard"},
		{"example.com", "not an IP literal"},
	} {
		out := runCliErr(t, "serve", "bind", "add", tc.bad)
		if !strings.Contains(out, tc.want) {
			t.Fatalf("add %q must be rejected (%q), got: %s", tc.bad, tc.want, out)
		}
	}

	// 2. add success + idempotent re-add + ls lists it
	out := runCli(t, "serve", "bind", "add", "192.168.50.10")
	if !strings.Contains(out, "approved 192.168.50.10") {
		t.Fatalf("add must print approved: %s", out)
	}
	runCli(t, "serve", "bind", "add", "192.168.50.10") // idempotent — must NOT error
	if ls = runCli(t, "serve", "bind", "ls"); !strings.Contains(ls, "192.168.50.10") {
		t.Fatalf("ls must list entry: %s", ls)
	}

	// 3. rm by an equivalent (non-canonical) text form hits the canonical row
	//    (store canonicalizes via net.IP.String(); the CLI echoes the argument
	//    as typed) + the shrink note.
	out = runCli(t, "serve", "bind", "rm", "::ffff:192.168.50.10")
	if !strings.Contains(out, "revoked") || !strings.Contains(out, "~15s") {
		t.Fatalf("rm must print the revoked + shrink note: %s", out)
	}
	if ls = runCli(t, "serve", "bind", "ls"); strings.Contains(ls, "192.168.50.10") {
		t.Fatalf("entry must be gone: %s", ls)
	}

	// rm of an absent entry: soft "no whitelist entry", not an error
	out = runCli(t, "serve", "bind", "rm", "192.168.50.10")
	if !strings.Contains(out, "no whitelist entry for 192.168.50.10") {
		t.Fatalf("rm absent must print no-entry note: %s", out)
	}
}
