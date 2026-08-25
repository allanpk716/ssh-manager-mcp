package mcpserver

import (
	"fmt"
	"log"
	"sync"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// forwardIdleTimeout is how long a tunnel may stay IDLE before the sweeper
// reaps it (spec §3): lastActivity advances on real traffic via the Tunnel's
// onActivity hook (30s-throttled) wired into Touch — a busy tunnel survives
// indefinitely; an idle one dies after 10 min.
const forwardIdleTimeout = 10 * time.Minute

// forwardSweepInterval is the ticker period for the tunnel sweeper goroutine.
// One minute is fine-grained enough that a tunnel is reaped within ~10–11 min
// of creation, and coarse enough that the sweeper is idle work in steady state.
const forwardSweepInterval = 1 * time.Minute

// managedTunnel is a forward held by the TunnelManager. The client is the
// long-lived SSH connection the tunnel dials through; it is closed when the
// tunnel is unregistered (Close/SweepIdle/CloseAll). This is the first
// stateful broker resource — it persists across MCP tool calls, held in-process
// until close_port, the tunnel sweeper, or MCP shutdown tears it down.
type managedTunnel struct {
	tunnel       *sshbroker.Tunnel
	client       *sshbroker.Client
	lastActivity time.Time
	meta         TunnelMeta
}

// TunnelMeta carries the registry-mirror fields an Open must persist
// (spec §6). ListenHost is the canonical form.
type TunnelMeta struct {
	ProjectID  string
	ServerID   string
	Remote     string
	ListenHost string
}

// TunnelManager holds the open forwards for the MCP server process, keyed by
// tunnel id (the sshbroker.Tunnel.ID UUID). forward_port opens; close_port
// closes; the tunnel sweeper (SweepIdle) closes tunnels whose lastActivity is
// older than forwardIdleTimeout. All state is in-process and dies with the MCP
// server process — CloseAll (wired to MCP shutdown in RunStdio) is the clean
// teardown path; process exit reclaims any residual fds as a backstop.
//
// Resource-cleanup contract (the load-bearing concern for the first stateful
// broker op): Close, SweepIdle, and CloseAll each close BOTH the tunnel
// listener (tunnel.Close — idempotent) AND the owning *sshbroker.Client
// (client.Close), then delete the entry from the registry. Tests verify no leak
// by capturing the client ref before Close and asserting a follow-up op errors.
//
// The struct is safe for concurrent use (every method takes mu). The tunnel
// sweeper goroutine (started via StartSweeper, stopped via CloseAll) is the
// only background user; Open/Close/SweepIdle are driven by the MCP tool
// handlers on the tool-call goroutine.
//
// Mirror pipeline (Plan 35 spec §6): with a store attached (AttachStore),
// Open INSERTs a tunnel_registry row (fail-the-Open on failure) and every
// teardown path (Close/SweepIdle/CloseAll/closeAllTunnels) mirror-deletes it;
// failed DELETEs land in pendingDeletes and are retried by the control loop
// (Task 4). A bare manager (no AttachStore — existing tests) stays fully
// inert: no mirror writes, no control duties.
type TunnelManager struct {
	mu             sync.Mutex
	tunnels        map[string]*managedTunnel
	quit           chan struct{}
	startOnce      sync.Once
	stopOnce       sync.Once
	wg             sync.WaitGroup
	storeFn        func() *store.Store // nil = bare manager (tests): control loop no-ops
	projectID      string
	pendingDeletes map[string]struct{} // mirror DELETEs that failed; retried each control tick (spec §6)

	// Control-loop discipline counters (spec §4): consecutive failed ticks,
	// process-local memory (a restart zeroes them). Only the control
	// goroutine (sweepLoop) touches them — no lock needed.
	renewalFail   int // lease renewal incomplete (fail-the-renewal, >8 ⇒ close all)
	cascadeFail   int // cascade GetProject read failures (>8 ⇒ close all)
	whitelistFail int // whitelist read failures (>8 ⇒ close non-loopback only)
}

// NewTunnelManager returns an empty TunnelManager. The tunnel-sweeper goroutine
// is NOT started here — call StartSweeper (NewServer does this in production)
// to launch it. Tests that want hermetic Open/Close/SweepIdle timing may omit
// StartSweeper and drive SweepIdle directly.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels:        map[string]*managedTunnel{},
		quit:           make(chan struct{}),
		pendingDeletes: map[string]struct{}{},
	}
}

// AttachStore wires the live store source + project scope for mirror writes
// and the control loop (spec §4 接线点). Called by NewServerFromSource; bare
// managers (existing tests) stay control-inert.
func (m *TunnelManager) AttachStore(storeFn func() *store.Store, projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storeFn = storeFn
	m.projectID = projectID
}

