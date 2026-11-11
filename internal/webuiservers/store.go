// Package webuiservers 把服务器管理模块适配成 Web UI 的窄接口，页面处理器
// 因此不接触数据库结构与模块细节。
package webuiservers

import (
	"errors"
	"strconv"

	"ssh-manager-mcp/internal/importer"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/webui"
)

// managerOpener 返回一个挂在解锁存储上的服务器管理模块和关闭函数。固定实
// 例与按请求重开存储的来源都走这一个形状，适配器只有一套。
type managerOpener func() (*servermanage.Manager, func(), error)

// fixedManagerOpener 把固定实例包装成与按请求重开相同的形状。
func fixedManagerOpener(manager *servermanage.Manager) managerOpener {
	return func() (*servermanage.Manager, func(), error) {
		return manager, func() {}, nil
	}
}

// FromManager 把一个已挂在解锁存储上的服务器管理模块适配给 Web UI。
func FromManager(manager *servermanage.Manager) webui.ServerSource {
	return sourceAdapter{open: fixedManagerOpener(manager)}
}

// FromOpener 每次页面请求重新打开存储并挂上服务器管理模块，调用结束后关
// 闭。单机回环 Web UI 没有常驻进程持有存储句柄，用这个入口避免长期占用。
func FromOpener(open managerOpener) webui.ServerSource {
	return sourceAdapter{open: open}
}

// WriterFromManager 返回写操作适配器。
func WriterFromManager(manager *servermanage.Manager) webui.ServerWriter {
	return writerAdapter{open: fixedManagerOpener(manager)}
}

// WriterFromOpener 每次写请求重新打开存储。
func WriterFromOpener(open managerOpener) webui.ServerWriter {
	return writerAdapter{open: open}
}

// withManager 打开服务器管理模块执行一次操作，结束后关闭存储。固定实例的
// 关闭函数是空操作。
func withManager(open managerOpener, operation func(*servermanage.Manager) error) error {
	manager, closeStore, err := open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(manager)
}

// sourceAdapter 是 Web UI 读取服务器数据的适配器。
type sourceAdapter struct {
	open managerOpener
}

func (s sourceAdapter) Servers(_ string) ([]webui.ServerRow, error) {
	var rows []webui.ServerRow
	err := withManager(s.open, func(manager *servermanage.Manager) error {
		items, err := manager.List(false)
		if err != nil {
			return err
		}
		for _, item := range items {
			rows = append(rows, webui.ServerRow{
				ID: item.Server.ID, Name: item.Server.Name, User: item.Server.User,
				Host: item.Server.Host, Port: item.Server.Port,
				NeedsAttention: len(item.AttentionReasons) > 0,
			})
		}
		return nil
	})
	return rows, err
}

func (s sourceAdapter) ServerList(_ string, attentionOnly bool) ([]webui.ServerListEntry, error) {
	var rows []webui.ServerListEntry
	err := withManager(s.open, func(manager *servermanage.Manager) error {
		var listErr error
		rows, listErr = ListEntries(manager, attentionOnly)
		return listErr
	})
	return rows, err
}

func (s sourceAdapter) ServerDetail(_ string, identifier string) (*webui.ServerDetailView, error) {
	var detail *webui.ServerDetailView
	err := withManager(s.open, func(manager *servermanage.Manager) error {
		var detailErr error
		detail, detailErr = DetailView(manager, identifier)
		return detailErr
	})
	return detail, err
}

// ListEntries 把模块列表换成 Web UI 展示行。
func ListEntries(manager *servermanage.Manager, attentionOnly bool) ([]webui.ServerListEntry, error) {
	items, err := manager.List(attentionOnly)
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerListEntry, 0, len(items))
	for _, item := range items {
		rows = append(rows, webui.ServerListEntry{
			ID: item.Server.ID, Name: item.Server.Name, User: item.Server.User,
			Host: item.Server.Host, Port: item.Server.Port,
			AttentionReasons: item.AttentionReasons,
		})
	}
	return rows, nil
}

