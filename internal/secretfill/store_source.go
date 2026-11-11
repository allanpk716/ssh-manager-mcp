package secretfill

import (
	"context"

	"ssh-manager-mcp/internal/store"
)

// StoreSource 从本机 vault 或已导入缓存快照的只读存储取值。单机部署与
// 多机客户端使用同一个填值包，只在这里换数据来源。
type StoreSource struct {
	Store *store.Store
}

// NewStoreSource 返回基于 store.Store 的机密项取值来源。
func NewStoreSource(valueStore *store.Store) *StoreSource {
	return &StoreSource{Store: valueStore}
}

// Secrets 只解密调用方引用的机密项，并读取每个机密项的可写入服务器。
func (source *StoreSource) Secrets(_ context.Context, names []string) (SourceResult, error) {
	values, failed, err := source.Store.SecretValuesForUpload(names)
	if err != nil {
		return SourceResult{}, err
	}
	result := SourceResult{
		Secrets:         make(map[string]Secret, len(values)),
		ReadFailedNames: failed,
	}
	for name, value := range values {
		secret := Secret{Name: name, Value: value.Value}
		for _, serverID := range value.WritableServerIDs {
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
