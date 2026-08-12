package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
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
