package mcpserver

// Plan 35 T3 mirror-pipeline pins (spec §6): a tunnel that cannot be killed
// must not be allowed to exist. Open mirrors into tunnel_registry (event-driven
// INSERT, fail-the-Open); every teardown path mirror-deletes; failed DELETEs
// land in a retry set that drains once the store accepts writes again.

import (
	"context"
	"testing"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// mirrorMgr builds the T3 fixture: a real testsshd, an in-profile seeded
// server, a project, and a TunnelManager with the store seam attached — the
// mirror pipeline under test. Sweeper is NOT started (hermetic, no tick).
// Brief-vs-codebase note: the snippet passed profile NAME "p" to
// ForwardForProfile, which wants the profile ID (AddProfile returns a
// generated id) — the pid is threaded through here instead.
func mirrorMgr(t *testing.T) (*TunnelManager, *store.Store, string, string, func()) {
	t.Helper()
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, _, _ := st.AddProject("proj", pid)
	mgr := NewTunnelManager()
	mgr.AttachStore(func() *store.Store { return st }, projID)
	return mgr, st, pid, srvID, cleanup
}

// openTestTunnel opens a REAL tunnel through ForwardForProfile (full connect +
// ForwardLocal + mgr.Open path) targeting the echo listener. The 10-arg
// signature's listenHost defaults to "" (loopback) — the mirror pipeline does
// not care about the bind host.
func openTestTunnel(t *testing.T, mgr *TunnelManager, st *store.Store, projID, pid, srvID string) (string, error) {
	t.Helper()
	out, err := ForwardForProfile(context.Background(), st, projID, pid, srvID, "127.0.0.1", echoPortForMirror(t), 0, "", mgr)
	return out.TunnelID, err
}

// echoPortForMirror reuses the core_test echo listener as the tunnel's remote
// target (startEchoListener is t.Cleanup-wired — no extra teardown here).
func echoPortForMirror(t *testing.T) int { return startEchoListener(t) }

// dropRegistry removes tunnel_registry so registry INSERT/DELETE must fail —
// the failure injection behind fail-the-Open and the DELETE-retry pin.
func dropRegistry(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.ExecForTest(`DROP TABLE tunnel_registry`); err != nil {
		t.Fatalf("drop tunnel_registry: %v", err)
	}
}

// restoreRegistry recreates an EMPTY tunnel_registry (schema verbatim from the
// store.go migration) so store writes succeed again.
func restoreRegistry(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.ExecForTest(`CREATE TABLE IF NOT EXISTS tunnel_registry (
  tunnel_id    TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL,
  server_id    TEXT NOT NULL,
  remote       TEXT NOT NULL,
  local_addr   TEXT NOT NULL,
  listen_host  TEXT NOT NULL,
  opened_at    INTEGER NOT NULL,
  last_renewed INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("restore tunnel_registry: %v", err)
	}
}

// TestMirrorInsertOnOpenAndDeleteOnClose pins the event-driven mirror: the row
// exists RIGHT AFTER Open (before any tick), carries the manager's project +
// the opened server + the canonical listen host, and is deleted on Close.
func TestMirrorInsertOnOpenAndDeleteOnClose(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()

	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	has, _ := st.HasTunnelRegistryRow(id)
	if !has {
		t.Fatal("registry row must exist right after Open (event-driven insert)")
	}
	rows, _ := st.ListTunnelRegistry()
	if len(rows) != 1 || rows[0].ProjectID != mgr.projectID || rows[0].ServerID != srvID || rows[0].ListenHost != "127.0.0.1" {
		t.Fatalf("mirror row: %+v", rows)
	}
	if !mgr.Close(id) {
		t.Fatal("close")
	}
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("row must be deleted on Close")
	}
}

// TestMirrorFailTheOpen pins fail-the-Open (spec §6): a writable-store INSERT
// failure surfaces as an Open error (no tunnel id), and the half-registered
// tunnel leaves NOTHING behind — memory registry empty, tunnel + client closed.
func TestMirrorFailTheOpen(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	// Failure injection: DROP the table → INSERT must fail.
	dropRegistry(t, st)
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err == nil {
		t.Fatalf("fail-the-Open must surface error (got tunnel %s)", id)
	}
	if id != "" {
		t.Fatal("no id on failure")
	}
	// 隧道不可留:内存注册表必须为空
	mgr.mu.Lock()
	n := len(mgr.tunnels)
	mgr.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed open left %d tunnels registered", n)
	}
}

// TestMirrorDeleteFailureRetries pins the retry set (spec §6): a teardown whose
// mirror DELETE fails (table dropped) is recorded in pendingDeletes; once the
// store accepts writes again (table restored), retryPendingDeletes drains it.
func TestMirrorDeleteFailureRetries(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	// DROP the table so Close's mirror DELETE must fail → retry-set 记账。
	dropRegistry(t, st)
	if !mgr.Close(id) {
		t.Fatal("close")
	}
	if len(mgr.pendingDeletes) == 0 {
		t.Fatal("failed mirror DELETE must land in retry set")
	}
	restoreRegistry(t, st) // rebuild an empty table — writes succeed again
	mgr.retryPendingDeletes()
	if len(mgr.pendingDeletes) != 0 {
		t.Fatal("retry must drain the set once the store accepts writes again")
	}
}
