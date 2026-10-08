# 票 04 · reload_self 工具与首代换手集成

## What to build

1. **reload_self 工具注册**(工具面第 14 把):无参数;返回当前进程版本、盘上代际与版本(读票 01 库)、GitHub 最新版本(仅被调用时联网查一次,复用 internal/updater 发现逻辑的只读部分;查询失败如实带错误不致命);盘上有新代际时执行换手并返回结果;忙时返回活跃清单(票 02 库);发起换手的本次调用自身豁免忙判据、其应答在退位前写回。
2. **首代换手集成**:直连形态与缓存形态两入口都接入 hotswap 库——桥(标准输入输出直连宿主)经「领养拉起→就绪等待→应答写回→泵化」完成首代换手;继任以领养参数起服务(会话领养的 SDK 缝:若所用 Go SDK 无「已握手起服务」入口,在接入层注入协议版本与能力);继任接管后无条件发一次 `tools/list_changed` 通知。

规格依据:spec.md 实施决策第 3/4 条(D13/D3/D6/D7/D11)。

## 验收标准

- [ ] 伪宿主端到端:initialize 握手→调用 reload_self(盘上有新代际)→换手完成→泵存活、继任应答后续请求、宿主侧收到一次 tools/list_changed 通知
- [ ] 伪宿主读到的继任应答与其版本一致(继任自报版本证明换代)
- [ ] 忙(伪造活跃隧道/任务)→reload_self 返回拒绝+活跃清单,不换手
- [ ] 继任不可用(就绪文件始终不出现)→返回失败与原因,原桥继续应答后续请求(回退)
- [ ] GitHub 查询失败→latest 字段带错误说明,其余字段正常返回
- [ ] 直连与缓存两形态各自跑通换手
- [ ] `go test ./internal/mcpserver/ -run 'Reload|HotSwap|Adopt'` 全绿

## Blocked by

01, 02, 03

## 涉及路径

- internal/mcpserver/server.go
- internal/mcpserver/run.go
- internal/mcpserver/hotswap/(集成胶水与对接文件)
- 对应 _test.go

## 副作用声明

- 独占验证命令:`go test ./internal/mcpserver/ -run 'Reload|HotSwap|Adopt|Busy'`(包级)。

## decision_refs

D13(reload_self 一把,查询+触发)、D3(当前会话可用,依赖清单变更通知)、D6(引导不改注册)、D7(换手路径零网络取码)、D11(被动版本查询)

## review_blocks

无