// DetailView 把模块详情换成不含凭据内容的页面数据。
func DetailView(manager *servermanage.Manager, identifier string) (*webui.ServerDetailView, error) {
	detail, err := manager.Detail(identifier)
	if err != nil || detail == nil {
		return nil, err
	}
	return &webui.ServerDetailView{
		ID: detail.Server.ID, Name: detail.Server.Name, User: detail.Server.User,
		Host: detail.Server.Host, Port: detail.Server.Port,
		Shell:          string(models.NormalizeShellType(string(detail.Server.ShellType))),
		CredentialType: string(detail.CredentialType), HasKeyPassphrase: detail.HasKeyPassphrase,
		HasSudoPassword: detail.HasSudoPassword, Description: detail.Server.Description,
		Location: detail.Server.Location, Hardware: detail.Server.Hardware,
		Services: detail.Server.Services, Role: detail.Server.Role, Caveats: detail.Server.Caveats,
		Tags: detail.Server.Tags, ExposeHost: detail.Server.ExposeHost,
		DeviceName: detail.DeviceName, HasDevice: detail.HasDevice,
		AttentionReasons: detail.AttentionReasons,
		Revision:         detail.Server.Revision,
	}, nil
}

// writerAdapter 是 Web UI 修改服务器数据的适配器。
type writerAdapter struct {
	open managerOpener
}

func (w writerAdapter) EditServer(id string, form webui.ServerEditForm, source webui.ServerWriteSource) (*webui.ServerEditResultView, *webui.ServerEditConflictView, error) {
	var result *webui.ServerEditResultView
	var conflict *webui.ServerEditConflictView
	err := withManager(w.open, func(manager *servermanage.Manager) error {
		var editErr error
		result, conflict, editErr = EditServer(manager, id, form, source)
		return editErr
	})
	return result, conflict, err
}

