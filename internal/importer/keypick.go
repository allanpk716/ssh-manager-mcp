package importer

import (
	"crypto/sha256"
	"os"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
)

// KeyPick is PickKey's outcome for one candidate: the credential to attach
// (nil = no readable key), the raw key bytes for later passphrase re-minting,
// and the batch-dedup bookkeeping the caller must finish.
type KeyPick struct {
	// Cred is the credential to hand AddServerWithCredentials for this
	// candidate; nil when no IdentityFile resolved to a readable key
	// (needs-credential — the server imports credential-less).
	Cred *models.Credential
	// KeyBytes is the raw content of the picked key file. Populated whenever a
	// key was read — including on the dedup-reuse path where Cred carries only
	// an ID — so a passphrase supplement can re-mint from memory without
	// re-reading the disk. Nil when Cred is nil.
	KeyBytes []byte
	// Sum is sha256 of the key content; meaningful only when Minted.
	Sum [32]byte
	// Minted reports that Cred is NEW (no ID yet): the caller inserts it via
	// AddServerWithCredentials and then records seen[Sum] = backfilled Cred.ID
	// so later candidates with the same key file reuse the row.
	Minted bool
	// NeedsPass reports the key is passphrase-encrypted: import it as-is and
	// mark the server needs-passphrase (connects will fail until the
	// passphrase is supplemented).
	NeedsPass bool
}

// PickKey resolves cand's IdentityFile list (configDir-relative, via
// ResolveKeyPaths) to at most ONE credential: the first readable key file
// wins. A key whose content hash is already in seen reuses that credential row
// (Cred.ID set, no mint — the AddServerWithCredentials batch-dedup contract);
// otherwise a fresh private-key credential is proposed (Minted; the insert
// backfills Cred.ID). An encrypted key is proposed as-is with NeedsPass set —
// including on the reuse path, since a second server sharing an encrypted key
// still lacks the passphrase. This is the ONE seam shared by the CLI import
// (`servers import`) and the TUI import flow — do not re-implement it.
func PickKey(cand Candidate, configDir string, seen map[[32]byte]string) KeyPick {
	for _, kp := range ResolveKeyPaths(cand.KeyPaths, configDir) {
		keyBytes, err := os.ReadFile(kp)
		if err != nil {
			continue // try the next IdentityFile
		}
		sum := sha256.Sum256(keyBytes)
		needsPass := false
		if _, err := ssh.ParsePrivateKey(keyBytes); err != nil {
			if _, missing := err.(*ssh.PassphraseMissingError); missing {
				needsPass = true
			}
		}
		if id, ok := seen[sum]; ok && id != "" {
			return KeyPick{
				Cred:      &models.Credential{ID: id, Type: models.CredPrivateKey},
				KeyBytes:  keyBytes,
				NeedsPass: needsPass,
			}
		}
		return KeyPick{
			Cred:      &models.Credential{Type: models.CredPrivateKey, Secret: keyBytes},
			KeyBytes:  keyBytes,
			Sum:       sum,
			Minted:    true,
			NeedsPass: needsPass,
		}
	}
	return KeyPick{}
}
