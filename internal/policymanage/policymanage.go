// Package policymanage 是命令策略的管理层(owner 管理规则用的入口,
// ticket #15):把权威 vault 里的自定义规则与停用记录组装成判定内核的
// 生效规则集,并提供增、删、停用、恢复四个管理操作(各写 owner 审计)。
// 它是 CLI(`sshmgr policy`)与后续 TUI 页(ticket #16,ADR 0003 的
// CLI 与 TUI 对等)共用的可复用层——判定语义一律在 internal/policy,
// 本层不做任何规则判定。
package policymanage

import (
	"fmt"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

// 审计动作名(沿现有 owner 面 `server.add` / `cache-token.*` 的点分风格)。
const (
	actionRuleAdd     = "policy-rule.add"
	actionRuleRm      = "policy-rule.rm"
	actionRuleDisable = "policy-rule.disable"
	actionRuleEnable  = "policy-rule.enable"
)

// RuleView 是列出命令(及后续 TUI 页)的一行:内置默认规则(带停用
// 标注)或存储的自定义规则(带全部结构化字段)。
type RuleView struct {
	Custom       bool // true = 自定义规则;false = 内置默认规则
	ID           string
	Disposition  policy.Disposition
	Explanation  string
	Alternative  string
	Disabled     bool               // 仅默认规则:被 owner 按编号停用
	Shells       []policy.ShellType // 仅自定义规则
	Commands     []string           // 仅自定义规则
	Args         []string           // 仅自定义规则
	PathCategory string             // 仅自定义规则
	Regex        string             // 仅自定义规则
}

// ruleSources 是列出面与生效规则集共用的数据源:被停用的默认规则编号
// 加上全部自定义规则。权威库与快照两侧各自己读出这个形态,后续构造
// (RuleView 列表 / 判定内核的生效规则集)只写一次。
type ruleSources struct {
	disabledDefaults []string
	customRules      []policy.CustomRule
}

// ruleSourcesFromStore 从权威库的两张策略表读出数据源。
func ruleSourcesFromStore(st *store.Store) (ruleSources, error) {
	disabled, err := st.ListDisabledDefaultRuleIDs()
	if err != nil {
		return ruleSources{}, err
	}
	customs, err := st.ListPolicyRules()
	if err != nil {
		return ruleSources{}, err
	}
	return ruleSources{disabledDefaults: disabled, customRules: customs}, nil
}

// ruleSourcesFromSnapshot 从快照的策略段读出数据源(客户端只读镜像的
// 数据面);nil 快照等于空数据源(旧快照形态)。
func ruleSourcesFromSnapshot(snap *store.Snapshot) ruleSources {
	if snap == nil {
		return ruleSources{}
	}
	return ruleSources{
		disabledDefaults: snap.PolicyDisabledDefaults,
		customRules:      customRulesFromSnapshot(snap),
	}
}

// EffectiveRuleSet 读权威库的两张策略表,组装判定内核的生效规则集:
// 内置默认规则减去停用表,加上全部自定义规则。空库返回 nil——对判定
// 内核与空规则集等价(默认规则全部生效、无自定义规则)。
func EffectiveRuleSet(st *store.Store) (*policy.RuleSet, error) {
	src, err := ruleSourcesFromStore(st)
	if err != nil {
		return nil, err
	}
	return src.effectiveRuleSet(), nil
}

// EffectiveRuleSetFromSnapshot 从快照的策略段组装生效规则集(客户端
// 只读镜像的数据面)。旧快照无策略段时返回的规则集等于 nil——内置
// 默认规则全部生效,由判定内核兑现。
func EffectiveRuleSetFromSnapshot(snap *store.Snapshot) *policy.RuleSet {
	return ruleSourcesFromSnapshot(snap).effectiveRuleSet()
}

// effectiveRuleSet 把数据源组装成判定内核的生效规则集;两段全空时
// 返回 nil(nil 与空规则集对判定内核等价)。
func (s ruleSources) effectiveRuleSet() *policy.RuleSet {
	if len(s.disabledDefaults) == 0 && len(s.customRules) == 0 {
		return nil
	}
	return &policy.RuleSet{
		DisabledDefaultRuleIDs: s.disabledDefaults,
		CustomRules:            s.customRules,
	}
}

// customRulesFromSnapshot 把快照的自定义规则段转回判定内核的规则形态。
// 处置代号在写入侧只来自 ParseDisposition 的合法值;坏代号(库被手改)
// 退化为需确认而不是让整个规则集失效。
func customRulesFromSnapshot(snap *store.Snapshot) []policy.CustomRule {
	var out []policy.CustomRule
	for _, r := range snap.PolicyRules {
		cr := policy.CustomRule{
			ID:           r.ID,
			Commands:     r.Commands,
			Args:         r.Args,
			PathCategory: r.PathCategory,
			Regex:        r.Regex,
			Explanation:  r.Explanation,
			Alternative:  r.Alternative,
		}
		for _, sh := range r.Shells {
			cr.Shells = append(cr.Shells, policy.ShellType(sh))
		}
		d, err := policy.ParseDisposition(r.Disposition)
		if err != nil {
			d = policy.DispositionConfirmRequired
		}
		cr.Disposition = d
		out = append(out, cr)
	}
	return out
}

// ListRules 列出当前的全部规则:内置默认规则(带停用标注)在前,
// 自定义规则(按编号序)在后。
func ListRules(st *store.Store) ([]RuleView, error) {
	src, err := ruleSourcesFromStore(st)
	if err != nil {
		return nil, err
	}
	return buildRuleViews(src), nil
}

// ListRulesFromSnapshot 从快照段构造列出面(客户端只读面与后续 TUI 页
// 共用):内置默认规则 + 停用标注 + 自定义规则。nil 快照 = 只列默认规则
// (全部生效,旧快照形态)。
func ListRulesFromSnapshot(snap *store.Snapshot) []RuleView {
	return buildRuleViews(ruleSourcesFromSnapshot(snap))
}

// buildRuleViews 从数据源构造列出面:内置默认规则(带停用标注)在前,
// 自定义规则(带全部结构化字段)在后——权威库与快照两侧同一构造。
func buildRuleViews(src ruleSources) []RuleView {
	disabledSet := make(map[string]bool, len(src.disabledDefaults))
	for _, id := range src.disabledDefaults {
		disabledSet[id] = true
	}
	views := make([]RuleView, 0, len(policy.DefaultRuleInfos())+len(src.customRules))
	for _, info := range policy.DefaultRuleInfos() {
		views = append(views, RuleView{
			ID:          info.ID,
			Disposition: info.Disposition,
			Explanation: info.Explanation,
			Alternative: info.Alternative,
			Disabled:    disabledSet[info.ID],
		})
	}
	for _, cr := range src.customRules {
		views = append(views, RuleView{
			Custom:       true,
			ID:           cr.ID,
			Disposition:  cr.Disposition,
			Explanation:  cr.Explanation,
			Alternative:  cr.Alternative,
			Shells:       cr.Shells,
			Commands:     cr.Commands,
			Args:         cr.Args,
			PathCategory: cr.PathCategory,
			Regex:        cr.Regex,
		})
	}
	return views
}

// writeAuditAfter 把「命令结果 + 一条审计行」收尾:操作成功写 Status=ok
// 的行;操作失败写 Status=error 的行且不掩盖原错误;操作成功而审计写
// 失败时把审计错误作为命令错误上抛(照抄 cache-tokens 的收尾模式)。
// 返回值即 opErr(可能被审计错误替换)。
func writeAuditAfter(st *store.Store, action string, summary map[string]any, opErr error) error {
	if opErr == nil {
		return st.WriteOwnerAudit(action, summary)
	}
	row := store.OwnerAuditRow(action, summary)
	row.Status = "error"
	_ = st.WriteAudit(row)
	return opErr
}

// AddCustomRule 增加一条自定义规则并写审计(policy-rule.add)。校验:
// 规则自描述完整(policy.CustomRule.Validate)、编号不得撞内置默认规则、
// 编号不得与已有自定义规则重复。
func AddCustomRule(st *store.Store, rule policy.CustomRule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	for _, id := range policy.DefaultRuleIDs() {
		if rule.ID == id {
			return fmt.Errorf("rule id %q is a built-in default rule id; default rules can only be disabled by id, never rewritten — pick another id", rule.ID)
		}
	}
	summary := map[string]any{
		"id":          rule.ID,
		"disposition": rule.Disposition.Code(),
		"regex":       rule.Regex != "",
	}
	if len(rule.Commands) > 0 {
		summary["commands"] = rule.Commands
	}
	if len(rule.Shells) > 0 {
		summary["shells"] = shellStrings(rule.Shells)
	}
	err := st.AddPolicyRule(rule)
	return writeAuditAfter(st, actionRuleAdd, summary, err)
}

// DeleteCustomRule 按编号删除一条自定义规则并写审计(policy-rule.rm)。
// 编号不存在时报错(宁拒勿默)。
func DeleteCustomRule(st *store.Store, id string) error {
	summary := map[string]any{"id": id}
	deleted, err := st.DeletePolicyRule(id)
	if err == nil && !deleted {
		err = fmt.Errorf("no custom rule with id %q", id)
	}
	return writeAuditAfter(st, actionRuleRm, summary, err)
}

// DisableDefaultRule 按编号停用一条内置默认规则并写审计
// (policy-rule.disable)。编号必须是已知默认规则编号;已停用时幂等
// 成功。停用是 owner 自担风险的放开(ADR 0007),审计留痕。
func DisableDefaultRule(st *store.Store, id string) (bool, error) {
	if err := requireDefaultRuleID(id); err != nil {
		return false, err
	}
	summary := map[string]any{"id": id}
	inserted, err := st.DisableDefaultRule(id)
	if aerr := writeAuditAfter(st, actionRuleDisable, summary, err); aerr != nil {
		return inserted, aerr
	}
	return inserted, nil
}

// EnableDefaultRule 按编号恢复一条已停用的默认规则并写审计
// (policy-rule.enable)。未停用时幂等成功且不写审计行(无事发生,
// 与 host key 清理命令的幂等语义一致)。
func EnableDefaultRule(st *store.Store, id string) (bool, error) {
	if err := requireDefaultRuleID(id); err != nil {
		return false, err
	}
	removed, err := st.EnableDefaultRule(id)
	if err != nil {
		return removed, writeAuditAfter(st, actionRuleEnable, map[string]any{"id": id}, err)
	}
	if !removed {
		return false, nil
	}
	return true, writeAuditAfter(st, actionRuleEnable, map[string]any{"id": id}, nil)
}

// requireDefaultRuleID 校验编号属于内置默认规则清单——停用/恢复只认
// 已发布的默认规则编号,拼错直接报错,不留停用表死行。
func requireDefaultRuleID(id string) error {
	for _, known := range policy.DefaultRuleIDs() {
		if id == known {
			return nil
		}
	}
	return fmt.Errorf("rule id %q is not a built-in default rule id (known: see `sshmgr policy ls`)", id)
}

// shellStrings 把 shell 类型集合转成字符串列表(审计摘要用)。
func shellStrings(shells []policy.ShellType) []string {
	out := make([]string, 0, len(shells))
	for _, sh := range shells {
		out = append(out, string(sh))
	}
	return out
}
