package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

const (
	encPass1 = "gen-pass-alpha-1"
	encPass2 = "gen-pass-beta-2"
)

// writePassFile writes a passphrase file (0600) in dir and returns its path.
func writePassFile(t *testing.T, dir, pass string) string {
	t.Helper()
	p := filepath.Join(dir, "backup.pass")
	if err := os.WriteFile(p, []byte(pass), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runVerifyArgs runs `backup verify [args...]` and returns stdout.
func runVerifyArgs(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := NewRootCmd()
	root.SetArgs(append([]string{"backup", "verify"}, args...))
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// smeFiles globs bdir for vault-*.sme.
func smeFiles(t *testing.T, bdir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(bdir, "vault-*.sme"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestBackupCreateEncrypted_WritesSSHMGRV1SME: --passphrase-file flips create
// into encrypted mode: exactly one vault-<UTC>.sme, SSHMGRV1 magic, no .sha256
// sidecar (ciphertext is self-authenticating), and the blob decrypts back to
// the seeded snapshot.
func TestBackupCreateEncrypted_WritesSSHMGRV1SME(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), encPass1)

	out, err := runCreate(t, bdir, "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("encrypted create: %v", err)
	}
	matches := smeFiles(t, bdir)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 vault-*.sme, got %v", matches)
	}
	base := filepath.Base(matches[0])
	if !strings.HasPrefix(base, "vault-") || !strings.HasSuffix(base, ".sme") {
		t.Fatalf("unexpected product name: %s", base)
	}
	blob, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !vaultio.IsEncrypted(blob) {
		t.Fatal("product must start with the SSHMGRV1 magic header")
	}
	plain, err := vaultio.Decrypt([]byte(encPass1), blob)
	if err != nil {
		t.Fatalf("product must decrypt with the file passphrase: %v", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		t.Fatalf("decrypted product must re-parse as a snapshot: %v", err)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "gpu" {
		t.Fatalf("snapshot content mismatch: %+v", snap.Servers)
	}
	// encrypted products carry NO .sha256 sidecar
	if sidecars, _ := filepath.Glob(filepath.Join(bdir, "vault-*.sme.sha256")); len(sidecars) != 0 {
		t.Fatalf("encrypted backup must not write a .sha256 sidecar: %v", sidecars)
	}
	if !bytes.Contains(out.Bytes(), []byte(".sme")) {
		t.Fatalf("stdout should name the .sme backup: %q", out.String())
	}
}

// TestBackupCreateEncrypted_GuardsStillApply: marker and .git guardrails run
// BEFORE the encrypted branch — both must still fail closed, writing nothing.
func TestBackupCreateEncrypted_GuardsStillApply(t *testing.T) {
	seedVaultForBackup(t)
	passFile := writePassFile(t, t.TempDir(), encPass1)

	bdir := t.TempDir() // no marker
	if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err == nil {
		t.Fatal("encrypted create must fail closed on missing marker")
	}
	if m := smeFiles(t, bdir); len(m) != 0 {
		t.Fatalf("no .sme may be written when the marker guard fires: %v", m)
	}

	bdir2 := t.TempDir()
	touchMarker(t, bdir2)
	if err := os.Mkdir(filepath.Join(bdir2, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runCreate(t, bdir2, "--passphrase-file", passFile); err == nil {
		t.Fatal("encrypted create must fail closed when --dir contains .git")
	}
	if m := smeFiles(t, bdir2); len(m) != 0 {
		t.Fatalf("no .sme may be written when the .git guard fires: %v", m)
	}
}

// TestBackupCreateEncrypted_ConcurrentLockSkip: a live (non-stale) backup lock
// makes the encrypted branch skip with exit 0, writing nothing — same semantics
// as the plaintext branch.
func TestBackupCreateEncrypted_ConcurrentLockSkip(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), encPass1)

	lk, err := acquireBackupLock(bdir)
	if err != nil {
		t.Fatalf("acquire test lock: %v", err)
	}
	defer lk.Release()

	out, err := runCreate(t, bdir, "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("concurrent backup must skip, not error: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("another backup in progress")) {
		t.Fatalf("expected skip message, got: %q", out.String())
	}
	if m := smeFiles(t, bdir); len(m) != 0 {
		t.Fatalf("skipped run must not write: %v", m)
	}
}

// TestNextEncryptedBackupName_SameSecondSuffix pins the -2 collision suffix
// deterministically at the unit level.
func TestNextEncryptedBackupName_SameSecondSuffix(t *testing.T) {
	dir := t.TempDir()
	base := "vault-" + time.Now().UTC().Format("20060102-150405")
	if err := os.WriteFile(filepath.Join(dir, base+".sme"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := nextEncryptedBackupName(dir, "vault")
	if err != nil {
		t.Fatal(err)
	}
	if name != base+"-2.sme" {
		t.Fatalf("expected -2 suffix on same-second collision, got %s", name)
	}
}

// TestBackupCreateEncrypted_NeverOverwrites: a sentinel occupying this second's
// name must survive the run untouched (the create either suffixes -2 or rolls
// to the next second) and exactly one new file appears. The sentinel is a REAL
// encrypted backup (same passphrase) so the pre-flight generation check — which
// must refuse to run after an undecryptable newest .sme — stays satisfied.
func TestBackupCreateEncrypted_NeverOverwrites(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), encPass1)

	plainSentinel, err := json.Marshal(store.Snapshot{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	sentinelBlob, err := vaultio.Encrypt([]byte(encPass1), plainSentinel)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(bdir, "vault-"+time.Now().UTC().Format("20060102-150405")+".sme")
	if err := os.WriteFile(sentinel, sentinelBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err != nil {
		t.Fatalf("create with sentinel: %v", err)
	}
	b, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, sentinelBlob) {
		t.Fatal("sentinel was overwritten — same-second collision handling is broken")
	}
	if m := smeFiles(t, bdir); len(m) != 2 {
		t.Fatalf("expected sentinel + 1 new file, got %v", m)
	}
}

// TestBackupCreateEncrypted_GenerationCheck: pre-flight passphrase generation
// check — no prior .sme runs fine; a passphrase that cannot decrypt the latest
// existing .sme is a hard error and writes nothing; the correct passphrase
// proceeds (encrypted mode has no skip-unchanged).
func TestBackupCreateEncrypted_GenerationCheck(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	pass1 := writePassFile(t, t.TempDir(), encPass1)
	pass2 := writePassFile(t, t.TempDir(), encPass2)

	if _, err := runCreate(t, bdir, "--passphrase-file", pass1); err != nil {
		t.Fatalf("first encrypted create (no prior .sme): %v", err)
	}
	_, err := runCreate(t, bdir, "--passphrase-file", pass2)
	if err == nil {
		t.Fatal("wrong-passphrase create must be rejected by the generation check")
	}
	if m := smeFiles(t, bdir); len(m) != 1 {
		t.Fatalf("rejected run must not write a new file; got %v", m)
	}
	if _, err := runCreate(t, bdir, "--passphrase-file", pass1); err != nil {
		t.Fatalf("second create with the same passphrase must proceed: %v", err)
	}
	if m := smeFiles(t, bdir); len(m) != 2 {
		t.Fatalf("same-passphrase create should write a new backup (no skip in encrypted mode); got %v", m)
	}
}

// TestBackupCreateEncrypted_EmptyPassphrase_FailClosed: a passphrase file whose
// content is only a newline trims to empty — reject before writing anything.
func TestBackupCreateEncrypted_EmptyPassphrase_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), "\n")
	if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err == nil {
		t.Fatal("empty passphrase must be rejected")
	}
	if m := smeFiles(t, bdir); len(m) != 0 {
		t.Fatalf("no file may be written on an empty passphrase: %v", m)
	}
}

// TestBackupPostWriteVerifyEncrypted_TamperAndWrongPass exercises the exact
// function the encrypted create runs post-write: healthy file passes; one
// flipped byte or a wrong passphrase fails. (Tamper simulation at the unit
// seam — create itself verifies immediately after its own atomic write.)
func TestBackupPostWriteVerifyEncrypted_TamperAndWrongPass(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), encPass1)
	if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err != nil {
		t.Fatal(err)
	}
	path := smeFiles(t, bdir)[0]
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pw := []byte(encPass1)

	if err := verifyEncryptedWritten(path, pw); err != nil {
		t.Fatalf("healthy .sme must pass post-write verify: %v", err)
	}

	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xFF
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyEncryptedWritten(path, pw); err == nil {
		t.Fatal("tampered .sme must fail post-write verify")
	}

	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyEncryptedWritten(path, []byte(encPass2)); err == nil {
		t.Fatal("wrong passphrase must fail post-write verify")
	}
}

// TestBackupCreateEncrypted_RotationSmeOnly_MixedDir: --keep rotates ONLY the
// .sme lineage; a cohabiting plaintext .json + .sha256 lineage is untouched,
// and no orphan residue is left behind.
func TestBackupCreateEncrypted_RotationSmeOnly_MixedDir(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), encPass1)

	// pre-existing PLAINTEXT lineage: 2 .json + sidecars — must survive intact
	jsonNames := []string{"vault-20260101-000000.json", "vault-20260102-000000.json"}
	for _, n := range jsonNames {
		if err := os.WriteFile(filepath.Join(bdir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bdir, n+".sha256"), []byte("file_sha256=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := runCreate(t, bdir, "--passphrase-file", passFile, "--keep", "2"); err != nil {
			t.Fatalf("encrypted run %d: %v", i, err)
		}
	}
	if m := smeFiles(t, bdir); len(m) != 2 {
		t.Fatalf("keep=2 over 3 .sme runs should leave exactly 2; got %v", m)
	}
	// every surviving .sme still decrypts (rotation must not leave junk)
	for _, p := range smeFiles(t, bdir) {
		blob, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vaultio.Decrypt([]byte(encPass1), blob); err != nil {
			t.Fatalf("surviving %s does not decrypt: %v", filepath.Base(p), err)
		}
	}
	jsons, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(jsons) != 2 {
		t.Fatalf(".json lineage must be untouched by .sme rotation; got %v", jsons)
	}
	scs, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json.sha256"))
	if len(scs) != 2 {
		t.Fatalf(".json sidecars must be untouched by .sme rotation; got %v", scs)
	}
	// no orphan residue: any vault-* entry must be .sme / .json / .json.sha256
	entries, err := os.ReadDir(bdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "vault-") &&
			!strings.HasSuffix(n, ".sme") &&
			!strings.HasSuffix(n, ".json") &&
			!strings.HasSuffix(n, ".json.sha256") {
			t.Fatalf("unexpected residue in backup dir: %s", n)
		}
	}
}

