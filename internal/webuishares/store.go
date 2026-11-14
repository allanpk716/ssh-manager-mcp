// Package webuishares 把共享存储登记与客户端快照适配成 Web UI 的窄接口。
// 页面处理器因此不接触数据库结构；写入一律走存储层既有的领域函数。
package webuishares

import (
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

type storeSource struct {
	open func() (*store.Store, func(), error)
}

type writerSource struct {
	open func() (*store.Store, func(), error)
}

// SnapshotSource 把客户端本地加密快照里的共享存储无值视图适配给 Web UI。
// loader 由命令行按当前客户端实例提供；这里不读取任何机密项值。
func SnapshotSource(load func(instance string) (*store.Snapshot, error)) webui.ShareSource {
	return snapshotSource{load: load}
}

// FromClientSnapshot 与 SnapshotSource 相同；命名与服务器页、机密项页的
// 客户端适配入口保持一致。
func FromClientSnapshot(load func(instance string) (*store.Snapshot, error)) webui.ShareSource {
	return SnapshotSource(load)
}

type snapshotSource struct {
	load func(instance string) (*store.Snapshot, error)
}

func (s snapshotSource) ListShares(instance, device, server string) ([]webui.ShareRow, error) {
	snapshot, err := s.load(instance)
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ShareRow, 0, len(snapshot.SharedStorages))
	for _, share := range snapshot.SharedStorages {
		view := snapshotShareRow(snapshot, &share)
		if device != "" && !strings.EqualFold(view.DeviceName, strings.TrimSpace(device)) {
			continue
		}
		if server != "" && !shareRowHasServer(view, server) {
			continue
		}
		rows = append(rows, view)
	}
	return rows, nil
}

func (s snapshotSource) ShareDetail(instance, identifier string) (*webui.ShareRow, error) {
	rows, err := s.ListShares(instance, "", "")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.ID == identifier || strings.EqualFold(row.Name, strings.TrimSpace(identifier)) {
			return &row, nil
		}
	}
	return nil, &webui.ShareNotFoundError{Identifier: identifier}
}

// snapshotShareRow 把快照里的一处共享存储整理成展示行。可挂载服务器与
// 挂载密码机密项名字都按快照携带的关联推导；推导不出名字时显示占位说
// 明，绝不读取值。
func snapshotShareRow(snapshot *store.Snapshot, share *store.SnapshotSharedStorage) webui.ShareRow {
	row := webui.ShareRow{
		ID: share.ID, Name: share.Name, DeviceName: share.DeviceName,
		Drive: share.Drive, Version: share.Version,
		MountableServers: snapshotMountableServers(snapshot, share),
		MountUser:        share.MountUser, Note: share.Note, ChangeNote: share.ChangeNote,
	}
	row.MountableCount = len(row.MountableServers)
	row.PasswordSecret = "（本地快照未包含该机密项名字）"
	row.PasswordSecretID = share.PasswordSecretID
	for _, secret := range snapshot.Secrets {
		if secret.ID == share.PasswordSecretID {
			row.PasswordSecret = secret.Name
			break
		}
	}
	for _, directory := range share.Directories {
		row.Directories = append(row.Directories, webui.ShareDirectoryView{
			Purpose: directory.Purpose, Path: directory.Path,
		})
	}
	return row
}

