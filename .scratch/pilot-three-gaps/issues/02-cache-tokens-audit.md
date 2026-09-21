# 02 · cache-tokens 三命令入审计

## What to build
`sshmgr cache-tokens add / revoke / bind` 三个命令写审计行,动作名 `cache-token.add / cache-token.revoke / cache-token.bind`。用 01 票的 WriteOwnerAudit(或 OwnerAuditRow+WriteAudit);成功 Status=ok;失败路径(如 profile 不存在导致命令报错)也写行,Status=error,摘要按白名单能取多少取多少。失败行的 ExitCode/DurationMS 按既有 AuditRow 惯例取值(实现顺手钉断言——评审提示 F5)。

Command 摘要白名单:
- cache-token.add:`{"name":<设备码名>,"profile":<轮廓名>}`
- cache-token.revoke:`{"name":<设备码名>}`
- cache-token.bind:`{"name":<设备码名>,"profile":<轮廓名>}`
白名单外字段一律不落;**任何令牌/码明文绝不入摘要**(add 命令打印的授权码只在 stdout,不进审计)。

## 验收标准
- [ ] 三命令成功执行后 `AuditRows` 断言出现对应 action 行,Command 摘要字段与白名单一致
- [ ] 失败路径(未知 profile 的 add/bind;未知 name 的 revoke)留 Status=error 行
- [ ] 摘要零敏感明文(断言 Command 不含 token 值)
- [ ] 既有 cache-tokens 测试全绿

## Blocked by
01

## 涉及路径
- internal/cli/cache_tokens.go
- internal/cli/cache_tokens_audit_test.go(新建,审计断言集中此文件)

## 副作用声明
无(只跑 `go test ./internal/cli/ -run CacheToken`)

decision_refs: D4、D6
review_blocks: 无(F5 随本票钉断言)