// TestBackupVerify_Encrypted covers the three .sme branches: no passphrase =>
// explicit error pointing at --passphrase-file; correct passphrase => ok;
// tampered file => failure even with the right passphrase.
func TestBackupVerify_Encrypted(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passFile := writePassFile(t, t.TempDir(), encPass1)
	if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err != nil {
		t.Fatal(err)
	}
	path := smeFiles(t, bdir)[0]

	// (a) no passphrase
	_, err := runVerifyArgs(t, path)
	if err == nil {
		t.Fatal("verify of an .sme without --passphrase-file must fail")
	}
	if !strings.Contains(err.Error(), "--passphrase-file") {
		t.Fatalf("error must point at --passphrase-file: %v", err)
	}

	// (b) correct passphrase
	out, err := runVerifyArgs(t, path, "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("verify healthy .sme with passphrase: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("ok")) {
		t.Fatalf("expected ok output: %q", out.String())
	}

	// (c) tampered file, correct passphrase
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xFF
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runVerifyArgs(t, path, "--passphrase-file", passFile); err == nil {
		t.Fatal("verify tampered .sme must fail")
	}
}

// TestBackupVerify_Plaintext_IgnoresPassphraseFile: the plaintext .json branch
// keeps its exact behavior — the (now existing) --passphrase-file flag is
// ignored there, mirroring `import`'s plaintext sniff semantics.
func TestBackupVerify_Plaintext_IgnoresPassphraseFile(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	if _, err := runCreate(t, bdir); err != nil {
		t.Fatal(err)
	}
	passFile := writePassFile(t, t.TempDir(), encPass1)
	matches, err := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("need exactly one plaintext backup: %v %v", matches, err)
	}
	out, err := runVerifyArgs(t, matches[0], "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("plaintext verify must stay on the unchanged plaintext branch: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("ok")) {
		t.Fatalf("expected ok output: %q", out.String())
	}
}

