// Package webuiservers 把服务器管理模块适配成 Web UI 的窄接口，页面处理器
// 因此不接触数据库结构与模块细节。
package webuiservers

import (
	"errors"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/webui"
)

type managerSource struct {
	manager *servermanage.Manager
}

type openerSource struct {
	open func() (*servermanage.Manager, func(), error)
}

// FromManager 把一个已挂在解锁存储上的服务器管理模块适配给 Web UI。
func FromManager(manager *servermanage.Manager) webui.ServerSource {
	return managerSource{manager: manager}
}

// FromOpener 每次页面请求重新打开存储并挂上服务器管理模块，调用结束后关
// 闭。单机回环 Web UI 没有常驻进程持有存储句柄，用这个入口避免长期占用。
func FromOpener(open func() (*servermanage.Manager, func(), error)) webui.ServerSource {
	return openerSource{open: open}
}

// WriterFromManager 返回写操作适配器。
func WriterFromManager(manager *servermanage.Manager) webui.ServerWriter {
	return writerStore{manager: manager}
}

// WriterFromOpener 每次写请求重新打开存储。
func WriterFromOpener(open func() (*servermanage.Manager, func(), error)) webui.ServerWriter {
	return writerOpener{open: open}
}

func (s openerSource) withManager(operation func(*servermanage.Manager) error) error {
	manager, closeStore, err := s.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(manager)
}

func (s openerSource) Servers() ([]webui.ServerRow, error) {
	var rows []webui.ServerRow
	err := s.withManager(func(manager *servermanage.Manager) error {
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

func (s openerSource) ServerList(attentionOnly bool) ([]webui.ServerListEntry, error) {
	var rows []webui.ServerListEntry
	err := s.withManager(func(manager *servermanage.Manager) error {
		var listErr error
		rows, listErr = ListEntries(manager, attentionOnly)
		return listErr
	})
	return rows, err
}

func (s openerSource) ServerDetail(identifier string) (*webui.ServerDetailView, error) {
	var detail *webui.ServerDetailView
	err := s.withManager(func(manager *servermanage.Manager) error {
		var detailErr error
		detail, detailErr = DetailView(manager, identifier)
		return detailErr
	})
	return detail, err
}

func (s managerSource) Servers() ([]webui.ServerRow, error) {
	items, err := s.manager.List(false)
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, webui.ServerRow{
			ID: item.Server.ID, Name: item.Server.Name, User: item.Server.User,
			Host: item.Server.Host, Port: item.Server.Port,
			NeedsAttention: len(item.AttentionReasons) > 0,
		})
	}
	return rows, nil
}

func (s managerSource) ServerList(attentionOnly bool) ([]webui.ServerListEntry, error) {
	return ListEntries(s.manager, attentionOnly)
}

func (s managerSource) ServerDetail(identifier string) (*webui.ServerDetailView, error) {
	return DetailView(s.manager, identifier)
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
			AttentionReasons: servermanage.ReasonLabels(item.AttentionReasons),
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
		AttentionReasons: servermanage.ReasonLabels(detail.AttentionReasons),
		Revision:         detail.Server.Revision,
	}, nil
}

type writerStore struct{ manager *servermanage.Manager }

type writerOpener struct {
	open func() (*servermanage.Manager, func(), error)
}

func (w writerOpener) withManager(operation func(*servermanage.Manager) error) error {
	manager, closeStore, err := w.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(manager)
}

func (w writerStore) EditServer(id string, form webui.ServerEditForm, source webui.ServerWriteSource) (*webui.ServerEditResultView, *webui.ServerEditConflictView, error) {
	result, conflict, err := EditServer(w.manager, id, form, source)
	return result, conflict, err
}

