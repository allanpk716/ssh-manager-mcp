package cli

import (
	"ssh-manager-mcp/internal/store"
)

// roleForVaultBackedCommand 统一「vault 优先、客户端缓存次之」的角色判定；
// 两者都没有时返回调用方传入的初始化引导错误。
func roleForVaultBackedCommand(instance string, missingVault error) (policyRole, error) {
	role, err := resolvePolicyRoleFor(instance)
	if err != nil {
		return 0, err
	}
	if role == policyRoleUnenrolled {
		return 0, missingVault
	}
	return role, nil
}

// requireOwnerVaultForCommand 断言本机持有 vault 并返回已解锁的存储；缓存
// 客户端得到调用方传入的权威端指引。
func requireOwnerVaultForCommand(missingVault, clientRefusal error) (*store.Store, error) {
	role, err := roleForVaultBackedCommand("", missingVault)
	if err != nil {
		return nil, err
	}
	if role == policyRoleClient {
		return nil, clientRefusal
	}
	return openUnlockedStore()
}
