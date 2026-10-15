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
// 因读不到规则放行一切或拦死一切;需确认档由命令策略后续的 GitHub
// Issue 在调用点接一次性确认码,本票只对拦截档动作。
func policyJudge(st *store.Store, shell models.ShellType, command string) policy.Decision {
	rules, err := policymanage.EffectiveRuleSet(st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: command policy rule lookup failed (judging with built-in defaults only): %v\n", err)
		rules = nil
	}
	return policy.Evaluate(policy.ShellType(shell), command, rules)
}

// policyBlockError 把拦截结论拼成 agent 能直接向用户转述的一行错误:
// 规则编号 + 为什么危险 + 该怎么做。只此一行——不返回规则全文,
// 也不列出其他规则 (ADR 0007:agent 看不到规则清单)。
func policyBlockError(d policy.Decision) error {
	return fmt.Errorf("blocked by command policy [%s]: %s — instead: %s", d.RuleID, d.Explanation, d.Alternative)
}
