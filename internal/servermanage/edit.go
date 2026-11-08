package servermanage

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// RevisionConflict 表示编辑表单打开后，服务器条目已被别的入口修改。Current
// 是当前值，界面用它重新渲染表单；命令行不带修订号时不会遇到这个错误。
type RevisionConflict struct {
	Current *models.Server
}

func (e *RevisionConflict) Error() string {
	return fmt.Sprintf("服务器条目已被其他入口修改（当前修订号 %d），请核对最新值后重试", e.Current.Revision)
}

// EditRequest 是一次按字段的部分更新。指针字段为 nil 表示不修改；凭据字段
// 为空串表示不修改。KeyBytes 优先于 KeyPath（Web UI 粘贴内容，命令行读文
// 件）。
type EditRequest struct {
	Identifier       string
	ExpectedRevision int64
	CheckRevision    bool

	Name        *string
	Host        *string
	Port        *int
	User        *string
	Shell       *string
	Description *string
	Location    *string
	Hardware    *string
	Services    *string
	Role        *string
	Caveats     *string
	Tags        *[]string
	ExposeHost  *bool

	// 凭据字段用指针区分「未提交」与「提交了空值」：nil 表示不修改；非 nil
	// 的空值会按更换凭据的校验规则报错。KeyContent 是界面粘贴的私钥内容，
	// KeyPath 是文件路径，两者取先出现者。
	Password        *string
	KeyPath         *string
	KeyContent      *string
	KeyPassphrase   *string
	ClearCredential bool
	SudoPassword    *string
}

// EditResult 是一次编辑的结果：保存后的服务器条目、实际写入的字段名，以及
// 「只补私钥口令」时共用同一份凭据而一并移除待补标记的服务器名字。
type EditResult struct {
	Server          *models.Server
	ChangedFields   []string
	AffectedServers []string
}