// EditServer 执行一次编辑并把结果换成页面数据；修订号冲突以冲突视图返回。
func EditServer(manager *servermanage.Manager, id string, form webui.ServerEditForm, source webui.ServerWriteSource) (*webui.ServerEditResultView, *webui.ServerEditConflictView, error) {
	req := servermanage.EditRequest{
		Identifier: id, ExpectedRevision: form.ExpectedRevision, CheckRevision: true,
		Name: form.Name, Host: form.Host, Port: form.Port, User: form.User, Shell: form.Shell,
		Description: form.Description, Location: form.Location, Hardware: form.Hardware,
		Services: form.Services, Role: form.Role, Caveats: form.Caveats,
		ExposeHost: form.ExposeHost, SudoPassword: stringPointer(form.SudoPassword),
	}
	switch form.CredentialAction {
	case "password":
		req.Password = stringPointer(form.Password)
	case "key":
		req.KeyContent = stringPointer(form.KeyContent)
		req.KeyPassphrase = stringPointer(form.KeyPassphrase)
	case "key-passphrase":
		req.KeyPassphrase = stringPointer(form.KeyPassphrase)
	case "clear":
		req.ClearCredential = true
	}
	if req.SudoPassword != nil && *req.SudoPassword == "" {
		req.SudoPassword = nil
	}
	if form.Tags != nil {
		req.Tags = &form.Tags
	}
	result, err := manager.Edit(req, servermanage.WebUISource(source.RemoteAddress))
	if err != nil {
		var conflict *servermanage.RevisionConflict
		if asConflict, ok := err.(*servermanage.RevisionConflict); ok {
			conflict = asConflict
		}
		if conflict != nil {
			current, viewErr := DetailView(manager, id)
			if viewErr != nil {
				return nil, nil, viewErr
			}
			return nil, &webui.ServerEditConflictView{Current: current}, nil
		}
		var fieldErr *servermanage.FieldError
		if errors.As(err, &fieldErr) {
			return nil, nil, &webui.ServerFormError{Message: fieldErr.Page}
		}
		return nil, nil, err
	}
	detail, err := DetailView(manager, result.Server.ID)
	if err != nil {
		return nil, nil, err
	}
	return &webui.ServerEditResultView{Detail: detail, AffectedServers: result.AffectedServers}, nil, nil
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (w writerAdapter) DeleteServerPreview(identifier string) (*webui.ServerDeletePreviewView, error) {
	var preview *webui.ServerDeletePreviewView
	err := withManager(w.open, func(manager *servermanage.Manager) error {
		var previewErr error
		preview, previewErr = DeletePreviewView(manager, identifier)
		return previewErr
	})
	return preview, err
}

// DeletePreviewView 把删除预览换成页面数据。
func DeletePreviewView(manager *servermanage.Manager, identifier string) (*webui.ServerDeletePreviewView, error) {
	preview, err := manager.DeletePreview(identifier)
	if err != nil || preview == nil {
		return nil, err
	}
	return deletePreviewView(preview), nil
}

// deletePreviewView 把服务器管理模块的删除预览换成页面数据。
func deletePreviewView(preview *servermanage.DeletePreview) *webui.ServerDeletePreviewView {
	view := &webui.ServerDeletePreviewView{
		ServerID: preview.Server.ID, ServerName: preview.Server.Name,
		HasDevice: preview.HasDevice, DeviceName: preview.DeviceName,
		AnchorCount: len(preview.Anchors), ProfileNames: preview.ProfileNames,
		SecretNames: preview.SecretNames, Digest: preview.Digest,
	}
	for _, record := range preview.DeliveryRecords {
		view.DeliveryRecords = append(view.DeliveryRecords, webui.DeliveryRecordImpactView{
			ServerName: record.ServerName, Path: record.Path, SecretNames: record.SecretNames,
		})
	}
	for _, impact := range preview.Credentials {
		label := credentialLabel(impact.Kind, impact.Type)
		if impact.Deleted {
			view.DeletedCredentials = append(view.DeletedCredentials, label)
		} else {
			view.KeptCredentials = append(view.KeptCredentials,
				label+"（仍被 "+itoa(impact.References)+" 台服务器引用）")
		}
	}
	return view
}

func credentialLabel(kind, typ string) string {
	kindLabel := "登录凭据"
	if kind == "sudo" {
		kindLabel = "sudo 凭据"
	}
	switch typ {
	case string(models.CredPassword), string(models.CredPrivateKey):
		return kindLabel + "（" + webui.CredentialTypeLabel(typ) + "）"
	default:
		return kindLabel
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

func (w writerAdapter) DeleteServer(identifier, digest string, source webui.ServerWriteSource) error {
	return withManager(w.open, func(manager *servermanage.Manager) error {
		return deleteServer(manager, identifier, digest, source)
	})
}

func (w writerAdapter) SharedCredentialServers(identifier string) ([]string, error) {
	var names []string
	err := withManager(w.open, func(manager *servermanage.Manager) error {
		var nameErr error
		names, nameErr = manager.SharedCredentialServers(identifier)
		return nameErr
	})
	return names, err
}

// deleteServer 执行删除并把服务器管理模块的错误换算成页面错误类型：预览
// 变化携带最新预览，条目不存在映射为 404，其余原样返回。
func deleteServer(manager *servermanage.Manager, identifier, digest string, source webui.ServerWriteSource) error {
	preview, err := manager.Delete(identifier, digest, servermanage.WebUISource(source.RemoteAddress))
	if err != nil {
		if errors.Is(err, servermanage.ErrDeletePreviewChanged) && preview != nil {
			return &webui.ServerDeletePreviewChanged{Preview: deletePreviewView(preview)}
		}
		return err
	}
	if preview == nil {
		return webui.ErrServerNotFound
	}
	return nil
}

func (w writerAdapter) ImportPlan(path, profile string) (*webui.ServerImportPlanView, error) {
	var plan *webui.ServerImportPlanView
	err := withManager(w.open, func(manager *servermanage.Manager) error {
		var planErr error
		plan, planErr = ImportPlanView(manager, path, profile)
		return planErr
	})
	return plan, err
}

// ImportPlanView 把导入计划换成页面数据；profile 选项来自 vault 现有
// profile。
func ImportPlanView(manager *servermanage.Manager, path, profile string) (*webui.ServerImportPlanView, error) {
	plan, err := manager.PlanImport(path, profile)
	if err != nil {
		return nil, err
	}
	view := &webui.ServerImportPlanView{
		Path: plan.Path, MatchWarning: plan.MatchWarning, ProfileID: plan.ProfileID,
	}
	profiles, err := manager.Profiles()
	if err != nil {
		return nil, err
	}
	for _, profile := range profiles {
		view.Profiles = append(view.Profiles, webui.ServerProfileOption{ID: profile.ID, Name: profile.Name})
	}
	for _, cand := range plan.Candidates {
		view.Candidates = append(view.Candidates, webui.ServerImportCandidateView{
			Name: cand.Name, User: cand.User, Host: cand.Host, Port: cand.Port, Note: noteLabel(cand.Note),
		})
	}
	for _, skipped := range plan.VaultSkipped {
		view.VaultSkipped = append(view.VaultSkipped, webui.ServerImportSkipView{Name: skipped.Name, Reason: skipReasonLabel(skipped.Reason)})
	}
	for _, skipped := range plan.ParseSkipped {
		view.ParseSkipped = append(view.ParseSkipped, webui.ServerImportSkipView{Name: skipped.Name, Reason: skipReasonLabel(skipped.Reason)})
	}
	return view, nil
}

func noteLabel(note string) string {
	switch note {
	case servermanage.ImportNoteKey:
		return "私钥"
	case servermanage.ImportNoteNoCredential:
		return "无凭据"
	case servermanage.ImportNoteNeedsPassphrase:
		return "待补私钥口令"
	default:
		return note
	}
}

func skipReasonLabel(reason string) string {
	switch reason {
	case importer.SkipExistingName:
		return "同名冲突"
	case importer.SkipExistingEndpoint:
		return "相同主机地址、端口、用户"
	case importer.SkipWildcardPattern:
		return "通配符条目"
	case importer.SkipBadPort:
		return "端口错误"
	case importer.SkipInternalDuplicate:
		return "批内重复主机地址"
	case servermanage.ImportSkipUnselected:
		return "未勾选执行"
	default:
		return reason
	}
}

func (w writerAdapter) ImportRun(path string, selected []string, profile string, source webui.ServerWriteSource) (*webui.ServerImportRunView, error) {
	var run *webui.ServerImportRunView
	err := withManager(w.open, func(manager *servermanage.Manager) error {
		var runErr error
		run, runErr = ImportRunView(manager, path, selected, profile, source)
		return runErr
	})
	return run, err
}

// ImportRunView 执行导入并换成页面数据。
func ImportRunView(manager *servermanage.Manager, path string, selected []string, profile string, source webui.ServerWriteSource) (*webui.ServerImportRunView, error) {
	run, err := manager.RunImport(path, selected, profile, servermanage.WebUISource(source.RemoteAddress))
	if err != nil && run == nil {
		return nil, err
	}
	view := &webui.ServerImportRunView{
		Imported: run.Imported, Skipped: run.Skipped, Failed: run.Failed, Pending: run.Pending,
		ImportedIDs: run.ImportedServerIDs,
	}
	for _, entry := range run.Entries {
		view.Entries = append(view.Entries, webui.ServerImportEntryView{Name: entry.Name, Status: entryStatusLabel(entry)})
	}
	if err != nil {
		return view, err
	}
	return view, nil
}

func entryStatusLabel(entry servermanage.ImportEntryStatus) string {
	switch entry.Outcome {
	case servermanage.ImportEntryImported:
		return "已导入（" + noteLabel(entry.Note) + "）"
	case servermanage.ImportEntryFailed:
		return "失败：" + entry.Error
	case servermanage.ImportEntrySkipped:
		return "跳过：" + skipReasonLabel(entry.SkipReason)
	}
	return entry.Outcome
}

func (w writerAdapter) CompleteServer(id string, form webui.ServerCompleteForm, source webui.ServerWriteSource) (*webui.ServerCompleteResultView, error) {
	var result *webui.ServerCompleteResultView
	err := withManager(w.open, func(manager *servermanage.Manager) error {
		var completeErr error
		result, completeErr = CompleteServerView(manager, id, form, source)
		return completeErr
	})
	return result, err
}

// CompleteServerView 保存补全表单并换成页面数据。
func CompleteServerView(manager *servermanage.Manager, id string, form webui.ServerCompleteForm, source webui.ServerWriteSource) (*webui.ServerCompleteResultView, error) {
	req := servermanage.CompleteRequest{
		Identifier:   id,
		SudoPassword: form.SudoPassword, Password: form.Password, KeyPassphrase: form.KeyPassphrase,
	}
	// 服务器元数据字段按提交值写入：表单预填当前值，空输入就是清空这个字
	// 段；与当前值相同时由服务器管理模块跳过写入。sudo 密码与条件凭据仍
	// 然是空值表示不变。
	req.Description = &form.Description
	req.Location = &form.Location
	req.Hardware = &form.Hardware
	req.Services = &form.Services
	req.Role = &form.Role
	req.Caveats = &form.Caveats
	result, err := manager.Complete(req, servermanage.WebUISource(source.RemoteAddress))
	if err != nil {
		var fieldErr *servermanage.FieldError
		if errors.As(err, &fieldErr) {
			return nil, &webui.ServerFormError{Message: fieldErr.Page}
		}
		return nil, err
	}
	detail, err := DetailView(manager, result.Server.ID)
	if err != nil {
		return nil, err
	}
	return &webui.ServerCompleteResultView{Detail: detail, AffectedServers: result.AffectedServers}, nil
}
