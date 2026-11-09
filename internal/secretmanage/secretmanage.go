// Package secretmanage 提供与界面无关的机密项管理模块：新增、换值、修改
// 元数据、删除、授权变更预览与执行、列表统一收拢在这里，命令行和 Web UI
// 共用同一套业务规则与审计来源。本模块不暴露任何读取机密项值的操作；
// 输出遮蔽与环境变量注入继续使用存储层内部的专用取值路径。
package secretmanage

import (
	"fmt"

	"ssh-manager-mcp/internal/store"
)

// Manager 是机密项管理模块的入口，内部只调用存储层的函数，不在模块里
// 重复实现名字、值与环境变量名校验。
type Manager struct {
	st *store.Store
}

// New 返回挂在一个已解锁存储上的机密项管理模块。
func New(st *store.Store) *Manager {
	return &Manager{st: st}
}

// Source 描述一次机密项写操作从哪里来：命令行，或 Web UI（附来源地址）。
// 模块按它给同一事务里的审计行填来源列。
type Source struct {
	origin store.WriteSource
}

// CLISource 返回命令行来源。
func CLISource() Source {
	return Source{origin: store.CLISource()}
}

// WebUISource 返回 Web UI 来源，address 是这次请求的来源地址。
func WebUISource(address string) Source {
	return Source{origin: store.WebUISource(address)}
}

// MetadataEdit 表示一次机密项元数据修改。指针为空表示不改这个字段；
// 环境变量名指针指向空字符串表示清除环境变量名。
type MetadataEdit struct {
	Note            *string
	EnvironmentName *string
}

// ListFilter 是机密项列表的筛选条件：Query 按名字或备注做不区分大小写的
// 子串匹配；Server 按服务器条目标识符或名字筛选可写入列表。两个条件为
// 空时返回全部机密项。
type ListFilter struct {
	Query  string
	Server string
}

// AddSecret 新增一个机密项。名字、值与环境变量名的校验和加密都在存储层
// 同一个事务边界内完成；审计摘要只含机密项名，永不含值。
func (m *Manager) AddSecret(name, note, environmentName string, value []byte, src Source) error {
	return m.st.AddSecretWithEnvironmentName(name, note, value, environmentName, src.origin)
}

// SetSecretValue 覆盖机密项的值。旧值移入上一个值字段供输出遮蔽继续使
// 用；本模块与 Web 适配层都不提供读回值的入口。
func (m *Manager) SetSecretValue(name string, value []byte, src Source) error {
	return m.st.SetSecretValue(name, value, src.origin)
}

// EditMetadata 在同一个事务里修改备注与环境变量名。字段缺席表示不改；
// 环境变量名给空字符串表示清除。
func (m *Manager) EditMetadata(name string, edit MetadataEdit, src Source) error {
	return m.st.EditSecretMetadata(name, store.SecretMetadataEdit{
		Note:            edit.Note,
		EnvironmentName: edit.EnvironmentName,
	}, src.origin)
}

// DeleteSecret 删除机密项及其全部授权行。数据行与审计行在同一个事务提
// 交，摘要不包含值。
func (m *Manager) DeleteSecret(name string, src Source) error {
	return m.st.DeleteSecret(name, src.origin)
}

// AllowSecrets 把若干服务器条目加入若干机密项的可写入列表。任何一个名
// 字不存在时存储层合并报错，整体不生效。
func (m *Manager) AllowSecrets(secretNames, serverNames []string, src Source) error {
	return m.st.AllowSecrets(secretNames, serverNames, src.origin)
}

// DisallowSecrets 把若干服务器条目移出若干机密项的可写入列表。错误语
// 义与 AllowSecrets 相同。
func (m *Manager) DisallowSecrets(secretNames, serverNames []string, src Source) error {
	return m.st.DisallowSecrets(secretNames, serverNames, src.origin)
}

// List 返回机密项可见列表。结果只有标识符、名字、备注、环境变量名、可
// 写入服务器条目名与时间，绝不含值、值的哈希或长度。
func (m *Manager) List(filter ListFilter) ([]store.SecretView, error) {
	serverName := filter.Server
	if serverName != "" {
		server, err := m.st.ResolveServer(serverName)
		if err != nil {
			return nil, err
		}
		if server == nil {
			return nil, fmt.Errorf("服务器条目 %q 不存在", filter.Server)
		}
		serverName = server.Name
	}
	views, err := m.st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil {
		return nil, err
	}
	views = store.FilterSecretsByServer(views, serverName)
	return store.FilterSecretsByQuery(views, filter.Query), nil
}
