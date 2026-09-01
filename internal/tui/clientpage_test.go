package tui

import (
	"path/filepath"
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

// ---------------------------------------------------------------------------
// Plan 45 T3: 配对向导接线 — 完成换槽刷新 / Esc 退回 / [p] 重配入口 / footer
// ---------------------------------------------------------------------------

// TestClientModel_WizardDoneSwitchesSlot: pairWizardDoneMsg carries the freshly
// paired instance — the session switches to THAT slot and the hand-back
// re-reads it (instancePickedMsg semantics; the slot is seeded with a
// distinguishable server name so a routing mix-up cannot pass).
func TestClientModel_WizardDoneSwitchesSlot(t *testing.T) {
	base := mkInstanceDir(t, "agentA")
	mem := tuiWithDEK(t)
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	seedTUISlot(t, filepath.Join(base, "instances", "agentA"), "agentA-dev", "paired-srv", dek)

	m := newClientModelForGate(t)
	m.overlay = &spyOverlay{} // the gate must not swallow the wizard's done msg
	nm, cmd := m.Update(pairWizardDoneMsg{instance: "agentA"})
	cm := nm.(clientModel)
	if cm.instance != "agentA" {
		t.Fatalf("the wizard's instance must become the session slot, got %q", cm.instance)
	}
	if cm.overlay != nil {
		t.Fatalf("done must close the wizard overlay, got %T", cm.overlay)
	}
	if cm.err != nil {
		t.Fatalf("done must clear the panel error, got %v", cm.err)
	}
	ready, ok := cmd().(dataReadyMsg)
	if !ok {
		t.Fatalf("the hand-back must re-read that slot (refreshDataCmdFor), got cmd()=%T", cmd())
	}
	if ready.instance != "agentA" {
		t.Fatalf("the refresh reply must carry the new slot, got %q", ready.instance)
	}
	if len(ready.snap.Servers) != 1 || ready.snap.Servers[0].Name != "paired-srv" {
		t.Fatalf("refresh must have read agentA's OWN snapshot, got %+v", ready.snap.Servers)
	}
}

// TestClientModel_WizardClosedReturnsToPage: Esc at any wizard step lands back
// on the client panel — overlay dropped, slot untouched, no refresh cmd (a
// zero-residue abort never touched the slot; if a mid-flight force abort left
// the slot half-cleaned, the next [s]/refresh surfaces the honest error).
func TestClientModel_WizardClosedReturnsToPage(t *testing.T) {
	m := newClientModelForGate(t)
	m.instance = "agentA"
	m.overlay = &spyOverlay{}
	nm, cmd := m.Update(pairWizardClosedMsg{})
	cm := nm.(clientModel)
	if cm.overlay != nil {
		t.Fatalf("closed must drop the overlay, got %T", cm.overlay)
	}
	if cm.instance != "agentA" {
		t.Fatalf("close must not switch the slot, got %q", cm.instance)
	}
	if cmd != nil {
		t.Fatalf("close is a pure return — no cmd, got %T", cmd())
	}
}

// TestClientModel_PickerPairOpensWizard: the picker's [p] request swaps the
// overlay for the wizard prefilled with the row's instance + Force (the
// wizard's own confirm screen gates the cleanup).
func TestClientModel_PickerPairOpensWizard(t *testing.T) {
	isolatedConfigDir(t)
	m := newClientModelForGate(t)
	m.overlay = newInstancePicker()
	nm, cmd := m.Update(instancePickerPairMsg{instance: "agentA"})
	cm := nm.(clientModel)
	w, ok := cm.overlay.(*pairWizard)
	if !ok {
		t.Fatalf("[p] must swap the picker for the wizard, got overlay %T", cm.overlay)
	}
	if w.prefill.Instance != "agentA" || !w.prefill.Force {
		t.Fatalf("the re-pair wizard must prefill Instance+Force, got %+v", w.prefill)
	}
	if cmd == nil {
		t.Fatal("the wizard's Init cmd must be handed back")
	}
}

// TestClientModel_PickerPairRefusedStaysOnPicker: a single-slot override makes
// newPairWizard refuse — the picker stays up and the refusal renders below it
// (M1 parity: an error set while an overlay is up must be visible).
func TestClientModel_PickerPairRefusedStaysOnPicker(t *testing.T) {
	isolatedConfigDir(t)
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // full single-slot override
	m := newClientModelForGate(t)
	pick := newInstancePicker()
	m.overlay = pick
	nm, cmd := m.Update(instancePickerPairMsg{instance: "agentA"})
	cm := nm.(clientModel)
	if cm.overlay != pick {
		t.Fatalf("a refused pair request must keep the picker up, got %T", cm.overlay)
	}
	if cm.err == nil {
		t.Fatal("the refusal must surface as the panel error")
	}
	if cmd != nil {
		t.Fatal("a refusal hands back no cmd")
	}
}

// TestClientView_FooterAdvertisesWizard: [c] really opens the wizard now — the
// footer drops Plan 42's retired "=pair" pointer; under a single-slot override
// the [c] hint disappears together with [i] (newPairWizard refuses to start
// there — the footer must not lie).
func TestClientView_FooterAdvertisesWizard(t *testing.T) {
	mkInstanceDir(t, "agentA") // isolation + both override envs cleared
	v := newClientModelForGate(t).View().Content
	if !strings.Contains(v, "[c]入网") {
		t.Fatalf("the multi-instance footer must advertise [c]入网, got:\n%s", v)
	}
	if strings.Contains(v, "=pair") {
		t.Fatalf("the retired =pair pointer must be gone, got:\n%s", v)
	}

	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // single-slot override
	v = newClientModelForGate(t).View().Content
	if strings.Contains(v, "[c]入网") {
		t.Fatalf("the single-slot footer must not advertise [c], got:\n%s", v)
	}
	// T4 review B-1: the cred==nil guidance line is the other [c] billboard —
	// under the override it must drop the [c] clause too and point at the CLI
	// path alone (the wizard refuses to start there; no screen may say [c]).
	if strings.Contains(v, "[c]") {
		t.Fatalf("the single-slot view must not advertise [c] anywhere (footer + guidance line), got:\n%s", v)
	}
	if !strings.Contains(v, "运行 sshmgr pair") {
		t.Fatalf("the single-slot guidance line must point at the CLI path, got:\n%s", v)
	}
}
