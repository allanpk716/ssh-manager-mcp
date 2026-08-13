package vault

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// OpenStore resolves the master key and opens the vault.
//
// The master-key KeyProvider is INJECTED by the caller. In production the cli
// package passes store.FileKeyProvider{} (Plan 16: dropped the DPAPI/keyring
// tier — spec §4.2; the vault package stays OS-agnostic and must not import
// cli, which imports vault). Tests inject a fake KeyProvider (MemKeyProvider)
// or pass nil to exercise the env + file tiers directly.
//
// resolveMasterKey order (spec §4.2, two tiers + injected-kp seam):
//  1. Injected kp.Get() (production: FileKeyProvider at the fixed path).
//     ErrNotFound → continue to tier 2. ANY OTHER error → HARD FAIL (never
//     silently degrade to plaintext via env — that would let a corrupted
//     master.key go unnoticed while an unrelated env var decrypts the vault).
//  2. SSHMGR_MASTERKEY_HEX env (hex) — dev / CLI scripting / tests.
//  3. FileKeyProvider at SSHMGR_FILEKEY_PATH (or default fixed path) — the
//     last resort when no kp is injected and no env is set.
//
// Returns a "vault locked" error if none yields a key.
func OpenStore(kp store.KeyProvider) (*store.Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	mk, err := resolveMasterKey(kp)
	if err != nil {
		return nil, err
	}
	return store.Open(path, mk)
}

func storePath() (string, error) {
	if p := os.Getenv("SSHMGR_STORE"); p != "" {
		return p, nil
	}
	return store.DefaultStorePath()
}

// resolveMasterKey resolves the vault master key (spec §4.2, Plan 16: two tiers
// after the DPAPI/keyring tier was dropped).
//
//  1. Injected kp (non-nil): kp.Get() decides.
//     - Success → returned verbatim (priority 1). This is the production path:
//       OpenStore's caller passes store.FileKeyProvider{}, which reads the
//       fixed-path master.key.
//     - ErrNotFound → fall through to tier 2 (legitimate: no key yet, or the
//       injected provider is a test fake that wants the env/file tier).
//     - ANY OTHER error (e.g. DPAPI decrypt failure on a corrupt blob, FS
//     permission error) → HARD FAIL. Never silent-fall-through to env or
//     plaintext FileKeyProvider — that would let a corrupted master.key go
//     unnoticed while an unrelated SSHMGR_MASTERKEY_HEX decrypts the vault
//     (spec §5.6 security guarantee).
//  2. SSHMGR_MASTERKEY_HEX env (hex) — dev/scripting/tests.
//  3. FileKeyProvider at SSHMGR_FILEKEY_PATH (or default fixed path) — the
//     last resort. ErrNotFound here → "vault locked".
//
// kp may be nil (tests pass nil to exercise only the env + file tiers).
func resolveMasterKey(kp store.KeyProvider) ([]byte, error) {
	if kp != nil {
		mk, err := kp.Get()
		if err == nil {
			return mk, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			// HARD FAIL: decrypt failure / FS error. Do NOT degrade to env /
			// plaintext. spec §5.6 (review codex#8/pi#8).
			return nil, fmt.Errorf("master key present but unreadable: %w — if the OS user password was admin-reset, restore the vault from a backup (see docs/backup-restore.md)", err)
		}
		// ErrNotFound → fall through to env / file tiers.
	}
	if h := os.Getenv("SSHMGR_MASTERKEY_HEX"); h != "" {
		mk, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("SSHMGR_MASTERKEY_HEX: %w", err)
		}
		return mk, nil
	}
	fp := fileKeyProvider()
	mk, err := fp.Get()
	if err == nil {
		return mk, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return nil, errors.New("vault locked: run `ssh-manager unlock` to populate the master key")
}

// fileKeyProvider builds the FileKeyProvider used as the last-resort tier (and
// the default production kp via OpenStore(store.FileKeyProvider{})). The path
// is taken from SSHMGR_FILEKEY_PATH (test override / explicit config) or left
// empty so the provider falls back to the program-fixed path
// (paths.MasterKeyPath — spec §3.1/§4.2).
func fileKeyProvider() store.FileKeyProvider {
	return store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")}
}

// AuthForServer resolves a server's stored credential into an SSH auth method.
func AuthForServer(st *store.Store, srv *models.Server) (ssh.AuthMethod, error) {
	cred, err := st.GetCredential(srv.CredentialID)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, fmt.Errorf("credential %s not found", srv.CredentialID)
	}
	switch srv.AuthMethod {
	case models.AuthPassword:
		return sshbroker.PasswordAuth(string(cred.Secret)), nil
	case models.AuthPrivateKey:
		return sshbroker.PrivateKeyAuth(cred.Secret, cred.Passphrase)
	}
	return nil, fmt.Errorf("unknown auth method %q", srv.AuthMethod)
}
