package clientops

// Plan 48 §2.2/§2.3 + §8 T5b — the cache-mode forwarding HostKeyStore. The
// wrapper composes the PinForwarder's branch table with ApplyForwardedHostKey
// on the CURRENT generation (real-time resolution). The TOFU-callback-level
// assertions here also lock the PresentedHostKeyError passthrough: the §2.1
// branch texts surface through HostKeyTOFU byte-for-byte, with no
// "save host key: " prefix (that prefix stays on raw store failures only).

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// newReadOnlyStore opens an empty read-only store (the hydrated-cache stand-in).
func newReadOnlyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gen.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetReadOnly(nil)
	return st
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

// TestForwardingHostKeys_AppliesToCurrentGeneration is the T5b mechanism at
// the wrapper level: the 201 lands while cur() still serves generation 0, and
// the apply MUST re-resolve cur() so the anchor lands in generation 1 (the
// generation a concurrent hot rebuild swapped in). A reconnect then matches
// against the current generation with ZERO second forward — the forwarding is
// never wasted.
func TestForwardingHostKeys_AppliesToCurrentGeneration(t *testing.T) {
	blob, pub := newHostKey(t)
	const host = "192.168.1.108"
	const port = 22

	pin := newCountingPinServer(t, http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"板"}`)
	fwd := newTestForwarder(t, pin.srv)

	gen0, gen1 := newReadOnlyStore(t), newReadOnlyStore(t)
	calls := 0
	cur := func() *store.Store {
		calls++
		if calls == 1 {
			return gen0 // the read that discovers "no anchor yet"
		}
		return gen1 // a hot rebuild fired during the forwarding HTTP call
	}
	hk := ForwardingHostKeys(fwd)(cur)

	cb, err := sshbroker.HostKeyTOFU(hk, host, port)
	if err != nil {
		t.Fatal(err)
	}
	// First handshake: unknown anchor → forward (201) → apply to the CURRENT
	// generation (gen1), never to the swapped-out gen0.
	if err := cb(host, nil, pub); err != nil {
		t.Fatalf("first handshake must continue after a 201 forward: %v", err)
	}
	if got, err := gen1.GetHostKey(host, port); err != nil || got == nil || !got.Matches(blob) {
		t.Fatalf("the anchor must land in the current generation: pin=%v err=%v", got, err)
	}
	if got, _ := gen0.GetHostKey(host, port); got != nil {
		t.Fatalf("the swapped-out generation must NOT receive the anchor: %v", got)
	}
	if n := pin.postCount(); n != 1 {
		t.Fatalf("posts = %d, want 1", n)
	}

	// Reconnect in the same process: the in-memory anchor in the current
	// generation matches — zero second forwarding.
	if err := cb(host, nil, pub); err != nil {
		t.Fatalf("reconnect must match the applied anchor: %v", err)
	}
	if n := pin.postCount(); n != 1 {
		t.Fatalf("reconnect re-forwarded: posts = %d, want still 1", n)
	}
}

// TestForwardingHostKeys_BranchWiringThroughTOFU walks the decisive branches
// of the §2.1 table through the REAL HostKeyTOFU callback, asserting the
// surfaced text byte-for-byte (PresentedHostKeyError passthrough — no
// "save host key: " prefix) and the 201/409-equal auto-apply.
func TestForwardingHostKeys_BranchWiringThroughTOFU(t *testing.T) {
	blob, pub := newHostKey(t)
	const host = "192.168.1.108"
	const port = 22

	t.Run("201 applies and continues", func(t *testing.T) {
		pin := newCountingPinServer(t, http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"板"}`)
		gen := newReadOnlyStore(t)
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, host, port)
		if err != nil {
			t.Fatal(err)
		}
		if err := cb(host, nil, pub); err != nil {
			t.Fatalf("201 must let the handshake continue: %v", err)
		}
		if got, _ := gen.GetHostKey(host, port); got == nil || !got.Matches(blob) {
			t.Fatal("201 must apply the key locally on the current store")
		}
	})

	t.Run("409 equal=true auto-closes", func(t *testing.T) {
		pin := newCountingPinServer(t, http.StatusConflict, `{"error":"already pinned","equal":true}`)
		gen := newReadOnlyStore(t)
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, host, port)
		if err != nil {
			t.Fatal(err)
		}
		if err := cb(host, nil, pub); err != nil {
			t.Fatalf("409 equal=true must auto-close the duplicate forward: %v", err)
		}
		if got, _ := gen.GetHostKey(host, port); got == nil || !got.Matches(blob) {
			t.Fatal("409 equal=true must apply the key locally (the broker holds an equal anchor)")
		}
	})

	t.Run("409 equal=false verbatim hard error", func(t *testing.T) {
		pin := newCountingPinServer(t, http.StatusConflict, `{"error":"already pinned","equal":false}`)
		gen := newReadOnlyStore(t)
		hk := ForwardingHostKeys(newTestForwarder(t, pin.srv))(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, host, port)
		if err != nil {
			t.Fatal(err)
		}
		err = cb(host, nil, pub)
		if err == nil {
			t.Fatal("409 equal=false must fail the handshake")
		}
		if err.Error() != forwardMsg409Diff(host, port) {
			t.Fatalf("surfaced text must be the verbatim §2.1 branch text (no prefix):\n got: %q\nwant: %q", err.Error(), forwardMsg409Diff(host, port))
		}
		var pe *sshbroker.PresentedHostKeyError
		if !errors.As(err, &pe) {
			t.Fatalf("the branch error must ride PresentedHostKeyError, got %T", err)
		}
		if got, _ := gen.GetHostKey(host, port); got != nil {
			t.Fatal("a rejected forward must not apply anything locally")
		}
	})

	t.Run("nil forwarder keeps the read-only refusal", func(t *testing.T) {
		gen := newReadOnlyStore(t)
		hk := ForwardingHostKeys(nil)(func() *store.Store { return gen })
		cb, err := sshbroker.HostKeyTOFU(hk, host, port)
		if err != nil {
			t.Fatal(err)
		}
		err = cb(host, nil, pub)
		if !errors.Is(err, store.ErrReadOnly) {
			t.Fatalf("nil forwarder must degrade to the plain read-only refusal, got: %v", err)
		}
		if got, _ := gen.GetHostKey(host, port); got != nil {
			t.Fatal("the read-only refusal must not pin anything")
		}
	})

	t.Run("apply race surfaces the apply text verbatim", func(t *testing.T) {
		// The §2.3 ultra-rare race: the read (generation A) finds no anchor,
		// the 201 lands, and by the time the apply runs a hot rebuild has
		// swapped in a generation holding a DIFFERENT anchor —
		// ApplyForwardedHostKey's own text (with its pull-and-retry guidance)
		// surfaces verbatim. The foreign anchor is preset on the SWAPPED-IN
		// generation only, so the TOFU read stays on the no-pin branch.
		blobOther, _ := newHostKey(t)
		pin := newCountingPinServer(t, http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"板"}`)
		genA, genB := newReadOnlyStore(t), newReadOnlyStore(t)
		if err := genB.ApplyForwardedHostKey(host, port, blobOther); err != nil {
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
		cb, err := sshbroker.HostKeyTOFU(hk, host, port)
		if err != nil {
			t.Fatal(err)
		}
		err = cb(host, nil, pub)
		want := fmt.Sprintf("apply forwarded host key for %s:%d: existing pin differs from the presented key (pull and retry)", host, port)
		if err == nil || err.Error() != want {
			t.Fatalf("apply-race text mismatch:\n got: %v\nwant: %q", err, want)
		}
		if n := pin.postCount(); n != 1 {
			t.Fatalf("posts = %d, want 1 (the broker accepted the key before the race)", n)
		}
	})
}
