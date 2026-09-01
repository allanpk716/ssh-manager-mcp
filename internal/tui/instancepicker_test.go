package tui

// Plan 40 批2 T6 §3.1/§3.2/§11.9: instance picker overlay — rows are LIGHT
// (name + bin age + profile from cache.meta.json, never decrypted), Enter picks,
// Esc closes, `[i]` opens, and the auto-picker fires ONCE on a true default-slot
// vacuum with named instances present.
//
// seed 形态沿用 clientpage_instance_test.go：APPDATA/XDG_CONFIG_HOME 重定向 +
// 单槽双 env 清空；需要真材料时复用 seedTUISlot + tuiWithDEK。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

// isolatedConfigDir redirects os.UserConfigDir to a fresh temp dir (so
// InstancesRoot/default-slot paths never touch the real machine) and clears the
// two single-slot override envs (empty string = unset for Getenv).
func isolatedConfigDir(t *testing.T) string {
	t.Helper()
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	return filepath.Join(userDir, "ssh-manager")
}

// mkInstanceDir creates an (empty) named-instance slot directory: presence of
// the directory alone is what ListInstances reports.
func mkInstanceDir(t *testing.T, names ...string) string {
	t.Helper()
	base := isolatedConfigDir(t)
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(base, "instances", n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func TestInstancePicker_RowsAndPick(t *testing.T) {
	mkInstanceDir(t, "agentB", "agentA") // seeding order ≠ row order (sorted read)
	p := newInstancePicker()
	if len(p.rows) != 3 {
		t.Fatalf("want default row + 2 instance rows, got %d (%+v)", len(p.rows), p.rows)
	}
	if p.rows[0].label != "（默认实例）" || p.rows[0].instance != "" {
		t.Fatalf("first row must be the literal default label with routing \"\", got %+v", p.rows[0])
	}
	if p.rows[1].label != "agentA" || p.rows[1].instance != "agentA" ||
		p.rows[2].label != "agentB" || p.rows[2].instance != "agentB" {
		t.Fatalf("instance rows must carry the directory name as both label and routing, got %+v", p.rows[1:])
	}
	for i, r := range p.rows {
		if r.profile != "" {
			t.Fatalf("row %d profile must stay empty without any meta file (no decryption happened), got %q", i, r.profile)
		}
	}

	// j moves the cursor onto agentA's row, then Enter picks it.
	p.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if p.cursor != 1 {
		t.Fatalf("j must advance the cursor, got %d", p.cursor)
	}
	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pick, ok := cmd().(instancePickedMsg)
	if !ok {
		t.Fatalf("Enter must produce instancePickedMsg, got %T", cmd())
	}
	if pick.instance != "agentA" {
		t.Fatalf("Enter on the agentA row must pick agentA, got %q", pick.instance)
	}
}

func TestInstancePicker_EscCloses(t *testing.T) {
	p := newInstancePicker()
	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if _, ok := cmd().(instancePickerClosedMsg); !ok {
		t.Fatalf("Esc must produce instancePickerClosedMsg, got %T", cmd())
	}
}

func TestClientModel_InstanceKeyOpensPicker(t *testing.T) {
	mkInstanceDir(t, "agentA")
	m := newClientModelForGate(t)
	nm, _ := m.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	cm := nm.(clientModel)
	if _, ok := cm.overlay.(*instancePicker); !ok {
		t.Fatalf("[i] must open the instance picker, got overlay %T", cm.overlay)
	}

	busy := newClientModelForGate(t)
	busy.busy = true
	bn, bcmd := busy.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if bcmd != nil {
		t.Fatal("[i] while busy must not start anything")
	}
	if bm := bn.(clientModel); bm.overlay != nil {
		t.Fatalf("[i] while busy must be a no-op, got overlay %T", bm.overlay)
	}
}

// TestClientModel_PickedSwitchesSlot: the picked routing value becomes the
// session's slot AND the returned command re-reads THAT slot — seeded with a
// real decryptable agentA slot whose snapshot server name differs from every
// other slot, so a routing mix-up cannot pass.
func TestClientModel_PickedSwitchesSlot(t *testing.T) {
	base := mkInstanceDir(t, "agentA")
	mem := tuiWithDEK(t)
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	seedTUISlot(t, filepath.Join(base, "instances", "agentA"), "agentA-dev", "picked-srv", dek)

	m := newClientModelForGate(t)
	nm, cmd := m.Update(instancePickedMsg{instance: "agentA"})
	cm := nm.(clientModel)
	if cm.instance != "agentA" {
		t.Fatalf("the picked value must become the session slot, got %q", cm.instance)
	}
	if cm.overlay != nil {
		t.Fatalf("picking must close the picker, got overlay %T", cm.overlay)
	}
	ready, ok := cmd().(dataReadyMsg)
	if !ok {
		t.Fatalf("the hand-back must re-read that slot (refreshDataCmdFor), got cmd()=%T (%v)", cmd(), cmd())
	}
	if ready.instance != "agentA" {
		t.Fatalf("the refresh reply must carry the named slot, got %q", ready.instance)
	}
	if len(ready.snap.Servers) != 1 || ready.snap.Servers[0].Name != "picked-srv" {
		t.Fatalf("refresh must have read agentA's OWN snapshot, got %+v", ready.snap.Servers)
	}
}

func TestClientModel_AutoPickerOnTrueVacuum(t *testing.T) {
	t.Run("true vacuum opens once (errMsg trigger)", func(t *testing.T) {
		mkInstanceDir(t, "agentA") // default slot untouched = true four-file vacuum
		m := newClientModelForGate(t)
		nm, _ := m.Update(errMsg{err: errors.New("no cache yet")})
		cm := nm.(clientModel)
		if _, ok := cm.overlay.(*instancePicker); !ok {
			t.Fatalf("first errMsg on a true vacuum with instances must open the picker, got overlay %T", cm.overlay)
		}
		cm.overlay = nil // simulate the user dismissing it
		nm2, _ := cm.Update(errMsg{err: errors.New("still nothing")})
		if cm2 := nm2.(clientModel); cm2.overlay != nil {
			t.Fatalf("auto-picker is one-shot, re-fired on the second errMsg")
		}
	})

	t.Run("true vacuum opens once (dataReadyMsg trigger consumes the reply)", func(t *testing.T) {
		mkInstanceDir(t, "agentA")
		m := newClientModelForGate(t)
		ready := dataReadyMsg{
			instance: "",
			cred:     &clientops.CacheCred{URL: "https://x", Token: "t"},
			snap:     &store.Snapshot{Servers: []store.SnapshotServer{{ID: "s", Name: "whatever"}}},
		}
		nm, _ := m.Update(ready)
		cm := nm.(clientModel)
		if _, ok := cm.overlay.(*instancePicker); !ok {
			t.Fatalf("first dataReadyMsg on a true vacuum with instances must open the picker, got overlay %T", cm.overlay)
		}
		if cm.snap != nil || cm.cred != nil {
			t.Fatalf("the triggering reply is consumed BY the probe (nothing painted behind it), got snap=%v cred=%v", cm.snap, cm.cred)
		}
	})

	t.Run("half-dead bin present = no vacuum, stays closed", func(t *testing.T) {
		base := mkInstanceDir(t, "agentA")
		if err := os.WriteFile(filepath.Join(base, "cache.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newClientModelForGate(t)
		nm, _ := m.Update(errMsg{err: errors.New("cache.auth.json missing")})
		if cm := nm.(clientModel); cm.overlay != nil {
			t.Fatalf("bin present (half-dead slot) must NOT open the picker, got overlay %T", cm.overlay)
		}
	})

	t.Run("meta present = intent marker, stays closed", func(t *testing.T) {
		base := mkInstanceDir(t, "agentA")
		if err := os.WriteFile(filepath.Join(base, "cache.meta.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newClientModelForGate(t)
		nm, _ := m.Update(errMsg{err: errors.New("x")})
		if cm := nm.(clientModel); cm.overlay != nil {
			t.Fatalf("meta present must NOT open the picker, got overlay %T", cm.overlay)
		}
	})

	t.Run("SSHMGR_CACHE_DIR override disables multi-instance UI", func(t *testing.T) {
		mkInstanceDir(t, "agentA")
		t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // single-slot full override
		m := newClientModelForGate(t)
		nm, _ := m.Update(errMsg{err: errors.New("x")})
		if cm := nm.(clientModel); cm.overlay != nil {
			t.Fatalf("single-slot override must keep the auto-picker off, got overlay %T", cm.overlay)
		}
	})
}

// TestClientGate_RegistersPickerMsgs (Plan 30 checklist): the two picker
// messages are CLIENT-owned types — while ANY overlay is open they must fall
// through to clientModel's own switch, never be swallowed by the gate's
// default branch. Same shape as TestClientGateOwnedFallsThrough.
func TestClientGate_RegistersPickerMsgs(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	for _, owned := range []tea.Msg{instancePickedMsg{}, instancePickerClosedMsg{}} {
		m.overlay = spy
		nm, _ := m.Update(owned)
		if _, ok := nm.(clientModel); !ok {
			t.Fatalf("Update must return clientModel, got %T", nm)
		}
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to clientModel's own case", owned)
		}
	}
}

// ---------------------------------------------------------------------------
// Plan 40 批2 T7 §3.5: single-slot override env → panel banner + footer
// variant + auto-picker stays off. The probe is the SAME predicate as §1.1
// cond 5 (clientops.SingleSlotOverrideEnvSet): SSHMGR_CACHE_DIR /
// SSHMGR_CACHE_DEK count; SSHMGR_CACHE_DEK_DIR (coherent directory-level
// seam) does not.
// ---------------------------------------------------------------------------

// TestClientView_SingleSlotBanner: with a full-override env present the panel
// renders the §3.5 warning banner under the title AND drops the [i] hint from
// the footer (the key would bounce off the Update guard) — the footer reverts
// to the pre-T6 copy.
func TestClientView_SingleSlotBanner(t *testing.T) {
	mkInstanceDir(t, "agentA")                // isolation + both override envs cleared
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // AFTER the helper's clear: full single-slot override

	v := newClientModelForGate(t).View().Content
	if !strings.Contains(v, "单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用") {
		t.Fatalf("single-slot env must render the banner under the title, got:\n%s", v)
	}
	if strings.Contains(v, "[i]实例") {
		t.Fatalf("footer must not advertise [i] while the multi-instance UI is disabled, got:\n%s", v)
	}
}

// TestClientSingleSlot_NoAutoPicker pins the probe exemption through the
// dataReadyMsg arm (T6's table covers errMsg): even on a TRUE default-slot
// vacuum with named instances present, a single-slot override keeps the picker
// closed and processes the reply normally.
func TestClientSingleSlot_NoAutoPicker(t *testing.T) {
	mkInstanceDir(t, "agentA") // true four-file vacuum + one named instance
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())

	m := newClientModelForGate(t)
	nm, _ := m.Update(dataReadyMsg{
		instance: "",
		cred:     &clientops.CacheCred{URL: "https://x", Token: "t"},
		snap:     &store.Snapshot{Servers: []store.SnapshotServer{{ID: "s", Name: "whatever"}}},
	})
	cm := nm.(clientModel)
	if cm.overlay != nil {
		t.Fatalf("single-slot override must keep the auto-picker closed, got overlay %T", cm.overlay)
	}
	if cm.snap == nil || len(cm.snap.Servers) != 1 {
		t.Fatalf("with the probe exempted the reply must be processed normally, got snap=%v", cm.snap)
	}
}

// TestClientSingleSlot_DEKDirExempt: SSHMGR_CACHE_DEK_DIR moves the whole DEK
// tree coherently and does NOT trip single-slot mode — no banner, and the
// multi-instance footer copy stays advertised.
func TestClientSingleSlot_DEKDirExempt(t *testing.T) {
	mkInstanceDir(t, "agentA")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir()) // the ONLY override-shaped env set

	v := newClientModelForGate(t).View().Content
	if strings.Contains(v, "单槽模式") {
		t.Fatalf("SSHMGR_CACHE_DEK_DIR must not render the single-slot banner, got:\n%s", v)
	}
	if !strings.Contains(v, "[s]同步 [i]实例 [c]入网 [t]TTL  q 退出") {
		t.Fatalf("multi-instance footer must stay advertised, got:\n%s", v)
	}
}

// ---------------------------------------------------------------------------
// Plan 45 T3: 已配对标记(auth.json 存在位)+ 已配对行 [p] 重配入口
// ---------------------------------------------------------------------------

// TestInstancePicker_PairedMarker: a named slot holding cache.auth.json rows
// with the 已配对 marker (the same judgment the wizard's form gate uses —
// clientops.IsEnrolled); empty slot dirs stay unpaired. Rows stay LIGHT: the
// marker is a bare stat, never a decrypt.
func TestInstancePicker_PairedMarker(t *testing.T) {
	base := mkInstanceDir(t, "paired", "nude")
	if err := os.WriteFile(filepath.Join(base, "instances", "paired", "cache.auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newInstancePicker()
	byName := map[string]pickerRow{}
	for _, r := range p.rows {
		byName[r.label] = r
	}
	if !byName["paired"].paired {
		t.Fatal("a slot with cache.auth.json must row as paired")
	}
	if byName["nude"].paired {
		t.Fatal("an empty slot dir must not row as paired")
	}
	if byName["（默认实例）"].paired {
		t.Fatal("a vacuum default slot must not row as paired")
	}
	if v := p.View().Content; !strings.Contains(v, "已配对") {
		t.Fatalf("paired rows must show the 已配对 marker, got:\n%s", v)
	}
}

// TestInstancePicker_PKeyRepairsPairedRow: [p] on a paired NAMED row asks the
// clientModel to re-pair that instance (wizard prefill Instance+Force).
func TestInstancePicker_PKeyRepairsPairedRow(t *testing.T) {
	base := mkInstanceDir(t, "agentA")
	if err := os.WriteFile(filepath.Join(base, "instances", "agentA", "cache.auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newInstancePicker()
	p.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // cursor → agentA (sorted read)
	_, cmd := p.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("[p] on a paired row must emit the pair request")
	}
	pick, ok := cmd().(instancePickerPairMsg)
	if !ok {
		t.Fatalf("[p] on a paired row must emit instancePickerPairMsg, got %T", cmd())
	}
	if pick.instance != "agentA" {
		t.Fatalf("the pair request must carry the row's instance, got %q", pick.instance)
	}
}

// TestInstancePicker_PKeyDisabledRows: the default row (instance="" — the
// wizard requires an instance name) and UNPAIRED rows (nothing to re-pair;
// their paths are Enter to switch and [c] to enroll new) must not offer [p].
func TestInstancePicker_PKeyDisabledRows(t *testing.T) {
	mkInstanceDir(t, "nude")
	p := newInstancePicker()
	if _, cmd := p.Update(tea.KeyPressMsg{Code: 'p', Text: "p"}); cmd != nil {
		t.Fatalf("[p] on the default row must be a no-op, got %T", cmd())
	}
	p.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // cursor → nude (unpaired)
	if _, cmd := p.Update(tea.KeyPressMsg{Code: 'p', Text: "p"}); cmd != nil {
		t.Fatalf("[p] on an unpaired row must be a no-op, got %T", cmd())
	}
	if v := p.View().Content; !strings.Contains(v, "[p]") {
		t.Fatalf("the picker hint must advertise [p], got:\n%s", v)
	}
}
