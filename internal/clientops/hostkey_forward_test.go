package clientops

// 客户端缓存的转发包装（规格 #88）：按服务器条目读写锚，转发成功或权威端
// 已有相同锚后，把锚写进当前热更新代的本地缓存；分支文案经
// HostKeyTOFU 原样透出，不再叠加前缀。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// newReadOnlyStoreWithEntry 水合出一个带固定标识符服务器条目的只读缓存库
// （热更新换代时条目标识符保持不变，与真实快照水合一致）。
func newReadOnlyStoreWithEntry(t *testing.T) (*store.Store, *models.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gen.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := time.Now().Unix()
	err = st.ImportSnapshot(&store.Snapshot{
		Version: 1,
		Servers: []store.SnapshotServer{{
			ID: "srv-target", Name: "target", Host: "192.168.1.108", Port: 22,
			User: "u", AuthMethod: string(models.AuthPassword), CreatedAt: ts, UpdatedAt: ts,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer("srv-target")
	if err != nil {
		t.Fatal(err)
	}
	st.SetForwardDevice("laptop")
	st.SetReadOnly(nil)
	return st, srv
}

// countingPinServer is a /pin-hostkey stand-in returning the given status and
// body, counting POSTs (the zero-re-forward assertion reads the counter).
type countingPinServer struct {
	srv   *httptest.Server
	posts *int32
}

func newCountingPinServer(t *testing.T, status int, body string) *countingPinServer {
	t.Helper()
	var posts int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &countingPinServer{srv: srv, posts: &posts}
}

func (c *countingPinServer) postCount() int { return int(atomic.LoadInt32(c.posts)) }

// TestForwardingHostKeys_AppliesToCurrentGeneration: the 201 lands while cur()
// still serves generation 0, and the apply MUST re-resolve cur() so the anchor
// lands in generation 1 (the generation a concurrent hot rebuild swapped in).
// A reconnect then matches against the current generation with ZERO second
// forward.
func TestForwardingHostKeys_AppliesToCurrentGeneration(t *testing.T) {
	blob, pub := newHostKey(t)

	pin := newCountingPinServer(t, http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"target"}`)
	fwd := newTestForwarder(t, pin.srv)

	gen0, srv0 := newReadOnlyStoreWithEntry(t)
	gen1, srv1 := newReadOnlyStoreWithEntry(t)
	calls := 0
	cur := func() *store.Store {
		calls++
		if calls == 1 {
			return gen0 // the read that discovers "no anchor yet"
		}
		return gen1 // a hot rebuild fired during the forwarding HTTP call
	}
	hk := ForwardingHostKeys(fwd)(cur)

	cb, err := sshbroker.HostKeyTOFU(hk, srv0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("192.168.1.108", nil, pub); err != nil {
		t.Fatalf("first handshake must continue after a 201 forward: %v", err)
	}
	if got, err := gen1.GetEntryPin(srv1); err != nil || got == nil || !got.Matches(blob) {
		t.Fatalf("the anchor must land in the current generation: pin=%v err=%v", got, err)
	}
	if got, _ := gen0.GetEntryPin(srv0); got != nil {
		t.Fatalf("the swapped-out generation must NOT receive the anchor: %v", got)
	}
	if n := pin.postCount(); n != 1 {
		t.Fatalf("posts = %d, want 1", n)
	}

	// Reconnect in the same process: the anchor in the current generation
	// matches — zero second forwarding.
	if err := cb("192.168.1.108", nil, pub); err != nil {
		t.Fatalf("reconnect must match the applied anchor: %v", err)
	}
	if n := pin.postCount(); n != 1 {
		t.Fatalf("reconnect re-forwarded: posts = %d, want still 1", n)
	}
}

// TestForwardingHostKeys_BranchWiringThroughTOFU walks the decisive branches
// through the REAL HostKeyTOFU callback, asserting the surfaced text
// byte-for-byte (PresentedHostKeyError passthrough — no prefix) and the
// 201/409-equal auto-apply.
func TestForwardingHostKeys_BranchWiringThroughTOFU(t *testing.T) {
	blob, pub := newHostKey(t)

	t.Run("201 applies and continues", func(t *testing.T) {
		pin := newCountingPinServer(t, http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"target"}`)
		gen, srv := newReadOnlyStoreWithEntry(t)
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, srv)
		if err != nil {
			t.Fatal(err)
		}
		if err := cb("192.168.1.108", nil, pub); err != nil {
			t.Fatalf("201 must let the handshake continue: %v", err)
		}
		if got, _ := gen.GetEntryPin(srv); got == nil || !got.Matches(blob) {
			t.Fatal("201 must apply the key locally on the current store")
		}
	})

	t.Run("409 equal=true auto-closes", func(t *testing.T) {
		pin := newCountingPinServer(t, http.StatusConflict, `{"error":"already pinned","equal":true}`)
		gen, srv := newReadOnlyStoreWithEntry(t)
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, srv)
		if err != nil {
			t.Fatal(err)
		}
		if err := cb("192.168.1.108", nil, pub); err != nil {
			t.Fatalf("409 equal=true must auto-close the duplicate forward: %v", err)
		}
		if got, _ := gen.GetEntryPin(srv); got == nil || !got.Matches(blob) {
			t.Fatal("409 equal=true must apply the key locally (the broker holds an equal anchor)")
		}
	})

	t.Run("409 equal=false verbatim hard error", func(t *testing.T) {
		pin := newCountingPinServer(t, http.StatusConflict, `{"error":"already pinned","equal":false}`)
		gen, srv := newReadOnlyStoreWithEntry(t)
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, srv)
		if err != nil {
			t.Fatal(err)
		}
		err = cb("192.168.1.108", nil, pub)
		if err == nil {
			t.Fatal("409 equal=false must fail the handshake")
		}
		if err.Error() != forwardMsg409Diff("192.168.1.108", 22) {
			t.Fatalf("surfaced text must be the branch text verbatim (no prefix):\n got: %q\nwant: %q", err.Error(), forwardMsg409Diff("192.168.1.108", 22))
		}
		var pe *sshbroker.PresentedHostKeyError
		if !errors.As(err, &pe) {
			t.Fatalf("the branch error must ride PresentedHostKeyError, got %T", err)
		}
		if got, _ := gen.GetEntryPin(srv); got != nil {
			t.Fatal("a rejected forward must not apply anything locally")
		}
	})

	t.Run("nil forwarder keeps the read-only refusal", func(t *testing.T) {
		gen, srv := newReadOnlyStoreWithEntry(t)
		hk := ForwardingHostKeys(nil)(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, srv)
		if err != nil {
			t.Fatal(err)
		}
		err = cb("192.168.1.108", nil, pub)
		if !errors.Is(err, store.ErrReadOnly) {
			t.Fatalf("nil forwarder must degrade to the plain read-only refusal, got: %v", err)
		}
		if got, _ := gen.GetEntryPin(srv); got != nil {
			t.Fatal("the read-only refusal must not pin anything")
		}
	})

	t.Run("apply race surfaces the apply text verbatim", func(t *testing.T) {
		// The ultra-rare race: the read (generation A) finds no anchor, the
		// 201 lands, and by the time the apply runs a hot rebuild has swapped
		// in a generation holding a DIFFERENT anchor — the store apply error's
		// own text (with its pull-and-retry guidance) surfaces verbatim.
		blobOther, _ := newHostKey(t)
		pin := newCountingPinServer(t, http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"target"}`)
		genA, srvA := newReadOnlyStoreWithEntry(t)
		genB, srvB := newReadOnlyStoreWithEntry(t)
		if err := genB.ApplyForwardedEntryPin(srvB, "192.168.1.108:22", blobOther); err != nil {
			t.Fatal(err)
		}
		calls := 0
		cur := func() *store.Store {
			calls++
			if calls == 1 {
				return genA // the read: no anchor — the forward fires
			}
			return genB // the generation the rebuild swapped in before the apply
		}
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(cur)
		cb, err := sshbroker.HostKeyTOFU(hk, srvA)
		if err != nil {
			t.Fatal(err)
		}
		err = cb("192.168.1.108", nil, pub)
		if err == nil || !strings.Contains(err.Error(), "existing pin differs from the presented key (pull and retry)") {
			t.Fatalf("apply-race text mismatch: %v", err)
		}
		if n := pin.postCount(); n != 1 {
			t.Fatalf("posts = %d, want 1 (the broker accepted the key before the race)", n)
		}
	})
}
