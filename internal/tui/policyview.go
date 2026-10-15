// policyview.go 承载命令策略的试算面与客户端只读查看器(票 #16,
// 规格 #8):
//   - policyTestOverlay 是主控台策略页 [t] 的试算 overlay:输入命令与
//     shell 类型,结论屏显示处置、命中规则编号、风险说明与替代做法
//     (与 `sshmgr policy test` 同一信息集)。生效规则集在后台命令里
//     从权威库组装,判定只调 internal/policy 的 Evaluate。
//   - policyViewer 是客户端 TUI 的只读策略查看器([p] 打开):规则
//     列表与试算都读缓存快照(policymanage 的快照面),无任何修改
//     入口——规则在权威端管理、随 cache pull 下发(ADR 0007),客户
//     端只读。查看器自含三态(列表/试算表单/结论屏),关闭时以
//     formDoneMsg 交还宿主(App 或 clientModel 均已有该收尾)。
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/policymanage"
	"ssh-manager-mcp/internal/store"
)

// policyTestDraft 是试算表单的状态:命令全文与 shell 类型代号。
type policyTestDraft struct {
	Command string
	Shell   string // select 值:posix / powershell / cmd
}

// newPolicyTestForm 构造试算表单(主控台与客户端共用同一张):命令
// 必填,shell 三选一,默认 POSIX。
func newPolicyTestForm(d *policyTestDraft) *huh.Form {
	if d.Shell == "" {
		d.Shell = string(policy.ShellPOSIX)
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("命令全文（按 shell 类型拆解判定）").Value(&d.Command).Validate(nonEmpty),
		huh.NewSelect[string]().Title("Shell 类型").
			Options(
				huh.NewOption("POSIX（Linux/macOS）", string(policy.ShellPOSIX)),
				huh.NewOption("PowerShell（Windows）", string(policy.ShellPowerShell)),
				huh.NewOption("cmd（Windows）", string(policy.ShellWindowsCmd)),
			).Value(&d.Shell),
	))
}

// policyTestResult 是一次试算的结论:输入的命令与 shell 类型,加上
// 判定内核给出的 Decision。
type policyTestResult struct {
	shell    policy.ShellType
	command  string
	decision policy.Decision
}

// policyTestResultBody 渲染结论屏正文(主控台与客户端共用):处置在
// 首行,命中规则时给出编号、为什么危险、该怎么做——与 agent 执行时
// 被退回的同一行反馈;放行时后三行不出现。
func policyTestResultBody(r *policyTestResult) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "命令    %s\nShell   %s\n处置    %s", r.command, r.shell, r.decision.Disposition.String())
	if r.decision.RuleID != "" {
		fmt.Fprintf(b, "\n规则编号%s", r.decision.RuleID)
	}
	if r.decision.Explanation != "" {
		fmt.Fprintf(b, "\n为什么  %s", r.decision.Explanation)
	}
	if r.decision.Alternative != "" {
		fmt.Fprintf(b, "\n怎么做  %s", r.decision.Alternative)
	}
	return b.String()
}

// policyTestComputedMsg 是试算的后台结论:表单完成后,规则集组装与
// Evaluate 在命令 goroutine 里跑完,经这条内部消息回到 overlay(overlay
// 开着时宿主的转发门把未知消息交给 overlay——App 与 clientModel 的门
// 都如此)。err 非空 = 规则集组装失败,结论屏如实报错。
type policyTestComputedMsg struct {
	shell    policy.ShellType
	command  string
	decision policy.Decision
	err      error
}

// policyTestCmd 组装并执行一次试算(后台命令):加载生效规则集后调用
// 判定内核。load 在主控台读权威库(可能失败),在客户端读缓存快照
// (内存组装,不失败)。
func policyTestCmd(load func() (*policy.RuleSet, error), d *policyTestDraft) tea.Cmd {
	return func() tea.Msg {
		rules, err := load()
		if err != nil {
			return policyTestComputedMsg{err: err}
		}
		shell, serr := policy.ParseShellType(d.Shell)
		if serr != nil {
			return policyTestComputedMsg{err: serr}
		}
		return policyTestComputedMsg{
			shell:    shell,
			command:  d.Command,
			decision: policy.Evaluate(shell, d.Command, rules),
		}
	}
}

// ---------------------------------------------------------------------------
// 主控台试算 overlay(策略页 [t])
// ---------------------------------------------------------------------------

// policyTestOverlay 是主控台的试算 overlay:表单态输入命令与 shell
// 类型,结论态显示判定;结论屏按任意键即整体关闭(formDoneMsg 空载,
// 宿主 App 落回策略页)。Esc 在表单态中止,同样整体关闭。
type policyTestOverlay struct {
	source string                          // 生效规则集来源标注(状态行)
	load   func() (*policy.RuleSet, error) // 生效规则集加载器(权威库)
	draft  *policyTestDraft
	form   *huh.Form
	result *policyTestResult // nil = 表单态
	err    error
	width  int
}

