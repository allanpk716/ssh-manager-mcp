// Package webuisecrets 把机密项管理模块和客户端快照适配成 Web UI 的窄接
// 口。适配层只传递要写入的值，不暴露任何读取值的方法。
package webuisecrets

import (
	"errors"
	"fmt"

	"ssh-manager-mcp/internal/secretmanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

type managerStore struct {
	manager *secretmanage.Manager
}

// FromManager 把一个已挂在解锁存储上的机密项管理模块适配给 Web UI。
func FromManager(manager *secretmanage.Manager) webui.SecretSource {
	return managerStore{manager: manager}
}

// WriterFromManager 返回机密项写操作适配器。
func WriterFromManager(manager *secretmanage.Manager) webui.SecretWriter {
	return managerStore{manager: manager}
}

// FromOpener 每次请求重新打开存储；单机回环 Web UI 用它避免长期持有存储
// 句柄。
func FromOpener(open func() (*secretmanage.Manager, func(), error)) webui.SecretSource {
	return openerStore{open: open}
}

// WriterFromOpener 每次写请求重新打开存储。
func WriterFromOpener(open func() (*secretmanage.Manager, func(), error)) webui.SecretWriter {
	return writerOpenerStore{open: open}
}

// SnapshotSource 把客户端本地加密快照里的机密项可见视图适配给 Web UI。
// loader 由命令行按当前客户端实例提供；这里不读取任何机密项值。
func SnapshotSource(load func(instance string) (*store.Snapshot, error)) webui.SecretSource {
	return snapshotSource{load: load}
}

type snapshotSource struct {
	load func(instance string) (*store.Snapshot, error)
}

func (s snapshotSource) ListSecrets(instance, query, server string) ([]webui.SecretRow, error) {
	snapshot, err := s.load(instance)
	if err != nil {
		return nil, err
	}
	serverName := server
	if serverName != "" {
		resolved := store.ResolveServerInSnapshot(snapshot, serverName)
		if resolved == nil {
			return nil, fmt.Errorf("服务器条目 %q 不存在", serverName)
		}
		serverName = resolved.Name
	}
	views := store.FilterSecretsByServer(store.SecretViewsFromSnapshot(snapshot), serverName)
	return secretRows(store.FilterSecretsByQuery(views, query)), nil
}

func (s managerStore) ListSecrets(_, query, server string) ([]webui.SecretRow, error) {
	views, err := s.manager.List(secretmanage.ListFilter{Query: query, Server: server})
	if err != nil {
		return nil, err
	}
	return secretRows(views), nil
}

func (s managerStore) AddSecret(name, note, environmentName string, value []byte, source webui.SecretWriteSource) error {
	return secretFormError(s.manager.AddSecret(
		name, note, environmentName, value, secretmanage.WebUISource(source.RemoteAddress),
	))
}

func (s managerStore) SetSecretValue(identifier string, value []byte, source webui.SecretWriteSource) error {
	view, err := s.secretViewByIDOrName(identifier)
	if err != nil {
		return err
	}
	return secretFormError(s.manager.SetSecretValue(view.Name, value, secretmanage.WebUISource(source.RemoteAddress)))
}

func (s managerStore) EditSecretMetadata(identifier, note, environmentName string, source webui.SecretWriteSource) error {
	view, err := s.secretViewByIDOrName(identifier)
	if err != nil {
		return err
	}
	edit := secretmanage.MetadataEdit{}
	if view.Note != note {
		edit.Note = &note
	}
	if view.EnvironmentName != environmentName {
		edit.EnvironmentName = &environmentName
	}
	if edit.Note == nil && edit.EnvironmentName == nil {
		return webui.ErrSecretMetadataNoChange
	}
	return secretFormError(s.manager.EditMetadata(view.Name, edit, secretmanage.WebUISource(source.RemoteAddress)))
}

func (s managerStore) DeleteSecret(identifier string, source webui.SecretWriteSource) error {
	view, err := s.secretViewByIDOrName(identifier)
	if err != nil {
		return err
	}
	return s.manager.DeleteSecret(view.Name, secretmanage.WebUISource(source.RemoteAddress))
}

func (s managerStore) PreviewAuthorization(target []webui.SecretAuthorizationCell) (webui.SecretAuthorizationPreview, error) {
	preview, err := s.manager.PreviewAuthorizationChanges(authorizationCells(target))
	if err != nil {
		return webui.SecretAuthorizationPreview{}, err
	}
	return previewView(preview), nil
}

func (s managerStore) ApplyAuthorization(target []webui.SecretAuthorizationCell, summary string, source webui.SecretWriteSource) (webui.SecretApplyResult, *webui.SecretAuthorizationConflict, error) {
	result, err := s.manager.ApplyAuthorizationChanges(
		authorizationCells(target), summary, secretmanage.WebUISource(source.RemoteAddress),
	)
	var conflict *secretmanage.AuthorizationConflictError
	if errors.As(err, &conflict) {
		return webui.SecretApplyResult{}, &webui.SecretAuthorizationConflict{
			Latest: previewView(conflict.Latest),
		}, nil
	}
	if err != nil {
		return webui.SecretApplyResult{}, nil, err
	}
	return webui.SecretApplyResult{
		Preview: previewView(result.Preview), Applied: result.Applied,
	}, nil, nil
}

func (s managerStore) secretViewByIDOrName(identifier string) (store.SecretView, error) {
	views, err := s.manager.List(secretmanage.ListFilter{})
	if err != nil {
		return store.SecretView{}, err
	}
	for _, view := range views {
		if view.ID == identifier || view.Name == identifier {
			return view, nil
		}
	}
	return store.SecretView{}, errSecretNotFound(identifier)
}

type openerStore struct {
	open func() (*secretmanage.Manager, func(), error)
}

func (s openerStore) ListSecrets(instance, query, server string) ([]webui.SecretRow, error) {
	manager, closeStore, err := s.open()
	if err != nil {
		return nil, err
	}
	defer closeStore()
	views, err := manager.List(secretmanage.ListFilter{Query: query, Server: server})
	if err != nil {
		return nil, err
	}
	return secretRows(views), nil
}

type writerOpenerStore struct {
	open func() (*secretmanage.Manager, func(), error)
}

func (s writerOpenerStore) withManager(operation func(*secretmanage.Manager) error) error {
	manager, closeStore, err := s.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(manager)
}

func (s writerOpenerStore) AddSecret(name, note, environmentName string, value []byte, source webui.SecretWriteSource) error {
	return s.withManager(func(manager *secretmanage.Manager) error {
		return managerStore{manager: manager}.AddSecret(name, note, environmentName, value, source)
	})
}

func (s writerOpenerStore) SetSecretValue(identifier string, value []byte, source webui.SecretWriteSource) error {
	return s.withManager(func(manager *secretmanage.Manager) error {
		return managerStore{manager: manager}.SetSecretValue(identifier, value, source)
	})
}

func (s writerOpenerStore) EditSecretMetadata(identifier, note, environmentName string, source webui.SecretWriteSource) error {
	return s.withManager(func(manager *secretmanage.Manager) error {
		return managerStore{manager: manager}.EditSecretMetadata(identifier, note, environmentName, source)
	})
}

func (s writerOpenerStore) DeleteSecret(identifier string, source webui.SecretWriteSource) error {
	return s.withManager(func(manager *secretmanage.Manager) error {
		return managerStore{manager: manager}.DeleteSecret(identifier, source)
	})
}

func (s writerOpenerStore) PreviewAuthorization(target []webui.SecretAuthorizationCell) (webui.SecretAuthorizationPreview, error) {
	var preview webui.SecretAuthorizationPreview
	err := s.withManager(func(manager *secretmanage.Manager) error {
		var previewErr error
		preview, previewErr = managerStore{manager: manager}.PreviewAuthorization(target)
		return previewErr
	})
	return preview, err
}

func (s writerOpenerStore) ApplyAuthorization(target []webui.SecretAuthorizationCell, summary string, source webui.SecretWriteSource) (webui.SecretApplyResult, *webui.SecretAuthorizationConflict, error) {
	var result webui.SecretApplyResult
	var conflict *webui.SecretAuthorizationConflict
	err := s.withManager(func(manager *secretmanage.Manager) error {
		var applyErr error
		result, conflict, applyErr = managerStore{manager: manager}.ApplyAuthorization(target, summary, source)
		return applyErr
	})
	return result, conflict, err
}

func secretRows(views []store.SecretView) []webui.SecretRow {
	rows := make([]webui.SecretRow, 0, len(views))
	for _, view := range views {
		rows = append(rows, webui.SecretRow{
			ID: view.ID, Name: view.Name, Note: view.Note,
			EnvironmentName: view.EnvironmentName, AllowedServers: view.AllowedServers,
			CreatedAt: view.CreatedAt, ValueUpdatedAt: view.ValueUpdatedAt,
		})
	}
	return rows
}

func authorizationCells(target []webui.SecretAuthorizationCell) []secretmanage.AuthorizationCell {
	cells := make([]secretmanage.AuthorizationCell, 0, len(target))
	for _, cell := range target {
		cells = append(cells, secretmanage.AuthorizationCell{
			SecretID: cell.SecretID, ServerID: cell.ServerID, Allow: cell.Allow,
		})
	}
	return cells
}

func previewView(preview secretmanage.AuthorizationPreview) webui.SecretAuthorizationPreview {
	out := webui.SecretAuthorizationPreview{
		Summary: preview.Summary, TotalAdded: preview.TotalAdded, TotalRemoved: preview.TotalRemoved,
	}
	for _, change := range preview.Changes {
		view := webui.SecretAuthorizationChange{
			SecretID: change.SecretID, SecretName: change.SecretName,
			Added: serverRefs(change.Added), Removed: serverRefs(change.Removed),
		}
		out.Changes = append(out.Changes, view)
	}
	return out
}

func serverRefs(servers []secretmanage.ServerRef) []webui.SecretAuthorizationServer {
	refs := make([]webui.SecretAuthorizationServer, 0, len(servers))
	for _, server := range servers {
		refs = append(refs, webui.SecretAuthorizationServer{ID: server.ID, Name: server.Name})
	}
	return refs
}

func errSecretNotFound(identifier string) error {
	return &webui.SecretNotFoundError{Identifier: identifier}
}

// secretFormError 把存储层的机密项输入校验错误标成可直接展示的页面错误；
// 其余错误原样返回，由页面改用通用文案，避免透出内部细节。
func secretFormError(err error) error {
	if store.IsSecretValidationError(err) {
		return &webui.SecretFormError{Message: err.Error()}
	}
	return err
}
