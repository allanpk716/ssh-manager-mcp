package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// Shared CLI test helpers. Formerly serve_bind_test.go — the serve bind tests
// retired with the command itself (Plan 42 批1 T1: the tunnel-whitelist CLI
// only served the removed ②a MCP-over-HTTP surface); the helpers stay because
// tunnels_test.go and the rest of the package drive the CLI through them.

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
	// precedent): none of these commands touch certs, but a typo'd
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