// Edit 按字段部分更新一台服务器，走存储层整行更新加凭据替换事务：修订号加
// 一，旧凭据按引用计数清理，重新挂凭据时移除待补私钥口令标签。审计动作是
// server.edit，摘要只含条目名与字段名。
func (m *Manager) Edit(req EditRequest, src Source) (*EditResult, error) {
	attempted := req.attemptedFields()
	srv, err := m.st.ResolveServer(req.Identifier)
	if err != nil {
		return nil, m.auditEditError(req.Identifier, attempted, err, src)
	}
	if srv == nil {
		notFound := fmt.Errorf("server %q not found", req.Identifier)
		return nil, m.auditEditError(req.Identifier, attempted, notFound, src)
	}
	if req.CheckRevision && srv.Revision != req.ExpectedRevision {
		return nil, m.auditEditError(srv.Name, attempted, &RevisionConflict{Current: srv}, src)
	}
	origName := srv.Name
	supplementAffected := []string(nil)
	// applyEditFields 把非凭据字段套用到一行服务器条目上并返回字段名。
	applyEditFields := func(target *models.Server) ([]string, error) {
		fields := make([]string, 0, len(attempted))
		apply := func(field string, set func()) {
			fields = append(fields, field)
			set()
		}
		if req.Name != nil {
			apply("name", func() { target.Name = *req.Name })
		}
		if req.Host != nil {
			apply("host", func() { target.Host = *req.Host })
		}
		if req.Port != nil {
			apply("port", func() { target.Port = *req.Port })
		}
		if req.User != nil {
			apply("user", func() { target.User = *req.User })
		}
		if req.Shell != nil {
			if _, parseErr := models.ParseShellType(*req.Shell); parseErr != nil {
				return nil, &FieldError{
					Field: "shell",
					CLI:   parseErr.Error(),
					Page:  "shell 类型无效（可选 posix、powershell、cmd）。",
				}
			}
			apply("shell", func() { target.ShellType = models.ShellType(*req.Shell) })
		}
		if req.Description != nil {
			apply("description", func() { target.Description = strings.TrimSpace(*req.Description) })
		}
		if req.Tags != nil {
			apply("tags", func() { target.Tags = *req.Tags })
		}
		if req.Location != nil {
			apply("location", func() { target.Location = strings.TrimSpace(*req.Location) })
		}
		if req.Hardware != nil {
			apply("hardware", func() { target.Hardware = strings.TrimSpace(*req.Hardware) })
		}
		if req.Services != nil {
			apply("services", func() { target.Services = strings.TrimSpace(*req.Services) })
		}
		if req.Role != nil {
			apply("role", func() { target.Role = strings.TrimSpace(*req.Role) })
		}
		if req.Caveats != nil {
			apply("special-handling", func() { target.Caveats = strings.TrimSpace(*req.Caveats) })
		}
		if req.ExposeHost != nil {
			apply("expose-host", func() { target.ExposeHost = *req.ExposeHost })
		}
		return fields, nil
	}
	changed, err := applyEditFields(srv)
	if err != nil {
		return nil, m.auditEditError(origName, attempted, err, src)
	}

	passwordSet := req.Password != nil
	keySet := req.KeyPath != nil || req.KeyContent != nil
	if passwordSet && keySet {
		err := &FieldError{
			Field: "password",
			CLI:   "--password and --key are mutually exclusive; provide exactly one",
			Page:  "密码与私钥只能选择其一。",
		}
		return nil, m.auditEditError(origName, attempted, err, src)
	}
	if req.ClearCredential && (passwordSet || keySet || req.KeyPassphrase != nil) {
		err := &FieldError{
			Field: "clear-credential",
			CLI:   "--clear-credential is mutually exclusive with --password/--key",
			Page:  "清除凭据不能与密码、私钥或私钥口令同时提交。",
		}
		return nil, m.auditEditError(origName, attempted, err, src)
	}
	if req.ClearCredential {
		if err := m.st.ClearServerCredential(srv.ID); err != nil {
			return nil, m.auditEditError(origName, []string{"clear-credential"}, err, src)
		}
		if err := m.writeAudit("server.edit", map[string]any{
			"name": origName, "changed": []string{"clear-credential"},
		}, nil, src); err != nil {
			return nil, err
		}
		cleared, err := m.st.GetServer(srv.ID)
		if err != nil {
			return nil, err
		}
		return &EditResult{Server: cleared, ChangedFields: []string{"clear-credential"}}, nil
	}
	if passwordSet && strings.TrimSpace(*req.Password) == "" {
		err := &FieldError{
			Field: "password",
			CLI:   "--password 不能为空（更换凭据请给新值；清除凭据请用 --clear-credential）",
			Page:  "密码不能为空（更换凭据请给新值；清除凭据请选择清除凭据）。",
		}
		return nil, m.auditEditError(origName, attempted, err, src)
	}
	if keySet && strings.TrimSpace(keyText(req)) == "" {
		err := &FieldError{
			Field: "key",
			CLI:   "--key 不能为空（更换凭据请给新值；清除凭据请用 --clear-credential）",
			Page:  "私钥不能为空（更换凭据请给新值；清除凭据请选择清除凭据）。",
		}
		return nil, m.auditEditError(origName, attempted, err, src)
	}
	if req.SudoPassword != nil && strings.TrimSpace(*req.SudoPassword) == "" {
		err := &FieldError{
			Field: "sudo-password",
			CLI:   "--sudo-password 不能为空（更换凭据请给新值）",
			Page:  "sudo 密码不能为空（更换凭据请给新值）。",
		}
		return nil, m.auditEditError(origName, attempted, err, src)
	}

	// 只补私钥口令：不重新粘贴私钥，直接校验并写入 vault 里已存私钥的口
	// 令。单独使用时只写凭据与标签；与其他字段一起提交时，先写口令并重新
	// 读取目标行，再把其余字段套用到最新行上，两部分同一请求生效。
	if req.KeyPassphrase != nil && *req.KeyPassphrase != "" && !keySet && !passwordSet {
		affected, err := m.applyKeyPassphrase(srv, *req.KeyPassphrase)
		if err != nil {
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) {
				err = &FieldError{Field: "key-passphrase", CLI: err.Error(), Page: err.Error()}
			}
			return nil, m.auditEditError(origName, attempted, err, src)
		}
		changed = append(changed, "key-passphrase")
		affectedNames := serverNames(affected)
		if len(changed) == 1 && req.SudoPassword == nil {
			var saved *models.Server
			for _, item := range affected {
				if item.ID == srv.ID {
					saved = item
				}
			}
			if saved == nil {
				if saved, err = m.st.GetServer(srv.ID); err != nil {
					return nil, err
				}
			}
			if err := m.writeAudit("server.edit", map[string]any{
				"name": origName, "changed": changed,
			}, nil, src); err != nil {
				return nil, err
			}
			return &EditResult{Server: saved, ChangedFields: changed, AffectedServers: affectedNames}, nil
		}
		if srv, err = m.st.GetServer(srv.ID); err != nil {
			return nil, err
		}
		fieldChanges, err := applyEditFields(srv)
		if err != nil {
			return nil, m.auditEditError(origName, attempted, err, src)
		}
		changed = append(changed, fieldChanges...)
		supplementAffected = affectedNames
	}

	var cred, sudoCred *models.Credential
	switch {
	case passwordSet:
		cred = &models.Credential{Type: models.CredPassword, Secret: []byte(*req.Password)}
		changed = append(changed, "password")
	case keySet:
		keyBytes := []byte(keyText(req))
		if req.KeyPath != nil && *req.KeyPath != "" {
			keyBytes, err = os.ReadFile(*req.KeyPath)
			if err != nil {
				return nil, m.auditEditError(origName, attempted, err, src)
			}
		}
		var passphrase []byte
		if req.KeyPassphrase != nil {
			passphrase = []byte(*req.KeyPassphrase)
			changed = append(changed, "key-passphrase")
		}
		cred = &models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: passphrase}
		changed = append(changed, "key")
	}
	if passwordSet || keySet {
		srv.Tags = stripTag(srv.Tags, models.TagNeedsPassphrase)
	}
	if req.SudoPassword != nil {
		sudoCred = &models.Credential{Type: models.CredPassword, Secret: []byte(*req.SudoPassword)}
		changed = append(changed, "sudo-password")
	}
	if err := m.st.UpdateServerWithCredentials(srv, cred, sudoCred); err != nil {
		return nil, m.auditEditError(origName, attempted, err, src)
	}
	sort.Strings(changed)
	if err := m.writeAudit("server.edit", map[string]any{
		"name": origName, "changed": changed,
	}, nil, src); err != nil {
		return nil, err
	}
	saved, err := m.st.GetServer(srv.ID)
	if err != nil {
		return nil, err
	}
	return &EditResult{Server: saved, ChangedFields: changed, AffectedServers: supplementAffected}, nil
}

