package tui

// pairing.go — broker TUI 的 Pairing 批准页(Plan 42 批1 T8,spec §3.3-3)。
//
// 密钥隔离裁决(控制器,覆盖 brief「三件套同屏」原文):SAS 推导需要 serve 进程
// 内存里的 X25519 私钥,本进程(TUI/CLI)只有 store 直连——因此批准面显示
// name+target_url 两件 + 「SAS 码见 client 屏幕」提示行,owner 对照 client 屏与
// 本行两件后批准,绝不伪造第三件。机械地址校验(ForeignTarget,批1 T5)在此
// 复算:目标 ≠ 本机地址 → 表单顶部 ⚠ + 必须键入 OVERRIDE 才能提交。
//
// 所有裁决经 store CAS(ApprovePairing/RejectPairing 的时间谓词事务),与 serve
// 进程跨进程共享同一张 pairing_pending 表。

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// pairingPage lists the actionable pairing queue (pending + approved rows the
// store still admits; expired rows are lazily cleaned by ListPendingPairing).
type pairingPage struct {
	items            []store.PendingPairing
	profiles         []*models.Profile
	defaultProfileID string // pair.default_profile 解析结果(空 = 无 profile 可选)
	panelList
}

func (p *pairingPage) Title() string { return "Pairing" }

// newPairingPage builds the approval page. defaultProfile is the RAW
// pair.default_profile setting value (profile NAME preferred; an id is also
// accepted) — resolved to an id here, falling back to the first profile.
func newPairingPage(items []store.PendingPairing, profiles []*models.Profile, defaultProfile string) *pairingPage {
	p := &pairingPage{items: items, profiles: profiles, defaultProfileID: resolvePairingDefault(profiles, defaultProfile)}
	p.panelList = newPanelList("配对")
	p.syncList()
	return p
}

// resolvePairingDefault maps the setting value (name first, id fallback) to a
// profile id; unknown/empty → first profile, else "".
func resolvePairingDefault(profiles []*models.Profile, want string) string {
	if len(profiles) == 0 {
		return ""
	}
	want = strings.TrimSpace(want)
	for _, pr := range profiles {
		if pr.Name == want || pr.ID == want {
			return pr.ID
		}
	}
	return profiles[0].ID
}

// pairingItem adapts one pending row to the list panel.
type pairingItem struct{ p store.PendingPairing }

func (i pairingItem) FilterValue() string {
	return stripRow(i.p.Name + " " + i.p.TargetURL + " " + i.p.ProfileHint)
}
func (i pairingItem) Title() string { return stripRow(i.p.Name) }

// Description is the row's one-line summary — the approval surface's two of
// the three-piece line (name is the Title) + IP + remaining window + flags.
func (i pairingItem) Description() string { return pairingRowDesc(i.p, time.Now().Unix()) }

// pairingRowDesc renders one row's flags/summary line (test-visible seam).
// name/target_url/profile_hint are UNAUTHENTICATED enroll input — every render
// strips C0/C1 (spec rev4 codex#4) so an ESC byte can't steer the terminal.
func pairingRowDesc(p store.PendingPairing, now int64) string {
	parts := []string{
		"@ " + stripRow(p.TargetURL),
		"来自 " + orDash(p.SourceIP),
		pairingWindowLabel(p, now),
	}
	if h := strings.TrimSpace(p.ProfileHint); h != "" {
		parts = append(parts, "hint:"+stripRow(h))
	}
	if p.ReplaceInactive {
		parts = append(parts, "⚠将替换未激活码")
	}
	if mcpserver.ForeignTarget(p.TargetURL) {
		parts = append(parts, "⚠目标≠本机")
	}
	return strings.Join(parts, " · ")
}

// pairingWindowLabel states the row's live window: pending rows count down
// their approval (enroll) window; approved rows count the client's finish
// window (120s — the owner should know the clock is running).
func pairingWindowLabel(p store.PendingPairing, now int64) string {
	deadline := p.EnrollDeadline
	label := "待批准"
	if p.State == "approved" {
		deadline = p.ApprovedDeadline
		label = "已批准·finish"
	}
	rem := deadline - now
	if rem < 0 {
		rem = 0
	}
	return fmt.Sprintf("%s 剩%ds", label, rem)
}

func (p *pairingPage) syncList() {
	items := make([]list.Item, len(p.items))
	for i, row := range p.items {
		items[i] = pairingItem{p: row}
	}
	p.setListItems(items, len(items))
}

func (p *pairingPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, row := range p.items {
		out[i] = row.Name
	}
	return out
}

// pairingIDHex renders the 32B row id for cross-surface reference (the CLI's
// approve/reject accept this hex form).
func pairingIDHex(p store.PendingPairing) string { return hex.EncodeToString(p.ID) }

