package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// The page must satisfy the App's overlay contract (tea.Model + Title).
var _ overlay = (*serverEditPage)(nil)

// newEditPageAt seeds one server (the page's edit target), prefills the
// draft exactly the way the App's `e` flow will (Plan 29 T3), and builds
// the page at the given width.
func newEditPageAt(t *testing.T, width int) (*serverEditPage, *store.Store, *models.Server) {
	t.Helper()
	st := newStore(t)
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: cid,
		Hardware: "hw", Role: "r",
	}); err != nil {
		t.Fatal(err)
	}
	orig, err := st.GetServerByName("gpu")
	if err != nil || orig == nil {
		t.Fatalf("seed server: %v %v", orig, err)
	}
	return newServerEditPage(st, orig, prefill(orig), width), st, orig
}

// press types one rune into the page (the huh input / list both consume it).
func press(p *serverEditPage, r rune) tea.Cmd {
	_, cmd := p.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	return cmd
}

// tap sends a non-printable key (Enter/Esc/arrows) to the page.
func tap(p *serverEditPage, code rune) tea.Cmd {
	_, cmd := p.Update(tea.KeyPressMsg{Code: code})
	return cmd
}

// ctrl sends ctrl+<r>.
func ctrl(p *serverEditPage, r rune) tea.Cmd {
	_, cmd := p.Update(tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl})
	return cmd
}

// openField drives the cursor FORWARD to field index i and presses Enter —
// the page must land in field state on that field. (Every caller walks
// forward from 0 or stays put; the bound catches a stuck cursor loudly.)
func openField(t *testing.T, p *serverEditPage, i int) {
	t.Helper()
	for n := 0; p.list.Index() != i; n++ {
		if n > 40 {
			t.Fatalf("field %d unreachable, cursor stuck at %d", i, p.list.Index())
		}
		tap(p, tea.KeyDown)
	}
	tap(p, tea.KeyEnter)
	if p.state != editStateField || p.field.Key != p.fields[i].Key {
		t.Fatalf("Enter on field %d must open its form, got state=%v field=%q", i, p.state, p.field.Key)
	}
}

// ① 初始 View：页 1 可见字段 + 保存项存在（跨页）+ 页码 + 帮助行；初始无任何
// 脏标记；↑↓ 跨页可达每一个字段（用户原始痛点）。
func TestEditPageInitialView(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	v := p.View().Content
	// page 1 carries the first fields (per-page 3 at this height), the
	// header, the page indicator and the footer help
	for _, want := range []string{
		"编辑服务器: gpu",
		"名称", "Host", "端口",
		"↑↓ 选择 · Enter 编辑 · Esc 取消",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("initial view missing %q:\n%s", want, v)
		}
	}
	if got := "第 1/" + strconv.Itoa(p.list.Paginator.TotalPages) + " 页"; !strings.Contains(v, got) {
		t.Fatalf("initial view missing page indicator %q:\n%s", got, v)
	}
	if p.list.Paginator.TotalPages < 2 {
		t.Fatalf("16 items must paginate at this height, TotalPages=%d", p.list.Paginator.TotalPages)
	}
	if strings.Contains(v, "●") {
		t.Fatalf("fresh page must show no dirty marks:\n%s", v)
	}
	if !strings.Contains(v, "22") {
		t.Fatalf("port value preview missing:\n%s", v)
	}
	// ↑↓ walks EVERY item across pages: each field label, the save sentinel
	// and a prefilled value preview (hw) surface somewhere along the walk.
	seen := v
	for i := 0; i < len(p.fields); i++ {
		tap(p, tea.KeyDown)
		seen += "\n" + p.View().Content
	}
	for _, want := range []string{
		"SSH 用户", "密码", "私钥路径", "密钥口令", "sudo 密码", "清除凭据",
		"硬件", "位置", "角色", "服务", "Caveats", "备注", "✓ 保存并退出", "hw",
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("↓-walk never surfaced %q:\n%s", want, seen)
		}
	}
}

// ①b 翻页：按 ↓ 跨过本页最后一项，页码走 第 2/Y 页。
func TestEditPagePagingAdvances(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	for i := 0; i < p.list.Paginator.PerPage; i++ {
		tap(p, tea.KeyDown)
	}
	if p.list.Paginator.Page != 1 {
		t.Fatalf("cursor past page 1 must advance the page, got %d", p.list.Paginator.Page)
	}
	if v := p.View().Content; !strings.Contains(v, "第 2/") {
		t.Fatalf("page-2 indicator missing:\n%s", v)
	}
}