// newPolicyTestOverlay 构造试算 overlay。source 用于状态行的来源标注
// (试算读的是哪一份规则集)。
func newPolicyTestOverlay(source string, load func() (*policy.RuleSet, error)) *policyTestOverlay {
	d := &policyTestDraft{}
	o := &policyTestOverlay{source: source, load: load, draft: d}
	o.form = newPolicyTestForm(d)
	return o
}

// newBrokerPolicyTest 是策略页 [t] 的装配:生效规则集从权威库组装
// (默认规则减停用,加自定义——与执行工具同一形态)。
func newBrokerPolicyTest(st *store.Store) *policyTestOverlay {
	return newPolicyTestOverlay("权威库", func() (*policy.RuleSet, error) {
		return policymanage.EffectiveRuleSet(st)
	})
}

func (o *policyTestOverlay) Title() string { return "试算命令" }
func (o *policyTestOverlay) Init() tea.Cmd { return o.form.Init() }

func (o *policyTestOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		o.width = ws.Width
		return o, nil
	}
	if o.result != nil || o.err != nil {
		// 结论态:任意键关闭(formDoneMsg 空载——App 落回策略页)。
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return o, func() tea.Msg { return formDoneMsg{} }
		}
		return o, nil
	}
	if kp, ok := msg.(tea.KeyPressMsg); ok && kp.Code == tea.KeyEsc {
		return o, func() tea.Msg { return formDoneMsg{aborted: true} }
	}
	if computed, ok := msg.(policyTestComputedMsg); ok {
		if computed.err != nil {
			o.err = computed.err
			return o, nil
		}
		o.result = &policyTestResult{shell: computed.shell, command: computed.command, decision: computed.decision}
		return o, nil
	}
	f, cmd := o.form.Update(msg)
	if nf, ok := f.(*huh.Form); ok {
		o.form = nf
	}
	switch o.form.State {
	case huh.StateCompleted:
		return o, policyTestCmd(o.load, o.draft)
	case huh.StateAborted:
		return o, func() tea.Msg { return formDoneMsg{aborted: true} }
	}
	return o, cmd
}

func (o *policyTestOverlay) View() tea.View {
	if o.err != nil {
		return tea.NewView(titleStyle.Render(" "+o.Title()+" ") + "\n（按任意键返回）\n\n" +
			errStyle.Render("✗ 读取生效规则集失败:"+o.err.Error()))
	}
	if o.result != nil {
		return tea.NewView(titleStyle.Render(" "+o.Title()+" ") + "\n（按任意键返回）\n\n" +
			policyTestResultBody(o.result) +
			"\n\n生效规则集:" + o.source + "(默认规则 − 停用 + 自定义)")
	}
	return tea.NewView(titleStyle.Render(" "+o.Title()+" ") + "\n（Esc 取消）\n" + o.form.View() +
		"\n" + clip(o.width, footerStyle.Render("试算不做任何连接;生效规则集:"+o.source)))
}

// ---------------------------------------------------------------------------
// 客户端只读查看器([p])
// ---------------------------------------------------------------------------

// policyViewerMode 是查看器的三个内部态:规则列表 / 试算表单 / 试算结论。
type policyViewerMode int

const (
	policyViewerList policyViewerMode = iota
	policyViewerTest
	policyViewerResult
)

// pvReadOnlyNote 是客户端只读姿态的统一提示(修改键只给这一句,与 CLI
// 的客户端拒绝文案同一语义):规则在权威端管理、随 cache pull 下发。
const pvReadOnlyNote = "客户端只读——命令策略规则在权威端维护,随 cache pull 下发,本机不可修改;如需变更请在权威端操作"

// policyViewer 是客户端 TUI 的只读策略查看器:规则列表与试算都来自
// 打开时传入的缓存快照(离线即最后一次拉到的形态;旧快照无策略段则
// 退回内置默认规则——policymanage 的快照面兑现)。不存在任何修改入口:
// 修改键只给只读提示。
type policyViewer struct {
	views  []policymanage.RuleView
	rules  *policy.RuleSet // 快照组装的生效规则集(试算输入)
	cursor int
	mode   policyViewerMode
	draft  *policyTestDraft
	form   *huh.Form
	result *policyTestResult
	err    error
	note   string // 瞬态提示行(修改键/导航时清)
	width  int
}

// newPolicyViewer 从缓存快照构造客户端只读查看器。
func newPolicyViewer(snap *store.Snapshot) *policyViewer {
	v := &policyViewer{
		views: policymanage.ListRulesFromSnapshot(snap),
		rules: policymanage.EffectiveRuleSetFromSnapshot(snap),
	}
	return v
}

