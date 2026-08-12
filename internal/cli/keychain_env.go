// Package cli: keychain seam.
//
// keychain is the master-key source (default real OS keychain; tests override).
// It is the single point the rest of the package uses to fetch/store the vault
// master key.
//
// The default implementation reads SSHMGR_KEYRING_SERVICE on EVERY Get/Set so
// spawned subprocesses (the eval harness's `ssh-manager mcp` child, or an
// mcp.json env override) can point at an isolated keychain service WITHOUT a
// recompile. This preserves the eval isolation contract (internal/eval/broker.go
// sets SSHMGR_KEYRING_SERVICE=ssh-manager-eval via mcp.json) — the prior
// resolveMasterKey did the same env read inline.
//
// T4 (Plan 14) splits this file by build-tag:
//   - keychain_unix.go (this env-aware KeyringKeyProvider, kept verbatim)
//   - keychain_windows.go (DpapiKeyProvider — DPAPI doesn't use keychain service
//     names, so SSHMGR_KEYRING_SERVICE is irrelevant there).
package cli

import (
	"os"

	"ssh-manager-mcp/internal/store"
)

// envKeyringKeyProvider forwards to a KeyringKeyProvider whose Service is read
// from SSHMGR_KEYRING_SERVICE at each call (empty → production default
// "ssh-manager"). Value receiver so the seam var is a plain value (tests that
// swap keychain for a fake still work).
type envKeyringKeyProvider struct{}

func (envKeyringKeyProvider) Get() ([]byte, error) {
	return store.KeyringKeyProvider{Service: os.Getenv("SSHMGR_KEYRING_SERVICE")}.Get()
}

func (envKeyringKeyProvider) Set(mk []byte) error {
	return store.KeyringKeyProvider{Service: os.Getenv("SSHMGR_KEYRING_SERVICE")}.Set(mk)
}