// ② Enter 进 field 态 → 打字实时改 draft → 提交回 list 且字段变 ●+（已改）。
func TestEditPageFieldEditMarksDirty(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	openField(t, p, 0) // 名称

	v := p.View().Content
	if !strings.Contains(v, "名称（唯一）") {
		t.Fatalf("field state must render the single-field form:\n%s", v)
	}
	if !strings.Contains(v, "Enter 确认 · Esc 放弃本字段") {
		t.Fatalf("field-state help missing:\n%s", v)
	}
	// clear the prefill (cursor sits at its end) and type a new name
	ctrl(p, 'u')
	for _, r := range "renamed" {
		press(p, r)
	}
	if p.d.Name != "renamed" {
		t.Fatalf("huh binds &d.Name — typing must mutate the draft live, got %q", p.d.Name)
	}
	tap(p, tea.KeyEnter)
	if p.state != editStateList {
		t.Fatalf("completed field form must return to list state, got %v", p.state)
	}
	v = p.View().Content
	if !strings.Contains(v, "● 名称") {
		t.Fatalf("dirty title missing after commit:\n%s", v)
	}
	if !strings.Contains(v, "（已改）") {
		t.Fatalf("dirty value suffix missing after commit:\n%s", v)
	}
	if !strings.Contains(v, "编辑服务器: gpu") {
		t.Fatalf("header must keep the ORIGINAL name while d.Name is edited:\n%s", v)
	}
}

// ③ field 态 Esc → 恢复进入该字段前的值 + 该字段脏标记消失。二次进入时
// 快照基准是“已提交值”，不是最初原值。
func TestEditPageFieldEscRestores(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	snap := snapshotDraft(p.d)

	openField(t, p, 9) // 硬件
	ctrl(p, 'u')
	for _, r := range "JUNK-VALUE" {
		press(p, r)
	}
	if p.d.Hardware != "JUNK-VALUE" {
		t.Fatalf("precondition: draft mutated, got %q", p.d.Hardware)
	}
	tap(p, tea.KeyEsc)
	if p.state != editStateList {
		t.Fatalf("field Esc must return to list state, got %v", p.state)
	}
	if p.d.Hardware != "hw" {
		t.Fatalf("field Esc must restore the pre-field value, got %q", p.d.Hardware)
	}
	dirty := dirtyAgainst(p.d, snap)
	if n := countDirty(dirty); n != 0 {
		t.Fatalf("restored field must be clean against the page snapshot, dirty=%v", dirty)
	}
	if v := p.View().Content; strings.Contains(v, "● 硬件") {
		t.Fatalf("restored field must not show a dirty mark:\n%s", v)
	}

	// committed-then-reenter: Esc restores the COMMITTED value, not orig.
	openField(t, p, 9)
	ctrl(p, 'u')
	for _, r := range "committed" {
		press(p, r)
	}
	tap(p, tea.KeyEnter)
	openField(t, p, 9)
	ctrl(p, 'u')
	for _, r := range "X" {
		press(p, r)
	}
	tap(p, tea.KeyEsc)
	if p.d.Hardware != "committed" {
		t.Fatalf("second Esc must restore the post-commit value, got %q", p.d.Hardware)
	}
	if !dirtyAgainst(p.d, snap)["hardware"] {
		t.Fatal("the earlier commit must stay dirty after the later Esc")
	}
}

// ③b 秘密字段的 Esc 恢复走 T1 缝合规则：快照值经 Set 写回（Get 只给状态串）。
func TestEditPageSecretFieldEscRestores(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	openField(t, p, 4) // 密码 — edit-mode prefill is "" (keep existing)
	for _, r := range "PW-ENTRY-SENTINEL" {
		press(p, r)
	}
	if p.d.Password == "" {
		t.Fatal("precondition: password typed")
	}
	tap(p, tea.KeyEsc)
	if p.d.Password != "" {
		t.Fatalf("secret field Esc must restore the entry snapshot (empty), got %q", p.d.Password)
	}
	if v := p.View().Content; strings.Contains(v, "● 密码") {
		t.Fatalf("restored secret must be clean:\n%s", v)
	}
}

// ④ 保存项 Enter → submit 动作 + formDoneMsg（aborted=false）。
func TestEditPageSaveItemFiresSubmit(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	captured := false
	p.submit = func() tea.Cmd { captured = true; return nil }

	openField(t, p, 9) // dirty one field first — a real save scenario
	ctrl(p, 'u')
	for _, r := range "2x4090" {
		press(p, r)
	}
	tap(p, tea.KeyEnter)

	// walk to the sentinel (last item) and Enter
	for p.list.Index() != len(p.fields) {
		tap(p, tea.KeyDown)
	}
	cmd := tap(p, tea.KeyEnter)
	if cmd == nil {
		t.Fatal("sentinel Enter must produce a formDoneMsg cmd")
	}
	done, ok := cmd().(formDoneMsg)
	if !ok || done.aborted {
		t.Fatalf("save must be formDoneMsg{aborted:false}, got %#v", done)
	}
	if !captured {
		t.Fatal("the submit seam must run (submitServer in production)")
	}
}

