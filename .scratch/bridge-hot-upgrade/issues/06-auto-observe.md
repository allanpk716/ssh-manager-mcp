# 票 06 · 自动换手循环、多实例独立性与 doctor 代际行

## What to build

1. **自动换手循环**:桥内定期(约 30 秒)轮询盘上代际;较出生代际新且不忙→自动完成换手(静默,不发错误);忙→跳过本轮。工具强制触发与自动触发共用同一换手入口。
2. **多实例独立性**:同机两把桥(不同缓存实例)各自记出生代际、各自换血,互不干扰——以集成测试钉死(双实例并行换手,各自成功)。
3. **doctor 代际行**:doctor 输出新增「盘上代际」行,只读信号文件显示代际与版本,零网络调用。

规格依据:spec.md 实施决策第 1/5/6 条(D12/D2/D11)。

## 验收标准

- [ ] 伪宿主挂机:落新代际信号→一个轮询周期内自动完成换手,宿主侧收到清单变更通知
- [ ] 忙时自动触发静默跳过(无错误输出、不换手);空闲后下一轮完成
- [ ] 双实例并行换血互不干扰(两伪宿主各自验证换代成功)
- [ ] doctor 输出含盘上代际与版本;信号文件缺失时该行如实显示无信号;doctor 全程零网络(既有约束不破)
- [ ] `go test ./internal/mcpserver/ -run 'AutoSwap|Idle'` 与 `go test ./internal/cli/ -run Doctor` 全绿

## Blocked by

05

## 涉及路径

- internal/mcpserver/run.go
- internal/mcpserver/hotswap/
- internal/cli/doctor.go
- 对应 _test.go

## 副作用声明

- 独占验证命令:`go test ./internal/mcpserver/ -run 'AutoSwap|Idle'`;`go test ./internal/cli/ -run Doctor`。

## decision_refs

D12(空闲自动采纳)、D2(通用机制,多宿主多实例)、D11(doctor 零网络)

## review_blocks

无