// StartSweeper launches the tunnel-sweeper goroutine at most once. It is a no-op
// after CloseAll. NewServer calls this; tests usually don't (they call
// SweepIdle directly for deterministic timing).
func (m *TunnelManager) StartSweeper() {
	m.startOnce.Do(func() {
		m.wg.Add(1)
		go m.sweepLoop()
	})
}

// sweepLoop is the tunnel sweeper AND the control loop: every
// forwardSweepInterval it calls SweepIdle, and every controlInterval it runs
// one control tick (Plan 35 spec §4 — the tick is test-driven via
// runControlTick, this is only the production heartbeat). Exits when quit is
// closed (CloseAll). Holds one wg ticket for its lifetime so CloseAll can
// Wait for a clean shutdown.
func (m *TunnelManager) sweepLoop() {
	defer m.wg.Done()
	idle := time.NewTicker(forwardSweepInterval)
	defer idle.Stop()
	ctrl := time.NewTicker(controlInterval)
	defer ctrl.Stop()
	for {
		select {
		case <-idle.C:
			m.SweepIdle()
		case <-ctrl.C:
			m.runControlTick()
		case <-m.quit:
			return
		}
	}
}

// Open mirrors a tunnel into tunnel_registry FIRST, then registers it (with
// its owning client) in the in-memory map. lastActivity seeds to now; the
// activity hook keeps it fresh. Insert-before-register is load-bearing: the
// kill-domain membership (the registry row) must exist before the tunnel
// becomes reachable/reported — registering first opens a window where a
// concurrent control-tick heartbeat snapshots the in-memory id with no DB row
// (zero-row renew ⇒ self-close kills the brand-new tunnel) and then the
// INSERT lands, leaving Open "successful" over a dead tunnel + a ghost row
// lingering ≤30min in `tunnels ls`. The reverse window is benign: the
// heartbeat only iterates in-memory ids (nothing to renew for a row whose
// owner hasn't registered yet); a kill order seeing the fresh row just stays
// pending and catches the tunnel on the next tick.
// fail-the-Open (spec §6): when a writable store is attached and the registry
// INSERT fails, the tunnel is closed immediately and the error is returned —
// a tunnel that cannot be killed is not allowed to exist. Nothing was
// registered yet, so there is nothing to unregister.
func (m *TunnelManager) Open(t *sshbroker.Tunnel, c *sshbroker.Client, meta TunnelMeta) (string, error) {
	m.mu.Lock()
	storeFn := m.storeFn
	m.mu.Unlock()
	if storeFn != nil {
		st := storeFn()
		if st != nil && !st.IsReadOnly() {
			now := time.Now().Unix()
			err := st.InsertTunnelRegistry(store.TunnelRegistryRow{
				TunnelID: t.ID, ProjectID: meta.ProjectID, ServerID: meta.ServerID,
				Remote: meta.Remote, LocalAddr: t.LocalAddr(), ListenHost: meta.ListenHost,
				OpenedAt: now, LastRenewed: now,
			})
			if err != nil {
				// fail-the-Open: the tunnel was never registered — close the
				// raw tunnel + client, nothing to unregister.
				_ = t.Close()
				_ = c.Close()
				return "", fmt.Errorf("tunnel registry mirror failed, tunnel closed (fail-the-Open): %w", err)
			}
		}
	}
	m.mu.Lock()
	m.tunnels[t.ID] = &managedTunnel{tunnel: t, client: c, lastActivity: time.Now(), meta: meta}
	m.mu.Unlock()
	return t.ID, nil
}

// mirrorDelete removes registry rows for ids; on writable-store failure the
// ids land in the per-tick retry set (ghost rows also self-heal via the
// 30-min GC — spec §6). Store I/O runs WITHOUT m.mu (no lock nesting with
// sqlite); only the retry-set mutation re-takes the lock.
func (m *TunnelManager) mirrorDelete(ids []string) {
	if len(ids) == 0 {
		return
	}
	m.mu.Lock()
	storeFn := m.storeFn
	m.mu.Unlock()
	if storeFn == nil {
		return
	}
	st := storeFn()
	if st == nil || st.IsReadOnly() {
		return // offline hydrated store: no mirror to maintain
	}
	if err := st.DeleteTunnelRegistry(ids); err != nil {
		m.mu.Lock()
		for _, id := range ids {
			m.pendingDeletes[id] = struct{}{}
		}
		m.mu.Unlock()
	}
}

