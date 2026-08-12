package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// TestExportImport_CLIRoundTrip exercises the `export` and `import` COMMANDS
// end-to-end: seed store A directly (capturing a project plaintext token), export
// to a passphrase-encrypted file via the CLI, import the file into a FRESH store
// B that has a DIFFERENT master key, then assert the original plaintext token
// still validates on B (token_hash/salt are preserved verbatim across the
// master-key-independent envelope).
func TestExportImport_CLIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	outFile := filepath.Join(dir, "vault.export")

	// --- seed store A directly via store.Open (skipping `servers add` CLI) ---
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey A: %v", err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbA,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk),
	})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	cid, err := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	if err != nil {
		t.Fatalf("SetCredential A: %v", err)
	}
	srvID, err := stA.AddServer(&models.Server{
		Name:         "gpu",
		Host:         "192.0.2.10",
		User:         "deploy",
		AuthMethod:   models.AuthPassword,
		CredentialID: cid,
	})
	if err != nil {
		t.Fatalf("AddServer A: %v", err)
	}
	profID, err := stA.AddProfile("team-a")
	if err != nil {
		t.Fatalf("AddProfile A: %v", err)
	}
	if err := stA.GrantServers(profID, []string{srvID}); err != nil {
		t.Fatalf("GrantServers A: %v", err)
	}
	_, token, err := stA.AddProject("my-agent", profID)
	if err != nil {
		t.Fatalf("AddProject A: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}

	// swap the passphrase seams to a fixed value (export reads twice; import once)
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	t.Cleanup(func() {
		passphrasePrompt = orig
		passphraseConfirmPrompt = origConfirm
	})

	mustCliA := func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		root.SetArgs(args)
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		if err := root.Execute(); err != nil {
			t.Fatalf("A %v: %v", args, err)
		}
		return out
	}

	// --- export from A ---
	mustCliA("export", "--out", outFile)
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("export file not written: %v", err)
	}

	// --- import into a FRESH store B (different master key) ---
	mk2, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey B: %v", err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk2),
	})
	root := NewRootCmd()
	root.SetArgs([]string{"import", outFile})
	if err := root.Execute(); err != nil {
		t.Fatalf("import: %v", err)
	}

	// --- verify on B: server present + ORIGINAL token validates (cross-master-key) ---
	stB, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer stB.Close()
	got, err := stB.GetServerByName("gpu")
	if err != nil {
		t.Fatalf("GetServerByName B: %v", err)
	}
	if got == nil || got.Host != "192.0.2.10" {
		t.Fatalf("server not imported into B: %+v", got)
	}
	if pj, err := stB.VerifyToken(token); err != nil || pj == nil {
		t.Fatalf("ORIGINAL TOKEN does not validate on B after CLI import: err=%v pj=%+v", err, pj)
	}
}

