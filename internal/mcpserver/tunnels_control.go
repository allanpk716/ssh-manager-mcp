package mcpserver

// Plan 35 T4: the TunnelManager control loop (spec §4). One tick every
// controlInterval owns five duties — order state machine, row cleanup,
// cascade recheck, whitelist recheck, lease heartbeat — plus the discipline
// counters (fail-the-renewal + two INDEPENDENT enforcement-degradation
// counters) and the read-only exemption. Everything is idempotent and safe
// to re-run: a crash mid-tick leaves at worst a still-pending order the next
// tick replays.

import (
	"log"
	"net"
	"strconv"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// controlInterval is the control-loop tick (spec §4): order state machine,
// row cleanup, cascade recheck, whitelist recheck, lease heartbeat — all
// idempotent, all safe to re-run every 15s. Spec §1's ≤15s kill/cascade/
// whitelist-shrink promise is one tick.
const controlInterval = 15 * time.Second

// renewalFailThreshold: consecutive failed/incomplete ticks before the
// bounded shutdowns fire (spec §4: >8 ticks ≈ 2min at 15s). The lease
// counter and the two enforcement counters (cascade / whitelist) each use it
// INDEPENDENTLY — different failure signals, different triggered actions.
const renewalFailThreshold = 8

// runControlTick executes one control cycle (spec §4 duties 1-5). Called by
// the sweepLoop's control ticker; tests drive it directly (same hermetic
// convention as SweepIdle). A bare manager (no store attached — the legacy
// test fixtures) is control-inert, and a read-only hydrated store (offline
// stdio) skips every duty: orders are never exported into snapshots, the
// stale project status would misfire the cascade, and offline tunnels are
// not in the lease registry's jurisdiction at all (spec §4 只读豁免).
func (m *TunnelManager) runControlTick() {
	m.mu.Lock()
	storeFn := m.storeFn
	m.mu.Unlock()
	if storeFn == nil {
		return // bare manager (existing tests): control-inert
	}
	st := storeFn()
	if st == nil || st.IsReadOnly() {
		return // offline hydrated store: exempt from every control duty
	}

	// duty 1 — order state machine (spec §4). A read failure abandons the
	// whole tick (orders must not be half-executed on a stale view); the
	// abandonment still counts against the lease counter below.
	orders, err := st.PendingTunnelOrders()
	if err != nil {
		log.Printf("tunnel control: read pending orders failed, retry next tick: %v", err)
		m.abandonTick()
		return
	}
	for _, o := range orders {
		if o.TunnelID != "" {
			// tunnel order: the owner executes (Close → mirror delete →
			// mark, 标记后置); anyone marks when the target is globally
			// absent from the registry (absent ⇒ achieved).
			if m.closeTunnelIfOwned(o.TunnelID, "kill order applied") {
				m.markApplied(o.ID)
				continue
			}
			if has, herr := st.HasTunnelRegistryRow(o.TunnelID); herr == nil && !has {
				m.markApplied(o.ID)
			}
			continue
		}
		if o.ProjectID == m.projectID {
			// project order = idempotent fan-out: each manager tears down
			// whatever it holds RIGHT NOW (weak semantics — pending 期间;
			// it does not bar reopens, spec §4 rev4).
			m.closeAllTunnels("kill order " + strconv.FormatInt(o.ID, 10) + " (project)")
		}
	}
	// project orders completion: the FIRST zero-count observation marks the
	// order applied — decoupled from execution, any manager may observe it.
	for _, o := range orders {
		if o.ProjectID == "" {
			continue
		}
		if n, cerr := st.CountTunnelRegistryProject(o.ProjectID); cerr == nil && n == 0 {
			m.markApplied(o.ID)
		}
	}

	// duty 2 — row cleanup (applied 7d / pending target-absent 7d) + the
	// registry's 30min ghost GC. Both idempotent; failures log and continue
	// (they cannot corrupt order state, and the ghosts self-heal next tick).
	if err := st.CleanupTunnelOrders(); err != nil {
		log.Printf("tunnel control: order cleanup failed: %v", err)
	}
	if err := st.GCTunnelRegistry(time.Now().Add(-30 * time.Minute).Unix()); err != nil {
		log.Printf("tunnel control: registry GC failed: %v", err)
	}

	// duty 3 — cascade recheck (spec §5); duty 4 — whitelist stock recheck
	// (spec §2); duty 5 — lease heartbeat + zero-row self-close + the
	// failed-DELETE retry drain (spec §4/§6).
	m.cascadeCheck(st)
	m.whitelistCheck(st)
	m.heartbeat(st)
	m.retryPendingDeletes()
}

// markApplied flips a pending order to applied. The UPDATE itself carries
// the outcome-IS-NULL guard, so concurrent markers are safe.
func (m *TunnelManager) markApplied(orderID int64) {
	m.mu.Lock()
	storeFn := m.storeFn
	m.mu.Unlock()
	if storeFn == nil {
		return
	}
	if st := storeFn(); st != nil && !st.IsReadOnly() {
		_, _ = st.MarkTunnelOrderApplied(orderID)
	}
}

// closeTunnelIfOwned tears down the tunnel when THIS manager owns it in
// memory (Close → mirror delete) and logs the reason. Returns ownership.
// Shared by the kill order (owner path), the whitelist shrink, and the
// zero-row heartbeat self-close.
func (m *TunnelManager) closeTunnelIfOwned(id, reason string) bool {
	m.mu.Lock()
	mt, ok := m.tunnels[id]
	if ok {
		_ = mt.tunnel.Close()
		_ = mt.client.Close()
		delete(m.tunnels, id)
	}
	m.mu.Unlock()
	if ok {
		m.mirrorDelete([]string{id})
		log.Printf("%s — closed tunnel %s", reason, id)
	}
	return ok
}

// abandonTick records a whole-tick store-failure abandonment against the
// lease counter (spec §4 计数细则: 整 tick 因 store 故障提前放弃计入「续约
// 失败 tick」). With no live tunnels the counter stays suspended — there is
// no lease discipline to degrade.
func (m *TunnelManager) abandonTick() {
	m.mu.Lock()
	live := len(m.tunnels)
	m.mu.Unlock()
	if live == 0 {
		return
	}
	m.renewalTickFailed()
}

// renewalTickFailed advances the fail-the-renewal counter and, past the
// threshold, closes every tunnel (no lease ⇒ no tunnel, spec §4). The log
// line comes out of closeAllTunnels in the pinned verbatim shape.
func (m *TunnelManager) renewalTickFailed() {
	m.renewalFail++
	if m.renewalFail > renewalFailThreshold {
		m.closeAllTunnels("lease renewal failed " + strconv.Itoa(m.renewalFail) + " ticks")
		m.renewalFail = 0
	}
}

// cascadeCheck is duty 3 (spec §5): with live tunnels, re-read the project
// status — nil (deleted) or non-active (revoked/disabled) tears down every
// tunnel this manager holds. Read failures carry their own INDEPENDENT
// bounded-degradation counter (>8 ⇒ close all): a store that cannot answer
// the authorization question does not get to keep tunnels alive either.
func (m *TunnelManager) cascadeCheck(st *store.Store) {
	m.mu.Lock()
	live := len(m.tunnels)
	pid := m.projectID
	m.mu.Unlock()
	if live == 0 {
		m.cascadeFail = 0 // nothing to enforce: counter rests
		return
	}
	p, err := st.GetProject(pid)
	switch {
	case err != nil:
		m.cascadeFail++
		log.Printf("tunnel control: cascade GetProject failed (%d consecutive): %v", m.cascadeFail, err)
	case p == nil || p.Status != models.ProjectActive:
		reason := "revoked/deleted"
		if p != nil {
			reason = string(p.Status)
		}
		m.closeAllTunnels("cascade: project " + pid + " status=" + reason)
		m.cascadeFail = 0
	default:
		m.cascadeFail = 0 // active + read succeeded: enforcement healthy
	}
	if m.cascadeFail > renewalFailThreshold {
		m.closeAllTunnels("enforcement degraded: cascade read failed " + strconv.Itoa(m.cascadeFail) + " ticks")
		m.cascadeFail = 0
	}
}

// whitelistCheck is duty 4 (spec §2/§4): every live tunnel whose listen host
// is NON-LOOPBACK must still be in the owner whitelist (canonical form —
// meta stores the canonical form, so this is a direct compare). A miss
// closes that tunnel (复用 kill 关闭 + 镜像 DELETE + 日志). Loopback tunnels
// need no approval and are never touched. Read failures carry their own
// INDEPENDENT counter; past the threshold the downgrade closes only the
// non-loopback stock (the ones whose approval cannot be verified).
func (m *TunnelManager) whitelistCheck(st *store.Store) {
	type suspect struct{ id, host string }
	m.mu.Lock()
	var suspects []suspect
	for id, mt := range m.tunnels {
		if !isLoopbackHost(mt.meta.ListenHost) {
			suspects = append(suspects, suspect{id, mt.meta.ListenHost})
		}
	}
	m.mu.Unlock()
	if len(suspects) == 0 {
		m.whitelistFail = 0 // nothing to verify: counter rests
		return
	}
	hosts, err := st.ListForwardBindHosts()
	if err != nil {
		m.whitelistFail++
		log.Printf("tunnel control: whitelist read failed (%d consecutive): %v", m.whitelistFail, err)
		if m.whitelistFail > renewalFailThreshold {
			m.closeAllTunnelsNonLoopback("enforcement degraded: whitelist read failed " + strconv.Itoa(m.whitelistFail) + " ticks")
			m.whitelistFail = 0
		}
		return
	}
	m.whitelistFail = 0
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[h] = true
	}
	for _, s := range suspects {
		if !allowed[s.host] {
			m.closeTunnelIfOwned(s.id, "bind whitelist revoked for "+s.host)
		}
	}
}

