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
// Order: SSHMGR_MASTERKEY_HEX env (dev/CLI scripting) → OS keychain (production/MCP).
// Returns a "vault locked" error if neither yields a key (e.g. MCP spawned before any `unlock`).
func OpenStore() (*store.Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	mk, err := resolveMasterKey()
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

func resolveMasterKey() ([]byte, error) {
	if hexKey := os.Getenv("SSHMGR_MASTERKEY_HEX"); hexKey != "" {
		return hex.DecodeString(hexKey)
	}
	kp := store.KeyringKeyProvider{}
	mk, err := kp.Get()
	if err == nil {
		return mk, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("vault locked: run `ssh-manager unlock` to populate the keychain (the MCP server cannot prompt)")
	}
	return nil, fmt.Errorf("keychain unavailable: %w", err)
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
