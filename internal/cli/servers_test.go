package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

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
