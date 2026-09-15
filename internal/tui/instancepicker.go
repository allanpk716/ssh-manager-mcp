package tui

// instancePicker lists the default slot + every named instance (spec §3.1).
// Rows are LIGHT — pure existence stats (four-element integrity, cache.bin
// age, profile from meta, the bare auth.json-existence bit), NEVER decrypted
// (a DEK fault must not break listing). The default row's label is the
// literal 「（默认实例）」 so a legal instance literally named "default"
// stays distinguishable (spec §0.14/§3.1).
//
// Plan 46 T3 redo: every row now carries the four-element integrity judgment
// (auth+bin+meta+DEK 齐 = 完整; a directory with anything missing = 残缺,
// the missing list inline), the ★ current-slot prefix (clientModel.instance;
// "" = the default row), the ⚠ half-state prefix (the user-accident shape is
// visible FIRST), runewidth column alignment (the old %-14s padded by BYTES
// and broke column boundaries on Chinese instance names), and the trailing
// local-view footnote. Keys: Enter switches; [p] opens the force re-pair
// wizard for ANY named row (完整 or 残缺 — the wizard's confirm screen tiers
// the 419 advisory); the default row has no name to re-pair, so [p] explains
// itself instead (the pinned hint must never suggest `--instance` — a
// reviewed contradiction); [d] hands the delete flow to clientModel (confirm
// screen → RemoveInstance); Esc/q closes.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/paths"
)

// pickerDefaultRowHint is the PINNED Plan 46 copy for [p] on the default row.
// It must never grow a `--instance <名>` suggestion: the default slot has no
// name and no picker re-pair path, so pointing at the flag from here would
// advertise exactly the route this key refuses.
const pickerDefaultRowHint = "默认槽为本机原始身份,不支持 picker 重配;如需重配请先了解默认槽语义后在 CLI 操作"

// pickerDefaultRowNoDelete is the [d] sibling: the default slot is not
// removable through the picker (no name — RemoveInstance refuses); whole-
// machine teardown stays `sshmgr clear` (T2 CLI parity).
const pickerDefaultRowNoDelete = "默认槽不支持 [d] 删除——清空本机全部 sshmgr 数据请用 sshmgr clear"

// pickerLocalViewNote is the trailing footnote: 完整/已配对 are LOCAL file
// judgments — the broker-side revocation state is invisible from here.
const pickerLocalViewNote = "完整/已配对标注为本地视角——远端吊销状态不可见"

type instancePicker struct {
	rows   []pickerRow
	cursor int
	note   string // transient hint line ([p]/[d] on the default row); cleared on navigation
	width  int    // terminal width from WindowSizeMsg (0 = not yet reported) — only the footnote clips
}

type pickerRow struct {
	label    string // display label
	instance string // routing value: "" = default
	age      string
	profile  string
	paired   bool // Plan 45 T3: cache.auth.json exists — the 已配对 marker
	current  bool // Plan 46 T3: this row IS the session's selected slot (★)
	info     slotStat
}

// slotStat is one slot's four-element existence stat (plus whether the slot
// DIRECTORY itself exists — the vacuum/half-state discriminator). Plain
// stats: nothing is ever decrypted, a DEK fault never breaks listing.
type slotStat struct {
	dir  bool
	auth bool
	bin  bool
	meta bool
	dek  bool
}

// complete is the four-element integrity judgment (Plan 46 定案): auth+bin+
// meta+DEK all present. meta counts because with maxOffline>0 it is what the
// offline load stands on.
func (s slotStat) complete() bool { return s.auth && s.bin && s.meta && s.dek }

// halfState: the slot directory exists but material is missing — the user-
// accident shape that must be visible FIRST (⚠ prefix). A directory-less
// slot (the legal fresh default) is NOT half-state.
func (s slotStat) halfState() bool { return s.dir && !s.complete() }

