package importer_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/importer"
	"ssh-manager-mcp/internal/models"
)

// genPEM writes a synthetic key file (plaintext or legacy-encrypted PEM — the
// encrypted form is what ssh.ParsePrivateKey answers PassphraseMissingError
// for) and returns its bytes. Synthetic keys only; this repo is public.
func genPEM(t *testing.T, path, passphrase string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block, err = x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES256)
		if err != nil {
			t.Fatal(err)
		}
	}
	b := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPickKey walks every outcome of the shared per-candidate seam the CLI
// import and the TUI import flow both consume.
func TestPickKey(t *testing.T) {
	dir := t.TempDir()
	plain := genPEM(t, filepath.Join(dir, "plain"), "")
	enc := genPEM(t, filepath.Join(dir, "enc"), "secret-pass")
	plainSL := filepath.ToSlash(filepath.Join(dir, "plain"))
	encSL := filepath.ToSlash(filepath.Join(dir, "enc"))

	cand := func(paths ...string) importer.Candidate {
		return importer.Candidate{Name: "x", Host: "h", Port: 22, User: "u", KeyPaths: paths}
	}

	t.Run("no key paths needs credential", func(t *testing.T) {
		got := importer.PickKey(cand(), dir, map[[32]byte]string{})
		if got.Cred != nil || got.KeyBytes != nil || got.Minted || got.NeedsPass {
			t.Fatalf("empty KeyPaths must pick nothing: %+v", got)
		}
	})

	t.Run("unreadable key falls through to next", func(t *testing.T) {
		got := importer.PickKey(cand("no-such-file", plainSL), dir, map[[32]byte]string{})
		if got.Cred == nil || !got.Minted {
			t.Fatalf("second IdentityFile must win: %+v", got)
		}
		if string(got.KeyBytes) != string(plain) {
			t.Fatal("KeyBytes must carry the picked file's content")
		}
	})

	t.Run("plain key mints", func(t *testing.T) {
		got := importer.PickKey(cand(plainSL), dir, map[[32]byte]string{})
		if got.Cred == nil || got.Cred.ID != "" || got.Cred.Type != models.CredPrivateKey {
			t.Fatalf("mint shape wrong: %+v", got.Cred)
		}
		if string(got.Cred.Secret) != string(plain) || !got.Minted || got.NeedsPass {
			t.Fatalf("plain key pick: %+v", got)
		}
	})

	t.Run("encrypted key mints with NeedsPass", func(t *testing.T) {
		got := importer.PickKey(cand(encSL), dir, map[[32]byte]string{})
		if !got.Minted || !got.NeedsPass || len(got.Cred.Passphrase) != 0 {
			t.Fatalf("encrypted key must import as-is with NeedsPass: %+v", got)
		}
		if string(got.KeyBytes) != string(enc) {
			t.Fatal("KeyBytes must carry the encrypted PEM for later re-mint")
		}
	})

	t.Run("seen hash reuses row id, still reports NeedsPass", func(t *testing.T) {
		// two hosts share one ENCRYPTED key: the second must reuse the first's
		// credential row AND still carry the ⚠ — it lacks the passphrase too.
		seen := map[[32]byte]string{}
		first := importer.PickKey(cand(encSL), dir, seen)
		seen[first.Sum] = "cred-1" // caller backfills after insert
		second := importer.PickKey(cand(encSL), dir, seen)
		if second.Minted || second.Cred == nil || second.Cred.ID != "cred-1" {
			t.Fatalf("reuse shape wrong: %+v", second)
		}
		if !second.NeedsPass {
			t.Fatal("shared encrypted key must keep the needs-passphrase ⚠")
		}
		if string(second.KeyBytes) != string(enc) {
			t.Fatal("reuse path must still expose KeyBytes for in-memory re-mint")
		}
	})
}
