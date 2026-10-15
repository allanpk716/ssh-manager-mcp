// policypage.go 是主控台的第七页「命令策略」(票 #16,ADR
// 0003 的 CLI 与 TUI 对等):列出全部生效规则(内置默认规则带停用
// 标注,自定义规则带结构化字段),并提供四个动作——[a] 增加自定义
// 规则、[d] 删除自定义规则、[s] 按编号停用/恢复默认规则、[t] 试算。
// 一切规则管理与审计都经可复用管理层 internal/policymanage 落库
// (与 `sshmgr policy` 命令组同一层),本页只做展示与接线,不做任何
// 规则判定——判定语义在 internal/policy,试算只调用它的 Evaluate。
package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/policymanage"
	"ssh-manager-mcp/internal/store"
)

// policyPage 列出命令策略的全部规则:默认规则在前、自定义规则在后
// (policymanage.ListRules 的顺序,与 `sshmgr policy ls` 一致)。页面
// 数据随每次刷新整体重建(与其它实体页同一节奏)。
type policyPage struct {
	views []policymanage.RuleView
	panelList
}

func newPolicyPage(st *store.Store) (*policyPage, error) {
	views, err := policymanage.ListRules(st)
	if err != nil {
		return nil, err
	}
	p := &policyPage{views: views}
	p.panelList = newPanelList("命令策略")
	p.syncList()
	return p, nil
}

func (p *policyPage) Title() string { return "命令策略" }

// policyRuleItem 把一条规则适配进列表面板;FilterValue 用编号,`/`
// 过滤即按编号找规则。
type policyRuleItem struct{ v policymanage.RuleView }

func (i policyRuleItem) FilterValue() string { return i.v.ID }
func (i policyRuleItem) Title() string       { return policyRuleRow(i.v) }
func (i policyRuleItem) Description() string {
	if i.v.Disabled {
		return "已停用"
	}
	return i.v.Disposition.String()
}

func (p *policyPage) syncList() {
	items := make([]list.Item, len(p.views))
	for i, v := range p.views {
		items[i] = policyRuleItem{v: v}
	}
	p.setListItems(items, len(items))
}

func (p *policyPage) Rows() []string {
	out := make([]string, len(p.views))
	for i, v := range p.views {
		out[i] = policyRuleRow(v)
	}
	return out
}

// current 解析光标当前指向的规则(过滤后的可见子集内)。
func (p *policyPage) current() *policymanage.RuleView {
	vis := p.list.VisibleItems()
	i := p.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(policyRuleItem)
	if !ok {
		return nil
	}
	return &it.v
}

// Detail 渲染光标指向的规则的详情面(非桌面版式的回退路径;桌面版式
// 经 Render 走同一个渲染函数)。
func (p *policyPage) Detail() string { return policyRuleDetail(p.current()) }

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *policyPage) Render(width, height int) string {
	return renderPanel(&p.list, policyRuleDetail(p.current()), width, height)
}

// policyRuleRow 渲染一行规则(主控台页与客户端查看器共用):种类、
// 编号、处置、风险说明摘要,默认规则的停用标注在行尾。与 CLI
// printPolicyRuleLine 同一信息集,列表面板版式。
func policyRuleRow(v policymanage.RuleView) string {
	// 「默认」补两空格与「自定义」显示宽对齐(CJK 按字节补位会错位,
	// 见 instancepicker 的 runewidth 注记;此处两词宽度固定,手工对齐即可)。
	kind := "默认  "
	if v.Custom {
		kind = "自定义"
	}
	row := fmt.Sprintf("%s  %-28s %s  %s", kind, v.ID, v.Disposition.String(), v.Explanation)
	if !v.Custom && v.Disabled {
		row += "  [已停用]"
	}
	return row
}

