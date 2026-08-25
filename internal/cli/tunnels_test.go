package cli

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// cliTestStore opens a DIRECT store handle on the env-pinned vault (for
// seeding registry rows / reading orders without going through the CLI).
func cliTestStore(t *testing.T, path string, mk []byte) *store.Store {
	t.Helper()
	st, err := store.Open(path, mk)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	return st
}

// stInsertRegistry seeds one tunnel_registry mirror row with a FRESH lease
// heartbeat (the row a live broker would hold).
func stInsertRegistry(t *testing.T, st *store.Store, tunnelID, projectID string) {
	t.Helper()
	stInsertRegistryAt(t, st, tunnelID, projectID, time.Now().Unix())
}

// stInsertRegistryAt seeds one tunnel_registry row with an explicit
// last_renewed (lease heartbeat) so staleness can be manufactured.
func stInsertRegistryAt(t *testing.T, st *store.Store, tunnelID, projectID string, lastRenewed int64) {
	t.Helper()
	err := st.InsertTunnelRegistry(store.TunnelRegistryRow{
		TunnelID:    tunnelID,
		ProjectID:   projectID,
		ServerID:    "srv-1",
		Remote:      "127.0.0.1:5432",
		LocalAddr:   "127.0.0.1:15432",
		ListenHost:  "127.0.0.1",
		OpenedAt:    lastRenewed,
		LastRenewed: lastRenewed,
	})
	if err != nil {
		t.Fatalf("seed registry row %s: %v", tunnelID, err)
	}
}

// countOrders counts the orders currently pending in the vault. In these
// assertions no order has been applied yet, so pending == placed.
func countOrders(t *testing.T, st *store.Store) int {
	t.Helper()
	pend, err := st.PendingTunnelOrders()
	if err != nil {
		t.Fatalf("list pending orders: %v", err)
	}
	return len(pend)
}