// ④b 端到端：默认 submit（submitServer）真落库。
func TestEditPageSaveEndToEnd(t *testing.T) {
	p, st, _ := newEditPageAt(t, 80)
	openField(t, p, 9)
	ctrl(p, 'u')
	for _, r := range "2x4090" {
		press(p, r)
	}
	tap(p, tea.KeyEnter)
	for p.list.Index() != len(p.fields) {
		tap(p, tea.KeyDown)
	}
	cmd := tap(p, tea.KeyEnter)
	done, ok := cmd().(formDoneMsg)
	if !ok || done.aborted || done.after == nil {
		t.Fatalf("end-to-end save must chain submitServer via after, got %#v", done)
	}
	if _, ok := done.after().(actionDoneMsg); !ok {
		t.Fatal("submitServer must succeed (actionDoneMsg)")
	}
	got, _ := st.GetServerByName("gpu")
	if got == nil || got.Hardware != "2x4090" {
		t.Fatalf("saved server must carry the edited value: %+v", got)
	}
}

// ⑤ list 态 Esc → formDoneMsg{aborted:true} 且 store 无写入（即使 draft 已脏）。
func TestEditPageListEscAbortsNoWrite(t *testing.T) {
	p, st, _ := newEditPageAt(t, 80)
	openField(t, p, 9)
	ctrl(p, 'u')
	for _, r := range "SHOULD-NOT-PERSIST" {
		press(p, r)
	}
	tap(p, tea.KeyEnter) // committed to draft — dirty
	cmd := tap(p, tea.KeyEsc)
	if cmd == nil {
		t.Fatal("list Esc must produce a formDoneMsg cmd")
	}
	done, ok := cmd().(formDoneMsg)
	if !ok || !done.aborted || done.after != nil {
		t.Fatalf("list Esc must be formDoneMsg{aborted:true} with no after, got %#v", done)
	}
	got, _ := st.GetServerByName("gpu")
	if got == nil || got.Hardware != "hw" {
		t.Fatalf("abort must not write the store: %+v", got)
	}
}

// ⑥ 秘密预览掩码（哨兵）：经真实 field 流程输入的明文绝不出现在任何 View。
func TestEditPageSecretMaskingSentinel(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	openField(t, p, 4) // 密码
	for _, r := range "PW-FIELD-SENTINEL" {
		press(p, r)
	}
	tap(p, tea.KeyEnter)  // commit → back to list, dirty + masked preview
	v := p.View().Content // cursor at 4 → its page shows the 密码 row
	if strings.Contains(v, "SENTINEL") {
		t.Fatalf("secret plaintext leaked into the list view:\n%s", v)
	}
	if !strings.Contains(v, "● 密码") || !strings.Contains(v, "已设（新值）") {
		t.Fatalf("dirty secret must show the status wording, not content:\n%s", v)
	}
	// same for 密钥口令 — then its page also shows the clean sudo status
	openField(t, p, 6)
	for _, r := range "KP-FIELD-SENTINEL" {
		press(p, r)
	}
	tap(p, tea.KeyEnter)
	v = p.View().Content
	if strings.Contains(v, "SENTINEL") {
		t.Fatalf("secret plaintext leaked into the list view:\n%s", v)
	}
	if !strings.Contains(v, "● 密钥口令") {
		t.Fatalf("dirty keypass row missing:\n%s", v)
	}
	if !strings.Contains(v, "（留空=保持现有）") {
		t.Fatalf("clean sudo secret must show its keep-existing status:\n%s", v)
	}
}

// ⑥b field 态同样掩码：密钥口令的 EchoModePassword 输入 View 不含哨兵明文。
func TestEditPageSecretFieldStateMasked(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	openField(t, p, 6) // 密钥口令 — EchoModePassword
	for _, r := range "KP-FIELD-SENTINEL" {
		press(p, r)
	}
	if v := p.View().Content; strings.Contains(v, "SENTINEL") {
		t.Fatalf("EchoModePassword field view leaked the plaintext:\n%s", v)
	}
}

// ⑦ 宽度 60/120 两档：list 态与 field 态 View 的每一行都 ≤ width。
func TestEditPageWidthFit(t *testing.T) {
	for _, w := range []int{60, 120} {
		p, _, _ := newEditPageAt(t, w)
		checkWidth := func(v string, phase string) {
			t.Helper()
			for i, line := range strings.Split(v, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Fatalf("width %d %s: line %d is %d cols (> %d):\n%s", w, phase, i, lw, w, v)
				}
			}
		}
		checkWidth(p.View().Content, "list state")
		openField(t, p, 13) // Caveats — the longest field title
		checkWidth(p.View().Content, "field state")
	}
}

// ⑦b 宽度跟随：WindowSizeMsg 重设 list 宽度（T3 若转发即生效；行宽不破）。
func TestEditPageWidthFollowsResize(t *testing.T) {
	p, _, _ := newEditPageAt(t, 120)
	p.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	if p.width != 60 || p.list.Width() != 58 {
		t.Fatalf("resize must refit the list: width=%d list=%d", p.width, p.list.Width())
	}
	for i, line := range strings.Split(p.View().Content, "\n") {
		if lw := lipgloss.Width(line); lw > 60 {
			t.Fatalf("post-resize line %d is %d cols (> 60):\n%s", i, lw, p.View().Content)
		}
	}
}
