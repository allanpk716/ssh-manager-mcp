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
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

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
	p := newInstancePicker("")
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
	p := newInstancePicker("")
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
	p := newInstancePicker("")
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
	p := newInstancePicker("")
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

// TestInstancePicker_PKeyDefaultRowHint (Plan 46 T3): [p] on the default row
// shows the PINNED hint instead of emitting anything — and the hint must never
// grow a `--instance` suggestion (reviewed contradiction: the default slot has
// no name and no picker re-pair path, so the flag would advertise the route
// this key refuses). REWRITES Plan 45 T3's TestInstancePicker_PKeyDisabledRows
// unpaired-row half: Plan 46 widened [p] to ANY named row (完整 or 残缺 — a
// 残缺 slot is exactly the one that needs re-pairing), so an unpaired NAMED
// row now emits the pair request (covered by PKeyRepairsPairedRow's gate).
func TestInstancePicker_PKeyDefaultRowHint(t *testing.T) {
	mkInstanceDir(t, "nude")
	p := newInstancePicker("")
	if _, cmd := p.Update(tea.KeyPressMsg{Code: 'p', Text: "p"}); cmd != nil {
		t.Fatalf("[p] on the default row must not emit, got %T", cmd())
	}
	if v := p.View().Content; !strings.Contains(v, pickerDefaultRowHint) {
		t.Fatalf("the default row's [p] must show the pinned hint, got:\n%s", v)
	}
	if v := p.View().Content; strings.Contains(v, "--instance") {
		t.Fatalf("the picker must never suggest --instance (矛盾文案), got:\n%s", v)
	}
	// 提示是瞬态的:导航即清(picker 重建/换行后不再挂旧话)。
	p.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // cursor → nude
	if v := p.View().Content; strings.Contains(v, pickerDefaultRowHint) {
		t.Fatalf("navigating must clear the transient hint, got:\n%s", v)
	}
	// named 行的 [p] 照常发向导请求(含未配对/残缺行——Plan 46 放宽)。
	_, cmd := p.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	pick, ok := cmd().(instancePickerPairMsg)
	if !ok || pick.instance != "nude" {
		t.Fatalf("[p] on an unpaired NAMED row must emit the pair request, got %T (%+v)", cmd(), pick)
	}
	if v := p.View().Content; !strings.Contains(v, "[p]") {
		t.Fatalf("the picker hint must advertise [p], got:\n%s", v)
	}
}

// ---------------------------------------------------------------------------
// Plan 46 T3:行状态四要素判定 / ★ 当前槽 / ⚠ 半态 / runewidth 列对齐 / [d]
// ---------------------------------------------------------------------------

// TestSlotState_FourElementMatrix:auth/bin/meta/DEK 的 16 组合矩阵逐格断言
// (完整=四者齐;任缺=残缺且行内点名缺什么;无目录=空,永不半态)。
func TestSlotState_FourElementMatrix(t *testing.T) {
	for mask := 0; mask < 16; mask++ {
		auth, bin, meta, dek := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
		var wantMissing []string
		for _, e := range []struct {
			name string
			have bool
		}{{"auth", auth}, {"bin", bin}, {"meta", meta}, {"DEK", dek}} {
			if !e.have {
				wantMissing = append(wantMissing, e.name)
			}
		}
		label, missing := slotState(auth, bin, meta, dek, true)
		if len(wantMissing) == 0 {
			if label != "完整" || missing != nil {
				t.Fatalf("mask %02d: want 完整/无缺项, got %q %v", mask, label, missing)
			}
			continue
		}
		want := "缺 " + strings.Join(wantMissing, "·")
		if label != want || !reflect.DeepEqual(missing, wantMissing) {
			t.Fatalf("mask %02d: want %q %v, got %q %v", mask, want, wantMissing, label, missing)
		}
	}
	// 无目录 = 合法全新默认槽:既非残缺也非半态。
	if label, missing := slotState(false, false, false, false, false); label != "空" || missing != nil {
		t.Fatalf("a directory-less slot must read 空, got %q %v", label, missing)
	}
	if s := (slotStat{dir: true, auth: true}); !s.halfState() {
		t.Fatal("dir present with material missing must be halfState (⚠ 事故形态)")
	}
	if s := (slotStat{}); s.halfState() {
		t.Fatal("a directory-less vacuum must not be halfState")
	}
	if s := (slotStat{dir: true, auth: true, bin: true, meta: true, dek: true}); !s.complete() || s.halfState() {
		t.Fatal("four-of-four must be complete and not half-state")
	}
}

