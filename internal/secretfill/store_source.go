package secretfill

import (
	"context"

	"ssh-manager-mcp/internal/store"
)

// StoreSource 从本机 vault 或已导入缓存快照的只读存储取值。单机部署与
// 多机客户端使用同一个填值包，只在这里换数据来源。
type StoreSource struct {
	Store       *store.Store
	serverIndex map[string]bool
	serverIDs   []string
}

// NewStoreSource 返回基于 store.Store 的机密项取值来源。profileServerIDs
// 是当前调用方 profile 可见的服务器标识，用来判定机密项本身是否对调用方
// 可见，并裁剪诊断里的可写入服务器名。
func NewStoreSource(valueStore *store.Store, profileServerIDs []string) *StoreSource {
	serverIndex := make(map[string]bool, len(profileServerIDs))
	for _, serverID := range profileServerIDs {
		serverIndex[serverID] = true
	}
	return &StoreSource{Store: valueStore, serverIndex: serverIndex, serverIDs: profileServerIDs}
}

// Secrets 只解密调用方引用的机密项，并读取每个机密项的可写入服务器。
func (source *StoreSource) Secrets(_ context.Context, names []string) (SourceResult, error) {
	visibleViews, err := source.Store.ListSecretsInScope(store.ScopeToServers(source.serverIDs...))
	if err != nil {
		return SourceResult{}, err
	}
	visibleNames := make(map[string]bool, len(visibleViews))
	for _, view := range visibleViews {
		visibleNames[view.Name] = true
	}
	values, failed, err := source.Store.SecretValuesForUpload(names)
	if err != nil {
		return SourceResult{}, err
	}
	for name := range values {
		if !visibleNames[name] {
			delete(values, name)
		}
	}
	visibleFailed := make([]string, 0, len(failed))
	for _, name := range failed {
		if visibleNames[name] {
			visibleFailed = append(visibleFailed, name)
		}
	}
	result := SourceResult{
		Secrets:         make(map[string]Secret, len(values)),
		ReadFailedNames: visibleFailed,
	}
	for name, value := range values {
		secret := Secret{Name: name, Value: value.Value, ValueVersion: value.ValueVersion}
		for _, serverID := range value.WritableServerIDs {
			if !source.serverIndex[serverID] {
				continue
			}
			serverRef := ServerRef{ID: serverID}
			if server, err := source.Store.GetServer(serverID); err == nil && server != nil {
				serverRef.Name = server.Name
			}
			secret.WritableServers = append(secret.WritableServers, serverRef)
		}
		result.Secrets[name] = secret
	}
	return result, nil
}
