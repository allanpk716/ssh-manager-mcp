package cli

// Audit-trail assertions for the cache-tokens owner mutations (add / revoke /
// bind): every successful command leaves one audit row carrying exactly the
// whitelisted summary fields (device name, profile name — never the one-time
// code), and every failure after the store is open leaves a Status=error row.
// ExitCode/DurationMS stay at the zero values every owner audit row uses.

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// cacheTokenAuditEnv wires an isolated vault (store + master key) and
// serve-cert seams into the environment, and returns a CLI runner that fails
// the test on any command error, plus the store path and master key so the
// audit table can be re-read after the commands have closed their stores.
func cacheTokenAuditEnv(t *testing.T) (run func(args ...string) *bytes.Buffer, storePath string, mk []byte) {
	t.Helper()
	dir := t.TempDir()
	mk, _ = store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		// `cache-tokens add` loads the serve cert for the fingerprint; keep
		// that away from the developer's real vault dir.
		"SSHMGR_SERVE_CERT": filepath.Join(dir, "serve-cert.pem"),
		"SSHMGR_SERVE_KEY":  filepath.Join(dir, "serve-key.pem"),
	})
	run = func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		return out
	}
	return run, filepath.Join(dir, "test.db"), mk
}

// mustCacheTokenAuditFail runs a CLI invocation that must fail and returns the
// error (so the caller proves the failure happened without killing the test).
func mustCacheTokenAuditFail(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		t.Fatalf("cli %v must fail", args)
	}
	return err
}

// cacheTokenAuditRows reopens the vault and returns the owner audit rows for
// the given actions (newest first; no action names = all owner rows).
func cacheTokenAuditRows(t *testing.T, storePath string, mk []byte, actions ...string) []store.AuditRow {
	t.Helper()
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: actions})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

// assertCacheTokenAuditRow pins one audit row's owner shape: the action, the
// status, the exact whitelisted summary (nothing beyond it), no project/server
// attachment, and the ExitCode/DurationMS zero values owner rows use.
func assertCacheTokenAuditRow(t *testing.T, r store.AuditRow, wantAction, wantStatus, wantCommand string) {
	t.Helper()
	if r.Action != wantAction {
		t.Fatalf("action = %q, want %q", r.Action, wantAction)
	}
	if r.Status != wantStatus {
		t.Fatalf("%s: status = %q, want %q", r.Action, r.Status, wantStatus)
	}
	if r.ProjectID != "" || r.ServerID != "" || r.Sudo {
		t.Fatalf("%s: owner row must carry no project/server/sudo, got %+v", r.Action, r)
	}
	if r.Command != wantCommand {
		t.Fatalf("%s: command summary = %s, want %s (whitelist: no extra fields)", r.Action, r.Command, wantCommand)
	}
	if r.ExitCode != 0 || r.DurationMS != 0 {
		t.Fatalf("%s: owner rows keep ExitCode/DurationMS at zero (existing AuditRow convention), got %d/%d", r.Action, r.ExitCode, r.DurationMS)
	}
}

// TestCacheTokensAudit_AddRevokeRows: a successful add and revoke each leave
// exactly one owner audit row with the whitelisted summary, and the one-time
// code printed to stdout never reaches any audit row.
func TestCacheTokensAudit_AddRevokeRows(t *testing.T) {
	run, storePath, mk := cacheTokenAuditEnv(t)
	run("profiles", "add", "team-a")
	addOut := run("cache-tokens", "add", "--name", "laptop", "--profile", "team-a")
	run("cache-tokens", "revoke", "laptop")

	rows := cacheTokenAuditRows(t, storePath, mk, "cache-token.add", "cache-token.revoke")
	if len(rows) != 2 {
		t.Fatalf("want exactly one add row + one revoke row, got %d: %+v", len(rows), rows)
	}
	// Newest first: revoke ran last.
	assertCacheTokenAuditRow(t, rows[0], "cache-token.revoke", "ok", `{"name":"laptop"}`)
	assertCacheTokenAuditRow(t, rows[1], "cache-token.add", "ok", `{"name":"laptop","profile":"team-a"}`)

	// Zero sensitive plaintext: extract the one-time code from the add output
	// (same technique as TestCacheTokens_AddLsRevoke) and require it to be
	// absent from EVERY audit row in the vault.
	addLines := strings.Split(strings.TrimSpace(addOut.String()), "\n")
	code := strings.TrimSpace(strings.Split(addLines[len(addLines)-1], "--token ")[1])
	if code == "" {
		t.Fatalf("could not extract the one-time code from add output: %s", addOut.String())
	}
	for _, r := range cacheTokenAuditRows(t, storePath, mk) {
		if strings.Contains(r.Command, code) {
			t.Fatalf("audit row %s leaked the one-time code: %s", r.Action, r.Command)
		}
	}
}

// TestCacheTokensAudit_BindRow: a successful bind leaves one owner audit row
// naming the device and the profile it was bound to.
func TestCacheTokensAudit_BindRow(t *testing.T) {
	run, storePath, mk := cacheTokenAuditEnv(t)
	run("profiles", "add", "team-a")
	run("profiles", "add", "team-b")
	run("cache-tokens", "add", "--name", "desk", "--profile", "team-a")
	run("cache-tokens", "bind", "desk", "team-b")

	rows := cacheTokenAuditRows(t, storePath, mk, "cache-token.bind")
	if len(rows) != 1 {
		t.Fatalf("want exactly one bind audit row, got %d: %+v", len(rows), rows)
	}
	assertCacheTokenAuditRow(t, rows[0], "cache-token.bind", "ok", `{"name":"desk","profile":"team-b"}`)
}

// TestCacheTokensAudit_ErrorRows: commands that fail after the store is open
// still leave an audit row, Status=error, summary filled with whatever
// whitelist fields the command line carried.
func TestCacheTokensAudit_ErrorRows(t *testing.T) {
	run, storePath, mk := cacheTokenAuditEnv(t)
	run("profiles", "add", "team-a")
	mustCacheTokenAuditFail(t, "cache-tokens", "add", "--name", "laptop", "--profile", "ghost")
	mustCacheTokenAuditFail(t, "cache-tokens", "revoke", "nope")
	mustCacheTokenAuditFail(t, "cache-tokens", "bind", "ghost", "team-a")
	mustCacheTokenAuditFail(t, "cache-tokens", "bind", "also-ghost", "ghost")

	rows := cacheTokenAuditRows(t, storePath, mk, "cache-token.add", "cache-token.revoke", "cache-token.bind")
	if len(rows) != 4 {
		t.Fatalf("want exactly four error audit rows, got %d: %+v", len(rows), rows)
	}
	// Newest first, i.e. reverse execution order.
	assertCacheTokenAuditRow(t, rows[0], "cache-token.bind", "error", `{"name":"also-ghost","profile":"ghost"}`)
	assertCacheTokenAuditRow(t, rows[1], "cache-token.bind", "error", `{"name":"ghost","profile":"team-a"}`)
	assertCacheTokenAuditRow(t, rows[2], "cache-token.revoke", "error", `{"name":"nope"}`)
	assertCacheTokenAuditRow(t, rows[3], "cache-token.add", "error", `{"name":"laptop","profile":"ghost"}`)
}
