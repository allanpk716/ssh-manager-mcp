package mcpserver

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// RunStdio resolves the token to a project+profile, builds the scoped server, and runs it over stdio.
// Returns an error if the store is locked or the token is unknown (caller prints to stderr + exits).
//
// The platform master-key KeyProvider is INJECTED by the caller (the cli/keychain
// seam) so this package stays OS-agnostic and doesn't import cli. vault.OpenStore
// resolves env → kp → FileProvider (3-tier, spec §5.6).
func RunStdio(token string, kp store.KeyProvider) error {
	st, err := vault.OpenStore(kp)
	if err != nil {
		return err
	}
	defer st.Close()
	project, err := st.VerifyToken(token)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("invalid or unknown token")
	}
	srv, tunnels, err := NewServer(st, project.ProfileID, project.ID)
	if err != nil {
		return err
	}
	// MCP-shutdown teardown: when the agent disconnects (stdin closes) srv.Run
	// returns and the deferred CloseAll tears down every open forward_port
	// tunnel — listener + owning ssh.Client — so the process exits with no
	// leaked SSH connections. The idle sweeper goroutine is also stopped. (The
	// go-sdk MCP server has no per-server shutdown hook on the Run path — its
	// session onClose fires per-session; we teardown at Run-return instead,
	// which is the single-session stdio case.)
	defer tunnels.CloseAll()
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

