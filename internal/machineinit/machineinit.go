// Package machineinit 是初始化模块：命令行 init 与 Web UI 初始化向导共用
// 的本机状态改动入口。对外只暴露少数操作，现有角色解析、vault 判定与
// 管理员口令读写保留在原包里，由本包调用。
package machineinit

import (
	"errors"
	"fmt"
	"os"

	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// ResultKind 描述一次初始化操作改变了什么。
type ResultKind int

const (
	// ResultCreated 表示空机器上新建了 vault 并写好 role.json。
	ResultCreated ResultKind = iota
	// ResultRoleRewritten 表示 vault 已存在，本次只改写了 role.json
	//（单机升级为权威端，或补齐 setup_complete）。
	ResultRoleRewritten
	// ResultAlreadyInitialized 表示目标状态已经达成，本次没有改动。
	ResultAlreadyInitialized
)

// Result 是「初始化为某角色」的结果。
type Result struct {
	Kind          ResultKind
	MasterKeyPath string
}

// Initialize 把本机初始化为单机或权威端：没有 vault 时生成随机主密钥写
// 入密钥文件并建库（不引入口令），再写 role.json（setup_complete 为真）；
// 已有 vault 的单机初始化为权威端只改写 role.json，vault 内容不动。拒绝
// 权威端降为单机、拒绝在客户端机器上执行；目标状态已达成时返回
// ResultAlreadyInitialized，不报错。
func Initialize(target roles.Role) (Result, error) {
	result := Result{}
	switch target {
	case roles.RoleStandalone, roles.RoleServer:
	default:
		return result, fmt.Errorf("初始化只支持单机（standalone）与权威端（server），不支持 %q", string(target))
	}

	state, err := roles.Load()
	if err != nil {
		return result, err
	}
	current, err := currentRole(state)
	if err != nil {
		return result, err
	}
	if current == roles.RoleClient || roles.CachePresent() {
		return result, errors.New("本机是客户端，客户端不能初始化为单机或权威端；如需重建，先运行 `sshmgr clear` 清除本机客户端数据")
	}
	if current == roles.RoleServer && target == roles.RoleStandalone {
		return result, errors.New("本机已是权威端，权威端不能降级为单机；单机与权威端的数据模型不同，降级不受支持")
	}

	keyPath := os.Getenv("SSHMGR_FILEKEY_PATH")
	if keyPath == "" {
		keyPath, err = paths.MasterKeyPath()
		if err != nil {
			return result, err
		}
	}
	result.MasterKeyPath = keyPath

	if roles.VaultExists() {
		if err := roles.VaultOpenErr(); err != nil {
			return result, fmt.Errorf("本机 vault 已存在但锁定或不可读：%w — 先运行 `sshmgr unlock`（不会覆盖既有 vault）", err)
		}
		result.Kind = ResultRoleRewritten
	} else {
		if err := EnsureVault(); err != nil {
			return result, err
		}
		result.Kind = ResultCreated
	}
	if state != nil && state.Role == target && state.SetupComplete {
		result.Kind = ResultAlreadyInitialized
		return result, nil
	}
	if err := roles.Save(roles.State{Role: target, SetupComplete: true}); err != nil {
		return result, err
	}
	return result, nil
}

// SelectRoleWithoutVault 只写入向导第一步选出的角色，不创建 vault：
// setup_complete 保持为假，供中途退出后的下一次 `sshmgr ui` 从「建 vault」
// 这一步续做。客户端入网走命令行配对，不调用这个入口。
func SelectRoleWithoutVault(target roles.Role) error {
	switch target {
	case roles.RoleStandalone, roles.RoleServer:
	default:
		return fmt.Errorf("向导只支持选择单机（standalone）或权威端（server），不支持 %q", string(target))
	}
	state, err := roles.Load()
	if err != nil {
		return err
	}
	current, err := currentRole(state)
	if err != nil {
		return err
	}
	if current == roles.RoleClient || roles.CachePresent() {
		return errors.New("本机是客户端，不能进入单机或权威端初始化向导；如需重建，先运行 `sshmgr clear` 清除本机客户端数据")
	}
	if roles.VaultExists() {
		return errors.New("本机已有 vault，不能再执行「只选角色」；请继续完成初始化，或运行 `sshmgr doctor` 检查")
	}
	return roles.Save(roles.State{Role: target, SetupComplete: false})
}

// currentRole 判定本机当前角色：role.json 存在时以它为准；不存在时沿用
// 现有角色解析链推断（vault → serve 证书 → 客户端缓存 → 空机器），保证
// 旧机器上的初始化与启动判定得到同一个结论。
func currentRole(state *roles.State) (roles.Role, error) {
	if state != nil {
		return state.Role, nil
	}
	launch, err := roles.ResolveMode("")
	if err != nil {
		return "", err
	}
	switch launch.Kind {
	case roles.LaunchBroker:
		return launch.Role, nil
	case roles.LaunchClient:
		return roles.RoleClient, nil
	default:
		return roles.RoleStandalone, nil
	}
}

// EnsureVault 确保一个可打开的 vault 存在：生成随机主密钥，经
// FileKeyProvider 写入固定路径的密钥文件（Windows 上收紧文件访问控制），
// 再创建 store.db 与数据库结构。已有锁定的 vault 是错误，绝不覆盖。
func EnsureVault() error {
	if roles.VaultExists() {
		if err := roles.VaultOpenErr(); err != nil {
			return fmt.Errorf("本机 vault 已存在但锁定或不可读：%w — 先运行 `sshmgr unlock`（不会覆盖既有 vault）", err)
		}
		return nil
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		return err
	}
	provider := store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")}
	if err := provider.Set(masterKey); err != nil {
		return err
	}
	vaultPath := os.Getenv("SSHMGR_STORE")
	if vaultPath == "" {
		vaultPath, err = store.DefaultStorePath()
		if err != nil {
			return err
		}
	}
	st, err := store.Open(vaultPath, masterKey)
	if err != nil {
		return err
	}
	return st.Close()
}