// markOrderAppliedFor simulates a broker control tick claiming the kill order
// for tunnelID: it waits (≤5s) for the CLI to place the order, marks it
// applied, and reports the order's created_by over ch. It never touches `t`
// (non-test goroutine — no t.Fatalf there).
func markOrderAppliedFor(st *store.Store, tunnelID string, ch chan<- string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pend, err := st.PendingTunnelOrders()
		if err == nil {
			for _, o := range pend {
				if o.TunnelID == tunnelID {
					if _, err := st.MarkTunnelOrderApplied(o.ID); err == nil {
						ch <- o.CreatedBy
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	ch <- ""
}

// markProjectOrderApplied is markOrderAppliedFor for the --project form (the
// order carries project_id, tunnel_id NULL).
func markProjectOrderApplied(st *store.Store, projectID string, ch chan<- string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pend, err := st.PendingTunnelOrders()
		if err == nil {
			for _, o := range pend {
				if o.ProjectID == projectID {
					if _, err := st.MarkTunnelOrderApplied(o.ID); err == nil {
						ch <- o.CreatedBy
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	ch <- ""
}

func TestTunnelsLsAndKillFlow(t *testing.T) {
	path, mk := withCliStoreEnv(t)
	st := cliTestStore(t, path, mk)
	defer st.Close()
	stInsertRegistry(t, st, "tun-abc", "proj-1")

	// ls: shows tunnel_id + project_id (project row absent → name-less,
	// LEFT-JOIN semantics: the id is always displayed).
	out := runCli(t, "tunnels", "ls")
	if !strings.Contains(out, "tun-abc") || !strings.Contains(out, "proj-1") {
		t.Fatalf("ls output: %s", out)
	}

	// kill precheck: no-such → fast fail, NO order placed
	out = runCliErr(t, "tunnels", "kill", "no-such")
	if !strings.Contains(out, "no open tunnel no-such") {
		t.Fatalf("precheck: %s", out)
	}
	if !strings.Contains(out, "OFFLINE cache clients") {
		t.Fatalf("precheck miss must name the offline-client domain: %s", out)
	}
	if n := countOrders(t, st); n != 0 {
		t.Fatalf("precheck failure must not place an order, got %d", n)
	}

	// kill real target: order placed, a simulated broker claims it → applied
	ch := make(chan string, 1)
	go markOrderAppliedFor(st, "tun-abc", ch)
	out = runCli(t, "tunnels", "kill", "tun-abc")
	if !strings.Contains(out, "applied") {
		t.Fatalf("kill outcome: %s", out)
	}
	// created_by recorded on the order (owner-action traceability)
	if by := <-ch; by == "" {
		t.Fatal("order must record created_by (or the broker simulation failed)")
	}
}

func TestTunnelsLsEmptyAndStaleFlag(t *testing.T) {
	path, mk := withCliStoreEnv(t)
	st := cliTestStore(t, path, mk)
	defer st.Close()

	// empty registry: the offline-domain note rides along
	out := runCli(t, "tunnels", "ls")
	if !strings.Contains(out, "no open tunnels") || !strings.Contains(out, "OFFLINE cache clients") {
		t.Fatalf("empty ls must carry the offline-domain note: %s", out)
	}

	// a ghost row (heartbeat older than staleHeartbeatSec) gets the flag; a
	// fresh row on the same listing must NOT.
	stInsertRegistryAt(t, st, "tun-ghost", "proj-1", time.Now().Add(-2*time.Minute).Unix())
	stInsertRegistry(t, st, "tun-fresh", "proj-1")
	out = runCli(t, "tunnels", "ls")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "tun-ghost") && !strings.Contains(line, "stale heartbeat") {
			t.Fatalf("ghost row must be flagged: %s", line)
		}
		if strings.Contains(line, "tun-fresh") && strings.Contains(line, "stale heartbeat") {
			t.Fatalf("fresh row must not be flagged: %s", line)
		}
	}
}

func TestTunnelsKillProject(t *testing.T) {
	path, mk := withCliStoreEnv(t)
	st := cliTestStore(t, path, mk)
	defer st.Close()
	profID, err := st.AddProfile("default")
	if err != nil {
		t.Fatalf("add profile: %v", err)
	}
	pid, _, err := st.AddProject("alpha", profID)
	if err != nil {
		t.Fatalf("add project: %v", err)
	}
	if _, _, err := st.AddProject("beta", profID); err != nil { // beta: exists, zero tunnels
		t.Fatalf("add project: %v", err)
	}
	stInsertRegistry(t, st, "tun-a1", pid)

	// exactly-one-of target form: neither → error
	if out := runCliErr(t, "tunnels", "kill"); !strings.Contains(out, "exactly one of") {
		t.Fatalf("no target form must error: %s", out)
	}
	// exactly-one-of: both → error
	if out := runCliErr(t, "tunnels", "kill", "tun-a1", "--project", "alpha"); !strings.Contains(out, "exactly one of") {
		t.Fatalf("both target forms must error: %s", out)
	}

	// unknown project: fast fail, no order
	if out := runCliErr(t, "tunnels", "kill", "--project", "nope"); !strings.Contains(out, `project "nope" not found`) {
		t.Fatalf("unknown project: %s", out)
	}
	if n := countOrders(t, st); n != 0 {
		t.Fatalf("unknown project must not place an order, got %d", n)
	}

	// known project with zero live rows: refused, NO order placed
	out := runCliErr(t, "tunnels", "kill", "--project", "beta")
	if !strings.Contains(out, "no open tunnels for project beta") {
		t.Fatalf("zero-rows project must be refused: %s", out)
	}
	if n := countOrders(t, st); n != 0 {
		t.Fatalf("zero-rows project must not place an order, got %d", n)
	}

	// by name → order placed → simulated broker applies
	ch := make(chan string, 1)
	go markProjectOrderApplied(st, pid, ch)
	if out = runCli(t, "tunnels", "kill", "--project", "alpha"); !strings.Contains(out, "applied") {
		t.Fatalf("project kill outcome: %s", out)
	}
	if by := <-ch; by == "" {
		t.Fatal("project order must record created_by (or the broker simulation failed)")
	}

	// by id (resolve falls through name → id) — the registry row still exists,
	// so a fresh order is placed and applied again.
	ch2 := make(chan string, 1)
	go markProjectOrderApplied(st, pid, ch2)
	if out = runCli(t, "tunnels", "kill", "--project", pid); !strings.Contains(out, "applied") {
		t.Fatalf("project-by-id kill outcome: %s", out)
	}
	if by := <-ch2; by == "" {
		t.Fatal("project-by-id order must record created_by (or the broker simulation failed)")
	}
}