func snapshotMountableServers(snapshot *store.Snapshot, share *store.SnapshotSharedStorage) []webui.ShareServerView {
	servers := make(map[string]store.SnapshotServer, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		servers[server.ID] = server
	}
	views := make([]webui.ShareServerView, 0)
	for _, allow := range snapshot.SecretAllows {
		if allow.SecretID != share.PasswordSecretID {
			continue
		}
		server, ok := servers[allow.ServerID]
		if !ok {
			continue
		}
		shellType := models.NormalizeShellType(server.ShellType)
		views = append(views, webui.ShareServerView{
			ID: server.ID, Name: server.Name, DeviceName: snapshotDeviceName(snapshot, server.DeviceID),
			ShellType:  string(shellType),
			MountPoint: store.SharedStorageMountPoint(share.Drive, share.Name, shellType),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

func snapshotDeviceName(snapshot *store.Snapshot, deviceID string) string {
	for _, device := range snapshot.Devices {
		if device.ID == deviceID {
			return device.Name
		}
	}
	return ""
}

func shareRowHasServer(row webui.ShareRow, server string) bool {
	for _, candidate := range row.MountableServers {
		if candidate.ID == server || strings.EqualFold(candidate.Name, strings.TrimSpace(server)) {
			return true
		}
	}
	return false
}

// FromStore 把一个按请求打开的共享存储读取来源适配给 Web UI。
func FromStore(open func() (*store.Store, func(), error)) webui.ShareSource {
	return storeSource{open: open}
}

// WriterFromStore 把存储层的共享存储写函数适配给 Web UI。
func WriterFromStore(open func() (*store.Store, func(), error)) webui.ShareWriter {
	return writerSource{open: open}
}

func withStore(open func() (*store.Store, func(), error), operation func(st *store.Store) error) error {
	st, closeStore, err := open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(st)
}

func (s storeSource) withStore(operation func(st *store.Store) error) error {
	st, closeStore, err := s.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(st)
}

func (s storeSource) ListShares(_, device, server string) ([]webui.ShareRow, error) {
	var rows []webui.ShareRow
	err := s.withStore(func(st *store.Store) error {
		shares, listErr := st.ListSharedStorages(store.SharedStorageFilter{Device: device, Server: server})
		if listErr != nil {
			return listErr
		}
		rows = shareRows(shares)
		return nil
	})
	return rows, err
}

func (s storeSource) ShareDetail(_, identifier string) (*webui.ShareRow, error) {
	var row *webui.ShareRow
	err := s.withStore(func(st *store.Store) error {
		shares, listErr := st.ListSharedStorages(store.SharedStorageFilter{})
		if listErr != nil {
			return listErr
		}
		for _, share := range shares {
			if share.ID == identifier || strings.EqualFold(share.Name, strings.TrimSpace(identifier)) {
				view := shareRow(share)
				row = &view
				return nil
			}
		}
		return &webui.ShareNotFoundError{Identifier: identifier}
	})
	return row, err
}

func (w writerSource) AddShare(form webui.ShareAddForm, source webui.ShareWriteSource) (*webui.ShareRow, error) {
	var row *webui.ShareRow
	err := w.withStore(func(st *store.Store) error {
		directories, parseErr := store.ParseSharedStorageDirectoryLines(form.Directories)
		if parseErr != nil {
			return shareFormError(parseErr)
		}
		share, addErr := st.AddSharedStorage(store.SharedStorageInput{
			Name: form.Name, Device: form.Device, User: form.User,
			PasswordSecret: form.PasswordSecret, Drive: form.Drive,
			Directories: directories, Note: form.Note,
		}, store.WebUISource(source.RemoteAddress))
		if addErr != nil {
			return shareFormError(addErr)
		}
		view := shareRow(share)
		row = &view
		return nil
	})
	return row, err
}

func (w writerSource) EditShare(identifier string, form webui.ShareEditForm, source webui.ShareWriteSource) (*webui.ShareRow, error) {
	var row *webui.ShareRow
	err := w.withStore(func(st *store.Store) error {
		name, err := shareNameForWrite(st, identifier)
		if err != nil {
			return err
		}
		share, editErr := st.EditSharedStorage(name, store.SharedStorageChanges{
			Name: form.Name, Device: form.Device, User: form.User,
			PasswordSecret: form.PasswordSecret, Drive: form.Drive,
			Directories: form.Directories, Note: form.Note, ChangeNote: form.ChangeNote,
		}, store.WebUISource(source.RemoteAddress))
		if editErr != nil {
			return shareFormError(editErr)
		}
		view := shareRow(share)
		row = &view
		return nil
	})
	return row, err
}

func (w writerSource) DeleteShare(identifier string, source webui.ShareWriteSource) error {
	return w.withStore(func(st *store.Store) error {
		name, err := shareNameForWrite(st, identifier)
		if err != nil {
			return err
		}
		_, err = st.DeleteSharedStorage(name, store.WebUISource(source.RemoteAddress))
		return shareFormError(err)
	})
}

// shareNameForWrite 把页面路径里的标识符或共享名解析成存储层写入口需要
// 的共享名；不存在时返回页面的共享存储不存在错误。
func shareNameForWrite(st *store.Store, identifier string) (string, error) {
	shares, err := st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return "", err
	}
	for _, share := range shares {
		if share.ID == identifier || strings.EqualFold(share.Name, strings.TrimSpace(identifier)) {
			return share.Name, nil
		}
	}
	return "", &webui.ShareNotFoundError{Identifier: identifier}
}

func (w writerSource) withStore(operation func(st *store.Store) error) error {
	return withStore(w.open, operation)
}

// shareFormError 把存储层与目录约定解析的输入校验错误标成可直接展示的页
// 面错误；其余错误原样返回，由页面改用通用文案。
func shareFormError(err error) error {
	if err == nil {
		return nil
	}
	if store.IsSharedStorageValidationError(err) {
		return &webui.ShareFormError{Message: err.Error()}
	}
	return err
}

func shareRows(shares []*models.SharedStorage) []webui.ShareRow {
	rows := make([]webui.ShareRow, 0, len(shares))
	for _, share := range shares {
		rows = append(rows, shareRow(share))
	}
	return rows
}

func shareRow(share *models.SharedStorage) webui.ShareRow {
	row := webui.ShareRow{
		ID: share.ID, Name: share.Name, DeviceName: share.DeviceName,
		Drive: share.Drive, Version: share.Version,
		MountableServers: serverViews(share.MountableServers),
		MountableCount:   len(share.MountableServers),
		PasswordSecret:   share.PasswordSecretName, PasswordSecretID: share.PasswordSecretID,
		MountUser: share.MountUser,
		Note:      share.Note, ChangeNote: share.ChangeNote,
	}
	for _, directory := range share.Directories {
		row.Directories = append(row.Directories, webui.ShareDirectoryView{
			Purpose: directory.Purpose, Path: directory.Path,
		})
	}
	return row
}

func serverViews(servers []models.SharedStorageServer) []webui.ShareServerView {
	views := make([]webui.ShareServerView, 0, len(servers))
	for _, server := range servers {
		views = append(views, webui.ShareServerView{
			ID: server.ID, Name: server.Name, DeviceName: server.DeviceName,
			ShellType: string(server.ShellType), MountPoint: server.MountPoint,
		})
	}
	return views
}
