package mcpserver

// Plan 35 T4 control-loop pins (spec §4): the 15s tick owns five duties —
// order state machine, row cleanup, cascade recheck, whitelist recheck, lease
// heartbeat — plus the discipline counters (fail-the-renewal + two
// independent enforcement-degradation counters) and the read-only exemption.
//
// Brief-vs-codebase adaptations (same pattern as the T3 note):
//   - mirrorMgr returns 5 values (mgr, st, pid, srvID, cleanup) and
//     openTestTunnel takes 6 args (t, mgr, st, projID, pid, srvID) — calls
//     below follow the real signatures.
//   - ForwardForProfile has no listen_host param until Task 5, so the
//     whitelist fixtures open their non-loopback tunnel through
//     openTestTunnelOnHost (direct connect → ForwardLocal(listenHost) →
//     mgr.Open) — the gate is Task 5's concern, T4 tests the recheck.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// liveCount is the white-box live-tunnel count (control-loop pin helper).
func (m *TunnelManager) liveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tunnels)
}

// localAddrOf returns the tunnel's local listener address — the probe target
// for port-unreachable assertions. Call BEFORE the tick closes the tunnel.
func localAddrOf(t *testing.T, m *TunnelManager, id string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	mt, ok := m.tunnels[id]
	if !ok {
		t.Fatalf("no tunnel %s", id)
	}
	return mt.tunnel.LocalAddr()
}

// pickLocalNonLoopbackIP is the sshbroker-test helper of the same name copied
// into this package (it is unexported there, unreachable from mcpserver).
func pickLocalNonLoopbackIP(t *testing.T) net.IP {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP
		}
	}
	t.Skip("no non-loopback IPv4 on this host")
	return nil
}

// openTestTunnelOnHost opens a REAL tunnel bound to listenHost — the T4
// whitelist-recheck fixture. ForwardForProfile's listen_host param is Task 5
// (core.go still hardcodes 127.0.0.1 + the gate), so this helper wires
// connect → ForwardLocal(listenHost) → mgr.Open directly with the canonical
// meta the gate will eventually produce.
func openTestTunnelOnHost(t *testing.T, mgr *TunnelManager, st *store.Store, projID, srvID, listenHost, remoteHost string, remotePort int) (string, error) {
	t.Helper()
	srv, err := st.GetServer(srvID)
	if err != nil || srv == nil {
		return "", fmt.Errorf("server %s not found: %v", srvID, err)
	}
	auth, err := vault.AuthForServer(st, srv)
	if err != nil {
		return "", err
	}
	hkCb, err := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
	if err != nil {
		return "", err
	}
	cli, err := sshbroker.Connect(context.Background(), srv.Host, srv.Port, srv.User, auth, hkCb)
	if err != nil {
		return "", err
	}
	tun, err := cli.ForwardLocal(0, listenHost, remoteHost, remotePort, nil)
	if err != nil {
		_ = cli.Close() // not registered with the manager — close here, no leak
		return "", err
	}
	return mgr.Open(tun, cli, TunnelMeta{
		ProjectID:  projID,
		ServerID:   srvID,
		Remote:     net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)),
		ListenHost: listenHost,
	})
}

// TestControlTickKillsTunnelOrder pins duty 1's owner path: a pending tunnel
// kill order makes THIS manager (the id's owner) tear down the tunnel
// (listener + client + mirror row) and mark the order applied — port dead.
func TestControlTickKillsTunnelOrder(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	addr := localAddrOf(t, mgr, id) // capture before the tick reaps it
	oid, _ := st.CreateTunnelOrder(id, "", "tester")
	mgr.runControlTick()
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("mirror row must be gone after kill")
	}
	o, _ := st.GetTunnelOrder(oid)
	if o == nil || o.Outcome == nil || *o.Outcome != "applied" {
		t.Fatalf("order must be applied, got %+v", o)
	}
	if _, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		t.Fatal("port must be unreachable after kill")
	}
}

// TestControlTickAbsentTargetAchievesOrder pins the absent-target rule: a
// pending tunnel order whose target has NO registry row (never existed or
// already swept) is marked applied by ANY manager's tick — absent ⇒ achieved.
func TestControlTickAbsentTargetAchievesOrder(t *testing.T) {
	mgr, st, _, _, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	oid, _ := st.CreateTunnelOrder("no-such-tunnel", "", "tester")
	mgr.runControlTick()
	o, _ := st.GetTunnelOrder(oid)
	if o == nil || o.Outcome == nil || *o.Outcome != "applied" {
		t.Fatal("absent target ⇒ applied")
	}
}

