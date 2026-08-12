//go:build windows

// Package cli: cache-DEK key provider seam (Windows).
//
// dekProvider returns the KeyProvider holding the cache DEK (the symmetric key
// guarding cache.bin). On Windows the cache DEK lives in a DPAPI-encrypted file
// at dpapiCacheDekPath() — the SAME path the v0.2.0 → DPAPI migration writes
// (migrate_windows.go, spec §5.7). This closes the Plan 14 T5 review F1 gap:
// before this file existed the migration wrote cache-dek.key but the reader
// (loadOrCreateDEK) still consulted the OS keychain slot, so the migrated DEK
// was orphaned and a subsequent `cache pull` generated a fresh DEK.
//
// Keeping the read path and write path pinned to dpapiCacheDekPath() (single
// helper) guarantees they cannot drift — a reader/writer path mismatch is now
// impossible by construction.
//
// DpapiKeyProvider works across RDP / sshd / Task-Scheduler sessions (spec 12
// spike FINDING 9), so `cache pull` invoked by a served MCP broker / Task
// Scheduler job reads the DEK the same as an interactive `cache pull`.
//
// Unix builds see cache_dek_unix.go instead (env-aware keychain slot, unchanged
// from before — Plan 14 does not move the cache DEK medium on Unix).
package cli

import "ssh-manager-mcp/internal/store"

// dekProvider returns the cache-DEK KeyProvider (Windows: DpapiKeyProvider at
// the migration's cache-dek.key path). A package seam so tests inject a fake
// (MemKeyProvider) without touching the real DPAPI file. Tests swap this var
// directly (see cache_test.go withDEK).
var dekProvider = func() store.KeyProvider {
	return &store.DpapiKeyProvider{Path: dpapiCacheDekPath()}
}