// slotState renders the integrity column from the raw bits (pure — the 16-
// combination auth/bin/meta/DEK matrix is table-tested): "完整" when all
// four exist, "空" for a directory-less slot, else "缺 <names>" with the
// missing elements in canonical order (T2 `cache instances ls` naming).
func slotState(auth, bin, meta, dek, dir bool) (label string, missing []string) {
	if !dir {
		return "空", nil
	}
	for _, e := range []struct {
		name string
		have bool
	}{{"auth", auth}, {"bin", bin}, {"meta", meta}, {"DEK", dek}} {
		if !e.have {
			missing = append(missing, e.name)
		}
	}
	if len(missing) == 0 {
		return "完整", nil
	}
	return "缺 " + strings.Join(missing, "·"), missing
}

// instancePickedMsg carries the chosen routing value to clientModel.
type instancePickedMsg struct{ instance string }

// instancePickerClosedMsg is the Esc path (no change).
type instancePickerClosedMsg struct{}

// instancePickerPairMsg (Plan 45 T3, Plan 46 widened) asks clientModel to
// open the force re-pair wizard for a NAMED row — the wizard's confirm
// screen carries the 419 advisory tier matching the slot's actual state.
type instancePickerPairMsg struct{ instance string }

// instancePickerDeleteMsg (Plan 46 T3) asks clientModel to run the delete
// flow for a NAMED row: its huh confirm screen → clientops.RemoveInstance.
type instancePickerDeleteMsg struct{ instance string }

func newInstancePicker(current string) *instancePicker {
	p := &instancePicker{}
	p.rows = append(p.rows, p.newRow("（默认实例）", "", current == ""))
	if names, err := clientops.ListInstances(); err == nil {
		for _, n := range names {
			p.rows = append(p.rows, p.newRow(n, n, n == current))
		}
	}
	return p
}

func (p *instancePicker) newRow(label, instance string, current bool) pickerRow {
	r := pickerRow{label: label, instance: instance, current: current}
	r.age, r.profile, r.paired, r.info = pickerSlotInfo(instance)
	return r
}

// pickerSlotInfo stats one slot's display facts WITHOUT decrypting: cache.bin
// age, meta profile, the 已配对 bit (clientops.IsEnrolled — the same judgment
// the wizard's form gate uses) and the four-element slotStat. A stat fault
// reads as absence: listing never breaks on a bad slot.
func pickerSlotInfo(instance string) (age, profile string, paired bool, st slotStat) {
	dir, bin, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		return "-", "", false, slotStat{}
	}
	if _, serr := os.Stat(dir); serr == nil {
		st.dir = true
	}
	exists := func(p string) bool { _, e := os.Stat(p); return e == nil }
	st.auth = exists(filepath.Join(dir, "cache.auth.json"))
	st.bin = exists(bin)
	st.meta = exists(metaPath)
	if dp, derr := paths.CacheDekPathFor(instance); derr == nil {
		st.dek = exists(dp) // stat only — key material is never read
	}
	if fi, serr := os.Stat(bin); serr == nil {
		age = time.Since(fi.ModTime()).Round(time.Minute).String() + " 前"
	} else {
		age = "无缓存"
	}
	if b, rerr := os.ReadFile(metaPath); rerr == nil {
		var m struct {
			Scoped bool   `json:"scoped"`
			Device string `json:"device_name"`
		}
		if json.Unmarshal(b, &m) == nil && m.Scoped && m.Device != "" {
			profile = m.Device
		}
	}
	if en, eerr := clientops.IsEnrolled(instance); eerr == nil {
		paired = en
	}
	return age, profile, paired, st
}

// slotArtifactsComplete is the four-element judgment for one instance — the
// default for the wizard's force-confirm advisory seam and the same verdict
// the picker rows render (one definition, no drift).
func slotArtifactsComplete(instance string) bool {
	_, _, _, st := pickerSlotInfo(instance)
	return st.complete()
}

func (p *instancePicker) Title() string { return "选择实例" }
func (p *instancePicker) Init() tea.Cmd { return nil }