// TestControlTickProjectOrderFanOut pins the project order's idempotent
// fan-out + decoupled completion: two managers on the same project each tear
// down THEIR OWN tunnels on their tick; the order flips applied at the FIRST
// zero-count observation (after B's tick drains the registry).
func TestControlTickProjectOrderFanOut(t *testing.T) {
	mgrA, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgrA.CloseAll()
	mgrB := NewTunnelManager()
	mgrB.AttachStore(func() *store.Store { return st }, mgrA.projectID)
	defer mgrB.CloseAll()
	if _, err := openTestTunnel(t, mgrA, st, mgrA.projectID, pid, srvID); err != nil {
		t.Fatal(err)
	}
	if _, err := openTestTunnel(t, mgrB, st, mgrA.projectID, pid, srvID); err != nil {
		t.Fatal(err)
	}
	oid, _ := st.CreateTunnelOrder("", mgrA.projectID, "tester")
	mgrA.runControlTick() // A closes its own; B's row keeps the count at 1 — no mark yet
	o, _ := st.GetTunnelOrder(oid)
	if o != nil && o.Outcome != nil {
		t.Fatal("order must stay pending while the project still has registry rows")
	}
	mgrB.runControlTick() // B closes its own; count hits 0 — first observation marks applied
	if n, _ := st.CountTunnelRegistryProject(mgrA.projectID); n != 0 {
		t.Fatalf("all project tunnels must be torn down, %d left", n)
	}
	o, _ = st.GetTunnelOrder(oid)
	if o == nil || o.Outcome == nil || *o.Outcome != "applied" {
		t.Fatal("project order applied after registry drained")
	}
}

// TestControlTickCascadeOnRevoke pins duty 3: a revoked project's live
// tunnels are torn down within one tick of the control loop.
func TestControlTickCascadeOnRevoke(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProjectStatus(mgr.projectID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	mgr.runControlTick()
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("revoked project's tunnel must be torn down ≤tick")
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("revoked project's tunnels must be gone from the manager, %d alive", n)
	}
}

// TestControlTickCascadeOnDisable is the disable twin of the revoke cascade:
// disable carries the same review semantics and tears down the same way.
func TestControlTickCascadeOnDisable(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProjectStatus(mgr.projectID, models.ProjectDisabled); err != nil {
		t.Fatal(err)
	}
	mgr.runControlTick()
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("disabled project's tunnel must be torn down ≤tick")
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("disabled project's tunnels must be gone, %d alive", n)
	}
}

// TestControlTickWhitelistShrinkClosesExisting pins duty 4: a live non-loopback
// tunnel whose listen host has been removed from the owner whitelist is closed
// by the next tick (gate covers new opens; the recheck covers the existing
// stock — spec §2 存量收缩).
func TestControlTickWhitelistShrinkClosesExisting(t *testing.T) {
	mgr, st, _, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	ip := pickLocalNonLoopbackIP(t)
	if err := st.AddForwardBindHost(ip.String()); err != nil {
		t.Fatal(err)
	}
	id, err := openTestTunnelOnHost(t, mgr, st, mgr.projectID, srvID, ip.String(), "127.0.0.1", echoPortForMirror(t))
	if err != nil {
		t.Skipf("cannot bind %s here: %v", ip, err)
	}
	if _, err := st.RemoveForwardBindHost(ip.String()); err != nil {
		t.Fatal(err)
	}
	mgr.runControlTick()
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("whitelist-revoked tunnel must close ≤tick")
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("whitelist-revoked tunnel must be gone from the manager, %d alive", n)
	}
}

// TestControlTickRenewalFailuresSelfClose pins fail-the-renewal: with the
// heartbeat write failing every tick (registry table dropped), 9 ticks
// (> 8) close the manager's tunnels — no lease, no tunnel.
func TestControlTickRenewalFailuresSelfClose(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	if _, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID); err != nil {
		t.Fatal(err)
	}
	dropRegistry(t, st) // heartbeat UPDATE must fail
	for i := 0; i < 9; i++ {
		mgr.runControlTick()
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("fail-the-renewal must self-close after >8 failed ticks, %d alive", n)
	}
}

// TestControlTickZeroRowHeartbeatSelfClose pins duty 5's zero-row rule: a
// live tunnel whose registry row vanished externally (GC got there first)
// self-closes on its next heartbeat — a tunnel outside the kill domain must
// not exist.
func TestControlTickZeroRowHeartbeatSelfClose(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTunnelRegistry([]string{id}); err != nil {
		t.Fatal(err)
	}
	mgr.runControlTick()
	if n := mgr.liveCount(); n != 0 {
		t.Fatal("zero-row heartbeat must self-close the tunnel")
	}
}

