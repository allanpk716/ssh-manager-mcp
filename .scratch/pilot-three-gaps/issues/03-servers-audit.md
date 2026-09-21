# 03 · servers 三命令入审计

## What to build
`sshmgr servers add / rm / edit` 三个命令写审计行,动作名 `server.add / server.rm / server.edit`。成功 Status=ok;失败路径(名称重复、条目未找到等)也写行 Status=error。

Command 摘要白名单:
- server.add:`{"name":<条目名>,"host":<主机地址>,"port":<端口>,"user":<用户名>}`
- server.rm:`{"name":<条目名>,"host":<主机地址>,"port":<端口>}`
- server.edit:`{"name":<条目名>,"changed":[<本次变更字段名列表,仅字段名>]}`
白名单外一律不落;`--password`/`--key`/`--key-passphrase`/`--sudo-password` 参数值永不出现(host/user/port 入摘要有纪律依据:owner-only 审计面不受 Plan 31 清洗约束,既有 pin-* 行已含 host)。

## 验收标准
- [ ] 三命令成功执行后审计断言出现对应 action 行,摘要字段与白名单一致
- [ ] edit 的 changed 列表只含字段名,不含值(口令类字段被改时 changed 含字段名但绝不含值)
- [ ] 失败路径留 Status=error 行
- [ ] 摘要零敏感明文
- [ ] 既有 servers 相关测试全绿

## Blocked by
01

## 涉及路径
- internal/cli/servers.go
- internal/cli/servers_audit_test.go(新建)

## 副作用声明
无(只跑 `go test ./internal/cli/ -run Servers`)

decision_refs: D4、D6、D7
review_blocks: 无
