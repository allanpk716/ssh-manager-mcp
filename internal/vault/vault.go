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
// The platform master-key KeyProvider (Windows: DpapiKeyProvider; Unix:
// KeyringKeyProvider) is INJECTED by the caller — the vault package is
// OS-agnostic and must not import cli (cli imports vault; that would be a
// cycle). The cli/keychain_* build-tag seam picks the right provider per
// platform and passes it here.
//
// resolveMasterKey order (spec §5.6):
//  1. SSHMGR_MASTERKEY_HEX env (dev/CLI scripting)
//  2. platform KeyProvider (the injected kp)
//  3. FileKeyProvider fallback (keychain-less envs: CI / containers / headless)
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

// resolveMasterKey resolves the vault master key in the documented 3-tier order
// (spec §5.6):
//
//  1. SSHMGR_MASTERKEY_HEX env (dev/scripting) — highest priority.
//  2. Platform KeyProvider (kp — Windows: DPAPI; Unix: keychain):
//     - ErrNotFound → continue to step 3 (legitimate first-run / keychain-less
//     env).
//     - ANY OTHER error (DPAPI decrypt failure / keychain service down) →
//     HARD FAIL. Never silent-fall-through to plaintext FileKeyProvider.
//  3. FileKeyProvider fallback (CI / containers / headless without keychain):
//     - ErrNotFound → "vault locked".
//
// The platform KeyProvider is a parameter (not constructed here) so vault stays
// OS-agnostic and can be unit-tested by injecting a fake KeyProvider.
func resolveMasterKey(kp store.KeyProvider) ([]byte, error) {
	if hexKey := os.Getenv("SSHMGR_MASTERKEY_HEX"); hexKey != "" {
		return hex.DecodeString(hexKey)
	}
	mk, err := kp.Get()
	if err == nil {
		return mk, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		// HARD FAIL: decrypt failure / service unavailable. Do NOT degrade to
		// plaintext. spec §5.6 (review codex#8/pi#8).
		return nil, fmt.Errorf("master key present but unreadable: %w — if the OS user password was admin-reset, restore the vault from a backup (see docs/backup-restore.md)", err)
	}
	// ErrNotFound → fall through to FileKeyProvider (legitimate keychain-less env).
	fp := fileKeyProvider()
	if mk, err := fp.Get(); err == nil {
		return mk, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return nil, errors.New("vault locked: run `ssh-manager unlock` to populate the master key")
}

// fileKeyProvider builds the FileKeyProvider fallback. The path is taken from
// SSHMGR_FILEKEY_PATH (test override / explicit config) or left empty so the
// provider falls back to UserConfigDir/ssh-manager/master.key.plain.
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
