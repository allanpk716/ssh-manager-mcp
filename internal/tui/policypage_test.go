package tui

// 命令策略页(票 #16)的行为测试:主控台第七页的接线(页数组、
// Tab 循环、页内容)、动作键分发(a 增自定义 / d 删自定义 / s 停用恢复
// 默认规则 / t 试算),以及验收项「TUI 改动与 CLI 改动写同样的审计」——
// 两条路都经 internal/policymanage 落库,这里只断言接线与审计行,不重复
// 测判定语义(判定在 internal/policy 的测试面)。

import (
	"ssh-manager-mcp/internal/models"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/policymanage"
	"ssh-manager-mcp/internal/store"
)

// newPolicyApp 开一个已种下「一条自定义规则 + 一条停用记录」的权威库,
// 构造主控台 App 并直接停在命令策略页。
func newPolicyApp(t *testing.T) (App, *store.Store) {
	t.Helper()
	st := newStore(t)
	if err := st.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []models.ShellType{models.ShellPOSIX},
		Commands:    []string{"reboot"},
		Disposition: policy.DispositionBlock,
		Explanation: "重启由 owner 排期,不由 agent 执行",
		Alternative: "请 owner 安排重启窗口",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DisableDefaultRule("POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatalf("NewBrokerApp: %v", err)
	}
	a.page = pagePolicy
	return a, st
}

// selectPolicyRule 把命令策略页的光标移到指定编号的规则行上(测试导航,
// 与真实按键等价——列表顺序即 ListRules 的顺序)。
func selectPolicyRule(t *testing.T, a App, id string) {
	t.Helper()
	pp, ok := a.pages[pagePolicy].(*policyPage)
	if !ok {
		t.Fatalf("pages[pagePolicy] must be a *policyPage, got %T", a.pages[pagePolicy])
	}
	for i, it := range pp.list.VisibleItems() {
		if rit, ok := it.(policyRuleItem); ok && rit.v.ID == id {
			pp.Select(i)
			return
		}
	}
	t.Fatalf("rule %q not visible on the policy page", id)
}

// policyAuditRows 查指定动作的 owner 审计行(与 CLI 测试同一查询口)。
func policyAuditRows(t *testing.T, st *store.Store, action string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// completeFormOverlay 把 App 上的表单 overlay 驱动到完成:每个字段一次
// Enter(草稿在构造表单前已预填,Enter 即确认当前值进下一项),每次按键
// 后把 cmd 产生的消息回灌 Update(与真实事件循环同形),直到 overlay
// 关闭。驱动不足会漏字段,由调用方的落库断言兜底报红。
func completeFormOverlay(t *testing.T, a App, fields int) App {
	t.Helper()
	for i := 0; i < fields+3 && a.overlay != nil; i++ {
		m, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		a = drain(t, m, cmd).(App)
	}
	return a
}

// TestPolicyPage_IsSeventhTab:页数组形状(第七页 = 命令策略)与页内容——
// 默认规则、自定义规则、停用标注都在列出面,停用标注恰在被停用的那一行。
func TestPolicyPage_IsSeventhTab(t *testing.T) {
	if int(pageCount) != 7 {
		t.Fatalf("pageCount = %d, want 7 (命令策略 is the seventh page)", pageCount)
	}
	a, _ := newPolicyApp(t)
	pp, ok := a.pages[pagePolicy].(*policyPage)
	if !ok {
		t.Fatalf("pages[pagePolicy] must be a *policyPage, got %T", a.pages[pagePolicy])
	}
	if title := pp.Title(); title != "命令策略" {
		t.Fatalf("page title = %q, want 命令策略", title)
	}
	rows := pp.Rows()
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"POSIX-FORMAT", "NO-REBOOT", "已停用"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("policy page rows must contain %q:\n%s", want, joined)
		}
	}
	for _, row := range rows {
		if strings.Contains(row, "POSIX-FORK-BOMB") != strings.Contains(row, "已停用") {
			t.Fatalf("已停用 must mark exactly the disabled rule row: %q", row)
		}
	}
}

// TestPolicyPage_Detail:详情面给出规则的完整自描述——默认规则带种类与
// 停用状态,自定义规则带处置、适用 shell、命令名、风险说明与替代做法。
func TestPolicyPage_Detail(t *testing.T) {
	a, _ := newPolicyApp(t)
	selectPolicyRule(t, a, "NO-REBOOT")
	pp, _ := a.pages[pagePolicy].(*policyPage)
	d := pp.Detail()
	for _, want := range []string{
		"NO-REBOOT", "自定义", "拦截", "posix", "reboot",
		"重启由 owner 排期", "请 owner 安排重启窗口",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("custom rule detail missing %q:\n%s", want, d)
		}
	}
	selectPolicyRule(t, a, "POSIX-FORK-BOMB")
	d = pp.Detail()
	for _, want := range []string{"POSIX-FORK-BOMB", "默认", "已停用"} {
		if !strings.Contains(d, want) {
			t.Fatalf("disabled default rule detail missing %q:\n%s", want, d)
		}
	}
}