// hydrateCacheStore builds a fresh temporary read-only store from snap and
// verifies token against it. The caller owns closing the store and removing
// tmpPath (cacheStoreHolder registers both). Shared by initial startup and
// every hot rebuild, so the two paths CANNOT drift.
func hydrateCacheStore(token string, snap *store.Snapshot, auditFile *os.File) (*store.Store, *models.Project, string, error) {
	mk, err := store.GenerateMasterKey() // throwaway key: creds re-sealed per hydration
	if err != nil {
		return nil, nil, "", err
	}
	tmp, err := os.CreateTemp("", "sshmgr-cache-*.db")
	if err != nil {
		return nil, nil, "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	st, err := store.Open(tmpPath, mk)
	if err != nil {
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	if err := st.ImportSnapshot(snap); err != nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	st.SetReadOnly(auditFile) // AFTER ImportSnapshot: mutations → ErrReadOnly
	project, err := st.VerifyToken(token)
	if err != nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	if project == nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", fmt.Errorf("invalid or unknown token")
	}
	return st, project, tmpPath, nil
}

// cacheStoreHolder owns the hot-reloading read-only store behind mcp --cache.
// Swapped-out stores are NOT closed on swap: the SDK dispatches tool calls on
// separate goroutines, so an in-flight call may still hold the old pointer —
// closing it would surface "sql: database is closed" as a tool error. They are
// registered in stores/tmpPaths and torn down once at process exit instead
// (rebuilds are rare; the leak is bounded and harmless).
type cacheStoreHolder struct {
	reload    func() (*store.Snapshot, bool, error)
	token     string
	auditFile *os.File
	profileID string

	mu       sync.Mutex // serializes rebuilds
	cur      atomic.Pointer[store.Store]
	lastSnap *store.Snapshot  // guarded by mu — last snapshot successfully hydrated
	stores   []*store.Store // every hydrated store, closed once in cleanup
	tmpPaths []string       // every temp db, removed once in cleanup
}

// Current returns the store to serve THIS tool call from, rebuilding first if
// the reload callback reports a change. Every failure path keeps serving the
// previous store — Lazy revocation semantics: a session outlives its token
// until the next spawn.
func (h *cacheStoreHolder) Current() *store.Store {
	if h.reload == nil {
		return h.cur.Load()
	}
	// Consult + memoize + hydrate must all happen under h.mu: consulting the
	// reloader outside the lock allows a goroutine holding a stale snapshot
	// pointer to be preempted, another goroutine to hydrate a NEWER change,
	// and the stale one to then re-hydrate over it — silently serving an
	// older snapshot until the next disk change. Serializing the whole
	// sequence prevents out-of-order stale serving.
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, changed, err := h.reload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager: cache reload check failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if !changed {
		return h.cur.Load()
	}
	if snap == nil {
		// changed=true with a nil snapshot is a broken reloader; hydrateCacheStore
		// would nil-deref in ImportSnapshot. Log + keep serving the old store.
		fmt.Fprintf(os.Stderr, "ssh-manager: cache reload reported a change with a nil snapshot (keeping current snapshot)\n")
		return h.cur.Load()
	}
	// A concurrent rebuild may have consumed this exact snapshot already (the
	// reloader advances its baseline on a successful load, but a one-shot
	// change report must not be dropped by a recheck, and must not trigger a
	// second hydration of the same snapshot either): memoize the pointer of
	// the last successfully hydrated snapshot. Pointer identity is sound — a
	// real reloader mints a fresh *Snapshot for every genuine change.
	if snap == h.lastSnap {
		return h.cur.Load()
	}
	st, project, tmpPath, err := hydrateCacheStore(h.token, snap, h.auditFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager: cache hot-reload failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if project.ProfileID != h.profileID {
		// The owner rebound the project to a different profile mid-session; the
		// tool closures still scope by the startup profileID, so serving the new
		// store would show the wrong set. Keep the old store + log.
		fmt.Fprintf(os.Stderr, "ssh-manager: cache snapshot changed the project's profile (keeping current snapshot to preserve scoping)\n")
		st.Close()
		os.Remove(tmpPath)
		return h.cur.Load()
	}
	h.lastSnap = snap
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmpPath)
	h.cur.Store(st) // old store intentionally left open (in-flight calls)
	return st
}

// cleanup closes every hydrated store and removes every temp db. Called once,
// deferred from RunStdioCache.
//
// Reading stores/tmpPaths without the mutex is safe: cleanup is invoked exactly
// once via defer after srv.Run returns, at which point no in-flight tool calls
// remain to race a rebuild.
func (h *cacheStoreHolder) cleanup() {
	for _, s := range h.stores {
		s.Close()
	}
	h.stores = nil
	for _, p := range h.tmpPaths {
		os.Remove(p)
	}
	h.tmpPaths = nil
}

// RunStdioCache hydrates a Snapshot into a temporary read-only store, verifies the SAME
// project token against the cached projects (iron rule + profile scoping intact offline), and
// runs the broker over stdio — identical agent surface to RunStdio. Offline audit lands in
// auditPath (a JSONL sidecar); every mutation is refused (ErrReadOnly). Unknown host keys are
// rejected (SaveHostKey returns ErrReadOnly → HostKeyTOFU fails closed). The temp store is
// deleted on exit; creds in it are sealed under a throwaway master key.
//
// reload != nil enables hot-reload: before every tool call the callback is consulted
// ((snap,true,nil) = rebuild; (nil,false,nil) = unchanged; error = keep serving the old
// store). Each genuine change MUST be reported with a FRESH *Snapshot pointer (pointer
// identity is what dedupes concurrent rebuilds); re-reporting the same pointer with
// changed=true is skipped. reload == nil disables it. Swapped-out stores stay open until
// process exit (in-flight SDK tool calls may hold the old pointer); cacheStoreHolder.cleanup
// tears every hydrated store down once.
//
// Agent-surface invariant: the broker reads the cache via the exact same
// list_servers / exec_command / download_file / upload_file / forward_port / close_port
// tools, gated by the SAME profile scoping (profileID from the verified project) and attributing
// audit to the SAME project id. The only difference from RunStdio is that the store is read-only
// (mutations refused) and audit is sidecar'd (per-machine, single-direction, zero-merge).
func RunStdioCache(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error)) error {
	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer af.Close()

	h := &cacheStoreHolder{reload: reload, token: token, auditFile: af}
	st, project, tmpPath, err := hydrateCacheStore(token, snap, af)
	if err != nil {
		return err
	}
	h.cur.Store(st)
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmpPath)
	defer h.cleanup()

	srv, tunnels, err := NewServerFromSource(h.Current, project.ProfileID, project.ID)
	if err != nil {
		return err
	}
	defer tunnels.CloseAll()
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}
