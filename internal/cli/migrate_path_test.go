package cli

import (
	"bytes"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"

	_ "modernc.org/sqlite"
)

// seedFileVault seeds a file-type vault at storePath with master key file at
// mkPath containing n password-credential servers. The master key is written
// as plaintext (FileKeyProvider format) and the store is opened+closed to
// persist servers under that key. Returns the master key bytes.
func seedFileVault(t *testing.T, storePath, mkPath string, n int) []byte {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(mkPath), 0o700); err != nil {
		t.Fatalf("mkdir mk dir: %v", err)
	}
	if err := os.WriteFile(mkPath, mk, 0o600); err != nil {
		t.Fatalf("write mk: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	for i := 0; i < n; i++ {
		cid, err := st.SetCredential(&models.Credential{
			Type:   models.CredPassword,
			Secret: []byte("pw-" + string(rune('A'+i))),
		})
		if err != nil {
			t.Fatalf("SetCredential %d: %v", i, err)
		}
		if _, err := st.AddServer(&models.Server{
			Name:         "srv-" + string(rune('A'+i)),
			Host:         "h", Port: 22, User: "u",
			AuthMethod:   models.AuthPassword,
			CredentialID: cid,
		}); err != nil {
			t.Fatalf("AddServer %d: %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	return mk
}

// countServersViaFile opens the store at storePath reading the master key from
// mkPath (file), and returns the number of servers. Fails the test if the store
// cannot be opened or the key cannot be read.
func countServersViaFile(t *testing.T, storePath, mkPath string) int {
	t.Helper()
	mk, err := os.ReadFile(mkPath)
	if err != nil {
		t.Fatalf("read mk at %s: %v", mkPath, err)
	}
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("open store at %s: %v", storePath, err)
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	return len(servers)
}

// runMigratePathWithFlags invokes runMigratePath with the given --from / --keep-old.
func runMigratePathWithFlags(t *testing.T, from string, keepOld bool) error {
	t.Helper()
	return runMigratePath(&bytes.Buffer{}, migratePathOpts{From: from, KeepOld: keepOld})
}

// TestMigratePath_FileVault seeds an old file-vault (7 servers) at an old dir
// and migrates to the NEW path (pointed at a temp dir via SSHMGR_STORE +
// SSHMGR_FILEKEY_PATH). Asserts the new store has 7 servers, all credentials
// decrypt under the copied master key, and the old files are deleted.
func TestMigratePath_FileVault(t *testing.T) {
	// --- old vault (source) ---
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	seedFileVault(t, oldStore, oldMK, 7)

	// --- new vault (destination) via paths.StorePath/MasterKeyPath env override ---
	// SSHMGR_STORE controls the NEW destination (paths.StorePath honors it);
	// SSHMGR_FILEKEY_PATH controls the NEW master key path. --from controls OLD.
	// This is the test seam the brief calls out: OLD via --from, NEW via env,
	// never colliding on SSHMGR_STORE's role.
	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	if err := runMigratePathWithFlags(t, oldDir, false); err != nil {
		t.Fatalf("runMigratePath: %v", err)
	}

	// new store has 7 servers, all decrypt under the copied key
	if got := countServersViaFile(t, newStore, newMK); got != 7 {
		t.Errorf("servers after migrate = %d, want 7", got)
	}

	// old files deleted (default: no --keep-old)
	if _, err := os.Stat(oldStore); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old store.db still exists (err=%v); want gone", err)
	}
	if _, err := os.Stat(oldMK); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old master.key still exists (err=%v); want gone", err)
	}
}

// TestMigratePath_UnreadableBackend_Errors simulates the NUC10 case: an old
// vault exists at the source dir but its master.key is NOT a readable file
// (here: a directory standing in for a DPAPI/keychain blob that can't be read
// in the current session). migrate-path MUST error with the export/import
// guidance and MUST NOT touch the new path or delete the old.
func TestMigratePath_UnreadableBackend_Errors(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	// seed store.db (so the vault IS detectable) but make master.key a directory
	// — simulating a non-file backend (DPAPI blob / keyring entry) that migrate-path
	// cannot and must not try to read.
	seedFileVault(t, oldStore, oldMK+"-real", 3) // key written to a DIFFERENT name
	if err := os.Mkdir(oldMK, 0o700); err != nil { // oldMK is a directory → unreadable
		t.Fatalf("mkdir fake mk: %v", err)
	}

	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	err := runMigratePathWithFlags(t, oldDir, false)
	if err == nil {
		t.Fatal("expected error for unreadable backend, got nil")
	}
	// the error must guide the user to export/import in a resolvable session
	msg := err.Error()
	for _, want := range []string{"export", "import", "RDP"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing guidance token %q", msg, want)
		}
	}
	// MUST NOT have written the new vault
	if _, statErr := os.Stat(newStore); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("new store.db was written (err=%v); migrate-path must NOT write on unreadable backend", statErr)
	}
	// MUST NOT have deleted the old store
	if _, statErr := os.Stat(oldStore); errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("old store.db was deleted on unreadable backend; must be preserved")
	}
}

