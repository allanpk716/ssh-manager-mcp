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

func (s openerStore) ListProjects(includeRevoked bool) ([]webui.AccessProjectSummary, error) {
	var projects []webui.AccessProjectSummary
	err := s.withManager(func(manager *access.Manager) error {
		rows, err := managerStore{manager: manager}.ListProjects(includeRevoked)
		if err != nil {
			return err
		}
		projects = rows
		return nil
	})
	return projects, err
}

func (s openerStore) AddProject(name, profileID string, source webui.AccessWriteSource) (webui.AccessProjectToken, error) {
	var token webui.AccessProjectToken
	err := s.withManager(func(manager *access.Manager) error {
		result, err := managerStore{manager: manager}.AddProject(name, profileID, source)
		token = result
		return err
	})
	return token, err
}

func (s openerStore) RotateProject(id string, source webui.AccessWriteSource) (webui.AccessProjectToken, error) {
	var token webui.AccessProjectToken
	err := s.withManager(func(manager *access.Manager) error {
		result, err := managerStore{manager: manager}.RotateProject(id, source)
		token = result
		return err
	})
	return token, err
}

func (s openerStore) DisableProject(id string, source webui.AccessWriteSource) error {
	return s.withManager(func(manager *access.Manager) error {
		return managerStore{manager: manager}.DisableProject(id, source)
	})
}

func (s openerStore) EnableProject(id string, source webui.AccessWriteSource) error {
	return s.withManager(func(manager *access.Manager) error {
		return managerStore{manager: manager}.EnableProject(id, source)
	})
}

func (s openerStore) RevokeProject(id string, source webui.AccessWriteSource) error {
	return s.withManager(func(manager *access.Manager) error {
		return managerStore{manager: manager}.RevokeProject(id, source)
	})
}

func (s openerStore) DeleteProject(id string, source webui.AccessWriteSource) error {
	return s.withManager(func(manager *access.Manager) error {
		return managerStore{manager: manager}.DeleteProject(id, source)
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

func (s managerStore) ListProjects(includeRevoked bool) ([]webui.AccessProjectSummary, error) {
	projects, err := s.manager.ListProjects(includeRevoked)
	if err != nil {
		return nil, err
	}
	out := make([]webui.AccessProjectSummary, 0, len(projects))
	for _, project := range projects {
		out = append(out, webui.AccessProjectSummary{
			ID:          project.ID,
			Name:        project.Name,
			TokenPrefix: project.TokenPrefix,
			Status:      string(project.Status),
			ProfileID:   project.ProfileID,
			ProfileName: project.ProfileName,
			CreatedAt:   project.CreatedAt,
		})
	}
	return out, nil
}

func (s managerStore) AddProject(name, profileID string, source webui.AccessWriteSource) (webui.AccessProjectToken, error) {
	result, err := s.manager.AddProject(name, profileID, access.WebUISource(source.RemoteAddress))
	if err != nil {
		return webui.AccessProjectToken{}, err
	}
	return projectToken(result), nil
}

func (s managerStore) RotateProject(id string, source webui.AccessWriteSource) (webui.AccessProjectToken, error) {
	result, err := s.manager.RotateProject(id, access.WebUISource(source.RemoteAddress))
	if err != nil {
		return webui.AccessProjectToken{}, err
	}
	return projectToken(result), nil
}

func (s managerStore) DisableProject(id string, source webui.AccessWriteSource) error {
	return s.manager.DisableProject(id, access.WebUISource(source.RemoteAddress))
}

func (s managerStore) EnableProject(id string, source webui.AccessWriteSource) error {
	return s.manager.EnableProject(id, access.WebUISource(source.RemoteAddress))
}

func (s managerStore) RevokeProject(id string, source webui.AccessWriteSource) error {
	return s.manager.RevokeProject(id, access.WebUISource(source.RemoteAddress))
}

func (s managerStore) DeleteProject(id string, source webui.AccessWriteSource) error {
	return s.manager.DeleteProject(id, access.WebUISource(source.RemoteAddress))
}

// projectToken 把访问管理模块的一次性签发或轮换结果转成 Web UI 展示结
// 构；片段保持模块生成的原文，保证与命令行输出完全一致。
func projectToken(result access.ProjectTokenResult) webui.AccessProjectToken {
	return webui.AccessProjectToken{
		ID: result.ID, Name: result.Name, Token: result.Token, Snippet: result.Snippet,
	}
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
