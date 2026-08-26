package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedGCStore points SSHMGR_STORE at a fresh vault holding 2 orphan credential
// rows, 1 in-use credential (bound to a server), 1 host_keys row and 1
// cache_tokens row — everything gc must NOT touch — then closes the store.
// Returns the master key so tests can re-open the vault and assert state.
func seedGCStore(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "gc.db")
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": db, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	st, err := store.Open(db, mk)
	if err != nil {
		t.Fatal(err)
	}
	// In-use credential: referenced by the server row.
	if _, err := st.AddServerWithCredentials(
		&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("in-use")},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	// 2 orphans: minted, never referenced by any server (the historical leak
	// shape gc exists for — e.g. legacy DeleteServer left these behind).
	for _, pw := range []string{"orphan-1", "orphan-2"} {
		if _, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(pw)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveHostKey("192.0.2.10", 22, []byte("hostkey-blob")); err != nil {
		t.Fatal(err)
	}
	gcProf, err := st.AddProfile("gc-p")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", gcProf); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return mk
}

// reopenGCStore re-opens the seeded vault for post-run assertions.
func reopenGCStore(t *testing.T, mk []byte) *store.Store {
	t.Helper()
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func runGC(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	full := append([]string{"gc"}, args...)
	root := NewRootCmd()
	root.SetArgs(full)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// TestGC_DryRunDefault: bare `gc` reports the orphan count and changes nothing.
func TestGC_DryRunDefault(t *testing.T) {
	mk := seedGCStore(t)
	out, err := runGC(t)
	if err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "2 orphan credential(s)") {
		t.Fatalf("dry-run output must report 2 orphans: %q", out.String())
	}
	if !strings.Contains(out.String(), "--apply") {
		t.Fatalf("dry-run output must point at --apply: %q", out.String())
	}
	st := reopenGCStore(t, mk)
	if n, err := st.CountOrphanCredentials(); err != nil || n != 2 {
		t.Fatalf("dry-run changed the db: orphans=%d err=%v want 2", n, err)
	}
}

// TestGC_ApplyDeletesOrphansOnly: `gc --apply` removes exactly the orphan
// credential rows; the in-use credential, host_keys and cache_tokens survive.
func TestGC_ApplyDeletesOrphansOnly(t *testing.T) {
	mk := seedGCStore(t)
	out, err := runGC(t, "--apply")
	if err != nil {
		t.Fatalf("gc --apply: %v", err)
	}
	if !strings.Contains(out.String(), "deleted 2 orphan credential(s)") {
		t.Fatalf("apply output must report 2 deletions: %q", out.String())
	}
	st := reopenGCStore(t, mk)
	if n, err := st.CountOrphanCredentials(); err != nil || n != 0 {
		t.Fatalf("apply left orphans: %d err=%v want 0", n, err)
	}
	// In-use credential survived and still decrypts.
	srv, err := st.GetServerByName("gpu")
	if err != nil || srv == nil {
		t.Fatalf("server gone after gc: %v %+v", err, srv)
	}
	if c, err := st.GetCredential(srv.CredentialID); err != nil || c == nil || string(c.Secret) != "in-use" {
		t.Fatalf("in-use credential damaged by gc: %v %+v", err, c)
	}
	// host_keys row untouched.
	hk, err := st.GetHostKey("192.0.2.10", 22)
	if err != nil || hk == nil || string(hk) != "hostkey-blob" {
		t.Fatalf("host_keys row must survive gc: %v %v", hk, err)
	}
	// cache_tokens row untouched.
	cts, err := st.ListCacheTokens()
	if err != nil || len(cts) != 1 || cts[0].Name != "laptop" {
		t.Fatalf("cache_tokens row must survive gc: %v %v", cts, err)
	}
}
