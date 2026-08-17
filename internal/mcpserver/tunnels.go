package mcpserver

import (
	"sync"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
)

// forwardIdleTimeout is how long a tunnel lives before the sweeper reaps it.
// NOTE: the signal is CREATION time (lastActivity = time.Now() in Open) —
// Touch(id) exists to refresh it but has NO production caller today, so a
// tunnel dies ~10 min after creation even under continuous traffic. Making
// this activity-aware (wiring Touch) is a tracked backlog item (see
// docs/backlog.md). Default 10 min per Plan 6 §T4.
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
type TunnelManager struct {
	mu        sync.Mutex
	tunnels   map[string]*managedTunnel
	quit      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewTunnelManager returns an empty TunnelManager. The tunnel-sweeper goroutine
// is NOT started here — call StartSweeper (NewServer does this in production)
// to launch it. Tests that want hermetic Open/Close/SweepIdle timing may omit
// StartSweeper and drive SweepIdle directly.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels: map[string]*managedTunnel{},
		quit:    make(chan struct{}),
	}
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

// sweepLoop is the tunnel sweeper: every forwardSweepInterval it calls SweepIdle.
// Exits when quit is closed (CloseAll). Holds one wg ticket for its lifetime so
// CloseAll can Wait for a clean shutdown.
func (m *TunnelManager) sweepLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(forwardSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.SweepIdle()
		case <-m.quit:
			return
		}
	}
}

// Open registers a tunnel + its owning client and returns the tunnel id the
// caller should hand back to the agent (close_port's input). The client stays
// open for the tunnel's life — the TunnelManager closes it on
// Close/SweepIdle/CloseAll. lastActivity is seeded to now (the MVP idle
// signal = open time; Touch refreshes it).
func (m *TunnelManager) Open(t *sshbroker.Tunnel, c *sshbroker.Client) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tunnels[t.ID] = &managedTunnel{tunnel: t, client: c, lastActivity: time.Now()}
	return t.ID
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
// from the registry. Returns false if no such tunnel (close_port surfaces this
// as a tool error). Resource-cleanup order: listener first (stop accepting new
// forwards), then the SSH client (tear down the underlying connection + its
// in-flight direct-tcpip channels).
func (m *TunnelManager) Close(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mt, ok := m.tunnels[id]
	if !ok {
		return false
	}
	_ = mt.tunnel.Close()
	_ = mt.client.Close()
	delete(m.tunnels, id)
	return true
}

// SweepIdle closes every tunnel whose lastActivity is older than
// forwardIdleTimeout and returns the ids it reaped. Called periodically by the
// sweeper goroutine; also callable directly (tests). Same per-tunnel cleanup as
// Close (listener + client + registry delete).
func (m *TunnelManager) SweepIdle() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var closed []string
	for id, mt := range m.tunnels {
		if time.Since(mt.lastActivity) > forwardIdleTimeout {
			_ = mt.tunnel.Close()
			_ = mt.client.Close()
			delete(m.tunnels, id)
			closed = append(closed, id)
		}
	}
	return closed
}

// CloseAll tears down every live tunnel (listener + owning client) and stops
// the tunnel-sweeper goroutine if it was started. This is the MCP-shutdown path:
// RunStdio defers it so that when the agent disconnects / stdin closes, every
// open forward is reaped cleanly (no leaked ssh.Clients or listeners stranding
// the process). Safe to call on a manager with no tunnels (no-op) and
// idempotent across Close/CloseAll combinations.
func (m *TunnelManager) CloseAll() {
	m.mu.Lock()
	for id, mt := range m.tunnels {
		_ = mt.tunnel.Close()
		_ = mt.client.Close()
		delete(m.tunnels, id)
	}
	m.mu.Unlock()
	// Signal the sweeper loop to exit (no-op if never started or already stopped)
	// and wait for it to return so a process exit doesn't race the ticker fire.
	m.stopOnce.Do(func() { close(m.quit) })
	m.wg.Wait()
}
