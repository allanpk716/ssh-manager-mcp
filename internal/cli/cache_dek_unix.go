//go:build !windows

// Package cli: cache-DEK key provider seam (Unix).
//
// dekProvider returns the KeyProvider holding the cache DEK (the symmetric key
// guarding cache.bin, the offline read-only snapshot from Plan 12). On Unix
// the cache DEK is a plaintext file at the program-fixed paths.CacheDekPath()
// (<vaultDir>/cache-dek.key), spec §3.1 / §4.2 (xcheck consensus A).
//
// Plan 16 T4 replaced the previous KeyringKeyProvider (read on every call
// from the SSHMGR_KEYRING_SERVICE env var). That env-aware path existed ONLY
// so the eval harness's spawned `ssh-manager mcp` child could target an
// isolated keychain service without a recompile — but the eval broker reads
// the master key, not the cache DEK, so the env coupling was vestigial after
// Plan 12 wired the cache to its own DEK. Plaintext-at-fixed-path puts the
// cache DEK on the same footing as master.key: L1+ threat model, single trust
// root (the vault dir), and SSHMGR_KEYRING_SERVICE is no longer consulted.
//
// A package seam so tests inject a fake (MemKeyProvider) without touching the
// real file. Tests swap this var directly (see cache_test.go withDEK).
//
// Windows builds see cache_dek_windows.go instead (same FileKeyProvider medium,
// build-tag split kept only so the seam stays a single var per build).
package cli

import (
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

// dekProvider returns the cache-DEK KeyProvider (Unix: FileKeyProvider at the
// program-fixed cache-dek.key path). A package seam so tests inject a fake
// (MemKeyProvider) instead of touching the real file. Tests swap this var
// directly (see cache_test.go withDEK).
//
// cache-dek.key uses paths.CacheDekPath() directly — it does NOT consult
// SSHMGR_FILEKEY_PATH (that env var redirects ONLY master.key). This keeps the
// cache DEK pinned to the vault dir, decoupled from master-key test overrides.
var dekProvider = func() store.KeyProvider {
	pth, err := paths.CacheDekPath()
	if err != nil || pth == "" {
		return &store.FileKeyProvider{} // last-resort default (test env with no fixed path)
	}
	return &store.FileKeyProvider{Path: pth}
}
