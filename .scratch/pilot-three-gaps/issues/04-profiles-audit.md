# 04 · profiles 三命令入审计

## What to build
`sshmgr profiles add / grant / remove` 三个命令写审计行,动作名 `profile.add / profile.grant / profile.rm`。成功 Status=ok;失败路径(名称重复、未知 server 导致 grant 中止等)也写行 Status=error。

Command 摘要白名单:
- profile.add:`{"name":<轮廓名>}`
- profile.grant:`{"profile":<轮廓名>,"servers":"<N>台:<条目名逗号列表>"}`(或等价结构含计数与名列表,实现取一种并测试钉住)
- profile.rm:`{"name":<轮廓名>}`
白名单外一律不落。

## 验收标准
- [ ] 三命令成功执行后审计断言出现对应 action 行,摘要与白名单一致
- [ ] 失败路径留 Status=error 行
- [ ] 既有 profiles 测试全绿

## Blocked by
01

## 涉及路径
- internal/cli/profiles.go
- internal/cli/profiles_audit_test.go(新建)

## 副作用声明
无(只跑 `go test ./internal/cli/ -run Profile`)

decision_refs: D4、D6
review_blocks: 无
