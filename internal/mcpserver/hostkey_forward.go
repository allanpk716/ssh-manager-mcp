package mcpserver

import (
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// HostKeyStoreBinder constructs the host-key store for the cache broker from
// the holder's current-store resolver (Plan 48 §2.2). The indirection is the
// real-time-resolution seam: the binder cannot be invoked before RunStdioCache
// has built the hot-reload holder, so the cache-mode HostKeyStore is bound to
// the LIVE generation resolver rather than any store pointer. cli supplies
// clientops.ForwardingHostKeys(fwd); nil = no override — the TOFU path binds
// the plain store (the pre-Plan-48 read-only cache behavior, and the vault
// mode default).
type HostKeyStoreBinder = func(cur func() *store.Store) sshbroker.HostKeyStore

// hostKeyStoreFor resolves the optional host-key store override threaded
// through the dialing tool functions (Plan 48 §2.2): omitted/nil = the store
// itself — the vault mode and every pre-Plan-48 caller, byte-identical
// behavior; cache mode passes the forwarding wrapper built by the RunStdioCache
// binder. The trailing-variadic shape keeps the ≈100 existing test call sites
// (which all mean "the store itself") untouched; the override is
// production-only.
func hostKeyStoreFor(st *store.Store, hk []sshbroker.HostKeyStore) sshbroker.HostKeyStore {
	if len(hk) > 0 && hk[0] != nil {
		return hk[0]
	}
	return st
}
