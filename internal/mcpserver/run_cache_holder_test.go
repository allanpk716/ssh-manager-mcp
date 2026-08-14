package mcpserver

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedSnap builds a snapshot with n servers bound to one profile/project and
// returns (snapshot, projectToken, profileID). The hydrated store round-trips
// the SAME token (ImportSnapshot preserves token hashes).
func seedSnap(t *testing.T, n int) (*store.Snapshot, string, string) {
	t.Helper()
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := st.AddServer(&models.Server{
			Name: fmt.Sprintf("srv%d", i), Host: "192.0.2.10", Port: 22, User: "u",
			AuthMethod: models.AuthPassword, CredentialID: cid,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, ids)
	_, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap, token, pid
}

func newHolder(t *testing.T, snap *store.Snapshot, token, profileID string,
	reload func() (*store.Snapshot, bool, error)) *cacheStoreHolder {
	t.Helper()
	af, err := os.OpenFile(filepath.Join(t.TempDir(), "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { af.Close() })
	h := &cacheStoreHolder{reload: reload, token: token, auditFile: af, profileID: profileID}
	st, _, tmp, err := hydrateCacheStore(token, snap, af)
	if err != nil {
		t.Fatal(err)
	}
	h.cur.Store(st)
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmp)
	t.Cleanup(h.cleanup)
	return h
}

func serverCount(t *testing.T, st *store.Store, pid string) int {
	t.Helper()
	out, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatalf("ListServersForProfile: %v", err)
	}
	return len(out)
}

func TestHolder_NoChange_SameStorePointer(t *testing.T) {
	snap, token, pid := seedSnap(t, 1)
	h := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) { return nil, false, nil })
	a, b, c := h.Current(), h.Current(), h.Current()
	if a != b || b != c {
		t.Fatal("unchanged reload must keep the same store pointer")
	}
}

func TestHolder_Changed_SwapsAndOldStaysUsable(t *testing.T) {
	snap1, token, pid := seedSnap(t, 1)
	snap2, _, _ := seedSnap(t, 2) // same shape, 2 servers — token/project ids differ, see below
	// seedSnap mints fresh ids each call; rebuild snap2 over snap1's project:
	// hydrate requires the SAME token to verify, so graft: re-export snap2's
	// servers into snap1's project. Simplest: hydrate snap1 first, then mutate
	// a COPY of snap1 with snap2's extra server row appended.
	grafted := *snap1
	// graft snap2's credential too: the appended server's credential_id FKs to it
	grafted.Credentials = append(append([]store.SnapshotCredential{}, snap1.Credentials...), snap2.Credentials...)
	// servers.name is globally UNIQUE and ExportSnapshot orders by random id, so
	// snap2's last row may be named "srv0" and collide with snap1's — take any
	// snap2 server and give it a deterministic fresh name.
	extra := snap2.Servers[len(snap2.Servers)-1]
	extra.Name = "srv-extra"
	grafted.Servers = append(append([]store.SnapshotServer{}, snap1.Servers...), extra)
	grafted.Grants = append(append([]store.SnapshotGrant{}, snap1.Grants...), store.SnapshotGrant{ProfileID: pid, ServerID: extra.ID})

	// fire the change on the SECOND reload consult: the first Current() call
	// (old := h.Current()) is the pre-change baseline, the second one swaps.
	calls := 0
	h := newHolder(t, snap1, token, pid, func() (*store.Snapshot, bool, error) {
		calls++
		if calls == 2 {
			return &grafted, true, nil
		}
		return nil, false, nil
	})
	old := h.Current()
	if got := serverCount(t, old, pid); got != 1 {
		t.Fatalf("initial: %d servers, want 1", got)
	}
	cur := h.Current()
	if cur == old {
		t.Fatal("changed reload must swap the store")
	}
	if got := serverCount(t, cur, pid); got != 2 {
		t.Fatalf("after swap: %d servers, want 2", got)
	}
	// Old store must stay USABLE (in-flight call safety): not closed on swap.
	if got := serverCount(t, old, pid); got != 1 {
		t.Fatalf("old store closed on swap: %v", got)
	}
}

func TestHolder_ReloadErrorAndBadSnapshot_KeepOld(t *testing.T) {
	snap, token, pid := seedSnap(t, 1)
	// error path
	h := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) {
		return nil, false, fmt.Errorf("stat failed")
	})
	old := h.Current()
	if h.Current() != old {
		t.Fatal("reload error must keep the old store")
	}
	// revoked-token path: snapshot without the project → VerifyToken fails in hydrate
	noProj := *snap
	noProj.Projects = nil
	h2 := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) {
		return &noProj, true, nil
	})
	old2 := h2.Current()
	if h2.Current() != old2 {
		t.Fatal("token revoked in new snapshot must keep the old store")
	}
}

func TestHolder_ProfileDrift_KeepsOld(t *testing.T) {
	snap, token, pid := seedSnap(t, 1)
	// same project/token but bound to a different profile id
	drifted := *snap
	drifted.Projects = append([]store.SnapshotProject{}, snap.Projects...)
	drifted.Projects[0].ProfileID = "other-profile"
	h := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) {
		return &drifted, true, nil
	})
	old := h.Current()
	if h.Current() != old {
		t.Fatal("profile drift must keep the old store")
	}
}

func TestHolder_ConcurrentCurrent_RebuildsOnce(t *testing.T) {
	snap1, token, pid := seedSnap(t, 1)
	snap2, _, _ := seedSnap(t, 2)
	grafted := *snap1
	// graft snap2's credential too: the appended server's credential_id FKs to it
	grafted.Credentials = append(append([]store.SnapshotCredential{}, snap1.Credentials...), snap2.Credentials...)
	// servers.name is globally UNIQUE and ExportSnapshot orders by random id, so
	// snap2's last row may be named "srv0" and collide with snap1's — take any
	// snap2 server and give it a deterministic fresh name.
	extra := snap2.Servers[len(snap2.Servers)-1]
	extra.Name = "srv-extra"
	grafted.Servers = append(append([]store.SnapshotServer{}, snap1.Servers...), extra)
	grafted.Grants = append(append([]store.SnapshotGrant{}, snap1.Grants...), store.SnapshotGrant{ProfileID: pid, ServerID: extra.ID})

	var mu sync.Mutex
	pending := true
	h := newHolder(t, snap1, token, pid, func() (*store.Snapshot, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if pending {
			return &grafted, true, nil
		}
		return nil, false, nil
	})
	var wg sync.WaitGroup
	final := make([]*store.Store, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			final[i] = h.Current()
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(final); i++ {
		if final[i] != final[0] {
			t.Fatal("concurrent Current() must converge on one store")
		}
	}
	if got := serverCount(t, final[0], pid); got != 2 {
		t.Fatalf("final store: %d servers, want 2", got)
	}
}
