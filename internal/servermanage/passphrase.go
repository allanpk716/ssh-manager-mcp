package servermanage

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
)

// SupplementKeyPassphrase 只补私钥口令：先校验口令能解开 vault 里已存的私
// 钥，再把口令写进这份凭据，并从所有引用这份凭据的服务器条目上移除待补口
// 令标签（每台修订号加一）。返回保存后的目标条目、字段名与全部受影响条目
// 名字，界面据此提示共用同一份私钥的其他服务器。
func (m *Manager) SupplementKeyPassphrase(identifier, passphrase string, src Source) (*EditResult, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, fmt.Errorf("server %q not found", identifier)
	}
	auditError := func(opErr error) error {
		_ = m.writeAudit("server.edit", map[string]any{
			"name": srv.Name, "changed": []string{"key-passphrase"},
		}, opErr, src)
		return opErr
	}
	affected, err := m.applyKeyPassphrase(srv, passphrase)
	if err != nil {
		return nil, auditError(err)
	}
	if err := m.writeAudit("server.edit", map[string]any{
		"name": srv.Name, "changed": []string{"key-passphrase"},
	}, nil, src); err != nil {
		return nil, err
	}
	var saved *models.Server
	for _, item := range affected {
		if item.ID == srv.ID {
			saved = item
		}
	}
	if saved == nil {
		saved, err = m.st.GetServer(srv.ID)
		if err != nil {
			return nil, err
		}
	}
	return &EditResult{
		Server: saved, AffectedServers: serverNames(affected), ChangedFields: []string{"key-passphrase"},
	}, nil
}

// applyKeyPassphrase 校验口令能解开 vault 里已存的私钥，再把口令写进这份
// 凭据，并从所有引用这份凭据的服务器条目上移除待补口令标签。只做数据写
// 入，不写审计；审计摘要由两个调用入口按各自请求构造。
func (m *Manager) applyKeyPassphrase(srv *models.Server, passphrase string) ([]*models.Server, error) {
	if srv.CredentialID == "" {
		return nil, errors.New("这台服务器还没有登录凭据，不能只补私钥口令")
	}
	cred, err := m.st.GetCredential(srv.CredentialID)
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Type != models.CredPrivateKey {
		return nil, errors.New("这台服务器的登录凭据不是私钥，不能补私钥口令")
	}
	if err := ValidateKeyPassphrase(cred.Secret, []byte(passphrase)); err != nil {
		return nil, fmt.Errorf("私钥口令校验失败，未保存：%w", err)
	}
	affected, err := m.st.UpdateCredentialPassphrase(cred.ID, []byte(passphrase))
	if err != nil {
		return nil, err
	}
	return affected, nil
}

// ValidateKeyPassphrase 校验口令确实能解开这把私钥。先走 OpenSSH 支持的
// 加密格式；再退回 PEM 传统加密格式（x509 加密块）手动解密并解析，两类格
// 式都不能用错误口令解开。
func ValidateKeyPassphrase(keyPEM, passphrase []byte) error {
	if _, err := ssh.ParsePrivateKeyWithPassphrase(keyPEM, passphrase); err == nil {
		return nil
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return errors.New("私钥内容不是有效的 PEM 格式")
	}
	if !x509.IsEncryptedPEMBlock(block) {
		return errors.New("这把私钥没有加密，不需要私钥口令")
	}
	der, err := x509.DecryptPEMBlock(block, passphrase)
	if err != nil {
		return err
	}
	decrypted := pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: der})
	if _, err := ssh.ParsePrivateKey(decrypted); err != nil {
		return fmt.Errorf("解密后的私钥无法解析：%w", err)
	}
	return nil
}

func serverNames(servers []*models.Server) []string {
	names := make([]string, 0, len(servers))
	for _, srv := range servers {
		names = append(names, srv.Name)
	}
	return names
}

// SharedCredentialServers 返回与这台服务器共用同一份登录凭据的全部服务器
// 名字（含这台服务器本身），供补私钥口令前列出受影响条目；没有登录凭据时
// 返回空列表。
func (m *Manager) SharedCredentialServers(identifier string) ([]string, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, fmt.Errorf("server %q not found", identifier)
	}
	if srv.CredentialID == "" {
		return nil, nil
	}
	servers, err := m.st.ServersUsingCredential(srv.CredentialID)
	if err != nil {
		return nil, err
	}
	return serverNames(servers), nil
}
