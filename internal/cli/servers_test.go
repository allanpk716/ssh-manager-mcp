package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestServersEditNeedsPassphraseTag (final review I-2): the needs-passphrase
// tag the import flows write means "current credential lacks its passphrase" —
// any successful CLI re-credential (--password OR --key) must drop it; a
// field-only edit must keep it (the ⚠ stays until a real re-credential).
// Ordering: an explicit --tags list passed alongside a re-credential flag is
// authoritative as written, but the strip still applies to whatever srv.Tags
// holds then — after re-credential the marker would be false.
func TestServersEditNeedsPassphraseTag(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		// never the developer machine's real master.key (newVaultEnv pattern)
		"SSHMGR_FILEKEY_PATH": filepath.Join(dir, "no-such-master.key"),
	})

	run := func(args ...string) {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, out.String())
		}
	}
	inspect := func(name string) []string {
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName(name)
		if srv == nil {
			t.Fatalf("server %q not found", name)
		}
		return srv.Tags
	}

	// two tagged servers (password- and key-credentialed re-credential targets)
	run("servers", "add", "--name", "enc", "--host", "h", "--user", "u",
		"--password", "pw", "--tags", "needs-passphrase,gpu")
	run("servers", "add", "--name", "enc2", "--host", "h", "--user", "u",
		"--password", "pw", "--tags", "needs-passphrase")

	// field-only edit: tag kept
	run("servers", "edit", "enc", "--role", "prod ml")
	if tags := inspect("enc"); len(tags) != 2 || tags[0] != "needs-passphrase" || tags[1] != "gpu" {
		t.Fatalf("edit --role only must keep the tag: %v", tags)
	}

	// re-credential via --password: tag dropped, other tags kept
	run("servers", "edit", "enc", "--password", "newpw")
	if tags := inspect("enc"); len(tags) != 1 || tags[0] != "gpu" || contains(tags, "needs-passphrase") {
		t.Fatalf("edit --password must drop needs-passphrase and keep gpu: %v", tags)
	}

	// re-credential via --key: same strip (shared pwSet||keySet branch)
	keyPath := filepath.Join(dir, "plain_key")
	genKeyFile(t, keyPath, "")
	run("servers", "edit", "enc2", "--key", keyPath)
	if tags := inspect("enc2"); len(tags) != 0 || contains(tags, "needs-passphrase") {
		t.Fatalf("edit --key must drop the tag (empty stays empty): %v", tags)
	}

	// explicit --tags + re-credential: strip applies to whatever --tags wrote
	run("servers", "edit", "enc", "--tags", "gpu,needs-passphrase", "--password", "pw2")
	if tags := inspect("enc"); contains(tags, "needs-passphrase") || len(tags) != 1 || tags[0] != "gpu" {
		t.Fatalf("re-credential must strip the tag even from an explicit --tags list: %v", tags)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestServersEditRejectsEmptyCredentialValues (Plan 22 T2): an explicitly
// passed empty/whitespace credential flag is a quoting artifact, not a
// "clear my credential" request — minting an empty-secret row would brick
// exec with a confusing auth failure. Changed() + TrimSpace(value)=="" must
// error (password/key point at --clear-credential, the only clear path) and
// leave the vault's credential rows and the server's bindings untouched.
func TestServersEditRejectsEmptyCredentialValues(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		// never the developer machine's real master.key (newVaultEnv pattern)
		"SSHMGR_FILEKEY_PATH": filepath.Join(dir, "no-such-master.key"),
	})

	run := func(args ...string) error {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs(args)
		return root.Execute()
	}
	credCount := func(t testing.TB) int {
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		snap, err := st.ExportSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		return len(snap.Credentials)
	}
	inspect := func(t testing.TB) *models.Server {
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName("enc")
		if srv == nil {
			t.Fatal("server \"enc\" not found")
		}
		return srv
	}

	// one password+sudo server → exactly 2 credential rows to keep
	if err := run("servers", "add", "--name", "enc", "--host", "h", "--user", "u",
		"--password", "pw", "--sudo-password", "spw"); err != nil {
		t.Fatalf("add: %v", err)
	}
	before := inspect(t)
	oldCred, oldSudo := before.CredentialID, before.SudoCredentialID
	if oldCred == "" || oldSudo == "" {
		t.Fatalf("baseline server must carry both credentials: %+v", before)
	}
	if n := credCount(t); n != 2 {
		t.Fatalf("baseline credential rows = %d, want 2", n)
	}

	cases := []struct {
		name    string
		args    []string
		wantSub []string
	}{
		{"password empty", []string{"--password", ""}, []string{"--password 不能为空", "清除凭据请用 --clear-credential"}},
		{"key empty", []string{"--key", ""}, []string{"--key 不能为空", "清除凭据请用 --clear-credential"}},
		{"sudo-password empty", []string{"--sudo-password", ""}, []string{"--sudo-password 不能为空"}},
		{"password whitespace", []string{"--password", "   "}, []string{"--password 不能为空", "清除凭据请用 --clear-credential"}},
		{"key whitespace", []string{"--key", " \t "}, []string{"--key 不能为空", "清除凭据请用 --clear-credential"}},
		{"sudo-password whitespace", []string{"--sudo-password", "  "}, []string{"--sudo-password 不能为空"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(append([]string{"servers", "edit", "enc"}, tc.args...)...)
			if err == nil {
				t.Fatalf("edit %v must fail, got success", tc.args)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("error must mention %q, got: %v", sub, err)
				}
			}
			if n := credCount(t); n != 2 {
				t.Fatalf("credential rows must stay 2 after rejection, got %d", n)
			}
			after := inspect(t)
			if after.CredentialID != oldCred || after.SudoCredentialID != oldSudo || after.AuthMethod != before.AuthMethod {
				t.Fatalf("rejected edit must leave the server's credential bindings unchanged:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// TestServersEditClearCredential (Plan 21 A2): --clear-credential is the
// reverse operation of re-credential — an EXCLUSIVE action that resets the
// server to the credential-less form (store ClearServerCredential). Mutually
// exclusive with --password/--key; a rejected combination must leave the
// store untouched.
func TestServersEditClearCredential(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		// never the developer machine's real master.key (newVaultEnv pattern)
		"SSHMGR_FILEKEY_PATH": filepath.Join(dir, "no-such-master.key"),
	})

	run := func(args ...string) error {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs(args)
		return root.Execute()
	}
	inspect := func(name string) *models.Server {
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName(name)
		if srv == nil {
			t.Fatalf("server %q not found", name)
		}
		return srv
	}
	credAlive := func(id string) bool {
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		c, _ := st.GetCredential(id)
		return c != nil
	}

	if err := run("servers", "add", "--name", "enc", "--host", "h", "--user", "u",
		"--password", "pw", "--tags", "needs-passphrase,gpu"); err != nil {
		t.Fatalf("add: %v", err)
	}
	oldCred := inspect("enc").CredentialID

	// clear: back to the credential-less form, tag stripped, exclusive row gone
	if err := run("servers", "edit", "enc", "--clear-credential"); err != nil {
		t.Fatalf("edit --clear-credential: %v", err)
	}
	got := inspect("enc")
	if got.CredentialID != "" || got.AuthMethod != "" || got.SudoCredentialID != "" {
		t.Fatalf("server must be credential-less: %+v", got)
	}
	if contains(got.Tags, "needs-passphrase") || len(got.Tags) != 1 || got.Tags[0] != "gpu" {
		t.Fatalf("needs-passphrase must be stripped, gpu kept: %v", got.Tags)
	}
	if credAlive(oldCred) {
		t.Fatal("exclusively-owned old credential row must be dropped")
	}

	// re-credential a fresh server, then pin the two mutex rejections
	if err := run("servers", "add", "--name", "enc2", "--host", "h", "--user", "u",
		"--password", "pw"); err != nil {
		t.Fatalf("add enc2: %v", err)
	}
	enc2Cred := inspect("enc2").CredentialID
	keyPath := filepath.Join(dir, "plain_key")
	genKeyFile(t, keyPath, "")
	for _, args := range [][]string{
		{"--clear-credential", "--password", "x"},
		{"--clear-credential", "--key", keyPath},
	} {
		err := run(append([]string{"servers", "edit", "enc2"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("edit %v must fail with a mutually-exclusive error, got %v", args, err)
		}
		after := inspect("enc2")
		if after.CredentialID != enc2Cred || after.AuthMethod == "" {
			t.Fatalf("rejected combination must leave the store unchanged: %+v", after)
		}
		if !credAlive(enc2Cred) {
			t.Fatal("rejected combination must not delete the credential row")
		}
	}
}