// TestControlTickReadOnlyExemptions pins the offline exemption (spec §4): a
// read-only hydrated store skips every duty — no teardown, no counter moves,
// no panic — the offline lease is governed by §1's boundary, not by lease
// discipline.
func TestControlTickReadOnlyExemptions(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	if _, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID); err != nil {
		t.Fatal(err)
	}
	st.SetReadOnly(nil)
	for i := 0; i < 12; i++ {
		mgr.runControlTick() // must not panic, must not tear down, must not count
	}
	if n := mgr.liveCount(); n != 1 {
		t.Fatalf("read-only store must exempt lease discipline, %d alive", n)
	}
	if mgr.renewalFail != 0 || mgr.cascadeFail != 0 || mgr.whitelistFail != 0 {
		t.Fatalf("read-only ticks must not touch the discipline counters: renewal=%d cascade=%d whitelist=%d",
			mgr.renewalFail, mgr.cascadeFail, mgr.whitelistFail)
	}
}

// TestControlTickAbandonedTickCountsAsRenewalFailure pins the spec §4 计数细则
// clause the brief's Step 3 code omits: a whole tick abandoned on a store read
// failure IS a renewal-failure tick (有活隧道且该 tick 未完成续约——含整 tick
// 放弃). 9 abandoned ticks ⇒ fail-the-renewal fires.
func TestControlTickAbandonedTickCountsAsRenewalFailure(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	if _, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID); err != nil {
		t.Fatal(err)
	}
	// Drop tunnel_orders → the duty-1 read fails → every tick abandons whole.
	if err := st.ExecForTest(`DROP TABLE tunnel_orders`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		mgr.runControlTick()
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("tick abandonment must count toward fail-the-renewal, %d alive", n)
	}
}

// TestControlTickPartialRenewalAdvancesThenResets pins the rev4 counter rules:
// PARTIAL failure (one tunnel's renewal incomplete while another renews)
// ADVANCES the counter; only an all-success tick resets it; with no live
// tunnels the counter suspends (neither advances nor resets).
func TestControlTickPartialRenewalAdvancesThenResets(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	idA, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	// A's row removed externally → A's heartbeat is zero-row (self-close)
	// while B renews fine: the tick is INCOMPLETE ⇒ advance, not reset.
	if err := st.DeleteTunnelRegistry([]string{idA}); err != nil {
		t.Fatal(err)
	}
	mgr.runControlTick()
	if n := mgr.liveCount(); n != 1 {
		t.Fatalf("zero-row tunnel must self-close, %d alive", n)
	}
	if mgr.renewalFail != 1 {
		t.Fatalf("partial renewal must advance the counter, got %d", mgr.renewalFail)
	}
	mgr.runControlTick() // B renews — all (one) live tunnels renewed ⇒ reset
	if mgr.renewalFail != 0 {
		t.Fatalf("all-success tick must reset the counter, got %d", mgr.renewalFail)
	}
	// no live tunnels ⇒ suspended
	if !mgr.Close(idB) {
		t.Fatal("close idB")
	}
	mgr.renewalFail = 3
	mgr.runControlTick()
	if mgr.renewalFail != 3 {
		t.Fatalf("counter must suspend with no live tunnels, got %d", mgr.renewalFail)
	}
}

// TestControlTickCascadeReadFailuresCloseAll pins enforcement degradation on
// the cascade side (independent counter): GetProject unreadable for >8 ticks
// ⇒ close ALL of this manager's tunnels (≤2min bounded shutdown, spec §1).
func TestControlTickCascadeReadFailuresCloseAll(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	if _, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID); err != nil {
		t.Fatal(err)
	}
	if err := st.ExecForTest(`DROP TABLE projects`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		mgr.runControlTick()
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("cascade enforcement degraded must close all tunnels after >8 failed ticks, %d alive", n)
	}
}

// TestControlTickWhitelistReadFailuresCloseNonLoopbackOnly pins enforcement
// degradation on the whitelist side (independent counter, downgraded action):
// whitelist unreadable for >8 ticks ⇒ close only NON-LOOPBACK tunnels; a
// loopback tunnel needs no whitelist approval and must survive.
func TestControlTickWhitelistReadFailuresCloseNonLoopbackOnly(t *testing.T) {
	mgr, st, pid, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	ip := pickLocalNonLoopbackIP(t)
	if err := st.AddForwardBindHost(ip.String()); err != nil {
		t.Fatal(err)
	}
	nbID, err := openTestTunnelOnHost(t, mgr, st, mgr.projectID, srvID, ip.String(), "127.0.0.1", echoPortForMirror(t))
	if err != nil {
		t.Skipf("cannot bind %s here: %v", ip, err)
	}
	lbID, err := openTestTunnel(t, mgr, st, mgr.projectID, pid, srvID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ExecForTest(`DROP TABLE forward_bind_hosts`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		mgr.runControlTick()
	}
	if has, _ := st.HasTunnelRegistryRow(nbID); has {
		t.Fatal("non-loopback tunnel must close when the whitelist read degrades")
	}
	if n := mgr.liveCount(); n != 1 {
		t.Fatalf("loopback tunnel must survive whitelist enforcement degradation, %d alive", n)
	}
	if has, _ := st.HasTunnelRegistryRow(lbID); !has {
		t.Fatal("loopback tunnel's mirror row must survive")
	}
}
