# 票 03 · 黑洞形态后台任务用例（2 条）

## What to build

在 internal/mcpserver 新增黑洞形态的后台任务测试：后台引擎（TaskManager.Start → ConnectKeepAlive → runTask → ExecWriters）经黑洞代理连 testsshd，证明反馈 P2 真问题（exec_stop 与任务超时在不配合服务器上任务永远 running）已被三段式看门狗修复——任务必然进入终态。mcpserver 包内自建夹具（仿本包 tasks_exec_test.go 的 killableProxy 先例形态；Go 测试包不跨包共享 test helper，与 sshbroker 侧夹具少量重复是可接受的包边界惯例）。详见 spec Testing Decisions 第 2 层末条。

## 验收标准

- [ ] 新文件 tasks_blackhole_test.go 含本包黑洞代理夹具（转发 + blackout 停转保持双连 + cleanup 关双侧）
- [ ] TestBackgroundTimeoutBlackhole：Start 一个经代理连 testsshd 的后台任务（命令先输出可识别文本再阻塞；TimeoutSec=1 或经环境旋钮调短宽限）→ 轮询 exec_output（Output 路径）在有限窗口（≤ 10 秒外部死线）内观察到终态 status=timeout（而非永远 running）；断言输出含先前文本（终态视图带回已收输出）
- [ ] TestBackgroundStopBlackhole：同夹具起任务（长超时）→ mgr.Stop(id) → 有限窗口内观察到终态 status=stopped
- [ ] 文件头注释写明保活时序纪律：后台任务走 ConnectKeepAlive，保活判死阈值 30s×3=90 秒远大于用例外部死线 10 秒——死线内唯一可能的解锁源是看门狗第三段，防止未来改坏成 vacuous pass
- [ ] go test ./internal/mcpserver/ -count=1 全绿（既有后台用例含 TestBackgroundStopPath/TestBackgroundTimeoutPath 不受影响）

## Blocked by

票 01（三段式行为落地；runTask 的解锁依赖内核修复）

## 涉及路径

- internal/mcpserver/tasks_blackhole_test.go（新）

## 副作用声明

独占验证命令：go test ./internal/mcpserver/ -run 'Blackhole' -count=1 -v（秒级）；随后全包 go test ./internal/mcpserver/ -count=1（本包测试较慢，预计 2-5 分钟）

## decision_refs

D1（后台路径覆盖）、D3（五条用例之后台两条）、D6（A3 的自动代证）

## review_blocks

无