// TestMigratePath_UnreadableBackend_NoKeyFile_Errors exercises the OTHER
// unreadable shape: store.db present but master.key.plain entirely absent
// (the actual NUC10 machine-scope DPAPI case — there is no plaintext key file).
// Same export/import guidance applies.
func TestMigratePath_UnreadableBackend_NoKeyFile_Errors(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	// seed store.db with the key written elsewhere, then delete the elsewhere key
	// so the vault exists but no readable master.key.plain is at oldMK.
	seedFileVault(t, oldStore, oldMK+"-elsewhere", 2)
	_ = os.Remove(oldMK + "-elsewhere")

	newDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        filepath.Join(newDir, paths.StoreFilename),
		"SSHMGR_FILEKEY_PATH": filepath.Join(newDir, paths.MasterKeyFilename),
	})

	err := runMigratePathWithFlags(t, oldDir, false)
	if err == nil {
		t.Fatal("expected error for missing key file, got nil")
	}
	for _, want := range []string{"export", "import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing guidance token %q", err.Error(), want)
		}
	}
}

// TestMigratePath_KeepOld verifies --keep-old preserves the source files.
func TestMigratePath_KeepOld(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	seedFileVault(t, oldStore, oldMK, 4)

	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	if err := runMigratePathWithFlags(t, oldDir, true); err != nil {
		t.Fatalf("runMigratePath --keep-old: %v", err)
	}

	// new store populated
	if got := countServersViaFile(t, newStore, newMK); got != 4 {
		t.Errorf("new servers = %d, want 4", got)
	}
	// OLD files PRESERVED with --keep-old
	if _, err := os.Stat(oldStore); err != nil {
		t.Errorf("old store.db missing under --keep-old: %v", err)
	}
	if _, err := os.Stat(oldMK); err != nil {
		t.Errorf("old master.key missing under --keep-old: %v", err)
	}
}

// TestMigratePath_Idempotent runs migrate twice. The second run must not corrupt
// the (already-migrated) new vault and must not error destructively. Since the
// first run deletes the old source by default, the second run finds nothing at
// the old path and reports "nothing to migrate" (a clean no-op), leaving the
// new vault intact.
func TestMigratePath_Idempotent(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	seedFileVault(t, oldStore, oldMK, 5)

	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	// first migration: old → new, deletes old
	if err := runMigratePathWithFlags(t, oldDir, false); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// second migration: old path is now empty → "nothing to migrate" no-op
	if err := runMigratePathWithFlags(t, oldDir, false); err != nil {
		t.Fatalf("second migrate (should be no-op): %v", err)
	}
	// new vault still has 5 servers, all decrypt
	if got := countServersViaFile(t, newStore, newMK); got != 5 {
		t.Errorf("new servers after 2x migrate = %d, want 5 (idempotent)", got)
	}
}

// TestMigratePath_NewVaultNotEmpty_Refuses asserts that if the NEW destination
// already has a populated vault, migrate-path refuses to clobber it instead of
// silently merging.
func TestMigratePath_NewVaultNotEmpty_Refuses(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	seedFileVault(t, oldStore, oldMK, 3)

	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	// seed the NEW destination with 1 server so it's non-empty
	seedFileVault(t, newStore, newMK, 1)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	err := runMigratePathWithFlags(t, oldDir, false)
	if err == nil {
		t.Fatal("expected refusal when new vault is non-empty, got nil")
	}
	// the new vault must be UNCHANGED (still just 1 server — not merged into)
	if got := countServersViaFile(t, newStore, newMK); got != 1 {
		t.Errorf("new vault clobbered: servers = %d, want 1 (refuse must not merge)", got)
	}
	// old must be PRESERVED (refusal happened before delete)
	if _, statErr := os.Stat(oldStore); errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("old store deleted despite refusal")
	}
}

