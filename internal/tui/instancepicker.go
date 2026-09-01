package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"ssh-manager-mcp/internal/clientops"
)

// instancePicker lists the default slot + every named instance (spec §3.1).
// Rows are LIGHT — name + cache.bin age + profile from meta + a bare
// auth.json-existence bit, NEVER decrypted (a DEK fault must not break
// listing). The default row's label is the literal 「（默认实例）」 so a legal
// instance literally named "default" stays distinguishable (spec §0.14/§3.1).
// Plan 45 T3: paired named rows offer [p] = force re-pair through the wizard.
type instancePicker struct {
	rows   []pickerRow
	cursor int
}

type pickerRow struct {
	label    string // display label
	instance string // routing value: "" = default
	age      string
	profile  string
	paired   bool // Plan 45 T3: cache.auth.json exists — the 已配对 marker + [p] gate
}

// instancePickedMsg carries the chosen routing value to clientModel.
type instancePickedMsg struct{ instance string }

// instancePickerClosedMsg is the Esc path (no change).
type instancePickerClosedMsg struct{}

// instancePickerPairMsg (Plan 45 T3) asks clientModel to open the re-pair
// wizard for a paired NAMED instance ([p]; prefill Instance+Force).
type instancePickerPairMsg struct{ instance string }

func newInstancePicker() *instancePicker {
	p := &instancePicker{}
	p.rows = append(p.rows, pickerRow{label: "（默认实例）", instance: ""})
	// default-slot age/profile
	p.rows[0].age, p.rows[0].profile, p.rows[0].paired = pickerRowMeta("")
	if names, err := clientops.ListInstances(); err == nil {
		for _, n := range names {
			r := pickerRow{label: n, instance: n}
			r.age, r.profile, r.paired = pickerRowMeta(n)
			p.rows = append(p.rows, r)
		}
	}
	return p
}

// pickerRowMeta reads a slot's bin mtime + meta profile WITHOUT decrypting.
// Plan 45 T3: the third return is the 已配对 bit — cache.auth.json exists
// (clientops.IsEnrolled: the same judgment the wizard's form gate uses). A
// stat fault reads as unpaired: listing never breaks on a bad slot.
func pickerRowMeta(instance string) (age, profile string, paired bool) {
	_, bin, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		return "-", "", false
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
	return age, profile, paired
}

func (p *instancePicker) Title() string { return "选择实例" }
func (p *instancePicker) Init() tea.Cmd { return nil }

func (p *instancePicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	k := kp.Key()
	switch {
	case k.Code == tea.KeyUp, k.Text == "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case k.Code == tea.KeyDown, k.Text == "j":
		if p.cursor < len(p.rows)-1 {
			p.cursor++
		}
	case k.Code == tea.KeyEnter, k.Code == tea.KeySpace:
		row := p.rows[p.cursor]
		return p, func() tea.Msg { return instancePickedMsg{instance: row.instance} }
	case k.Text == "p":
		// Plan 45 T3: re-pair a PAIRED NAMED instance through the wizard
		// (prefill Instance+Force). The default row (instance="" — the wizard
		// requires an instance name) and unpaired rows (nothing to re-pair;
		// their paths are Enter to switch and [c] to enroll new) stay inert.
		row := p.rows[p.cursor]
		if row.instance != "" && row.paired {
			return p, func() tea.Msg { return instancePickerPairMsg{instance: row.instance} }
		}
	case k.Code == tea.KeyEsc, k.Text == "q":
		return p, func() tea.Msg { return instancePickerClosedMsg{} }
	}
	return p, nil
}

func (p *instancePicker) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" 选择实例") + "\n（↑/↓ 选择，Enter 确认，[p] 重配已配对实例，Esc 取消）\n\n")
	for i, r := range p.rows {
		cursor := "  "
		if i == p.cursor {
			cursor = "> "
		}
		paired := ""
		if r.paired {
			paired = "已配对"
		}
		line := fmt.Sprintf("%s%-14s %-14s %s", cursor, r.label, r.age, strings.TrimSpace(r.profile+" "+paired))
		b.WriteString(clip(0, line) + "\n") // width 0 = no truncation (app.go clip)
	}
	return altScreen(tea.NewView(b.String()))
}