// TestInstancePicker_HalfStateRow:残缺行 ⚠ 前缀 + 行内缺项清单;完整行读
// 完整、无 ⚠;auth.json 在场继续给已配对标注(本地视角,尾注兜底)。
func TestInstancePicker_HalfStateRow(t *testing.T) {
	base := mkInstanceDir(t, "full", "half")
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	// half:目录在、只有 auth → 缺 bin·meta·DEK(用户事故形态)
	if err := os.WriteFile(filepath.Join(base, "instances", "half", "cache.auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// full:四件齐(bin 为垃圾字节即可——完整性判定是纯 stat,永不解密)
	for _, fn := range []string{"cache.auth.json", "cache.bin", "cache.meta.json"} {
		if err := os.WriteFile(filepath.Join(base, "instances", "full", fn), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dekDir, "cache-dek-full.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := newInstancePicker("").View().Content
	half := pickerLineOf(v, "half")
	if !strings.Contains(half, "⚠") || !strings.Contains(half, "缺 bin·meta·DEK") {
		t.Fatalf("half-state row must carry ⚠ and the missing list, got %q", half)
	}
	full := pickerLineOf(v, "full")
	if strings.Contains(full, "⚠") || !strings.Contains(full, "完整") {
		t.Fatalf("complete row must read 完整 without ⚠, got %q", full)
	}
	if !strings.Contains(half, "已配对") {
		t.Fatalf("auth.json presence keeps the 已配对 marker, got %q", half)
	}
	if !strings.Contains(v, pickerLocalViewNote) {
		t.Fatalf("the local-view footnote must render, got:\n%s", v)
	}
}

// TestInstancePicker_CurrentSlotStar:★ 恰标一次、标在会话当前槽行
// (clientModel.instance;"" = 默认槽行)。
func TestInstancePicker_CurrentSlotStar(t *testing.T) {
	mkInstanceDir(t, "agentA")
	v := newInstancePicker("").View().Content
	if !strings.Contains(pickerLineOf(v, "（默认实例）"), "★") {
		t.Fatalf("with an empty session slot the DEFAULT row must carry ★, got:\n%s", v)
	}
	if strings.Contains(pickerLineOf(v, "agentA"), "★") {
		t.Fatalf("a non-current named row must not carry ★, got:\n%s", v)
	}
	v = newInstancePicker("agentA").View().Content
	if !strings.Contains(pickerLineOf(v, "agentA"), "★") {
		t.Fatalf("the session's slot row must carry ★, got:\n%s", v)
	}
	if strings.Contains(pickerLineOf(v, "（默认实例）"), "★") || strings.Count(v, "★") != 1 {
		t.Fatalf("★ must mark exactly the current slot once, got:\n%s", v)
	}
}

// TestInstancePicker_DeleteKey:[d] 具名行发删除请求;默认槽行不发、给钉死
// 提示(清空全机走 sshmgr clear,与 T2 CLI 同语义)。
func TestInstancePicker_DeleteKey(t *testing.T) {
	mkInstanceDir(t, "agentA")
	p := newInstancePicker("")
	p.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // cursor → agentA
	_, cmd := p.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	del, ok := cmd().(instancePickerDeleteMsg)
	if !ok || del.instance != "agentA" {
		t.Fatalf("[d] on a named row must emit instancePickerDeleteMsg, got %T (%+v)", cmd(), del)
	}
	p2 := newInstancePicker("")
	if _, cmd := p2.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}); cmd != nil {
		t.Fatalf("[d] on the default row must not emit, got %T", cmd())
	}
	if v := p2.View().Content; !strings.Contains(v, pickerDefaultRowNoDelete) {
		t.Fatalf("the default row's [d] must show the pinned hint, got:\n%s", v)
	}
}

// TestInstancePicker_CJKColumnAlignment:中文/ASCII/混合行渲染后状态列起点
// (显示宽度)必须一致——老的 %-14s 按字节补空格,中文名一出场列边界即漂移。
func TestInstancePicker_CJKColumnAlignment(t *testing.T) {
	base := mkInstanceDir(t, "agentA", "build-runner-01")
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	// agentA 完整;build-runner-01 残缺(缺 meta);默认行在本环境目录在而材料
	// 缺 → 缺全部。实例名受 instname 白名单约束必为 ASCII——行内真正的宽字符
	// 是默认行标签「（默认实例）」与状态/已配对列,而 %-14s 按字节补空格的老
	// 病根恰在此:18 字节的宽标签超出 14 就不再补位,列边界即漂移。
	for name, files := range map[string][]string{
		"agentA":          {"cache.auth.json", "cache.bin", "cache.meta.json"},
		"build-runner-01": {"cache.auth.json", "cache.bin"},
	} {
		for _, fn := range files {
			if err := os.WriteFile(filepath.Join(base, "instances", name, fn), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, n := range []string{"agentA", "build-runner-01"} {
		if err := os.WriteFile(filepath.Join(dekDir, "cache-dek-"+n+".key"), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := newInstancePicker("build-runner-01") // 当前槽带 ★、残缺带 ⚠ → 混合前缀行
	nameW, _, _ := p.columnWidths()
	stateStart := 2 + nameW + 2 // 光标列(2)+ 名称列 + 双空格栏距 —— 全按显示宽度

	wantStates := map[string]string{
		"（默认实例）":          "缺 auth·bin·meta·DEK",
		"agentA":          "完整",
		"build-runner-01": "缺 meta",
	}
	rows := 0
	for _, line := range strings.Split(p.View().Content, "\n") {
		key := ""
		for k := range wantStates {
			if strings.Contains(line, k) {
				key = k
			}
		}
		if key == "" {
			continue
		}
		rows++
		if rest := cutDisplayWidth(line, stateStart); !strings.HasPrefix(rest, wantStates[key]) {
			t.Fatalf("state column must start at display offset %d for %q (CJK 列对齐), line %q → %q",
				stateStart, key, line, rest)
		}
	}
	if rows != 3 {
		t.Fatalf("want 3 data rows, got %d in:\n%s", rows, p.View().Content)
	}
	// padW 本体:中/混内容补空格后显示宽度恰为列宽。
	if got := runewidth.StringWidth(padW("中文实例名", 14)); got != 14 {
		t.Fatalf("padW must pad to the DISPLAY width, got %d", got)
	}
	if got := runewidth.StringWidth(padW("中文abc", 12)); got != 12 {
		t.Fatalf("padW must pad mixed-width content to the display width, got %d", got)
	}
	if got := padW("超宽不截断", 4); got != "超宽不截断" {
		t.Fatalf("padW must never truncate, got %q", got)
	}
}

// TestInstancePicker_FootnoteClipsToWidth:尾注走 clip(known width 时截断,
// 未上报宽度时原样)。
func TestInstancePicker_FootnoteClipsToWidth(t *testing.T) {
	mkInstanceDir(t, "agentA")
	p := newInstancePicker("")
	if v := p.View().Content; !strings.Contains(v, pickerLocalViewNote) {
		t.Fatalf("footnote must render verbatim without a reported width, got:\n%s", v)
	}
	p.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
	for _, line := range strings.Split(p.View().Content, "\n") {
		if strings.Contains(line, "本地视角") && runewidth.StringWidth(line) > 20 {
			t.Fatalf("the footnote must clip to the reported width, got %q (%d cols)",
				line, runewidth.StringWidth(line))
		}
	}
}

// TestInstancePicker_VacuumDefaultRow:连目录都没有的默认槽(全新机)读作
// 「空」,无 ⚠——真空是合法态,不是事故(T2 ls 的 dirExists 同门)。
func TestInstancePicker_VacuumDefaultRow(t *testing.T) {
	isolatedConfigDir(t) // 不建任何目录:instances 根与默认槽目录都不存在
	v := newInstancePicker("").View().Content
	line := pickerLineOf(v, "（默认实例）")
	if strings.Contains(line, "⚠") || !strings.Contains(line, "空") {
		t.Fatalf("a directory-less default slot must read 空 without ⚠, got %q", line)
	}
}

// pickerLineOf returns the first view line containing needle ("" when none).
func pickerLineOf(v, needle string) string {
	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// cutDisplayWidth drops the first w display columns of s (runewidth walk) —
// the test-side oracle for which character sits at a column boundary.
func cutDisplayWidth(s string, w int) string {
	gone := 0
	for i, r := range s {
		if gone >= w {
			return s[i:]
		}
		gone += runewidth.RuneWidth(r)
	}
	return ""
}