func (p *instancePicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width = msg.Width
		return p, nil
	case tea.KeyPressMsg:
		k := msg.Key()
		switch {
		case k.Code == tea.KeyUp, k.Text == "k":
			p.note = ""
			if p.cursor > 0 {
				p.cursor--
			}
		case k.Code == tea.KeyDown, k.Text == "j":
			p.note = ""
			if p.cursor < len(p.rows)-1 {
				p.cursor++
			}
		case k.Code == tea.KeyEnter, k.Code == tea.KeySpace:
			row := p.rows[p.cursor]
			return p, func() tea.Msg { return instancePickedMsg{instance: row.instance} }
		case k.Text == "p":
			// Plan 46 T3: [p] on ANY named row (完整 or 残缺 — a 残缺 slot is
			// exactly the one that needs re-pairing) opens the force wizard
			// (prefill Instance+Force; Plan 45's paired-only gate is gone).
			// The default row has no name to re-pair: the pinned hint
			// explains instead, never a wizard, never a --instance pointer.
			row := p.rows[p.cursor]
			if row.instance != "" {
				return p, func() tea.Msg { return instancePickerPairMsg{instance: row.instance} }
			}
			p.note = pickerDefaultRowHint
		case k.Text == "d":
			// Plan 46 T3: [d] on a named row hands the delete flow to
			// clientModel (confirm overlay → RemoveInstance off the UI loop).
			// The default slot is not removable here (no name — `sshmgr
			// clear` is the whole-machine teardown).
			row := p.rows[p.cursor]
			if row.instance != "" {
				return p, func() tea.Msg { return instancePickerDeleteMsg{instance: row.instance} }
			}
			p.note = pickerDefaultRowNoDelete
		case k.Code == tea.KeyEsc, k.Text == "q":
			return p, func() tea.Msg { return instancePickerClosedMsg{} }
		}
	}
	return p, nil
}

func (p *instancePicker) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" 选择实例") + "\n（↑/↓ 选择，Enter 切换，[p] 重配，[d] 删除实例，Esc 取消）\n\n")
	nameW, stateW, ageW := p.columnWidths()
	for i, r := range p.rows {
		cursor := "  "
		if i == p.cursor {
			cursor = "> "
		}
		state, _ := slotState(r.info.auth, r.info.bin, r.info.meta, r.info.dek, r.info.dir)
		paired := ""
		if r.paired {
			paired = "已配对"
		}
		// Columns pad by DISPLAY width (runewidth) — byte padding pushed the
		// boundaries off on Chinese instance names (Plan 46 定案). Rows never
		// clip: data over decoration (the old clip(0, …) posture).
		line := cursor + padW(r.displayName(), nameW) + "  " + padW(state, stateW) + "  " +
			padW(r.age, ageW) + "  " + strings.TrimSpace(r.profile+" "+paired)
		b.WriteString(line + "\n")
	}
	if p.note != "" {
		b.WriteString("\n" + warnStyle.Render(p.note) + "\n")
	}
	b.WriteString(clip(p.width, footerStyle.Render(pickerLocalViewNote)))
	return altScreen(tea.NewView(b.String()))
}

// displayName applies the Plan 46 row prefixes: ⚠ flags a half-state slot
// (the user-accident shape — outermost, so it is visible first), ★ marks the
// session's current slot (clientModel.instance; "" marks the default row).
func (r pickerRow) displayName() string {
	name := r.label
	if r.current {
		name = "★" + name
	}
	if r.info.halfState() {
		name = "⚠" + name
	}
	return name
}

// columnWidths sizes the padded columns from the CURRENT rows' display
// widths (floored so a short list still reads as a table) — runewidth, not
// bytes, so CJK/mixed names keep every column start aligned.
func (p *instancePicker) columnWidths() (nameW, stateW, ageW int) {
	nameW, stateW, ageW = 10, 8, 10
	for _, r := range p.rows {
		if w := runewidth.StringWidth(r.displayName()); w > nameW {
			nameW = w
		}
		state, _ := slotState(r.info.auth, r.info.bin, r.info.meta, r.info.dek, r.info.dir)
		if w := runewidth.StringWidth(state); w > stateW {
			stateW = w
		}
		if w := runewidth.StringWidth(r.age); w > ageW {
			ageW = w
		}
	}
	return nameW, stateW, ageW
}

// padW right-pads s with spaces to the DISPLAY width w (runewidth) — the
// CJK-safe replacement for byte-width %-Ns padding. Never truncates.
func padW(s string, w int) string {
	if d := runewidth.StringWidth(s); d < w {
		return s + strings.Repeat(" ", w-d)
	}
	return s
}
