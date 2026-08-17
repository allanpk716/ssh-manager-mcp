package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"

	"ssh-manager-mcp/internal/models"
)

func TestServersPage_RowsAndDetail(t *testing.T) {
	// Complete server (credential + role): its row is the BARE name — the ⚠
	// prefix (T10) belongs to attention servers only (see
	// TestServersPageWarnSortFilter).
	sp := newServersPage([]*models.Server{{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		CredentialID: "c", Role: "r", Hardware: "2x3090", Tags: []string{"gpu"},
	}})
	if rows := sp.Rows(); len(rows) != 1 || rows[0] != "gpu" {
		t.Fatalf("rows: %v", rows)
	}
	d := sp.Detail()
	for _, want := range []string{"gpu", "192.0.2.10", "2x3090", "gpu"} {
		if !strings.Contains(d, want) {
			t.Fatalf("detail missing %q:\n%s", want, d)
		}
	}
}

// TestServerNeedsAttention is the ⚠ truth table (Plan 20 T10): no credential,
// no role, or the needs-passphrase tag each demand attention; a complete
// server does not.
func TestServerNeedsAttention(t *testing.T) {
	if !serverNeedsAttention(&models.Server{Name: "x"}) {
		t.Fatal("无凭据须 ⚠")
	}
	if !serverNeedsAttention(&models.Server{CredentialID: "c", Role: ""}) {
		t.Fatal("role 空须 ⚠")
	}
	if !serverNeedsAttention(&models.Server{CredentialID: "c", Role: "r", Tags: []string{"needs-passphrase"}}) {
		t.Fatal("缺口令标签须 ⚠")
	}
	if serverNeedsAttention(&models.Server{CredentialID: "c", Role: "r"}) {
		t.Fatal("完整不应 ⚠")
	}
}

// TestServersPageWarnSortFilter: ⚠ servers sort FIRST (stably) and carry the
// "⚠ " row prefix; warnOnly filters the rows down to exactly them; the cursor
// and the detail pane follow the FILTERED view, not raw items.
func TestServersPageWarnSortFilter(t *testing.T) {
	p := newServersPage([]*models.Server{
		{Name: "ok", CredentialID: "c", Role: "r"},
		{Name: "bare"},
		{Name: "ok2", CredentialID: "c", Role: "r"},
	})
	rows := p.Rows()
	if len(rows) != 3 || rows[0] != "⚠ bare" || rows[1] != "ok" || rows[2] != "ok2" {
		t.Fatalf("⚠ 置顶 + 前缀: %v", rows)
	}
	p.warnOnly = true
	p.rebuild()
	rows = p.Rows()
	if len(rows) != 1 || rows[0] != "⚠ bare" {
		t.Fatalf("! 过滤: %v", rows)
	}
	// cursor 0 in the filtered view must resolve to "bare", NOT items[0] "ok"
	if cur := p.current(); cur == nil || cur.Name != "bare" {
		t.Fatalf("current must track the filtered view: %+v", cur)
	}
	if p.Cursor() != 0 {
		t.Fatalf("cursor clamped into filtered range: %d", p.Cursor())
	}
}

// TestServersPageWarnStableOrder: the ⚠ block keeps its incoming relative
// order across repeated sorts (refresh churn must not reshuffle).
func TestServersPageWarnStableOrder(t *testing.T) {
	p := newServersPage([]*models.Server{
		{Name: "ok", CredentialID: "c", Role: "r"},
		{Name: "w1"},
		{Name: "w2", CredentialID: "c"}, // endpoint present but role missing
		{Name: "w3", Tags: []string{"needs-passphrase"}, CredentialID: "c", Role: "r"},
	})
	want := []string{"⚠ w1", "⚠ w2", "⚠ w3", "ok"}
	if got := p.Rows(); !equalRows(got, want) {
		t.Fatalf("stable ⚠ order: got %v want %v", got, want)
	}
	p.rebuild() // simulate a refresh re-applying the sort
	if got := p.Rows(); !equalRows(got, want) {
		t.Fatalf("re-sort must be idempotent: got %v want %v", got, want)
	}
}

func equalRows(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestServersPage_ListKeymapRebind: the bubbles list DEFAULT keymap binds
// single letters that collide with this console's actions (d=下一页 vs d=删除,
// u=上一页 vs u=升级, g=跳转 vs g=授权) — the page must rebind list paging and
// jump keys to non-letter keys only (arrows/pgup/pgdn/home/end).
func TestServersPage_ListKeymapRebind(t *testing.T) {
	sp := newServersPage(nil)
	for name, b := range map[string]key.Binding{
		"PrevPage":  sp.list.KeyMap.PrevPage,
		"NextPage":  sp.list.KeyMap.NextPage,
		"GoToStart": sp.list.KeyMap.GoToStart,
		"GoToEnd":   sp.list.KeyMap.GoToEnd,
	} {
		for _, k := range b.Keys() {
			if len(k) == 1 {
				t.Fatalf("list keymap %s keeps single-letter binding %q — it steals a page action key", name, k)
			}
		}
	}
}