// TestBackupEncrypted_RoundTrip_ImportEquivalent: fixture vault (servers,
// credentials incl. a private-key passphrase, profile, grant, project) ->
// encrypted create -> `import --passphrase-file` into a fresh vault under a
// DIFFERENT master key -> the two snapshots must be field-for-field identical,
// and the original project token must still validate.
func TestBackupEncrypted_RoundTrip_ImportEquivalent(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	bdir := filepath.Join(dir, "backup")
	if err := os.Mkdir(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	touchMarker(t, bdir)
	passFile := writePassFile(t, dir, encPass1)

	// --- seed store A ---
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey A: %v", err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": dbA, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	cid1, err := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	if err != nil {
		t.Fatalf("SetCredential 1: %v", err)
	}
	cid2, err := stA.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: []byte("KEYDATA"), Passphrase: []byte("keypass")})
	if err != nil {
		t.Fatalf("SetCredential 2: %v", err)
	}
	srv1, err := stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "deploy", AuthMethod: models.AuthPassword, CredentialID: cid1})
	if err != nil {
		t.Fatalf("AddServer 1: %v", err)
	}
	if _, err := stA.AddServer(&models.Server{
		Name: "nas", Host: "192.0.2.20", User: "backup", AuthMethod: models.AuthPrivateKey,
		CredentialID: cid2, SudoCredentialID: cid1, Description: "box", ExposeHost: true,
	}); err != nil {
		t.Fatalf("AddServer 2: %v", err)
	}
	profID, err := stA.AddProfile("team-a")
	if err != nil {
		t.Fatalf("AddProfile: %v", err)
	}
	if err := stA.GrantServers(profID, []string{srv1}); err != nil {
		t.Fatalf("GrantServers: %v", err)
	}
	_, token, err := stA.AddProject("my-agent", profID)
	if err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	snapA, err := stA.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot A: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	plainA, err := json.MarshalIndent(snapA, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	// --- encrypted create from A ---
	if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err != nil {
		t.Fatalf("encrypted create: %v", err)
	}
	matches := smeFiles(t, bdir)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 .sme, got %v", matches)
	}

	// --- import into fresh B (different master key) ---
	mk2, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey B: %v", err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": dbB, "SSHMGR_MASTERKEY_HEX": hexEncode(mk2)})
	root := NewRootCmd()
	root.SetArgs([]string{"import", matches[0], "--passphrase-file", passFile})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("import of encrypted backup: %v", err)
	}

	// --- snapshots must be field-for-field identical ---
	stB, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer stB.Close()
	snapB, err := stB.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot B: %v", err)
	}
	plainB, err := json.MarshalIndent(snapB, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(plainA) != string(plainB) {
		t.Fatalf("round-trip snapshots differ:\n--- A ---\n%s\n--- B ---\n%s", plainA, plainB)
	}
	// explicit count assertions (servers/credentials/profiles/projects/grants)
	if len(snapB.Servers) != 2 || len(snapB.Credentials) != 2 || len(snapB.Profiles) != 1 || len(snapB.Projects) != 1 || len(snapB.Grants) != 1 {
		t.Fatalf("round-trip counts wrong: servers=%d creds=%d profiles=%d projects=%d grants=%d",
			len(snapB.Servers), len(snapB.Credentials), len(snapB.Profiles), len(snapB.Projects), len(snapB.Grants))
	}
	// and the ORIGINAL token still validates on B (hash/salt preserved verbatim)
	if pj, err := stB.VerifyToken(token); err != nil || pj == nil {
		t.Fatalf("original token does not validate on B after encrypted round-trip: err=%v pj=%+v", err, pj)
	}
}
