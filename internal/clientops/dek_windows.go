//go:build windows

// Package clientops: cache-DEK key provider seam (Windows).
//
// DekProvider returns the KeyProvider holding the cache DEK (the symmetric key
// guarding cache.bin, the offline read-only snapshot from Plan 12). On Windows
// the cache DEK is a plaintext file at the program-fixed paths.CacheDekPath()
// (<vaultDir>/cache-dek.key), spec §3.1 / §4.2 (xcheck consensus A).
//
// Plan 16 T4 replaced the previous DpapiKeyProvider. The pre-T4 reader relied
// on the same DPAPI custody model as the master key, and the old
// cache_dek_windows.go self-described cross-session reads by a served MCP
// broker / Task Scheduler job — the SAME unreliable custody the master-key
// DPAPI path was retired for in Plan 15. Plaintext-at-fixed-path puts the
// cache DEK on the same footing as master.key: L1+ threat model, single trust
// root (the vault dir).
//
// A package seam so tests inject a fake (MemKeyProvider) without touching the
// real file. Tests swap this var directly (see withDEK in the clientops and
// cli test suites).
//
// Unix builds see dek_unix.go instead (same FileKeyProvider medium, build-tag
// split kept only so the seam stays a single var per build).
package clientops

import (
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

// DekProvider returns the cache-DEK KeyProvider (Windows: FileKeyProvider at
// the program-fixed cache-dek.key path). A package seam so tests inject a fake
// (MemKeyProvider) without touching the real file. Tests swap this var
// directly (see withDEK).
//
// cache-dek.key uses paths.CacheDekPath() directly — it does NOT consult
// SSHMGR_FILEKEY_PATH (that env var redirects ONLY master.key). This keeps the
// cache DEK pinned to the vault dir, decoupled from master-key test overrides.
var DekProvider = func() store.KeyProvider {
	pth, err := paths.CacheDekPath()
	if err != nil || pth == "" {
		return &store.FileKeyProvider{} // last-resort default (test env with no fixed path)
	}
	return &store.FileKeyProvider{Path: pth}
}
