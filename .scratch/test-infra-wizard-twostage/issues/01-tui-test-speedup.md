# 票 01 · 终端界面测试耗时治理(P2 #10)

## What to build
把 `internal/tui` 测试套件的墙钟从约 89.6 秒(editpage ~71s + 向导回环 ~7s + 端到端 ~7s)降到 30 秒以内。瓶颈已定位,勿重复探路:测试辅助函数 `drain`(internal/tui/routing_helpers_test.go:52)在同步执行命令时会真实执行 huh 表单光标闪烁的 `tea.Tick` 闭包(530 毫秒包级常量),睡满才把 BlinkMsg 丢弃——每次字段聚焦态按键 = 530 毫秒纯等待。已定修法方向(分批计划 D2,不得另辟蹊径):
1. `press()`(internal/tui/editpage_test.go:47)对**普通字符键**免等待——字段聚焦态下字符键只产生闪烁重臂命令,无输出可等,不需要走 drain 同步执行;
2. 睡眠主导的既有测试加 `t.Parallel()`。
「闪烁速度测试缝」已证不可行(cursor.Model 是 huh 内部逐字段实例,构造器不暴露),不要尝试。

**纯测试面改动:internal/tui 的生产文件(.go 非测试文件)零改动,生产行为零变化。** Enter/Esc/方向键等非打印键的 tap/ctrl 辅助函数语义保持(它们仍需 drain——导航/提交命令有真实输出要等)。

## 验收标准
- [ ] 治理前后计时对比留档:`go test ./internal/tui/ -count=1` 前值(实施前先跑一次记数)与后值写进回报(目标 < 30 秒;达不到如实写实际值与原因,不放宽)
- [ ] 改动过的测试文件 `go test ./internal/tui/ -count=10` 零失败(计时类验证除外,-count=10 计时可只跑代表性文件并说明)
- [ ] `go test ./internal/tui/` 全绿
- [ ] `git status` 显示改动只在 internal/tui 测试文件(生产文件零改动)
- [ ] 既有全部测试的行为断言未改(改的是等待方式;若某断言确需随等待方式调整,逐条说明理由)

## Blocked by
无,可立即开始。

## 涉及路径
internal/tui/editpage_test.go
internal/tui/routing_helpers_test.go
internal/tui/forms_test.go
internal/tui/editfields_test.go
internal/tui/wizard_test.go
internal/tui/wizardsteps_test.go
internal/tui/wizardsteps_docsync_test.go
internal/tui/wizard_routing_test.go
internal/tui/wizardserve_test.go
internal/tui/pairwizard_test.go
internal/tui/pairing_test.go
internal/tui/app_test.go
internal/tui/actions_test.go
internal/tui/importflow_test.go
internal/tui/secrethint_test.go
internal/tui/servers_test.go
internal/tui/upgrade_test.go
(加 t.Parallel 时可能触及其余 internal/tui/*_test.go;press/drain 改动只在上面前两个文件)

## 副作用声明
- 独占验证命令:`go test ./internal/tui/...`(实施期间独占本包测试运行;跑计时前先 `go clean -testcache` 该包)
- 测试产物目录:Go 构建缓存(不进仓库)

## decision_refs
D2(修法方向已定)、D6(判据不放宽/计时留档)、D7(销项需持续集成绿——本票只做本地绿,不销项)

## review_blocks
无
