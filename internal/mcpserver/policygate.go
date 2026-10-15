package mcpserver

// 命令策略的执行工具接入 (ticket #10 / ADR 0007):exec_command 与
// exec_background 在取凭据、建立 SSH 连接**之前**对 agent 下发的命令文本
// 做一次命令策略判定,拦截档直接拒绝——不建立连接,命令一个字节都不会
// 送到服务器。判定内核是 internal/policy 的纯函数 Evaluate,本层不重复
// 实现规则,只负责接线与审计。

import (
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
)

// policyJudge 是两个执行工具共用的判定入口。判定按服务器条目标注的
// shell 类型选择语法(ticket #14;存储层已把空值归一为 POSIX,能到这里的
// 未知值不改写——Evaluate 的保守分支判需确认,不悄悄放行)。后续票接
// 快照下发的生效规则集(换第三参)时只改这里,不动两个调用点的时序。
// 需确认档由命令策略后续的 GitHub Issue 在调用点接一次性确认码,本票
// 只对拦截档动作。
func policyJudge(shell models.ShellType, command string) policy.Decision {
	return policy.Evaluate(policy.ShellType(shell), command, nil)
}

// policyBlockError 把拦截结论拼成 agent 能直接向用户转述的一行错误:
// 规则编号 + 为什么危险 + 该怎么做。只此一行——不返回规则全文,
// 也不列出其他规则 (ADR 0007:agent 看不到规则清单)。
func policyBlockError(d policy.Decision) error {
	return fmt.Errorf("blocked by command policy [%s]: %s — instead: %s", d.RuleID, d.Explanation, d.Alternative)
}