// TestPolicyPage_Dispatch:命令策略页的键→动作映射,经真实 Update 路径。
// a 开「新增自定义规则」表单;t 开「试算」overlay;d 在默认规则行不开
// 删除(默认规则只能停用,给提示);s 在自定义规则行不开停用(给提示);
// g 是别页的键,在此必须是空操作。
func TestPolicyPage_Dispatch(t *testing.T) {
	cases := []struct {
		key         string
		selectID    string // 先把光标移到这条规则上("" = 保持首行)
		wantOverlay string // "" = 必须是空操作(无 overlay、无 cmd)
		statusPart  string // 空操作但有状态提示时的提示片段
	}{
		{"a", "POSIX-FORMAT", "新增自定义规则", ""},
		{"t", "POSIX-FORMAT", "试算命令", ""},
		{"d", "POSIX-FORMAT", "", "默认规则不可删除"},
		{"s", "NO-REBOOT", "", "自定义规则不参与停用"},
		{"g", "POSIX-FORMAT", "", ""},
	}
	for _, c := range cases {
		a, _ := newPolicyApp(t)
		if c.selectID != "" {
			selectPolicyRule(t, a, c.selectID)
		}
		m, cmd := a.Update(tea.KeyPressMsg{Code: rune(c.key[0]), Text: c.key})
		got := m.(App)
		if c.wantOverlay == "" {
			if got.overlay != nil || cmd != nil {
				t.Fatalf("key %q on policy page must be a no-op: overlay=%T cmd=%v", c.key, got.overlay, cmd)
			}
			if c.statusPart != "" && !strings.Contains(got.status, c.statusPart) {
				t.Fatalf("key %q must explain itself on the status line, got %q", c.key, got.status)
			}
			continue
		}
		if got.overlay == nil {
			t.Fatalf("key %q must open the %q overlay, got none", c.key, c.wantOverlay)
		}
		if title := got.overlay.Title(); !strings.Contains(title, c.wantOverlay) {
			t.Fatalf("key %q opened %q, want %q", c.key, title, c.wantOverlay)
		}
	}
}

// TestPolicyPage_AddViaForm_WritesAudit:验收用例——[a] 表单提交后规则
// 落库,且写下的 owner 审计行与 CLI 同一动作名(policy-rule.add)、摘要
// 带规则编号。草稿在表单构造前预填(即表单初值),Enter 走完提交。
func TestPolicyPage_AddViaForm_WritesAudit(t *testing.T) {
	st := newStore(t)
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatalf("NewBrokerApp: %v", err)
	}
	a.page = pagePolicy
	// 与 policyKey [a] 同一条装配路径,但草稿先预填(表单初值)。
	d := &policyRuleDraft{
		ID: "NO-SLEEP", Disposition: "block",
		Shells:      []string{"posix"},
		CommandsStr: "shutdown, poweroff",
		Why:         "关机由 owner 排期",
		Instead:     "请 owner 安排维护窗口",
	}
	a.overlay = newPolicyRuleFormOverlay(st, d)
	a = completeFormOverlay(t, a, 9)
	if a.overlay != nil {
		t.Fatalf("form must complete and close, still open: %T", a.overlay)
	}
	rules, err := policymanage.ListRules(st)
	if err != nil {
		t.Fatal(err)
	}
	var found *policymanage.RuleView
	for i := range rules {
		if rules[i].ID == "NO-SLEEP" {
			found = &rules[i]
		}
	}
	if found == nil {
		t.Fatalf("NO-SLEEP must land in the store via the TUI form, rules: %+v", rules)
	}
	if found.Disposition != policy.DispositionBlock ||
		len(found.Commands) != 2 || found.Commands[0] != "shutdown" ||
		len(found.Shells) != 1 || found.Shells[0] != models.ShellPOSIX {
		t.Fatalf("stored rule fields: %+v", found)
	}
	// 审计:恰一行 policy-rule.add,与 CLI 的动作名与摘要形态一致。
	rows := policyAuditRows(t, st, "policy-rule.add")
	if len(rows) != 1 || !strings.Contains(rows[0].Command, "NO-SLEEP") || rows[0].Status != "ok" {
		t.Fatalf("policy-rule.add rows = %+v", rows)
	}
	// 列出面经 actionDoneMsg 的重取后含新规则。
	if v := strings.Join(a.pages[pagePolicy].Rows(), "\n"); !strings.Contains(v, "NO-SLEEP") {
		t.Fatalf("refetched page must list the new rule:\n%s", v)
	}
}