// closeAllTunnelsNonLoopback is the enforcement-downgrade variant: closes
// only non-loopback tunnels (spec §4 — a failed whitelist read cannot verify
// the approval for exposed binds; loopback stays, no approval needed).
func (m *TunnelManager) closeAllTunnelsNonLoopback(reason string) {
	m.mu.Lock()
	var ids []string
	for id, mt := range m.tunnels {
		if !isLoopbackHost(mt.meta.ListenHost) {
			_ = mt.tunnel.Close()
			_ = mt.client.Close()
			delete(m.tunnels, id)
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	m.mirrorDelete(ids)
	if len(ids) > 0 {
		log.Printf("%s — closed %d tunnels", reason, len(ids))
	}
}

// isLoopbackHost reports whether the (canonical, gate-validated) listen host
// is a loopback IP.
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// heartbeat is duty 5 (spec §4/§6): renew every live tunnel's lease. ANY
// incompleteness — a write error on one row, or a zero-row renew (the mirror
// row is gone: that tunnel self-closes, it has fallen out of the kill
// domain) — marks the tick incomplete and advances the counter; only ALL
// live tunnels renewing constitutes a completed tick (reset). With no live
// tunnels the duty has no work: the counter suspends.
func (m *TunnelManager) heartbeat(st *store.Store) {
	m.mu.Lock()
	ids := m.tunnelIDsLocked()
	m.mu.Unlock()
	if len(ids) == 0 {
		return // no active tunnels: counter suspended (spec §4)
	}
	failed := false
	ts := time.Now().Unix()
	for _, id := range ids {
		ok, err := st.RenewTunnelHeartbeat(id, ts)
		if err != nil {
			failed = true
			continue
		}
		if !ok {
			m.closeTunnelIfOwned(id, "heartbeat zero-row — tunnel fell out of the kill domain")
			failed = true // this tick did not complete its renewal duty
		}
	}
	if failed {
		m.renewalTickFailed() // partial failure advances (spec §4 计数细则)
		return
	}
	m.renewalFail = 0
}
