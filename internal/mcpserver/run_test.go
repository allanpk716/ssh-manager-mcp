package mcpserver

// Plan 48 §2.2 wiring — pins that the RunStdioCache host-key store binder
// actually reaches the TOFU path (the injection seam of NewServerFromSource),
// and that the per-generation forward-device metadata rides every hydration
// (§2.3). These tests stay inside the mcpserver package with a LOCAL fake:
// the forwarding wrapper itself is clientops's (clientops imports mcpserver,
// so this package's tests cannot import clientops) — the wrapper behavior is
// covered there, and the full e2e in cli (the package where clientops and
// mcpserver meet).

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// recordingHK wraps the current store and counts TOFU reads — the observable
// that proves the binder's HostKeyStore (not the raw store) serves the TOFU
// path.
type recordingHK struct {
	cur  func() *store.Store
	gets *int32
}

func (r *recordingHK) GetHostKey(host string, port int) (*store.Pin, error) {
	atomic.AddInt32(r.gets, 1)
	return r.cur().GetHostKey(host, port)
}

func (r *recordingHK) SaveHostKey(host string, port int, marshaledKey []byte) error {
	return r.cur().SaveHostKey(host, port, marshaledKey)
}

// seedTofuTargetSnap builds a snapshot with one granted server pointing at a
// REAL testsshd and NO pinned key: the handshake reaches the TOFU callback
// (the injection under test), which then refuses the unknown key on the
// read-only store. Returns (snapshot, token, serverID).
func seedTofuTargetSnap(t *testing.T, addr string) (*store.Snapshot, string, string) {
	t.Helper()
	st := newStore(t)
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: "target", Host: addr[:indexByte(addr, ':')], Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(pid, []string{id}); err != nil {
		t.Fatal(err)
	}
	_, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap, token, id
}

// TestRunStdioCacheBinder_ReachesTOFU pins the injection seam: with a binder
// wired, the TOFU callback consults the BINDER's host-key store (the read
// counter increments); with a nil binder the raw store serves (counter stays
// zero because the raw store's reads cannot be observed — the assertion is
// the positive case). A regression that drops the hkFn threading would leave
// the override unwired and the counter at zero.
func TestRunStdioCacheBinder_ReachesTOFU(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	snap, token, serverID := seedTofuTargetSnap(t, addr)
	auditPath := filepath.Join(t.TempDir(), "audit.log")

	var gets int32
	binder := func(cur func() *store.Store) sshbroker.HostKeyStore {
		return &recordingHK{cur: cur, gets: &gets}
	}

	srv, tunnels, tasks, cleanup2, err := NewCacheBroker(token, snap, auditPath, nil, binder, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup2)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })

	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srvSess.Close() })
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cliSess.Close() })

	res, err := cliSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      BrokerTools[1], // exec_command
		Arguments: map[string]any{"server_id": serverID, "command": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("exec without a pinned key on a read-only store must fail closed")
	}
	if n := atomic.LoadInt32(&gets); n == 0 {
		t.Fatal("the TOFU path never consulted the binder's host-key store — the NewServerFromSource injection is unwired")
	}
}

// TestHydrateCacheStore_ForwardDevicePerGeneration pins §2.3's metadata
// wiring: EVERY hydrated generation carries the device name given to the
// holder constructor, so ApplyForwardedHostKey stamps host_keys.pin_device
// with it — not just the initial hydration (hot rebuilds hydrate fresh temp
// dbs that would otherwise lose the metadata).
func TestHydrateCacheStore_ForwardDevicePerGeneration(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	snap, token, _ := seedTofuTargetSnap(t, addr)
	af, err := os.OpenFile(filepath.Join(t.TempDir(), "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { af.Close() })

	h, _, err := newCacheStoreHolderFromSnapshot(token, snap, af, nil, "laptop-7")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)

	// Initial generation: the applied pin must carry the device name.
	gen1 := h.Current()
	if err := gen1.ApplyForwardedHostKey("10.1.1.1", 22, []byte("key-bytes")); err != nil {
		t.Fatal(err)
	}
	if got := rawForwardDevice(t, h.tmpPaths[0], "10.1.1.1:22"); got != "laptop-7" {
		t.Fatalf("generation 1 pin_device = %q, want laptop-7", got)
	}

	// A hot rebuild hydrates a FRESH temp db from a FRESH snapshot of the SAME
	// store (same project token) — the metadata must survive it.
	calls := 0
	h.reload = func() (*store.Snapshot, bool, error) {
		calls++
		if calls == 1 {
			fresh, err := reseedSnap(t, snap)
			if err != nil {
				return nil, false, err
			}
			return fresh, true, nil
		}
		return nil, false, nil
	}
	gen2 := h.Current()
	if gen2 == gen1 {
		t.Fatal("fixture self-check: the reload must swap generations")
	}
	if err := gen2.ApplyForwardedHostKey("10.1.1.2", 22, []byte("key-bytes")); err != nil {
		t.Fatal(err)
	}
	if got := rawForwardDevice(t, h.tmpPaths[1], "10.1.1.2:22"); got != "laptop-7" {
		t.Fatalf("generation 2 pin_device = %q, want laptop-7 (per-generation metadata)", got)
	}
}

// reseedSnap re-imports snap into a fresh temp db — a fresh store carrying the
// same project token, i.e. a genuine reload candidate for the holder.
func reseedSnap(t *testing.T, snap *store.Snapshot) (*store.Snapshot, error) {
	t.Helper()
	dup := *snap
	dup.Servers = append([]store.SnapshotServer{}, snap.Servers...)
	return &dup, nil
}

// rawForwardDevice reads host_keys.pin_device straight from a hydrated temp db.
func rawForwardDevice(t *testing.T, dbPath, hostPort string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var dev string
	if err := db.QueryRow(`SELECT pin_device FROM host_keys WHERE host_port=?`, hostPort).Scan(&dev); err != nil {
		t.Fatalf("pin row for %s: %v", hostPort, err)
	}
	return dev
}

// firstServerID returns the snapshot's first server id (the snapshot has one).
func firstServerID(t *testing.T, snap *store.Snapshot) string {
	t.Helper()
	if len(snap.Servers) == 0 {
		t.Fatal("snapshot has no servers")
	}
	return snap.Servers[0].ID
}