// policyRuleDetail 渲染一条规则的详情面(主控台页与客户端查看器
// 共用):编号、种类、处置、状态,自定义规则的结构化匹配条件,以及
// 风险说明与替代做法——与 agent 被拦下时拿到的同一行反馈内容。
func policyRuleDetail(v *policymanage.RuleView) string {
	if v == nil {
		return "(空)"
	}
	kind := "默认规则"
	state := "生效中"
	if v.Custom {
		kind = "自定义规则"
	} else if v.Disabled {
		state = "已停用(owner 自担风险的放开)"
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "编号    %s\n种类    %s\n处置    %s\n状态    %s", v.ID, kind, v.Disposition.String(), state)
	if v.Custom {
		shells := "全部"
		if len(v.Shells) > 0 {
			strs := make([]string, 0, len(v.Shells))
			for _, sh := range v.Shells {
				strs = append(strs, string(sh))
			}
			shells = strings.Join(strs, ", ")
		}
		fmt.Fprintf(b, "\nShell   %s", shells)
		if len(v.Commands) > 0 {
			fmt.Fprintf(b, "\n命令名  %s", strings.Join(v.Commands, ", "))
		}
		if len(v.Args) > 0 {
			fmt.Fprintf(b, "\n参数条件%s", strings.Join(v.Args, ", "))
		}
		switch v.PathCategory {
		case policy.PathCategoryCritical:
			b.WriteString("\n路径类别系统关键路径")
		case policy.PathCategoryDisk:
			b.WriteString("\n路径类别磁盘设备")
		}
		if v.Regex != "" {
			fmt.Fprintf(b, "\n正则    %s", v.Regex)
		}
	}
	fmt.Fprintf(b, "\n为什么  %s", orDash(v.Explanation))
	fmt.Fprintf(b, "\n怎么做  %s", orDash(v.Alternative))
	if !v.Custom {
		b.WriteString("\n\n默认规则只能按编号停用([s]),不能改写;自定义规则用 [a] 增加。")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// [a] 增加自定义规则 — huh 表单(结构化字段;正则是高级选项)
// ---------------------------------------------------------------------------

// policyRuleDraft 是 [a] 表单的状态。CommandsStr / ArgsStr 是命令名与
// 参数条件的逗号分隔镜像(CLI 接受重复旗标,表单单输入框用逗号分隔);
// 其余字段与 policy.CustomRule 一一对应。
type policyRuleDraft struct {
	ID           string
	Disposition  string // select 值:block / confirm
	Shells       []string
	CommandsStr  string
	ArgsStr      string
	PathCategory string // select 值:"" / critical / disk
	Regex        string
	Why          string
	Instead      string
}

// newPolicyRuleForm 构造新增自定义规则的表单:三组——编号与处置、匹配
// 条件(shell 集合/命令名/参数条件/目标路径类别/正则)、给 agent 的
// 反馈(风险说明与替代做法)。编号与风险说明的必填在表单内校验,
// 其余合法性(处置档、正则可编译、至少一个匹配条件、编号不撞默认
// 规则与已有自定义规则)由管理层 policymanage.AddCustomRule 拒绝,
// 错误经状态行可见。
func newPolicyRuleForm(d *policyRuleDraft) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("规则编号（唯一，不得撞内置默认规则编号）").Value(&d.ID).Validate(nonEmpty),
			huh.NewSelect[string]().Title("处置（命中时）").
				Options(
					huh.NewOption("拦截（永不执行）", "block"),
					huh.NewOption("需确认（退回 agent 征求同意）", "confirm"),
				).Value(&d.Disposition),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("适用 Shell（空格勾选；全不选=全部 Shell）").
				Options(
					huh.NewOption("POSIX（Linux/macOS）", "posix"),
					huh.NewOption("PowerShell（Windows）", "powershell"),
					huh.NewOption("cmd（Windows）", "cmd"),
				).Value(&d.Shells),
			huh.NewInput().Title("命令名及别名（逗号分隔，可空=不限命令名）").Value(&d.CommandsStr),
			huh.NewInput().Title("参数条件（逗号分隔，每个都须逐字出现在参数里）").Value(&d.ArgsStr),
			huh.NewSelect[string]().Title("目标路径类别").
				Options(
					huh.NewOption("不限", ""),
					huh.NewOption("系统关键路径", policy.PathCategoryCritical),
					huh.NewOption("磁盘设备（仅 POSIX）", policy.PathCategoryDisk),
				).Value(&d.PathCategory),
			huh.NewInput().Title("正则（高级选项，对整条命令原文匹配）").Value(&d.Regex),
		),
		huh.NewGroup(
			huh.NewInput().Title("为什么危险（命中时给 agent 的一行说明）").Value(&d.Why).Validate(nonEmpty),
			huh.NewInput().Title("更安全的做法（可空）").Value(&d.Instead),
		),
	)
}

