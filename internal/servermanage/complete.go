package servermanage

import (
	"errors"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// CompleteRequest 是一台服务器的补全提交：六个服务器元数据字段、sudo 密码
// 与一个条件凭据（无凭据机器的密码，或带待补口令标签机器的私钥口令）。条
// 件凭据与 sudo 密码为空表示不变。
type CompleteRequest struct {
	Identifier  string
	Description *string
	Location    *string
	Hardware    *string
	Services    *string
	Role        *string
	Caveats     *string

	SudoPassword  string
	Password      string
	KeyPassphrase string
}

// CompleteResult 是补全提交的结果。NoOp 表示提交值与当前值完全一致：不写
// 入、不加修订号、不写审计。条件凭据是私钥口令时，AffectedServers 列出共
// 用同一份凭据并一起去掉待补标记的服务器名字。
type CompleteResult struct {
	Server          *models.Server
	ChangedFields   []string
	AffectedServers []string
	NoOp            bool
}

// Complete 保存一台服务器的补全表单。审计动作沿用 server.edit，摘要只含字
// 段名。
func (m *Manager) Complete(req CompleteRequest, src Source) (*CompleteResult, error) {
	srv, err := m.st.ResolveServer(req.Identifier)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, errors.New("补全目标服务器不存在")
	}
	auditError := func(fields []string, opErr error) error {
		_ = m.writeAudit("server.edit", map[string]any{
			"name": srv.Name, "changed": fields,
		}, opErr, src)
		return opErr
	}
	attempted := []string{}
	add := func(set bool, name string) {
		if set {
			attempted = append(attempted, name)
		}
	}
	add(req.Description != nil, "description")
	add(req.Location != nil, "location")
	add(req.Hardware != nil, "hardware")
	add(req.Services != nil, "services")
	add(req.Role != nil, "role")
	add(req.Caveats != nil, "special-handling")
	add(req.SudoPassword != "", "sudo-password")
	add(req.Password != "", "password")
	add(req.KeyPassphrase != "", "key-passphrase")

	// 条件凭据出现条件与原终端图形界面一致：未挂凭据时可设密码；带待补
	// 口令标签时可补私钥口令。不满足条件的输入直接拒绝，不静默丢弃。
	if req.Password != "" && srv.CredentialID != "" {
		return nil, auditError(attempted, errors.New("这台服务器已有登录凭据，补全表单不能另设密码"))
	}
	if req.KeyPassphrase != "" && !models.ServerHasTag(srv.Tags, models.TagNeedsPassphrase) {
		return nil, auditError(attempted, errors.New("这台服务器没有待补私钥口令标记，不能在补全表单里填私钥口令"))
	}
	if req.Password != "" && req.KeyPassphrase != "" {
		return nil, auditError(attempted, errors.New("同一台服务器的补全表单只能提交一种条件凭据"))
	}

	changed := []string{}
	metadata := []struct {
		field  *string
		name   string
		target *string
	}{
		{req.Description, "description", &srv.Description},
		{req.Location, "location", &srv.Location},
		{req.Hardware, "hardware", &srv.Hardware},
		{req.Services, "services", &srv.Services},
		{req.Role, "role", &srv.Role},
		{req.Caveats, "special-handling", &srv.Caveats},
	}
	metadataChanged := false
	for _, item := range metadata {
		if item.field == nil {
			continue
		}
		value := strings.TrimSpace(*item.field)
		if value == *item.target {
			continue
		}
		*item.target = value
		changed = append(changed, item.name)
		metadataChanged = true
	}

	var affected []string
	if req.KeyPassphrase != "" {
		supplemented, err := m.SupplementKeyPassphrase(srv.ID, req.KeyPassphrase, src)
		if err != nil {
			return nil, auditError(attempted, err)
		}
		affected = supplemented.AffectedServers
		changed = append(changed, "key-passphrase")
		// 补口令后目标行已更新；重新读取，避免用旧快照覆盖标签。
		srv, err = m.st.GetServer(srv.ID)
		if err != nil {
			return nil, err
		}
	}
	if !metadataChanged && req.Password == "" && req.SudoPassword == "" {
		if req.KeyPassphrase == "" {
			current, err := m.st.GetServer(srv.ID)
			if err != nil {
				return nil, err
			}
			return &CompleteResult{Server: current, NoOp: true}, nil
		}
		if len(changed) == 0 {
			current, err := m.st.GetServer(srv.ID)
			if err != nil {
				return nil, err
			}
			return &CompleteResult{Server: current, AffectedServers: affected}, nil
		}
	}

	var cred, sudoCred *models.Credential
	if req.Password != "" {
		cred = &models.Credential{Type: models.CredPassword, Secret: []byte(req.Password)}
		changed = append(changed, "password")
	}
	if req.SudoPassword != "" {
		if strings.TrimSpace(req.SudoPassword) == "" {
			return nil, auditError(attempted, errors.New("sudo 密码不能为空"))
		}
		sudoCred = &models.Credential{Type: models.CredPassword, Secret: []byte(req.SudoPassword)}
		changed = append(changed, "sudo-password")
	}
	if err := m.st.UpdateServerWithCredentials(srv, cred, sudoCred); err != nil {
		return nil, auditError(attempted, err)
	}
	if err := m.writeAudit("server.edit", map[string]any{
		"name": srv.Name, "changed": changed,
	}, nil, src); err != nil {
		return nil, err
	}
	saved, err := m.st.GetServer(srv.ID)
	if err != nil {
		return nil, err
	}
	return &CompleteResult{Server: saved, ChangedFields: changed, AffectedServers: affected}, nil
}
