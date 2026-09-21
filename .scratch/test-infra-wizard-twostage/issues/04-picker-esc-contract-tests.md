# 票 04 · 实例选择器 + 非向导 Esc 键链契约测试(批4 收窄版)

## What to build
为验收册 plan-46-45 的「待测试代证」层补进程内行为契约测试,**只加测试,生产文件零改动**:

1. **实例选择器行状态四要素**(client 页 `[i]` 实例选择器:每行实例名 + 行状态要素——详见验收册 `docs/acceptance/plan-46-45-tui-flows.md` 的对应条目与 instancepicker 的现有实现)的契约测试;
2. **非向导 Esc 键链**的契约测试:列表过滤态 Esc 清空过滤、覆盖层(选择器/确认框)在场时 Esc 收起、删除确认取消回到选择器等既有 Esc 行为(部分已有零散测试——本票把键链契约钉全,不重复造轮子:先盘点已有覆盖,缺哪补哪)。

**明确边界(评审 D9)**:配对向导回环的契约测试**不在本票**(唯一归属票 03);本票不碰向导面文件。
**明确边界(测试缝)**:只加测试;若发现必须改生产代码才能测(需要测试缝),**回报 NEEDS_CONTEXT**,不自行开缝。

## 验收标准
- [ ] 先盘点:回报里列「已有覆盖 vs 本票新增」清单(不重复造轮子的证据)
- [ ] 新增测试全绿;`go test ./internal/tui/` 全绿
- [ ] 计时对照:`go test ./internal/tui/ -count=1` 相对票 01 留档基线的增量写进回报(批4 判据:耗时不回涨超批1 基线 = 批1 完成时留档的实际计时值)
- [ ] `git status` 显示改动只在测试文件(生产文件零改动)

## Blocked by
票 03(同包串行——非功能依赖,为独占本包验证、避免并发写文件互相污染;票 01 的前置已随票 03 传递满足)

## 涉及路径
internal/tui/instancepicker_test.go
internal/tui/clientpage_instance_test.go
internal/tui/clientpage_test.go
internal/tui/clientpage_routing_test.go
internal/tui/clientpage_delete_test.go
internal/tui/app_test.go
internal/tui/forms_test.go
internal/tui/routing_helpers_test.go
(选择器与 Esc 链测试文件;若需新建测试文件,同目录 internal/tui/ 下命名 `*_test.go`)

## 副作用声明
- 独占验证命令:`go test ./internal/tui/...`(实施期间独占本包测试运行)
- 测试产物目录:Go 构建缓存(不进仓库)

## decision_refs
D5(批4 依赖批1 的「先治后扩」)、D9(向导面归票 03)、D6(判据不放宽/计时留档)

## review_blocks
无