// toRule 把表单草稿组装成 policy.CustomRule:文本做去除首尾空白处理,
// 命令名与参数条件按逗号拆分。不做合法性判定——那是管理层的事。
func (d *policyRuleDraft) toRule() policy.CustomRule {
	splitList := func(s string) []string {
		var out []string
		for _, part := range strings.Split(s, ",") {
			if t := strings.TrimSpace(part); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	shells := make([]policy.ShellType, 0, len(d.Shells))
	for _, sh := range d.Shells {
		shells = append(shells, policy.ShellType(sh))
	}
	disposition := policy.DispositionBlock
	if d.Disposition == "confirm" {
		disposition = policy.DispositionConfirmRequired
	}
	return policy.CustomRule{
		ID:           strings.TrimSpace(d.ID),
		Shells:       shells,
		Commands:     splitList(d.CommandsStr),
		Args:         splitList(d.ArgsStr),
		PathCategory: d.PathCategory,
		Regex:        strings.TrimSpace(d.Regex),
		Disposition:  disposition,
		Explanation:  strings.TrimSpace(d.Why),
		Alternative:  strings.TrimSpace(d.Instead),
	}
}

// newPolicyRuleFormOverlay 装配 [a] 的表单 overlay:字段绑定草稿 d
// (构造表单前预填的值即表单初值),提交后经 policymanage 落库并写
// 与 CLI 同名的 owner 审计(policy-rule.add)。
func newPolicyRuleFormOverlay(st *store.Store, d *policyRuleDraft) *formOverlay {
	return newFormOverlay("新增自定义规则", newPolicyRuleForm(d), func() tea.Cmd {
		return doAction(st, func() (string, error) {
			rule := d.toRule()
			if err := policymanage.AddCustomRule(st, rule); err != nil {
				return "", err
			}
			return "已新增自定义规则 " + rule.ID + "(处置 " + rule.Disposition.String() + ")", nil
		})
	})
}

// ---------------------------------------------------------------------------
// [d] 删除自定义规则 / [s] 停用与恢复默认规则 — App 级动作键分发
// ---------------------------------------------------------------------------

// policyKey 分发命令策略页的动作键(a 增 / d 删 / s 停用恢复 / t 试算)
// ——与 serversKey/backupKey 同一逐页分发纪律:别页的字母在此是安静的
// 空操作,语义只属于当前光标指向的那条规则。开表单的键返回 overlay
// 的 Init cmd;直接执行的键返回动作 cmd;提示类键只写状态行。
func (a *App) policyKey(k tea.Key) tea.Cmd {
	pp, _ := a.pages[pagePolicy].(*policyPage)
	if pp == nil {
		return nil
	}
	cur := pp.current()
	switch k.Text {
	case "a":
		d := &policyRuleDraft{}
		a.overlay = newPolicyRuleFormOverlay(a.st, d)
	case "t":
		a.overlay = newBrokerPolicyTest(a.st)
	case "d":
		if cur == nil {
			return nil
		}
		if !cur.Custom {
			a.status = "默认规则不可删除——默认规则只能按编号停用([s]),不能改写或删除"
			return nil
		}
		id := cur.ID
		confirm := false
		form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
			Title(fmt.Sprintf("删除自定义规则 %q？", id)).Value(&confirm)))
		a.overlay = newFormOverlay("删除自定义规则", form, func() tea.Cmd {
			if !confirm {
				return nil
			}
			return doAction(a.st, func() (string, error) {
				if err := policymanage.DeleteCustomRule(a.st, id); err != nil {
					return "", err
				}
				return "已删除自定义规则 " + id, nil
			})
		})
	case "s":
		if cur == nil {
			return nil
		}
		if cur.Custom {
			a.status = "自定义规则不参与停用——用 [d] 删除,或 [a] 增加新规则"
			return nil
		}
		id := cur.ID
		if cur.Disabled {
			// 恢复是找回护栏,直接执行;未停用时管理层幂等成功且不写审计。
			return doAction(a.st, func() (string, error) {
				if _, err := policymanage.EnableDefaultRule(a.st, id); err != nil {
					return "", err
				}
				return "已恢复默认规则 " + id + " ——重新参与判定", nil
			})
		}
		// 停用是 owner 自担风险的放开(ADR 0007):确认表单里把这一句
		// 说清楚,确认后写审计留痕。
		confirm := false
		form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
			Title(fmt.Sprintf("停用默认规则 %q？它不再拦截/需确认任何命令,风险由你自担。", id)).
			Affirmative("停用").Negative("取消").
			Value(&confirm)))
		a.overlay = newFormOverlay("停用默认规则", form, func() tea.Cmd {
			if !confirm {
				return nil
			}
			return doAction(a.st, func() (string, error) {
				if _, err := policymanage.DisableDefaultRule(a.st, id); err != nil {
					return "", err
				}
				return "已停用默认规则 " + id + " ——它不再命中;风险由你自担", nil
			})
		})
	}
	if a.overlay == nil {
		return nil
	}
	return a.overlay.Init()
}