// TestExport_PassphraseFile_NoPrompt exercises `export --passphrase-file <path>`:
// the passphrase is read from a 0600 file, the passphrasePrompt seam is wired to
// t.Fatal so the test fails hard if export tries to touch the TTY, and confirm
// is skipped (flag mode does not confirm). Output must still be a valid
// SSHMGRV1 envelope that decrypts with the same passphrase.
func TestExport_PassphraseFile_NoPrompt(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	outFile := filepath.Join(dir, "vault.export")
	passFile := filepath.Join(dir, "pass.txt")

	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey A: %v", err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbA,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk),
	})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	cid, err := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	if err != nil {
		t.Fatalf("SetCredential A: %v", err)
	}
	if _, err := stA.AddServer(&models.Server{
		Name:         "gpu",
		Host:         "192.0.2.10",
		User:         "deploy",
		AuthMethod:   models.AuthPassword,
		CredentialID: cid,
	}); err != nil {
		t.Fatalf("AddServer A: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}

	// write passphrase to a 0600 file
	const pass = "strong-passphrase-123"
	if err := os.WriteFile(passFile, []byte(pass), 0o600); err != nil {
		t.Fatalf("write pass file: %v", err)
	}

	// BOTH seams must stay untouched in flag mode — fail the test if export
	// tries to read the TTY for either the passphrase or the confirm.
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) {
		t.Fatal("passphrasePrompt must NOT be called with --passphrase-file")
		return nil, nil
	}
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) {
		t.Fatal("passphraseConfirmPrompt must NOT be called with --passphrase-file")
		return nil, nil
	}
	t.Cleanup(func() {
		passphrasePrompt = orig
		passphraseConfirmPrompt = origConfirm
	})

	root := NewRootCmd()
	root.SetArgs([]string{"export", "--out", outFile, "--passphrase-file", passFile})
	if err := root.Execute(); err != nil {
		t.Fatalf("export --passphrase-file: %v", err)
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("export file not written: %v", err)
	}

	// the output must be an encrypted envelope that decrypts with the file passphrase
	blob, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !vaultio.IsEncrypted(blob) {
		t.Fatalf("export file is not an SSHMGRV1 envelope")
	}
	plain, err := vaultio.Decrypt([]byte(pass), blob)
	if err != nil {
		t.Fatalf("export file does not decrypt with file passphrase: %v", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		t.Fatalf("decrypted export is not a snapshot: %v", err)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "gpu" {
		t.Fatalf("snapshot mismatch: %+v", snap.Servers)
	}
}

// TestImport_PassphraseFile_Encrypted chains export --passphrase-file into
// import --passphrase-file: a fully non-interactive encrypted round-trip with
// the prompt seams set to t.Fatal on both sides. The imported server must land
// in a fresh store B under a DIFFERENT master key.
func TestImport_PassphraseFile_Encrypted(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	outFile := filepath.Join(dir, "vault.export")
	passFile := filepath.Join(dir, "pass.txt")

	// seed A
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey A: %v", err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbA,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk),
	})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	cid, err := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	if err != nil {
		t.Fatalf("SetCredential A: %v", err)
	}
	if _, err := stA.AddServer(&models.Server{
		Name:         "gpu",
		Host:         "192.0.2.10",
		User:         "deploy",
		AuthMethod:   models.AuthPassword,
		CredentialID: cid,
	}); err != nil {
		t.Fatalf("AddServer A: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}

	const pass = "strong-passphrase-123"
	if err := os.WriteFile(passFile, []byte(pass), 0o600); err != nil {
		t.Fatalf("write pass file: %v", err)
	}

	// both seams fail the test if anything in the export path tries the TTY
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) {
		t.Fatal("passphrasePrompt must NOT be called with --passphrase-file")
		return nil, nil
	}
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) {
		t.Fatal("passphraseConfirmPrompt must NOT be called with --passphrase-file")
		return nil, nil
	}
	t.Cleanup(func() {
		passphrasePrompt = orig
		passphraseConfirmPrompt = origConfirm
	})

	// --- export with file passphrase (no TTY) ---
	rootExp := NewRootCmd()
	rootExp.SetArgs([]string{"export", "--out", outFile, "--passphrase-file", passFile})
	if err := rootExp.Execute(); err != nil {
		t.Fatalf("export --passphrase-file: %v", err)
	}

	// --- import into a FRESH store B with a DIFFERENT master key ---
	mk2, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey B: %v", err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk2),
	})
	rootImp := NewRootCmd()
	rootImp.SetArgs([]string{"import", outFile, "--passphrase-file", passFile})
	if err := rootImp.Execute(); err != nil {
		t.Fatalf("import --passphrase-file: %v", err)
	}

	// verify server landed in B
	stB, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer stB.Close()
	got, err := stB.GetServerByName("gpu")
	if err != nil || got == nil || got.Host != "192.0.2.10" {
		t.Fatalf("server not imported into B: got=%v err=%v", got, err)
	}
}

