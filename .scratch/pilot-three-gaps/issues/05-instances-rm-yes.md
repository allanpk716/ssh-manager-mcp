# 05 · cache instances rm 显式 --yes 通道

## What to build
`sshmgr cache instances rm <实例名>` 增加确认旗标 `--yes`,语义对齐 `sshmgr update --yes`(internal/cli/update.go:187 先例):
- 在确认点一视同仁 assume-yes:**TTY 下带 --yes 跳过输名确认**;非 TTY 带 --yes 直接执行删除。
- 非 TTY 无 --yes:维持现状拒绝,错误文案不变(「需要交互式终端(stdin 不是 TTY)——为防止脚本误删,拒绝执行」)。
- 无其他确认类旗标,无组合面;删除语义(槽目录+数据加密密钥双根清理)与交互路径同一实现,只放行确认方式。

## 验收标准
- [ ] 非 TTY + `--yes`:删除成功(双根清理生效,与 ls 复核消失)
- [ ] 非 TTY 无 `--yes`:拒绝,文案与现状一致
- [ ] TTY + `--yes`:跳过输名确认直接删(经共享确认函数单测或等价测试形态覆盖;若测试基建无法模拟 TTY,则该路径以「确认判定函数对 --yes 的短路」单测钉住)
- [ ] 既有 instances 测试全绿

## Blocked by
无,可立即开始

## 涉及路径
- internal/cli/cache_instances.go
- internal/cli/cache_instances_test.go

## 副作用声明
无(只跑 `go test ./internal/cli/ -run Instances`)

decision_refs: D5、D1
review_blocks: 无
