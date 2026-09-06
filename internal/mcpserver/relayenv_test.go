package mcpserver

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestResolveRelayChunk pins the SSHMGR_TRANSFER_CHUNK fail-closed contract
// (Plan 47 spec §4): unset/empty → 256 MiB default; a legal value inside
// [16 MiB, 1 GiB] passes verbatim; unparsable / non-positive / below the
// 16 MiB floor / over the 1 GiB ceiling → error (a construction refusal,
// never a silent clamp — SSHMGR_CACHE_DEK lesson: every new production path
// gets a real seam).
func TestResolveRelayChunk(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    int64
		wantErr bool
	}{
		{"unset → 256 MiB default", "", 256 << 20, false},
		{"explicit legal value", "67108864", 64 << 20, false},
		{"exactly 16 MiB floor", "16777216", 16 << 20, false},
		{"exactly 1 GiB ceiling", "1073741824", 1 << 30, false},
		{"one over the ceiling", "1073741825", 0, true},
		{"one under the floor", "16777215", 0, true},
		{"non-numeric", "256MiB", 0, true},
		{"zero", "0", 0, true},
		{"negative", "-5", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSHMGR_TRANSFER_CHUNK", c.env)
			got, err := resolveRelayChunk()
			if c.wantErr {
				if err == nil {
					t.Fatalf("env=%q: want error, got chunk=%d", c.env, got)
				}
				if !strings.HasPrefix(err.Error(), "SSHMGR_TRANSFER_CHUNK:") {
					t.Fatalf("env=%q: error must name the seam, got: %v", c.env, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("env=%q: got chunk=%d err=%v, want %d nil", c.env, got, err, c.want)
			}
		})
	}
}

// TestResolveRelayParallel pins the SSHMGR_TRANSFER_PARALLEL contract (Plan 47
// spec §4): absent/empty or "1" → 1; ANY other value → error whose text names
// the reserved-for-a-future-version status (v1 is single-stream; the seam name
// and the final v2 clamp domain [1,8] are frozen so opening it later is a
// zero-migration change). The comparison is literal: only the exact string
// "1" is accepted ("01", " 1" → error — fail closed on anything unlisted).
func TestResolveRelayParallel(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		wantErr bool
	}{
		{"unset → 1", "", false},
		{"explicit 1", "1", false},
		{"2 rejected", "2", true},
		{"0 rejected", "0", true},
		{"negative rejected", "-1", true},
		{"non-numeric rejected", "abc", true},
		{"padded 1 rejected", " 1", true},
		{"01 rejected (literal seam)", "01", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSHMGR_TRANSFER_PARALLEL", c.env)
			got, err := resolveRelayParallel()
			if c.wantErr {
				if err == nil {
					t.Fatalf("env=%q: want error, got %d", c.env, got)
				}
				if !strings.Contains(err.Error(), "reserved for a future version") {
					t.Fatalf("env=%q: error text must name the reserved status, got: %v", c.env, err)
				}
				return
			}
			if err != nil || got != 1 {
				t.Fatalf("env=%q: got %d err=%v, want 1 nil", c.env, got, err)
			}
		})
	}
}

// TestNewServerFromSource_RelayChunkFailClosed: an invalid SSHMGR_TRANSFER_CHUNK
// must refuse construction (spec §8 env-seam row). Because the relay resolvers
// run BEFORE any tunnels/tasks manager construction and any StartSweeper
// (spec §4, rev3 codex#6), the error path returns with zero managers handed
// back — nothing was started, so nothing can leak a sweeper goroutine. The
// observable assertion is the nil-triple contract (nothing for the caller to
// CloseAll); the ordering guarantee itself is the source-level invariant that
// the resolver calls are the first statements of NewServerFromSource.
func TestNewServerFromSource_RelayChunkFailClosed(t *testing.T) {
	st := newStore(t)
	t.Setenv("SSHMGR_TRANSFER_CHUNK", "1024") // below the 16 MiB floor → refuse
	srv, mgr, tasks, err := NewServerFromSource(func() *store.Store { return st }, "p", "proj-relay-chunk")
	if err == nil {
		t.Fatalf("invalid SSHMGR_TRANSFER_CHUNK: want construction error, got srv=%v", srv != nil)
	}
	if srv != nil || mgr != nil || tasks != nil {
		t.Fatalf("error path must return zero managers (nothing started, nothing to leak): srv=%v mgr=%v tasks=%v",
			srv != nil, mgr != nil, tasks != nil)
	}
	if !strings.Contains(err.Error(), "SSHMGR_TRANSFER_CHUNK") {
		t.Fatalf("error must name the offending seam: %v", err)
	}
}

// TestNewServerFromSource_RelayParallelFailClosed: same construction-refusal
// contract for SSHMGR_TRANSFER_PARALLEL ≠ 1 — the error text must carry the
// reserved-for-a-future-version guidance.
func TestNewServerFromSource_RelayParallelFailClosed(t *testing.T) {
	st := newStore(t)
	t.Setenv("SSHMGR_TRANSFER_PARALLEL", "2")
	srv, mgr, tasks, err := NewServerFromSource(func() *store.Store { return st }, "p", "proj-relay-par")
	if err == nil {
		t.Fatalf("SSHMGR_TRANSFER_PARALLEL=2: want construction error, got srv=%v", srv != nil)
	}
	if srv != nil || mgr != nil || tasks != nil {
		t.Fatalf("error path must return zero managers: srv=%v mgr=%v tasks=%v",
			srv != nil, mgr != nil, tasks != nil)
	}
	if !strings.Contains(err.Error(), "reserved for a future version") {
		t.Fatalf("error must carry the reserved-status guidance: %v", err)
	}
}

// TestNewServerFromSource_RelayEnvLegalAccepts: legal relay env values must
// NOT break construction — and the happy path's managers close cleanly
// (CloseAll after construction, the brief's leak-hygiene shape).
func TestNewServerFromSource_RelayEnvLegalAccepts(t *testing.T) {
	st := newStore(t)
	t.Setenv("SSHMGR_TRANSFER_CHUNK", "33554432") // 32 MiB, inside [16 MiB, 1 GiB]
	t.Setenv("SSHMGR_TRANSFER_PARALLEL", "1")
	srv, mgr, tasks, err := NewServerFromSource(func() *store.Store { return st }, "p", "proj-relay-ok")
	if err != nil {
		t.Fatalf("legal relay env must construct: %v", err)
	}
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	if srv == nil {
		t.Fatal("want a constructed server")
	}
}
