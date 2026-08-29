package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

func TestClientHeader(t *testing.T) {
	h := clientHeader(&clientops.CacheCred{URL: "https://192.0.2.5:7878", Pin: "sha256:" + strings.Repeat("a", 64)}, nil, false, 3, 2*time.Minute)
	for _, want := range []string{"192.0.2.5", "sha256", "3 服务器", "2m"} {
		if !strings.Contains(h, want) {
			t.Fatalf("header missing %q:\n%s", want, h)
		}
	}
}

// TestClientHeaderShowsProfile (Plan 39): the profile segment appears ONLY
// when the pull recorded the serve's scope header (scoped=true) AND the
// snapshot carries exactly one profile. The scoped=false case is the
// code-review #3 fix: a legacy single-profile WHOLE-VAULT snapshot is
// shape-identical to a cropped one — showing its profile would pass the cache
// off as cropped while every vault credential is still on disk.
func TestClientHeaderShowsProfile(t *testing.T) {
	cred := &clientops.CacheCred{URL: "https://192.0.2.5:7878", Pin: "sha256:" + strings.Repeat("a", 64)}
	scoped := &store.Snapshot{Profiles: []store.SnapshotProfile{{Name: "e2e-profile"}}}
	if h := clientHeader(cred, scoped, true, 10, time.Minute); !strings.Contains(h, "profile e2e-profile") {
		t.Fatalf("scoped header must show the bound profile: %s", h)
	}
	// THE FIX: same single-profile snapshot, but pulled pre-Plan-39 (scoped=false) — no segment.
	if h := clientHeader(cred, scoped, false, 10, time.Minute); strings.Contains(h, "profile") {
		t.Fatalf("unverified (legacy) cache must NOT show the profile segment: %s", h)
	}
	if h := clientHeader(cred, nil, true, 10, time.Minute); strings.Contains(h, "profile") {
		t.Fatalf("no-profile snapshot must omit the segment: %s", h)
	}
	multi := &store.Snapshot{Profiles: []store.SnapshotProfile{{Name: "a"}, {Name: "b"}}}
	if h := clientHeader(cred, multi, true, 10, time.Minute); strings.Contains(h, "profile") {
		t.Fatalf("multi-profile (legacy whole-vault) must omit the segment: %s", h)
	}
}

func TestClientServerList(t *testing.T) {
	snap := &store.Snapshot{Servers: []store.SnapshotServer{{Name: "gpu", Host: "192.0.2.10", User: "u"}}}
	rows := clientServerRows(snap)
	if len(rows) != 1 || !strings.Contains(rows[0], "gpu") || !strings.Contains(rows[0], "192.0.2.10") {
		t.Fatalf("rows: %v", rows)
	}
}

// TestSyncCmdRefusesEmptyPin pins the TUI's own no-plaintext invariant: with a
// stored cred lacking a pin, sync must fail fast instead of ever attempting a
// plaintext (AllowPlain=false would only fail later, after a network round
// trip) pull. (The syncCmd wrapper was deleted in Plan 20 T1; the wizard flag
// was deleted in Plan 42 批1 T8 — the panel pull is syncCmdMode(cred, "").)
func TestSyncCmdRefusesEmptyPin(t *testing.T) {
	cred := &clientops.CacheCred{URL: "https://x", Token: "t", Pin: ""}
	msg := syncCmdMode(cred, "")()
	done, ok := msg.(syncDoneMsg)
	if !ok {
		t.Fatalf("want syncDoneMsg, got %T", msg)
	}
	if done.err == nil {
		t.Fatal("want non-nil err for empty pin")
	}
	if !strings.Contains(done.err.Error(), "明文") && !strings.Contains(done.err.Error(), "pin") {
		t.Fatalf("err should mention pin/明文: %v", done.err)
	}
}

// TestClientViewUsesAltScreen pins the 2026-08-17 feedback fix: inline mode
// (AltScreen unset) paints each frame below the previous one instead of
// refreshing in place. bubbletea v2 made altscreen a View field.
func TestClientViewUsesAltScreen(t *testing.T) {
	m := newClientModel()
	if v := m.View(); !v.AltScreen {
		t.Fatal("clientModel.View must set AltScreen (inline mode smears frames)")
	}
}

// TestClient_ColumnsFitTerminalWidth pins the 2026-08-17 feedback fix for the
// client panel: with a WindowSizeMsg known, every display line must fit the
// terminal width (the detail box wraps instead of pushing the frame past the
// edge), and the widest row keeps its gutter before the border.
func TestClient_ColumnsFitTerminalWidth(t *testing.T) {
	m := newClientModel()
	m.snap = &store.Snapshot{Servers: []store.SnapshotServer{{
		Name: "NUC10-authoritative-broker", User: "allan", Host: "192.0.2.5", Port: 22,
		AuthMethod:  "password",
		Hardware:    "NUC10 i7-10710U / 32G",
		Location:    "客厅电视柜第三层",
		Role:        "权威 broker",
		Services:    "sshmgr-serve:7878, docker, nginx, node-exporter",
		Description: "凭据 vault 权威端，跑 serve 服务，兼做内网跳板机和定时备份任务",
	}}}
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 60})
	m = m2.(clientModel)
	content := m.View().Content
	for i, line := range strings.Split(content, "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Fatalf("line %d width %d exceeds terminal width 60:\n%s", i, w, line)
		}
	}
	if strings.Contains(content, "broker╭") {
		t.Fatalf("widest row must not touch the detail border:\n%s", content)
	}
}

// TestClient_FilterLocksActions (2026-08-17 桌面化): while the `/` filter
// input is taking keys, action letters must stay in the filter — [s] must
// NOT start a sync (busy stays false).
func TestClient_FilterLocksActions(t *testing.T) {
	m := newClientModel()
	m.snap = &store.Snapshot{Servers: []store.SnapshotServer{
		{Name: "gpu", User: "u", Host: "192.0.2.10"},
		{Name: "nuc10", User: "allan", Host: "192.0.2.5"},
	}}
	m.syncList()
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	nm2, _ := nm.(clientModel).Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	nm3, _ := nm2.(clientModel).Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	got := nm3.(clientModel)
	if got.busy {
		t.Fatal("[s] typed into the filter must not start a sync")
	}
	if !got.filtering() {
		t.Fatal("filter input must still own the keys")
	}
	if got.list.FilterInput.Value() != "s" {
		t.Fatalf("the keypress must land in the filter input: %q", got.list.FilterInput.Value())
	}
}
