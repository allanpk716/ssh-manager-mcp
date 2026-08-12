//go:build !windows

// Package cli: cache-DEK key provider seam (Unix).
//
// dekProvider returns the KeyProvider holding the cache DEK (the symmetric key
// guarding cache.bin). On Unix the cache DEK lives in the OS keychain slot
// "cache-dek" under SSHMGR_KEYRING_SERVICE (env-aware — empty → production
// default "ssh-manager"), exactly as before Plan 14. Plan 14 does not move the
// cache DEK medium on Unix (spec §3.3: Unix keeps KeyringKeyProvider; same
// medium before and after), so there is no v0.2.0 migration for the DEK here.
//
// SSHMGR_KEYRING_SERVICE is read on EVERY call (NOT captured at init) so the
// eval harness's spawned `ssh-manager mcp` child (mcp.json sets the env var to
// "ssh-manager-eval") targets an isolated keychain service without a recompile
// — preserves the eval isolation contract (Plan 12 CF1, same shape as
// envKeyringKeyProvider in keychain_unix.go).
//
// Windows builds see cache_dek_windows.go instead (DpapiKeyProvider at the
// migration's cache-dek.key path).
package cli

import (
	"os"

	"ssh-manager-mcp/internal/store"
)

// dekProvider returns the cache-DEK KeyProvider (Unix: env-aware keychain slot
// "cache-dek"). A package seam so tests inject a fake (MemKeyProvider) instead
// of touching the real OS keychain. Tests swap this var directly (see
// cache_test.go withDEK).
var dekProvider = func() store.KeyProvider {
	return &store.KeyringKeyProvider{Service: os.Getenv("SSHMGR_KEYRING_SERVICE"), User: "cache-dek"}
}
