# 票 02 · SSH 代理连接取消测试的间歇失败稳定化(P2 #8)

## What to build
`internal/sshbroker` 的 TestConnectCancelContext(internal/sshbroker/client_test.go:92)在 Windows 偶发 wsarecv 竞态:取消路径与底层 socket 读的竞态窗口导致偶发红(历史本地一次,重跑即绿;持续集成连绿)。修法已定(分批计划 D3):**重试式稳定化**——同族先例 = mcpserver 侧三例已修的「固定轮询改期限轮询」姿势(branch `flake8-deadline-polling`,可 `git log` 查看该分支的修法):固定次数的重试/等待改成带截止时间(deadline)的轮询,游标推进保证多轮零重复。

被测契约本身不变:断言仍是「已取消的上下文必须中止在飞连接」。只改等待/重试方式,不改被测行为。

## 验收标准
- [ ] TestConnectCancelContext 的断言语义不变(仍是取消中止连接;若断言文案需微调,说明理由)
- [ ] `go test ./internal/sshbroker/ -run TestConnectCancelContext -count=10` 零失败
- [ ] `go test ./internal/sshbroker/ -count=1` 全绿
- [ ] 改动只在 internal/sshbroker/client_test.go(若需动同包其他测试文件,先在回报里说明)

## Blocked by
无,可立即开始。

## 涉及路径
internal/sshbroker/client_test.go

## 副作用声明
- 独占验证命令:`go test ./internal/sshbroker/...`(实施期间独占本包测试运行)
- 测试产物目录:Go 构建缓存(不进仓库);测试会起本地回环 socket(127.0.0.1 随机端口,进程内自收自发,不外联)

## decision_refs
D3(重试式稳定化)、D6(判据不放宽)

## review_blocks
无
