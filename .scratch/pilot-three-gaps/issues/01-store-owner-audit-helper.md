# 01 · store 层 owner 审计辅助(prefactor)

## What to build
为 owner CLI 变更命令写审计行提供 store 层共享辅助(后续 02/03/04 三张票全部依赖):一个构造「owner 身份审计行」的函数 + 一个把摘要映射序列化为脱敏 JSON 字符串的函数 + 一个便捷写入函数。owner 身份 = ProjectID 留空(与 pin-*/pair.* 现有形态一致);摘要只含调用方按白名单传入的字段,辅助函数本身不做白名单过滤以外的任何补全(宁缺勿泄:调用方没传的字段不存在)。

接口契约(后续票按此构建,不得偏离):
```go
// internal/store/audit_owner.go
// OwnerAuditRow 构造 owner 身份审计行:ProjectID="",TS=now,Status="ok"。
func OwnerAuditRow(action string, summary map[string]any) AuditRow
// MarshalAuditSummary 把白名单字段序列化为 JSON 字符串(键序稳定:json.Marshal 的 map 按键排序;错误时返回 "{}")。
func MarshalAuditSummary(summary map[string]any) string
// WriteOwnerAudit 便捷写入 = WriteAudit(OwnerAuditRow(action, summary))。
func (s *Store) WriteOwnerAudit(action string, summary map[string]any) error
```
Command 字段 = MarshalAuditSummary 的返回值。

## 验收标准
- [ ] 三个导出函数存在且行为如上(OwnerAuditRow 的 ProjectID 为空串、Status 为 ok、Command 为 JSON 字符串)
- [ ] MarshalAuditSummary 对 nil/空 map 返回 "{}";对含字符串/整数/字符串切片的 map 产出合法 JSON
- [ ] WriteOwnerAudit 在内存库上写入后,行可经既有查询(如 store 测试的 AuditRows 断言形态)读到,action/project 为空/project_id 为 owner 形态
- [ ] 单测覆盖以上各条(internal/store 既有测试先例形态)

## Blocked by
无,可立即开始

## 涉及路径
- internal/store/audit_owner.go(新建)
- internal/store/audit_owner_test.go(新建)

## 副作用声明
无(只跑本包单测:`go test ./internal/store/ -run AuditOwner`)

decision_refs: D4、D6
review_blocks: 无