// TestImport_PlaintextJSON_NoPassphrase writes a PLAINTEXT snapshot JSON (no
// SSHMGRV1 envelope), imports it into a fresh store, and asserts the import
// succeeds WITHOUT ever prompting for a passphrase (the prompt seam fails the
// test if called).
func TestImport_PlaintextJSON_NoPassphrase(t *testing.T) {
	dir := t.TempDir()
	dbB := filepath.Join(dir, "b.db")
	inFile := filepath.Join(dir, "vault.json")

	// seed store A, export to PLAINTEXT json (no encryption) by calling ExportSnapshot directly
	dbA := filepath.Join(dir, "a.db")
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbA, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "deploy", AuthMethod: models.AuthPassword, CredentialID: cid})
	snap, err := stA.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	stA.Close()
	plaintext, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inFile, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	// point at empty B with a DIFFERENT master key
	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbB, "SSHMGR_MASTERKEY_HEX": hexEncode(mk2)})

	// FAIL the test if import prompts for a passphrase on the plaintext path
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) {
		t.Fatal("passphrasePrompt must NOT be called for plaintext import")
		return nil, nil
	}
	t.Cleanup(func() { passphrasePrompt = orig })

	root := NewRootCmd()
	root.SetArgs([]string{"import", inFile})
	if err := root.Execute(); err != nil {
		t.Fatalf("plaintext import: %v", err)
	}

	// verify server landed in B
	stB, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	got, err := stB.GetServerByName("gpu")
	if got == nil || err != nil {
		t.Fatalf("server not imported from plaintext: %v %v", got, err)
	}
}

// TestImport_EncryptedFile_StillPrompts guards that the encrypted path is
// unchanged: a real SSHMGRV1 file still prompts and decrypts.
func TestImport_EncryptedFile_StillPrompts(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	outFile := filepath.Join(dir, "vault.export")

	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbA, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	stA, _ := store.Open(dbA, mk)
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "deploy", AuthMethod: models.AuthPassword, CredentialID: cid})
	stA.Close()

	// export encrypted (uses passphrasePrompt seam)
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	t.Cleanup(func() { passphrasePrompt = orig; passphraseConfirmPrompt = origConfirm })

	root := NewRootCmd()
	root.SetArgs([]string{"export", "--out", outFile})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	// import into fresh B — should prompt (seam already swapped) and succeed
	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbB, "SSHMGR_MASTERKEY_HEX": hexEncode(mk2)})
	root2 := NewRootCmd()
	root2.SetArgs([]string{"import", outFile})
	if err := root2.Execute(); err != nil {
		t.Fatalf("encrypted import: %v", err)
	}
}

// TestImport_Plaintext_IgnoresPassphraseFile feeds a plaintext JSON snapshot
// to `import --passphrase-file <path>`. The plaintext sniff (Plan 11 T3) must
// take the no-passphrase branch and IGNORE the flag — it's not an error to
// pass --passphrase-file with a plaintext file (the flag is simply irrelevant).
func TestImport_Plaintext_IgnoresPassphraseFile(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	inFile := filepath.Join(dir, "vault.json")
	passFile := filepath.Join(dir, "pass.txt")

	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbA, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	if _, err := stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "deploy", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	snap, err := stA.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	stA.Close()
	plaintext, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inFile, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}
	// the passphrase file's contents are irrelevant for plaintext — write bogus
	if err := os.WriteFile(passFile, []byte("ignored-bogus-pass"), 0o600); err != nil {
		t.Fatal(err)
	}

	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbB, "SSHMGR_MASTERKEY_HEX": hexEncode(mk2)})

	// plaintext path must NOT call the prompt seam
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) {
		t.Fatal("passphrasePrompt must NOT be called for plaintext import")
		return nil, nil
	}
	t.Cleanup(func() { passphrasePrompt = orig })

	root := NewRootCmd()
	root.SetArgs([]string{"import", inFile, "--passphrase-file", passFile})
	if err := root.Execute(); err != nil {
		t.Fatalf("plaintext import with --passphrase-file: %v", err)
	}

	stB, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	got, err := stB.GetServerByName("gpu")
	if got == nil || err != nil {
		t.Fatalf("server not imported from plaintext with flag: %v %v", got, err)
	}
}