func (p *pairingPage) Detail() string {
	row := p.current()
	if row == nil {
		return "(空)——无待裁决的配对请求\n\n新机在 client 端运行 ssh-manager pair 后\n会出现在这里等待批准。"
	}
	marks := "无"
	switch {
	case row.ReplaceInactive && mcpserver.ForeignTarget(row.TargetURL):
		marks = "⚠将替换未激活码 · ⚠目标≠本机"
	case row.ReplaceInactive:
		marks = "⚠将替换未激活码"
	case mcpserver.ForeignTarget(row.TargetURL):
		marks = "⚠目标≠本机"
	}
	return strings.Join([]string{
		"名称    " + stripRow(row.Name),
		"ID      " + pairingIDHex(*row),
		"目标    " + stripRow(row.TargetURL),
		"来源IP  " + orDash(row.SourceIP),
		"Hint    " + orDash(stripRow(strings.TrimSpace(row.ProfileHint))),
		"窗口    " + pairingWindowLabel(*row, time.Now().Unix()),
		"标记    " + marks,
		"SAS     见 client 屏幕(对照本行名称/地址一致后批准)",
	}, "\n")
}

// stripRow is the pairing surface's render guard: C0/C1 strip on every
// unauthenticated field before it touches the screen (clientops is the shared
// package tui may import — see internal/clientops/sanitize.go).
func stripRow(s string) string { return clientops.StripC0C1(s) }

func (p *pairingPage) current() *store.PendingPairing {
	vis := p.list.VisibleItems()
	i := p.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(pairingItem)
	if !ok {
		return nil
	}
	return &it.p
}

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *pairingPage) Render(width, height int) string {
	return renderPanel(&p.list, p.Detail(), width, height)
}

// ---------------------------------------------------------------------------
// 批准/拒绝提交路径
// ---------------------------------------------------------------------------

// pairingApproval carries the huh-bound answers of the approve form.
type pairingApproval struct {
	ProfileID string // the Select's VALUE — ApprovePairing wants the profile id
	Override  string // foreign 行必须精确键入 OVERRIDE(TrimSpace 容忍首尾空白)
}

// validatePairingOverride is the OVERRIDE gate: a foreign target needs the
// exact literal typed back; a local target never blocks.
func validatePairingOverride(override string, foreign bool) error {
	if !foreign {
		return nil
	}
	if strings.TrimSpace(override) != "OVERRIDE" {
		return errors.New("机械地址校验未通过:配对声明目标 ≠ 本机地址——确属本机请键入 OVERRIDE 覆盖")
	}
	return nil
}

// newPairingApproveForm builds the approval form: profile select (预选
// pair.default_profile) plus — only on a foreign target — the big-⚠ copy and
// the OVERRIDE input. The gate lives in the field's Validate, so the form
// cannot complete without it.
func newPairingApproveForm(p *store.PendingPairing, profiles []*models.Profile, ap *pairingApproval) *huh.Form {
	foreign := mcpserver.ForeignTarget(p.TargetURL)
	desc := fmt.Sprintf("%s @ %s  ·  来源 %s\nSAS 码见 client 屏幕——与本行名称/地址对照一致后再批准。",
		stripRow(p.Name), stripRow(p.TargetURL), orDash(p.SourceIP))
	var fields []huh.Field
	if foreign {
		desc = "⚠ 配对声明目标 ≠ 本机地址(疑似中继/假 discovery/错误网络)。\n\n" + desc
		fields = append(fields, huh.NewInput().Title("键入 OVERRIDE 确认覆盖机械地址校验").
			Value(&ap.Override).Validate(func(v string) error { return validatePairingOverride(v, true) }))
	}
	fields = append(fields, huh.NewSelect[string]().Title("授权 profile(决定该设备能拉到的服务器范围)").
		Options(projectProfileOptions(profiles)...).Value(&ap.ProfileID))
	return huh.NewForm(huh.NewGroup(fields...).Description(desc))
}

// submitPairingApproval runs the CAS approve AFTER the form closes. The
// OVERRIDE gate is re-checked here (defense in depth — the action closure is
// the only path that touches the store, and it must not trust its callers).
// The status line names the two displayable pieces and points at the client
// screen for the SAS — never a fabricated code.
func submitPairingApproval(st *store.Store, p *store.PendingPairing, ap *pairingApproval) tea.Cmd {
	return doAction(st, func() (string, error) {
		if st == nil || p == nil {
			return "", errors.New("内部错误:store 或配对行缺失")
		}
		if err := validatePairingOverride(ap.Override, mcpserver.ForeignTarget(p.TargetURL)); err != nil {
			return "", err
		}
		if strings.TrimSpace(ap.ProfileID) == "" {
			return "", errors.New("必须选择授权 profile")
		}
		ok, err := st.ApprovePairing(p.ID, ap.ProfileID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("批准未生效——该配对已过期或已被处理(CAS 未命中),刷新后重试")
		}
		return fmt.Sprintf("已批准 %s @ %s (对照 client 屏 SAS 后批准)——client 端 120 秒内完成配对",
			stripRow(p.Name), stripRow(p.TargetURL)), nil
	})
}

// submitPairingReject is reject's sibling: same CAS contract, terminal state.
func submitPairingReject(st *store.Store, p *store.PendingPairing) tea.Cmd {
	return doAction(st, func() (string, error) {
		if st == nil || p == nil {
			return "", errors.New("内部错误:store 或配对行缺失")
		}
		ok, err := st.RejectPairing(p.ID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("拒绝未生效——该配对已过期或已被处理(CAS 未命中),刷新后重试")
		}
		return fmt.Sprintf("已拒绝 %s 的配对请求(该设备无法凭本次请求入网)", stripRow(p.Name)), nil
	})
}