func (w writerOpener) EditServer(id string, form webui.ServerEditForm, source webui.ServerWriteSource) (*webui.ServerEditResultView, *webui.ServerEditConflictView, error) {
	var result *webui.ServerEditResultView
	var conflict *webui.ServerEditConflictView
	err := w.withManager(func(manager *servermanage.Manager) error {
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
		tags := parseTagList(*form.Tags)
		req.Tags = &tags
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

func (w writerStore) DeleteServerPreview(identifier string) (*webui.ServerDeletePreviewView, error) {
	return DeletePreviewView(w.manager, identifier)
}

func (w writerOpener) DeleteServerPreview(identifier string) (*webui.ServerDeletePreviewView, error) {
	var preview *webui.ServerDeletePreviewView
	err := w.withManager(func(manager *servermanage.Manager) error {
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
	case string(models.CredPassword):
		return kindLabel + "（密码）"
	case string(models.CredPrivateKey):
		return kindLabel + "（私钥）"
	default:
		return kindLabel
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

func (w writerStore) DeleteServer(identifier, digest string, source webui.ServerWriteSource) error {
	return deleteServer(w.manager, identifier, digest, source)
}

func (w writerStore) SharedCredentialServers(identifier string) ([]string, error) {
	return w.manager.SharedCredentialServers(identifier)
}

func (w writerOpener) DeleteServer(identifier, digest string, source webui.ServerWriteSource) error {
	var err error
	w.withManager(func(manager *servermanage.Manager) error {
		err = deleteServer(manager, identifier, digest, source)
		return nil
	})
	return err
}

func (w writerOpener) SharedCredentialServers(identifier string) ([]string, error) {
	var names []string
	var err error
	w.withManager(func(manager *servermanage.Manager) error {
		names, err = manager.SharedCredentialServers(identifier)
		return nil
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

func (w writerStore) ImportPlan(path, profile string) (*webui.ServerImportPlanView, error) {
	return ImportPlanView(w.manager, path, profile)
}

func (w writerOpener) ImportPlan(path, profile string) (*webui.ServerImportPlanView, error) {
	var plan *webui.ServerImportPlanView
	err := w.withManager(func(manager *servermanage.Manager) error {
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
	case "existing-name":
		return "同名冲突"
	case "existing-endpoint":
		return "相同主机地址、端口、用户"
	case "wildcard-pattern":
		return "通配符条目"
	case "bad-port":
		return "端口错误"
	case "internal-duplicate":
		return "批内重复主机地址"
	default:
		return reason
	}
}

func (w writerStore) ImportRun(path string, selected []string, profile string, source webui.ServerWriteSource) (*webui.ServerImportRunView, error) {
	return ImportRunView(w.manager, path, selected, profile, source)
}

func (w writerOpener) ImportRun(path string, selected []string, profile string, source webui.ServerWriteSource) (*webui.ServerImportRunView, error) {
	var run *webui.ServerImportRunView
	err := w.withManager(func(manager *servermanage.Manager) error {
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
	switch entry.Status {
	case "imported":
		return "已导入（" + noteLabel(entry.Note) + "）"
	case "failed":
		return "失败：" + entry.Error
	}
	if reason, ok := strings.CutPrefix(entry.Status, "skipped:"); ok {
		return "跳过：" + skipReasonLabel(reason)
	}
	return entry.Status
}

func (w writerStore) CompleteServer(id string, form webui.ServerCompleteForm, source webui.ServerWriteSource) (*webui.ServerCompleteResultView, error) {
	return CompleteServerView(w.manager, id, form, source)
}

func (w writerOpener) CompleteServer(id string, form webui.ServerCompleteForm, source webui.ServerWriteSource) (*webui.ServerCompleteResultView, error) {
	var result *webui.ServerCompleteResultView
	err := w.withManager(func(manager *servermanage.Manager) error {
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

// parseTagList 把逗号分隔的标签输入拆成列表；输入与当前标签串相同时由页面
// 层负责不提交。
func parseTagList(raw string) []string {
	var tags []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			tags = append(tags, item)
		}
	}
	return tags
}
