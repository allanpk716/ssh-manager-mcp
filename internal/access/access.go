// Package access 提供与界面无关的「访问管理」模块：profile、项目令牌、
// 设备码与配对的管理入口统一收拢在这里，命令行和 Web UI 共用同一套业
// 务规则与审计写入。本文件只放模块的公共构造；具体操作按领域分文件。
package access

import (
	"ssh-manager-mcp/internal/store"
)

// Manager 是访问管理模块的入口，内部只调用存储层的现有函数，不改变它
// 们的语义。
type Manager struct {
	st *store.Store
}

// New 返回挂在一个已解锁存储上的访问管理模块。
func New(st *store.Store) *Manager {
	return &Manager{st: st}
}

// Source 描述一次访问管理写操作从哪里来：命令行，或 Web UI（附来源地
// 址）。模块按它给审计行填来源列。
type Source struct {
	kind string
	addr string
}

// CLISource 返回命令行来源。
func CLISource() Source {
	return Source{kind: store.AuditSourceCLI}
}

// WebUISource 返回 Web UI 来源，addr 是这次请求的来源地址。
func WebUISource(addr string) Source {
	return Source{kind: store.AuditSourceWebUI, addr: addr}
}

// writeAudit 记录一次访问管理写操作的 owner 审计行：操作成功写 Status=
// ok，操作失败写 Status=error；摘要由调用方按白名单构造。数据操作成功
// 而审计写入失败时，把审计错误返回给调用方，保证失败可见。
func (m *Manager) writeAudit(action string, summary map[string]any, opErr error, src Source) error {
	row := store.OwnerAuditRow(action, summary)
	row.Source = src.kind
	row.SourceIP = src.addr
	if opErr != nil {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}