// attemptedFields 列出这次请求实际提交的字段名（用于失败审计）。
func (r EditRequest) attemptedFields() []string {
	var fields []string
	add := func(set bool, name string) {
		if set {
			fields = append(fields, name)
		}
	}
	add(r.Name != nil, "name")
	add(r.Host != nil, "host")
	add(r.Port != nil, "port")
	add(r.User != nil, "user")
	add(r.Shell != nil, "shell")
	add(r.Description != nil, "description")
	add(r.Location != nil, "location")
	add(r.Hardware != nil, "hardware")
	add(r.Services != nil, "services")
	add(r.Role != nil, "role")
	add(r.Caveats != nil, "special-handling")
	add(r.Tags != nil, "tags")
	add(r.ExposeHost != nil, "expose-host")
	add(r.Password != nil, "password")
	add(r.KeyPath != nil || r.KeyContent != nil, "key")
	add(r.KeyPassphrase != nil, "key-passphrase")
	add(r.SudoPassword != nil, "sudo-password")
	add(r.ClearCredential, "clear-credential")
	sort.Strings(fields)
	return fields
}

func (m *Manager) auditEditError(name string, changed []string, opErr error, src Source) error {
	if opErr == nil {
		return nil
	}
	_ = m.writeAudit("server.edit", map[string]any{"name": name, "changed": changed}, opErr, src)
	return opErr
}

func stripTag(tags []string, tag string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}

// keyText 返回提交的私钥来源文字（文件路径或粘贴内容），用于空值校验。
func keyText(req EditRequest) string {
	if req.KeyContent != nil {
		return *req.KeyContent
	}
	if req.KeyPath != nil {
		return *req.KeyPath
	}
	return ""
}

// FieldError 表示一次服务器提交里某个字段的校验失败。CLI 是命令行措辞（含
// 参数名），Page 是页面措辞（不含命令行参数名）；两个界面各取所需，存储
// 与审计不受影响。
type FieldError struct {
	Field string
	CLI   string
	Page  string
}

func (e *FieldError) Error() string { return e.CLI }
