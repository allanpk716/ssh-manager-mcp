package machineinit

import (
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// StateSnapshot 是本机初始化状态的只读快照，供 `sshmgr ui` 的去向判定与
// 初始化向导页面使用。只描述当前状态，不做任何改动。
type StateSnapshot struct {
	// Role 是 role.json 的内容；文件不存在时为 nil。
	Role *roles.State
	// DetectedRole 是没有 role.json 的旧机器按现有规则推断出的角色。
	DetectedRole roles.Role
	// VaultExists 表示 vault 数据库文件是否存在。
	VaultExists bool
	// VaultOpenable 表示 vault 是否能打开；vault 不存在时为 false。
	VaultOpenable bool
	// VaultOpenErr 是 vault 存在但打不开时的原因；其他情况为空。
	VaultOpenErr error
	// ClientCachePresent 表示本机是否存在客户端缓存（角色文件或任一
	// 实例的缓存凭据）。
	ClientCachePresent bool
	// AdminPasswordSet 表示权威端管理员口令是否已设置；vault 不存在或
	// 打不开时为 false。
	AdminPasswordSet bool
	// ServeWebUI 是 serve 独立 Web UI 的端口推导与运行探测结果。
	ServeWebUI ServeWebUIState
}

// ReadSnapshot 读取本机状态快照。
func ReadSnapshot() (StateSnapshot, error) {
	state, err := roles.Load()
	if err != nil {
		return StateSnapshot{}, err
	}
	snapshot := StateSnapshot{Role: state, ClientCachePresent: roles.CachePresent()}
	if state == nil {
		if launch, resolveErr := roles.ResolveMode(""); resolveErr == nil {
			snapshot.DetectedRole = launch.Role
		}
	}
	snapshot.VaultExists = roles.VaultExists()
	if snapshot.VaultExists {
		snapshot.VaultOpenErr = roles.VaultOpenErr()
		snapshot.VaultOpenable = snapshot.VaultOpenErr == nil
	}
	if !snapshot.VaultOpenable {
		return snapshot, nil
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return snapshot, err
	}
	defer st.Close()
	passwordSet, err := st.WebAdminPasswordIsSet()
	if err != nil {
		return snapshot, err
	}
	snapshot.AdminPasswordSet = passwordSet
	snapshot.ServeWebUI, err = probeServeWebUI(st)
	return snapshot, err
}

// probeServeWebUI 读取端口与监听地址推导结果，并对本机回环地址上的独立
// Web UI 端口发一次 HTTPS 探测请求。
func probeServeWebUI(st *store.Store) (ServeWebUIState, error) {
	port, plan, err := mcpserver.ReadWebUIListenPlan(st)
	if err != nil {
		return ServeWebUIState{}, err
	}
	state := ServeWebUIState{
		Known:             true,
		Port:              port,
		Addresses:         plan.Addresses,
		NeedsManualListen: plan.NeedsManualListen,
	}
	state.Running = loopbackHTTPSResponds(port)
	return state, nil
}
