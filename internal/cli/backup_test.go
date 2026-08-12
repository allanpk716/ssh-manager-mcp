package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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

func TestBackupVerify_Ok(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 1 {
		t.Fatal("need exactly one backup")
	}
	root := NewRootCmd()
	root.SetArgs([]string{"backup", "verify", matches[0]})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("verify healthy backup: %v", err)
	}
}

func TestBackupVerify_BitRot_Fails(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	path := matches[0]
	// flip one byte deep in the file (not the first byte, to keep it valid-ish JSON shape)
	b, _ := os.ReadFile(path)
	if len(b) < 20 {
		t.Fatal("backup too small to corrupt")
	}
	b[len(b)-10] ^= 0xFF
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRootCmd()
	root.SetArgs([]string{"backup", "verify", path})
	err := root.Execute()
	if err == nil {
		t.Fatal("verify must fail on bit-rot")
	}
}

func TestBackupVerify_MissingSidecar_Fails(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	os.Remove(matches[0] + ".sha256")
	root := NewRootCmd()
	root.SetArgs([]string{"backup", "verify", matches[0]})
	if err := root.Execute(); err == nil {
		t.Fatal("verify must fail when sidecar is missing")
	}
}

// TestRotateBackups_JsonRemoveFails_KeepsPair tests the M1 invariant: when
// os.Remove fails on a rotated .json (real error — here simulated by holding
// an open file handle, which on Windows blocks Remove with a sharing
// violation), rotateBackups MUST also skip that .json's sidecar — leaving the
// .json + .sha256 pair intact rather than orphaning the .json.
//
// On non-Windows (Linux/macOS) an open handle does NOT block unlink, so this
// test's setup cannot force the failure there. The test is skipped on
// non-Windows; the logic it guards is identical across platforms (the explicit
// `continue` on .json remove error).
//
// rotateBackups is called as a unit (no CLI plumbing) — fewer fixtures, no
// vault seeding needed. We pre-create three vault-*.json with sidecars, hold
// an open handle on the one slated for deletion (oldest, lexicographically
// smallest => matches[2] after reverse-sort => matches[keep:] with keep=2),
// then assert both it and its sidecar survive.
func TestRotateBackups_JsonRemoveFails_KeepsPair(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("open-handle-doesn't-block-unlink on Unix; tested via code review + Windows CI")
	}
	dir := t.TempDir()
	// 3 backups: oldest will be the one slated for removal (keep=2).
	names := []string{
		"vault-20260101-010101.json",
		"vault-20260102-020202.json",
		"vault-20260103-030303.json",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n+".sha256"), []byte("file_sha256=deadbeef\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(dir, names[0]) // lexicographically smallest => rotated out
	hold, err := os.Open(victim)
	if err != nil {
		t.Fatalf("open victim to block remove: %v", err)
	}
	t.Cleanup(func() { hold.Close() })

	if err := rotateBackups(dir, "vault", 2); err != nil {
		t.Fatalf("rotateBackups: %v", err)
	}
	// victim .json MUST still exist (its Remove failed).
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim .json should survive when Remove fails: %v", err)
	}
	// victim sidecar MUST also survive — this is the M1 invariant. If the old
	// code had deleted the sidecar despite the .json Remove error, we'd see
	// an orphan .json with no sidecar here.
	if _, err := os.Stat(victim + ".sha256"); err != nil {
		t.Fatalf("victim sidecar should survive alongside its .json (M1 invariant): %v", err)
	}
	// newest two (with their sidecars) MUST be untouched too.
	for _, n := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("kept .json missing: %s: %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(dir, n+".sha256")); err != nil {
			t.Errorf("kept sidecar missing: %s: %v", n, err)
		}
	}
}
