# 票 04 · 工具描述与 unknown 任务错误文案

## What to build

更新 sshmgr MCP 工具的四条描述与 unknown 任务错误文案，如实反映修复后的超时语义：超时=发出 SIGKILL 请求并断开连接、按时返回 timed_out=true 与已接收的部分输出；任务终态是 broker 侧判定而非远端进程已死的证明；部分服务器完全无视会话拆除；unknown 任务号不代表远端进程已消失。措辞以 spec Implementation Decisions 第 4 条（rev1 4.4 定稿）为准。

## 验收标准

- [ ] internal/mcpserver/server.go exec_command（BrokerTools[1]）描述：在现有超时说明处补"超时 = 发出 SIGKILL 请求并断开连接、按时返回 timed_out=true 与已接收的部分输出；少数服务器不清理远端进程——超时后用 ps/pgrep 自查、必要时手工清理；高延迟链路可用 SSHMGR_EXEC_KILL_GRACE 上调宽限"语义（可微调衔接措辞，不改既有其他内容）
- [ ] exec_background（[6]）描述补：超时或 exec_stop 时任务的 SSH 连接被拆除、任务必然及时结束；但任务终态是 broker 侧判定，不是远端进程已死的证明（部分服务器无视拆除，见 exec_stop）
- [ ] exec_output（[7]）描述补：timeout/stopped 是 broker 侧判定——无视拆除的服务器上远端进程可能仍在跑，必要时用 exec_command（ps/pgrep）核实
- [ ] exec_stop（[8]）描述：在既有 nohup/setsid 诚实警示后补"部分服务器完全无视会话拆除——停止仍及时终结任务与其连接，但远端进程可能继续运行（用 exec_command ps/pgrep 检查）"
- [ ] internal/mcpserver/bgtools.go ErrBgUnknownTask 追加句："; an unknown id does NOT mean the remote process is gone — the command may still be running server-side; check with exec_command (ps/pgrep) and clean up manually"
- [ ] 描述文本遵守仓库写作规范（无非常见缩写）；go test ./internal/mcpserver/ -count=1 全绿（已有文案断言只有 "unknown task_id" 子串，不受影响——bgtools_test.go:856/863）

## Blocked by

无，可立即开始

## 涉及路径

- internal/mcpserver/server.go
- internal/mcpserver/bgtools.go

## 副作用声明

独占验证命令：go test ./internal/mcpserver/ -count=1（本包测试较慢，预计 2-5 分钟）

## decision_refs

D4（文案定稿措辞）、F3/F6 修订采纳

## review_blocks

无