// retryPendingDeletes drains failed mirror DELETEs (control tick duty 5).
// Only the snapshotted ids are drained on success — an id that failed into
// the set concurrently with the draining DELETE keeps its retry.
func (m *TunnelManager) retryPendingDeletes() {
	m.mu.Lock()
	if len(m.pendingDeletes) == 0 || m.storeFn == nil {
		m.mu.Unlock()
		return
	}
	storeFn := m.storeFn
	ids := make([]string, 0, len(m.pendingDeletes))
	for id := range m.pendingDeletes {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	st := storeFn()
	if st == nil || st.IsReadOnly() {
		return
	}
	if err := st.DeleteTunnelRegistry(ids); err == nil {
		m.mu.Lock()
		for _, id := range ids {
			delete(m.pendingDeletes, id)
		}
		m.mu.Unlock()
	}
}

// tunnelIDsLocked returns the live tunnel ids. Caller must hold m.mu (control
// loop heartbeat duty — spec §4).
func (m *TunnelManager) tunnelIDsLocked() []string {
	ids := make([]string, 0, len(m.tunnels))
	for id := range m.tunnels {
		ids = append(ids, id)
	}
	return ids
}

// Touch refreshes a tunnel's lastActivity to now, deferring tunnel-sweeper
// reaping. MVP callers don't need it (idle = open-duration); it's exposed so a
// future forward_port refresh or per-byte activity hook can keep a long-lived
// tunnel alive without re-opening. Returns true if the tunnel was present.
func (m *TunnelManager) Touch(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mt, ok := m.tunnels[id]
	if !ok {
		return false
	}
	mt.lastActivity = time.Now()
	return true
}

// Close tears down a tunnel by id: closes the local listener (tunnel.Close,
// idempotent) AND the owning *sshbroker.Client (client.Close), then removes it
// from the registry and mirror-deletes its tunnel_registry row. Returns false
// if no such tunnel (close_port surfaces this as a tool error).
// Resource-cleanup order: listener first (stop accepting new forwards), then
// the SSH client (tear down the underlying connection + its in-flight
// direct-tcpip channels). The mirror DELETE runs outside m.mu.
func (m *TunnelManager) Close(id string) bool {
	m.mu.Lock()
	mt, ok := m.tunnels[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	_ = mt.tunnel.Close()
	_ = mt.client.Close()
	delete(m.tunnels, id)
	m.mu.Unlock()
	m.mirrorDelete([]string{id})
	return true
}

// SweepIdle closes every tunnel whose lastActivity is older than
// forwardIdleTimeout and returns the ids it reaped. Called periodically by the
// sweeper goroutine; also callable directly (tests). Same per-tunnel cleanup
// as Close (listener + client + registry delete), plus one batched mirror
// DELETE outside m.mu for all reaped ids.
func (m *TunnelManager) SweepIdle() []string {
	m.mu.Lock()
	var closed []string
	for id, mt := range m.tunnels {
		if time.Since(mt.lastActivity) > forwardIdleTimeout {
			_ = mt.tunnel.Close()
			_ = mt.client.Close()
			delete(m.tunnels, id)
			closed = append(closed, id)
		}
	}
	m.mu.Unlock()
	m.mirrorDelete(closed)
	return closed
}

// closeAllTunnels tears down every tunnel + mirror rows WITHOUT stopping the
// sweeper goroutine (CloseAll owns the quit path). Returns how many were
// closed. Used by the control loop (kill --project / cascade /
// fail-the-renewal) — spec §4.
func (m *TunnelManager) closeAllTunnels(reason string) int {
	m.mu.Lock()
	ids := make([]string, 0, len(m.tunnels))
	for id, mt := range m.tunnels {
		_ = mt.tunnel.Close()
		_ = mt.client.Close()
		delete(m.tunnels, id)
		ids = append(ids, id)
	}
	m.mu.Unlock()
	m.mirrorDelete(ids)
	if len(ids) > 0 {
		log.Printf("%s — closed %d tunnels", reason, len(ids))
	}
	return len(ids)
}

// CloseAll tears down every live tunnel (listener + owning client), mirror-
// deletes their registry rows, and stops the tunnel-sweeper goroutine if it
// was started. This is the MCP-shutdown path: RunStdio defers it so that when
// the agent disconnects / stdin closes, every open forward is reaped cleanly
// (no leaked ssh.Clients or listeners stranding the process). Safe to call on
// a manager with no tunnels (no-op) and idempotent across Close/CloseAll
// combinations.
func (m *TunnelManager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.tunnels))
	for id, mt := range m.tunnels {
		_ = mt.tunnel.Close()
		_ = mt.client.Close()
		delete(m.tunnels, id)
		ids = append(ids, id)
	}
	m.mu.Unlock()
	m.mirrorDelete(ids)
	// Signal the sweeper loop to exit (no-op if never started or already stopped)
	// and wait for it to return so a process exit doesn't race the ticker fire.
	m.stopOnce.Do(func() { close(m.quit) })
	m.wg.Wait()
}
