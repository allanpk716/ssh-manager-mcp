package mcpserver

// 命令策略的执行工具接入 (ticket #10 / ADR 0007):exec_command 与
// exec_background 在取凭据、建立 SSH 连接**之前**对 agent 下发的命令文本
// 做一次命令策略判定,拦截档直接拒绝——不建立连接,命令一个字节都不会
// 送到服务器。判定内核是 internal/policy 的纯函数 Evaluate,本层不重复
// 实现规则,只负责接线与审计。
//
// ticket #15 起判定按「生效规则集」进行:内置默认规则减去停用表,加上
// 自定义规则,直接从本次执行所用的 store 读取——权威端(本地 vault 或
// serve 库)与客户端(快照水合出的只读镜像库)走同一条代码路径,离线
// 时用最后一次拉到的规则。

import (
	"fmt"
	"os"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/policymanage"
	"ssh-manager-mcp/internal/store"
)

// policyJudge 是两个执行工具共用的判定入口:从执行的 store 组装生效
// 规则集(默认规则减停用、加自定义,ticket #15)后交给判定内核;语法按
// 服务器条目标注的 shell 类型选择(ticket #14;存储层已把空值归一为
// POSIX,能到这里的未知值不改写——Evaluate 的保守分支判需确认,不悄悄
// 放行)。权威端(本地 vault 或 serve 库)与客户端(快照水合出的只读
// 镜像库)走同一条代码路径,离线时用最后一次拉到的规则。规则表读取
// 失败时退回纯内置默认规则继续判定(默认规则仍然护住严重后果),绝不
// 因读不到规则放行一切或拦死一切。拦截档在调用点直接拒绝;需确认档
// 经 confirmcode.go 的一次性确认码门(ticket #11)。
func policyJudge(st *store.Store, shell models.ShellType, command string) policy.Decision {
	rules, err := policymanage.EffectiveRuleSet(st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: command policy rule lookup failed (judging with built-in defaults only): %v\n", err)
		rules = nil
	}
	return policy.Evaluate(shell, command, rules)
}

// policyBlockError 把拦截结论拼成 agent 能直接向用户转述的一行错误:
// 规则编号 + 为什么危险 + 该怎么做。只此一行——不返回规则全文,
// 也不列出其他规则 (ADR 0007:agent 看不到规则清单)。
func policyBlockError(d policy.Decision) error {
	return fmt.Errorf("blocked by command policy [%s]: %s — instead: %s", d.RuleID, d.Explanation, d.Alternative)
}

// policyGateActions 是一个执行工具的命令策略审计动作名三元组:拦截行、
// 需确认退回行、带确认码执行行。前台 exec_command 与后台
// exec_background 只差前缀,门链同构。
type policyGateActions struct {
	blocked         string // 拦截行的动作名(exec-blocked / exec-bg-blocked)
	confirmRequired string // 需确认退回行的动作名
	confirmed       string // 带确认码执行行的动作名(由确认码门自落)
}

// execPolicyActions 与 execBgPolicyActions 分别是前台与后台执行工具的
// 审计动作名。
var (
	execPolicyActions = policyGateActions{
		blocked:         "exec-blocked",
		confirmRequired: "exec-confirm-required",
		confirmed:       "exec-confirmed",
	}
	execBgPolicyActions = policyGateActions{
		blocked:         "exec-bg-blocked",
		confirmRequired: "exec-bg-confirm-required",
		confirmed:       "exec-bg-confirmed",
	}
)

// applyPolicyGate 对一条非放行的判定结论执行两个执行工具共用的门:
//   - 拦截:拼一行错误返回,granted=false;
//   - 需确认:经一次性确认码门(confirmcode.go)——未带码或码不对时
//     退回一行说明加(新)确认码,granted=false;核销通过则落「带确认码
//     执行」审计行并放行,granted=true。
//
// 返回的 action/status 供调用方的 defer 审计写本分支的行(状态恒为命中
// 规则编号);granted=true 时两者为零值,调用方继续正常执行链。
func applyPolicyGate(st *store.Store, projectID, serverID, command string, sudo bool, confirm ExecConfirmation, d policy.Decision, actions policyGateActions) (granted bool, action, status string, err error) {
	switch d.Disposition {
	case policy.DispositionBlock:
		return false, actions.blocked, d.RuleID, policyBlockError(d)
	case policy.DispositionConfirmRequired:
		granted, err = gateExecConfirmation(st, projectID, serverID, command, sudo, confirm, d, actions.confirmed)
		if !granted {
			return false, actions.confirmRequired, d.RuleID, err
		}
		return true, "", "", nil
	default:
		return true, "", "", nil
	}
}
