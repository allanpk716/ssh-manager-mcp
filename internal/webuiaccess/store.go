// Package webuiaccess 把访问管理模块适配成 Web UI 的窄接口，页面处理器
// 因此不接触数据库结构。
package webuiaccess

import (
	"errors"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/webui"
)

type managerStore struct {
	manager *access.Manager
}

type openerStore struct {
	open func() (*access.Manager, func(), error)
}

// FromManager 把一个已挂在解锁存储上的访问管理模块适配给 Web UI。
func FromManager(manager *access.Manager) webui.AccessStore {
	return managerStore{manager: manager}
}

// FromOpener 每次页面请求重新打开存储并挂上访问管理模块，调用结束后关闭。
// 单机回环 Web UI 没有常驻进程持有存储句柄，用这个入口避免长期占用。
func FromOpener(open func() (*access.Manager, func(), error)) webui.AccessStore {
	return openerStore{open: open}
}

func (s openerStore) withManager(operation func(*access.Manager) error) error {
	manager, closeStore, err := s.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(manager)
}

func (s openerStore) ListProfiles() ([]webui.AccessProfileSummary, error) {
	var profiles []webui.AccessProfileSummary
	err := s.withManager(func(manager *access.Manager) error {
		rows, err := managerStore{manager: manager}.ListProfiles()
		if err != nil {
			return err
		}
		profiles = rows
		return nil
	})
	return profiles, err
}

func (s openerStore) AddProfile(name string, source webui.AccessWriteSource) error {
	return s.withManager(func(manager *access.Manager) error {
		return managerStore{manager: manager}.AddProfile(name, source)
	})
}

func (s openerStore) DeleteProfile(id string, source webui.AccessWriteSource) (*webui.AccessProfileInUse, error) {
	var inUse *webui.AccessProfileInUse
	err := s.withManager(func(manager *access.Manager) error {
		blocked, err := managerStore{manager: manager}.DeleteProfile(id, source)
		inUse = blocked
		return err
	})
	return inUse, err
}

func (s openerStore) SetServerGrants(profileID string, expected, desired []string, source webui.AccessWriteSource) (webui.AccessGrantChange, *webui.AccessGrantConflict, error) {
	var change webui.AccessGrantChange
	var conflict *webui.AccessGrantConflict
	err := s.withManager(func(manager *access.Manager) error {
		result, blocked, err := managerStore{manager: manager}.SetServerGrants(profileID, expected, desired, source)
		change, conflict = result, blocked
		return err
	})
	return change, conflict, err
}

func (s openerStore) ListDeviceCodes() ([]webui.AccessDeviceCodeSummary, error) {
	var rows []webui.AccessDeviceCodeSummary
	err := s.withManager(func(manager *access.Manager) error {
		got, err := managerStore{manager: manager}.ListDeviceCodes()
		if err != nil {
			return err
		}
		rows = got
		return nil
	})
	return rows, err
}

func (s openerStore) AddDeviceCode(name, profile, serveURL string, source webui.AccessWriteSource) (webui.AccessDeviceCodeResult, error) {
	var result webui.AccessDeviceCodeResult
	err := s.withManager(func(manager *access.Manager) error {
		got, err := managerStore{manager: manager}.AddDeviceCode(name, profile, serveURL, source)
		result = got
		return err
	})
	return result, err
}

func (s openerStore) RevokeDeviceCode(id string, source webui.AccessWriteSource) error {
	return s.withManager(func(manager *access.Manager) error {
		return managerStore{manager: manager}.RevokeDeviceCode(id, source)
	})
}

func (s managerStore) ListProfiles() ([]webui.AccessProfileSummary, error) {
	profiles, err := s.manager.ListProfiles()
	if err != nil {
		return nil, err
	}
	out := make([]webui.AccessProfileSummary, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, webui.AccessProfileSummary{
			ID:          profile.ID,
			Name:        profile.Name,
			Servers:     serverRefs(profile.Servers),
			Projects:    references(profile.Projects),
			DeviceCodes: deviceReferences(profile.DeviceCodes),
		})
	}
	return out, nil
}

