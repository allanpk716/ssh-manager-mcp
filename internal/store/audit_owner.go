package store

import (
	"encoding/json"
	"time"
)

// OwnerAuditRow 构造一条 owner 身份的审计行(owner = 设备主人亲自在
// CLI 里做的变更,不挂任何项目):ProjectID 留空(与 pin-*/pair.* 现有形
// 态一致,查询面靠空 project_id 识别 owner 操作)、TS 取当前时间、Status
// 为 "ok"(命令失败的行由调用方自行改写 Status 后再写入)。Command 为
// MarshalAuditSummary(summary) 的返回值。摘要只含调用方按白名单传入的
// 字段——本函数不做白名单过滤以外的任何补全:调用方没传的字段在行里不
// 存在(宁缺勿泄)。
func OwnerAuditRow(action string, summary map[string]any) AuditRow {
	return AuditRow{
		TS:      time.Now(),
		Action:  action,
		Command: MarshalAuditSummary(summary),
		Status:  "ok",
	}
}

// MarshalAuditSummary 把白名单摘要字段序列化为 JSON 字符串。键序稳定:
// json.Marshal 对 map 按键排序。nil 与空 map 返回 "{}";序列化失败(值
// 无法转 JSON)也返回 "{}"——摘要只是审计行的附属文本,绝不让它把写入
// 整条审计行的动作搞失败。
func MarshalAuditSummary(summary map[string]any) string {
	if len(summary) == 0 {
		return "{}"
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// WriteOwnerAudit 便捷写入:= WriteAudit(OwnerAuditRow(action, summary))。
// 与既有写入管道同一条路(在线落 audit_log 表;只读缓存走旁车文件)。
func (s *Store) WriteOwnerAudit(action string, summary map[string]any) error {
	return s.WriteAudit(OwnerAuditRow(action, summary))
}
