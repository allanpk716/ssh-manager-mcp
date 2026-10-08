# 票 02 · 忙判据与活跃清单

## What to build

桥进程内的统一忙判据库:聚合**活跃隧道数、运行中后台任务数(终态保留期不算)、进行中传输数、在飞未答请求数**,输出统一的忙报告(是否忙+各分项计数,即「活跃清单」的数据形态)。在飞请求计数经工具处理器包装实现,并支持**豁免指定调用**(发起换手的 reload_self 自身不计入,否则工具触发永远自拒)。隧道管理器、任务管理器暴露各自计数;传输按任务槽口径(中继传输已挂任务槽)。

规格依据:spec.md 实施决策第 2 条(D5/F3)。

## 验收标准

- [ ] 有活跃隧道→忙,报告含隧道计数;隧道全关→该项归零
- [ ] 有运行中后台任务→忙;终态保留期内的已结束任务**不算**忙
- [ ] 在飞请求计数:包装器包裹的处理器执行期间计数>0,执行完归零;被豁免的调用不计数
- [ ] 全部空闲→不忙
- [ ] `go test ./internal/mcpserver/ -run 'Busy|Tunnel|Task'` 全绿

## Blocked by

无,可立即开始。

## 涉及路径

- internal/mcpserver/busy.go(新)
- internal/mcpserver/busy_test.go(新)
- internal/mcpserver/tunnels.go(暴露计数)
- internal/mcpserver/tasks.go(暴露计数)
- internal/mcpserver/server.go(处理器包装挂接点)

## 副作用声明

- 独占验证命令:`go test ./internal/mcpserver/ -run 'Busy|Tunnel|Task'`。

## decision_refs

D5(忙时拒绝+活跃清单)、F3(豁免发起换手的调用自身)

## review_blocks

无
