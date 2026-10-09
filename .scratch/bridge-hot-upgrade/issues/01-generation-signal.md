# 票 01 · 代际信号文件(update 侧写入 + 读库)

## What to build

`sshmgr update` 在**二进制替换成功点**于二进制同目录写代际信号文件 `sshmgr.update-gen`(JSON:gen=单调递增时间戳(unix 纳秒)、version=新版本号、time=写入时刻;写入前刷盘)。GitHub 直连与 `--file` 两条更新路径都在同一成功点落信号。同时提供读库:读取代际/版本;文件缺失或损坏时返回「无信号」而非报错;以及「出生代际快照」工具函数(供桥启动时记录自己出生时的盘上代际)。比较语义为**代际新旧**(gen 大小),不比版本字符串——降级同样较新。

规格依据:spec.md 实施决策第 1 条(D9/D12)。

## 验收标准

- [ ] 替换成功点函数被调用后,信号文件存在且三字段正确、内容为合法 JSON
- [ ] 连续两次写入 gen 严格递增
- [ ] `--file` 路径与 GitHub 发现路径共用同一成功点挂钩(代码路径核验)
- [ ] 文件缺失→读库返回「无信号」;文件损坏(截断/非法 JSON)→同样「无信号」,不 panic 不报错
- [ ] 出生代际快照函数:盘上无信号时返回零值代际
- [ ] `go test ./internal/updater/ -run Signal` 全绿

## Blocked by

无,可立即开始。

## 涉及路径

- internal/updater/signal.go(新)
- internal/updater/signal_test.go(新)
- internal/cli/update.go(成功点挂钩)

## 副作用声明

- 独占验证命令:`go test ./internal/updater/ -run Signal`;不跑 cli 全套(终局统一跑)。

## decision_refs

D9(本地文件信号,零监听面)、D12(空闲自动采纳,代际不比版本大小)

## review_blocks

无