// TestPolicyPage_AddFormEscAbortsNothing:Esc 中止表单不得落库、不得写审计
// (formOverlay 的中止路径不运行 action,此处钉住这个语义不被破坏)。
func TestPolicyPage_AddFormEscAbortsNothing(t *testing.T) {
	st := newStore(t)
	d := &policyRuleDraft{ID: "NO-SLEEP", CommandsStr: "shutdown", Disposition: "block", Why: "w"}
	a, _ := NewBrokerApp(st)
	a.page = pagePolicy
	a.overlay = newPolicyRuleFormOverlay(st, d)
	m, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	a = drain(t, m, cmd).(App)
	if a.overlay != nil {
		t.Fatal("Esc must close the form overlay")
	}
	// 列出面永远含默认规则,此处断言的是没有落下任何自定义规则。
	rules, _ := policymanage.ListRules(st)
	for _, v := range rules {
		if v.Custom {
			t.Fatalf("aborted form must not persist custom rules: %+v", v)
		}
	}
	if rows := policyAuditRows(t, st, "policy-rule.add"); len(rows) != 0 {
		t.Fatalf("aborted form must not write audit: %+v", rows)
	}
}

// TestPolicyPage_DeleteViaKey_WritesAudit:[d] 在自定义规则行开确认,
// 确认后规则删除、审计写 policy-rule.rm(与 CLI 同一动作名)。
func TestPolicyPage_DeleteViaKey_WritesAudit(t *testing.T) {
	a, st := newPolicyApp(t)
	selectPolicyRule(t, a, "NO-REBOOT")
	m, cmd := a.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	got := m.(App)
	if got.overlay == nil || !strings.Contains(got.overlay.Title(), "删除自定义规则") {
		t.Fatalf("d on a custom rule must open the delete confirm, got %v", got.overlay)
	}
	m = drain(t, m, cmd)
	m2, c := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}) // Confirm 肯定单键
	a = drain(t, m2, c).(App)
	for _, v := range a.pages[pagePolicy].Rows() {
		if strings.Contains(v, "NO-REBOOT") {
			t.Fatalf("deleted rule must leave the refetched list: %v", a.pages[pagePolicy].Rows())
		}
	}
	rows := policyAuditRows(t, st, "policy-rule.rm")
	if len(rows) != 1 || !strings.Contains(rows[0].Command, "NO-REBOOT") || rows[0].Status != "ok" {
		t.Fatalf("policy-rule.rm rows = %+v", rows)
	}
}

// TestPolicyPage_DisableEnableViaKey_WritesAudit:[s] 在默认规则行切换
// 停用/恢复:恢复直接执行,停用先经确认;两步各写与 CLI 同名的审计行,
// 列出面的停用标注随之变化。
func TestPolicyPage_DisableEnableViaKey_WritesAudit(t *testing.T) {
	a, st := newPolicyApp(t) // POSIX-FORK-BOMB 已停用
	selectPolicyRule(t, a, "POSIX-FORK-BOMB")

	// 已停用 → [s] 恢复:直接执行(恢复是找回护栏,无需确认)。
	m, cmd := a.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	if got := m.(App); got.overlay != nil {
		t.Fatalf("s on a disabled rule must run the restore directly, got overlay %T", got.overlay)
	}
	a = drain(t, m, cmd).(App)
	if rows := policyAuditRows(t, st, "policy-rule.enable"); len(rows) != 1 ||
		!strings.Contains(rows[0].Command, "POSIX-FORK-BOMB") || rows[0].Status != "ok" {
		t.Fatalf("policy-rule.enable rows = %+v", rows)
	}
	for _, row := range a.pages[pagePolicy].Rows() {
		if strings.Contains(row, "POSIX-FORK-BOMB") && strings.Contains(row, "已停用") {
			t.Fatalf("restored rule must lose the disabled mark: %q", row)
		}
	}

	// 生效中 → [s] 停用:确认表单(停用是 owner 自担风险的放开)。
	m, cmd = a.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	got := m.(App)
	if got.overlay == nil || !strings.Contains(got.overlay.Title(), "停用默认规则") {
		t.Fatalf("s on an enabled rule must open the disable confirm, got %v", got.overlay)
	}
	m = drain(t, m, cmd)
	m2, c := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	a = drain(t, m2, c).(App)
	if rows := policyAuditRows(t, st, "policy-rule.disable"); len(rows) != 1 ||
		!strings.Contains(rows[0].Command, "POSIX-FORK-BOMB") || rows[0].Status != "ok" {
		t.Fatalf("policy-rule.disable rows = %+v", rows)
	}
	for _, row := range a.pages[pagePolicy].Rows() {
		if strings.Contains(row, "POSIX-FORK-BOMB") && !strings.Contains(row, "已停用") {
			t.Fatalf("disabled rule must carry the mark: %q", row)
		}
	}
}

// TestPolicyPage_FooterAdvertisesKeys:页脚只宣传本页可用的键。
func TestPolicyPage_FooterAdvertisesKeys(t *testing.T) {
	a, _ := newPolicyApp(t)
	for _, want := range []string{"[a]新增", "[d]删除", "[s]停用", "[t]试算"} {
		if f := a.footer(); !strings.Contains(f, want) {
			t.Fatalf("policy page footer missing %q: %q", want, f)
		}
	}
}
