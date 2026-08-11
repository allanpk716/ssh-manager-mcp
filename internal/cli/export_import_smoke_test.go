package cli

import (
	"bytes"
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
