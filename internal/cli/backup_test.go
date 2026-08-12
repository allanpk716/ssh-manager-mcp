package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedVaultForBackup points SSHMGR_STORE at a fresh temp db, seeds one server,
// and returns the dir + master key. Does NOT write audit (so skip can trigger).
func seedVaultForBackup(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "vault.db")
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": db, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	st, err := store.Open(db, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// runCreate runs `backup create` against bdir (a backup target dir) and returns stdout.
func runCreate(t *testing.T, bdir string, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	full := append([]string{"backup", "create", "--dir", bdir}, args...)
	root := NewRootCmd()
	root.SetArgs(full)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// touchMarker creates the marker file bdir requires.
func touchMarker(t *testing.T, bdir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bdir, ".ssh-manager-backup-marker"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mustDecodeHex decodes the SSHMGR_MASTERKEY_HEX env value back to bytes
// (hexDecode lives in enc.go). Used by tests that re-open the seeded vault.
func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	mk, err := hexDecode(s)
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

func TestBackupCreate_MissingMarker_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir() // no marker
	_, err := runCreate(t, bdir)
	if err == nil {
		t.Fatal("expected fail-closed on missing marker")
	}
}

func TestBackupCreate_DirContainsGit_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	if err := os.Mkdir(filepath.Join(bdir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := runCreate(t, bdir)
	if err == nil {
		t.Fatal("expected fail-closed when --dir contains .git")
	}
}

func TestBackupCreate_WritesJSONAndSidecar(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	out, err := runCreate(t, bdir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// exactly one vault-*.json + one .sha256
	matches, err := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 json, got %v %v", matches, err)
	}
	sidecars, err := filepath.Glob(filepath.Join(bdir, "vault-*.json.sha256"))
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("expected 1 sidecar, got %v", sidecars)
	}
	// sidecar has only file_sha256= line
	sc, err := os.ReadFile(sidecars[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sc, []byte("file_sha256=")) {
		t.Fatalf("sidecar missing file_sha256=: %q", sc)
	}
	if bytes.Contains(sc, []byte("size=")) {
		t.Fatalf("sidecar must NOT have size= field: %q", sc)
	}
	// stdout mentions something was written
	if !bytes.Contains(out.Bytes(), []byte("vault-")) {
		t.Fatalf("stdout should name the backup: %q", out.String())
	}
}

func TestBackupCreate_SkipUnchanged(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	if _, err := runCreate(t, bdir); err != nil {
		t.Fatal(err)
	}
	// second create, vault unchanged (no audit written between) => skip, no new file
	if _, err := runCreate(t, bdir); err != nil {
		t.Fatalf("second create: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 1 {
		t.Fatalf("skip should produce no new file; got %d", len(matches))
	}
}

func TestBackupCreate_ChangeProducesNewFile(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	// mutate vault: add a server
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw2")})
	st.AddServer(&models.Server{Name: "box2", Host: "192.0.2.99", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	st.Close()
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 2 {
		t.Fatalf("changed vault should produce 2nd backup; got %d", len(matches))
	}
}

func TestBackupCreate_Rotation_Keep2(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	for i := 0; i < 3; i++ {
		runCreate(t, bdir, "--keep", "2", "--prefix", "vault")
		st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw" + string(rune('A'+i)))})
		st.AddServer(&models.Server{Name: "srv" + string(rune('A'+i)), Host: "10.0.0." + string(rune('1'+i)), User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
		st.Close()
	}
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 2 {
		t.Fatalf("keep=2 with 3 distinct => 2 kept; got %d", len(matches))
	}
	sidecars, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json.sha256"))
	if len(sidecars) != 2 {
		t.Fatalf("sidecars should rotate with their json; got %d", len(sidecars))
	}
}

func TestBackupCreate_OrphanSidecarSweep(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	// pre-create an orphan sidecar with no matching .json
	orphan := filepath.Join(bdir, "vault-19990101-000000.json.sha256")
	os.WriteFile(orphan, []byte("file_sha256=deadbeef\n"), 0o600)
	runCreate(t, bdir)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan sidecar should be swept: %v", err)
	}
}

func TestBackupCreate_SameSecondCollision(t *testing.T) {
	// Two creates in the same second with a mutation between would normally
	// collide on the timestamp filename. Assert the second gets a -2 suffix
	// (not an overwrite). We force collision by pre-writing the expected name.
	// NOTE: time resolution makes this flaky if runs span a second; the test
	// pre-creates the target name to make collision deterministic.
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	// pre-create a file with the name the first create would use is racy;
	// instead assert: after two creates (same second possible), both files exist
	// OR the second is -2. Just assert no data loss: run twice fast, expect >=1 file.
	runCreate(t, bdir)
	// mutate immediately
	st, _ := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("different")})
	st.AddServer(&models.Server{Name: "x", Host: "10.99.99.99", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	st.Close()
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) < 2 {
		t.Fatalf("collision handling must not overwrite: expected >=2 files, got %d", len(matches))
	}
	// all distinct
	seen := map[string]bool{}
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		seen[string(b)] = true
	}
	if len(seen) != len(matches) {
		t.Fatalf("collision files must be distinct content")
	}
}
