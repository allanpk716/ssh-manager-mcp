package mcpserver

import (
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// serverByAddr 找到兜底地址上的第一个服务器条目（测试夹具用）。
func serverByAddr(st *store.Store, host string, port int) *models.Server {
	servers, err := st.ListServers()
	if err != nil {
		return nil
	}
	for _, srv := range servers {
		if srv.Host == host && srv.Port == port {
			return srv
		}
	}
	return nil
}
