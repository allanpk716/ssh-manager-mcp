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
	if err := ValidateAdminPassword(password, password); err != nil {
		return 0, err
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return 0, err
	}
	defer st.Close()
	return st.SetWebAdminPasswordWithAudit(password, audit)
}

// ValidateAdminPassword 是命令行与初始化向导共用的管理员口令表单规则。
func ValidateAdminPassword(password, confirmation []byte) error {
	if string(password) != string(confirmation) {
		return errors.New("两次输入的管理员口令不一致")
	}
	if len(password) == 0 {
		return errors.New("管理员口令不能为空")
	}
	return nil
}

// SetAdminPasswordForWebUI 是初始化向导使用的设口令入口：沿用同一套校验、
// 哈希、会话代与审计规则，并把来源记为 Web UI。
func SetAdminPasswordForWebUI(password, confirmation []byte, remoteAddress string) (int64, error) {
	if err := ValidateAdminPassword(password, confirmation); err != nil {
		return 0, err
	}
	return SetAdminPassword(password, store.AuditRow{
		Action: "admin.password_set", Status: "ok",
		Source: store.AuditSourceWebUI, SourceIP: remoteAddress,
		Command: "{}",
	})
}