// TestMigratePath_SelfCheckActuallyDecrypts proves the N/N self-check decrypts
// EVERY credential (not just counts server rows). We seed a vault, then CORRUPT
// one credential blob in the source store.db with a byte flip. migrate-path
// must refuse: the self-check fails, the new vault is rolled back, and the old
// source is untouched. If the self-check were a row-count-only check, the
// corrupted credential would silently migrate and the user would only discover
// the breakage at connect time.
func TestMigratePath_SelfCheckActuallyDecrypts(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, paths.StoreFilename)
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	seedFileVault(t, oldStore, oldMK, 3)

	// Corrupt one credential blob directly in the source DB. We open the SQLite
	// file with the same driver store uses (modernc.org/sqlite) to flip a byte
	// deep inside the GCM ciphertext of one credential — without going through
	// the store API (which would re-seal). This keeps the store package's db
	// field private.
	db, err := sql.Open("sqlite", oldStore+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite to corrupt: %v", err)
	}
	defer db.Close()
	var credID string
	var blob []byte
	if err := db.QueryRow(`SELECT id, secret_blob FROM credentials LIMIT 1`).Scan(&credID, &blob); err != nil {
		t.Fatalf("pick credential: %v", err)
	}
	if len(blob) < 40 {
		t.Fatalf("blob too short to corrupt: %d", len(blob))
	}
	// flip a byte deep in the ciphertext (NOT salt/nonce prefix) so GCM auth fails
	blob[len(blob)-1] ^= 0xFF
	if _, err := db.Exec(`UPDATE credentials SET secret_blob=? WHERE id=?`, blob, credID); err != nil {
		t.Fatalf("write corrupt blob: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint after corrupt: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after corrupt: %v", err)
	}

	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	err = runMigratePathWithFlags(t, oldDir, false)
	if err == nil {
		t.Fatal("expected self-check to FAIL on corrupted credential, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "self-check") && !strings.Contains(msg, "decrypt") {
		t.Errorf("error %q does not mention self-check/decrypt failure", msg)
	}
	// new vault MUST be rolled back (no store.db at the new path)
	if _, statErr := os.Stat(newStore); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("new store.db left behind after self-check failure (err=%v); must be rolled back", statErr)
	}
	// old vault MUST be untouched (still 3 servers, still opens with mk)
	if got := countServersViaFile(t, oldStore, oldMK); got != 3 {
		t.Errorf("old store servers = %d after failed migrate, want 3 (untouched)", got)
	}
}

// TestMigratePath_UnreadableBackend_WrongKeyLength is the spec §5.3 case the
// other unreadable tests don't cover: master.key.plain EXISTS, is a FILE, and
// has the VALID length (32 bytes) — but is NOT the key the store was sealed
// under. migrate-path must refuse (the self-check fails on decrypt) and roll
// back the new vault, leaving the old untouched. Proves the wrong-key shape
// doesn't silently corrupt data.
func TestMigratePath_UnreadableBackend_WrongKeyLength(t *testing.T) {
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, paths.StoreFilename)
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)
	seedFileVault(t, oldStore, oldMK, 3)
	// Overwrite the real master key with a DIFFERENT valid-length (32-byte) key.
	// store.Open won't reject it (it doesn't validate the key); the failure
	// surfaces when the N/N self-check tries to decrypt credentials under it.
	wrongMK := make([]byte, 32)
	for i := range wrongMK {
		wrongMK[i] = 0xEE
	}
	if err := os.WriteFile(oldMK, wrongMK, 0o600); err != nil {
		t.Fatalf("write wrong mk: %v", err)
	}

	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	err := runMigratePathWithFlags(t, oldDir, false)
	if err == nil {
		t.Fatal("expected error for valid-length wrong key, got nil")
	}
	// new vault MUST be rolled back
	if _, statErr := os.Stat(newStore); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("new store.db left behind after self-check failure (err=%v); must be rolled back", statErr)
	}
	// old store.db MUST still exist (migrate-path must not delete on failure)
	if _, statErr := os.Stat(oldStore); errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("old store.db deleted on wrong-key failure; must be preserved")
	}
}

// TestMigratePath_NoOldVault_NoOp covers the idempotent re-run case directly:
// with no source vault at the --from dir, migrate-path is a clean no-op.
func TestMigratePath_NoOldVault_NoOp(t *testing.T) {
	emptyOldDir := t.TempDir() // nothing in it
	newDir := t.TempDir()
	newStore := filepath.Join(newDir, paths.StoreFilename)
	newMK := filepath.Join(newDir, paths.MasterKeyFilename)
	withEnv(t, map[string]string{
		"SSHMGR_STORE":        newStore,
		"SSHMGR_FILEKEY_PATH": newMK,
	})

	out := &bytes.Buffer{}
	if err := runMigratePath(out, migratePathOpts{From: emptyOldDir}); err != nil {
		t.Fatalf("no-op migrate should not error: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to migrate") {
		t.Errorf("expected 'nothing to migrate' message, got: %s", out.String())
	}
	// new path untouched
	if _, err := os.Stat(newStore); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("new store.db created on no-op (err=%v)", err)
	}
}

// TestMigratePath_CLIRegistered ensures migrate-path is wired into NewRootCmd
// so `ssh-manager migrate-path` dispatches to runMigratePath.
func TestMigratePath_CLIRegistered(t *testing.T) {
	root := NewRootCmd()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Use == "migrate-path" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("migrate-path subcommand not registered on root")
	}
	if found.RunE == nil {
		t.Fatal("migrate-path has no RunE")
	}
	if found.Flags().Lookup("from") == nil {
		t.Error("migrate-path missing --from flag")
	}
	if found.Flags().Lookup("keep-old") == nil {
		t.Error("migrate-path missing --keep-old flag")
	}
}
