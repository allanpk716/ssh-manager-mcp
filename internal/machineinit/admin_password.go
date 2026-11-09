package machineinit

import (
	"errors"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// SetAdminPassword 设置权威端管理员口令，复用现有存储与规则：argon2id
// 哈希、会话代加一（作废全部 Web 会话）、同一个事务里的审计行。调用方
// 负责先确认本机角色是权威端，并传入自己入口的审计来源。
func SetAdminPassword(password []byte, audit store.AuditRow) (int64, error) {
	if len(password) == 0 {
		return 0, errors.New("管理员口令不能为空")
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return 0, err
	}
	defer st.Close()
	return st.SetWebAdminPasswordWithAudit(password, audit)
}