func (v *policyViewer) Title() string { return "命令策略(只读)" }
func (v *policyViewer) Init() tea.Cmd { return nil }

// startTest 进入试算表单态(共享表单,规则集加载器读快照内存态);上
// 一轮的结论与错误一并清掉,不留陈旧屏。
func (v *policyViewer) startTest() tea.Cmd {
	v.draft = &policyTestDraft{}
	v.form = newPolicyTestForm(v.draft)
	v.mode = policyViewerTest
	v.result, v.err, v.note = nil, nil, ""
	return v.form.Init()
}

func (v *policyViewer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		v.width = ws.Width
		return v, nil
	}
	switch v.mode {
	case policyViewerResult:
		if _, ok := msg.(tea.KeyPressMsg); ok {
			v.mode = policyViewerList // 回规则列表,查看器不整体退出
			v.result, v.err = nil, nil
			return v, nil
		}
		return v, nil
	case policyViewerTest:
		if kp, ok := msg.(tea.KeyPressMsg); ok && kp.Code == tea.KeyEsc {
			v.mode = policyViewerList
			return v, nil
		}
		if computed, ok := msg.(policyTestComputedMsg); ok {
			if computed.err != nil {
				v.err = computed.err
				v.mode = policyViewerResult // 结论屏的出错分支(快照内存组装几乎不可达)
				return v, nil
			}
			v.result = &policyTestResult{shell: computed.shell, command: computed.command, decision: computed.decision}
			v.mode = policyViewerResult
			return v, nil
		}
		f, cmd := v.form.Update(msg)
		if nf, ok := f.(*huh.Form); ok {
			v.form = nf
		}
		switch v.form.State {
		case huh.StateCompleted:
			rules := v.rules // 快照内存态,直接捕获
			return v, policyTestCmd(func() (*policy.RuleSet, error) { return rules, nil }, v.draft)
		case huh.StateAborted:
			v.mode = policyViewerList
			return v, nil
		}
		return v, cmd
	}
	// 列表态
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return v, nil
	}
	k := kp.Key()
	switch {
	case k.Code == tea.KeyUp, k.Text == "k":
		v.note = ""
		if v.cursor > 0 {
			v.cursor--
		}
	case k.Code == tea.KeyDown, k.Text == "j":
		v.note = ""
		if v.cursor < len(v.views)-1 {
			v.cursor++
		}
	case k.Code == tea.KeyEsc, k.Text == "q":
		return v, func() tea.Msg { return formDoneMsg{} }
	case k.Text == "t":
		return v, v.startTest()
	case k.Text == "a", k.Text == "d", k.Text == "s":
		// 修改键:只给只读提示——客户端没有修改入口(验收项)。
		v.note = pvReadOnlyNote
	}
	return v, nil
}

func (v *policyViewer) View() tea.View {
	var b strings.Builder
	switch v.mode {
	case policyViewerResult:
		if v.err != nil {
			b.WriteString(titleStyle.Render(" "+v.Title()+" · 试算") + "\n（按任意键返回列表）\n\n" +
				errStyle.Render("✗ "+v.err.Error()))
			return altScreen(tea.NewView(b.String()))
		}
		b.WriteString(titleStyle.Render(" "+v.Title()+" · 试算") + "\n（按任意键返回列表）\n\n")
		b.WriteString(policyTestResultBody(v.result))
		b.WriteString("\n\n生效规则集:缓存快照(最后一次同步;旧快照退回内置默认规则)")
		return altScreen(tea.NewView(b.String()))
	case policyViewerTest:
		b.WriteString(titleStyle.Render(" "+v.Title()+" · 试算") + "\n（Esc 返回列表）\n" + v.form.View())
		return altScreen(tea.NewView(b.String()))
	}
	b.WriteString(titleStyle.Render(" "+v.Title()+" ") + "\n（↑/↓ 浏览,[t] 试算,Esc 返回;客户端只读,无修改入口）\n\n")
	if len(v.views) == 0 {
		// 结构上不可达(默认规则永在),仅防御:策略面元数据缺失时给一句实话。
		b.WriteString(warnStyle.Render("无规则可显示"))
		return altScreen(tea.NewView(b.String()))
	}
	for i, rv := range v.views {
		cursor := "  "
		if i == v.cursor {
			cursor = "> "
		}
		line := cursor + policyRuleRow(rv)
		if i == v.cursor {
			line = selStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + policyRuleDetail(&v.views[v.cursor]) + "\n")
	if v.note != "" {
		b.WriteString("\n" + warnStyle.Render(v.note) + "\n")
	}
	b.WriteString(clip(v.width, footerStyle.Render(pvReadOnlyNote)))
	return altScreen(tea.NewView(b.String()))
}
