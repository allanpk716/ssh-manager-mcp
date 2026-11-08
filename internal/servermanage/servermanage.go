// Package servermanage 提供与界面无关的「服务器管理」模块：服务器列表与注
// 意原因、详情、编辑、删除预览与确认、SSH 配置导入、逐台补全统一收拢在这
// 里，命令行和 Web UI 共用同一套业务规则与审计写入。
package servermanage

import (
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// Manager 是服务器管理模块的入口，内部只调用存储层的现有函数与本票新增
// 的预览函数，不改变它们的语义。
type Manager struct {
	st *store.Store
	// insertServer 是测试接缝：模拟某台服务器插入失败，生产路径为 nil。
	insertServer func(srv *models.Server, cred, sudo *models.Credential) (string, error)
}

// New 返回挂在一个已解锁存储上的服务器管理模块。
func New(st *store.Store) *Manager {
	return &Manager{st: st}
}

// Source 描述一次服务器管理写操作从哪里来：命令行，或 Web UI（附来源地
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

// writeAudit 记录一次服务器管理写操作的 owner 审计行：操作成功写 Status=
// ok，操作失败写 Status=error；摘要由调用方按白名单构造，只含字段名等非敏
// 感内容。
func (m *Manager) writeAudit(action string, summary map[string]any, opErr error, src Source) error {
	row := store.OwnerAuditRow(action, summary)
	row.Source = src.kind
	row.SourceIP = src.addr
	if opErr != nil {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}

// insertServerRow 走测试接缝或生产事务插入一台服务器。
func (m *Manager) insertServerRow(srv *models.Server, cred, sudo *models.Credential) (string, error) {
	if m.insertServer != nil {
		return m.insertServer(srv, cred, sudo)
	}
	return m.st.AddServerWithCredentials(srv, cred, sudo)
}