func (s managerStore) AddProfile(name string, source webui.AccessWriteSource) error {
	_, err := s.manager.AddProfile(name, access.WebUISource(source.RemoteAddress))
	return err
}

func (s managerStore) DeleteProfile(id string, source webui.AccessWriteSource) (*webui.AccessProfileInUse, error) {
	err := s.manager.DeleteProfile(id, access.WebUISource(source.RemoteAddress))
	var blocked *access.ProfileInUseError
	if errors.As(err, &blocked) {
		return &webui.AccessProfileInUse{
			ProfileName: blocked.ProfileName,
			Projects:    append([]string(nil), blocked.Projects...),
			DeviceCodes: append([]string(nil), blocked.DeviceCodes...),
		}, nil
	}
	return nil, err
}

func (s managerStore) SetServerGrants(profileID string, expected, desired []string, source webui.AccessWriteSource) (webui.AccessGrantChange, *webui.AccessGrantConflict, error) {
	result, err := s.manager.SetServerGrants(
		profileID, expected, desired, access.WebUISource(source.RemoteAddress),
	)
	var conflict *access.GrantSetConflictError
	if errors.As(err, &conflict) {
		return webui.AccessGrantChange{}, &webui.AccessGrantConflict{
			ProfileName: conflict.ProfileName,
			Current:     serverRefs(conflict.Current),
		}, nil
	}
	if err != nil {
		return webui.AccessGrantChange{}, nil, err
	}
	return webui.AccessGrantChange{Added: serverRefs(result.Added), Removed: serverRefs(result.Removed)}, nil, nil
}

func (s managerStore) ListDeviceCodes() ([]webui.AccessDeviceCodeSummary, error) {
	rows, err := s.manager.ListDeviceCodes()
	if err != nil {
		return nil, err
	}
	out := make([]webui.AccessDeviceCodeSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, webui.AccessDeviceCodeSummary{
			ID:            row.ID,
			Name:          row.Name,
			TokenPrefix:   row.TokenPrefix,
			Status:        string(row.Status),
			ProfileName:   row.ProfileName,
			LastPullAt:    row.LastPullAt,
			ClientVersion: row.ClientVersion,
		})
	}
	return out, nil
}

func (s managerStore) AddDeviceCode(name, profile, serveURL string, source webui.AccessWriteSource) (webui.AccessDeviceCodeResult, error) {
	result, err := s.manager.AddDeviceCode(name, profile, serveURL, access.WebUISource(source.RemoteAddress))
	if err != nil {
		return webui.AccessDeviceCodeResult{}, err
	}
	return webui.AccessDeviceCodeResult{
		Name:        result.Name,
		Code:        result.Code,
		Fingerprint: result.Fingerprint,
		PullCommand: result.PullCommand,
	}, nil
}

func (s managerStore) RevokeDeviceCode(id string, source webui.AccessWriteSource) error {
	return s.manager.RevokeDeviceCode(id, access.WebUISource(source.RemoteAddress))
}

func serverRefs(refs []access.ServerRef) []webui.AccessServerRef {
	out := make([]webui.AccessServerRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, webui.AccessServerRef{ID: ref.ID, Name: ref.Name})
	}
	return out
}

func references(refs []access.ProjectRef) []webui.AccessReference {
	out := make([]webui.AccessReference, 0, len(refs))
	for _, ref := range refs {
		out = append(out, webui.AccessReference{Name: ref.Name, Status: string(ref.Status)})
	}
	return out
}

func deviceReferences(refs []access.DeviceCodeRef) []webui.AccessReference {
	out := make([]webui.AccessReference, 0, len(refs))
	for _, ref := range refs {
		out = append(out, webui.AccessReference{Name: ref.Name, Status: string(ref.Status)})
	}
	return out
}
