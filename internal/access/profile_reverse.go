package access

import (
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ProfileRef 是反向授权结果里对一个 profile 的引用。
type ProfileRef struct {
	ID   string
	Name string
}

// ServerProfileAuthorizationPreview 是一台服务器条目的 profile 授权差异。
type ServerProfileAuthorizationPreview struct {
	Server  ServerRef
	Current []ProfileRef
	Desired []ProfileRef
	Added   []ProfileRef
	Removed []ProfileRef
	Summary string
}

// ServerProfileAuthorizationApplyResult 表示一次反向授权确认的结果。
type ServerProfileAuthorizationApplyResult struct {
	Preview ServerProfileAuthorizationPreview
	Applied bool
}

// ServerProfileAuthorizationConflictError 表示预览之后这台服务器的 profile
// 授权被其他入口修改；Latest 带最新差异供页面重新确认。
type ServerProfileAuthorizationConflictError struct {
	Latest ServerProfileAuthorizationPreview
}

func (e *ServerProfileAuthorizationConflictError) Error() string {
	return "服务器条目的 profile 授权已被其他入口修改，请按最新授权集合重新确认"
}

// PreviewServerProfileAuthorization 计算一台服务器条目的目标 profile 集
// 合与当前集合的差异。读取不写数据，也不写审计。
func (m *Manager) PreviewServerProfileAuthorization(
	server string, desired []string,
) (ServerProfileAuthorizationPreview, error) {
	srv, err := m.st.ResolveServer(server)
	if err != nil {
		return ServerProfileAuthorizationPreview{}, err
	}
	if srv == nil {
		return ServerProfileAuthorizationPreview{}, fmt.Errorf("server %q not found", server)
	}
	names, desiredIDs, err := m.profileNameIndex(desired)
	if err != nil {
		return ServerProfileAuthorizationPreview{}, err
	}
	currentIDs, err := m.st.ProfilesForServer(srv.ID)
	if err != nil {
		return ServerProfileAuthorizationPreview{}, err
	}
	preview := serverProfileAuthorizationPreview(
		ServerRef{ID: srv.ID, Name: srv.Name}, currentIDs, desiredIDs, names,
	)
	return preview, nil
}

// ApplyServerProfileAuthorization 确认一次反向授权差异。摘要与当前状态重
// 算结果不一致时整体拒绝；发生变化时逐个 profile 写 profile.grant 或
// profile.revoke 审计行，与 profile 授权页使用同一套审计形状。
func (m *Manager) ApplyServerProfileAuthorization(
	server string, desired []string, summary string, src Source,
) (ServerProfileAuthorizationApplyResult, *ServerProfileAuthorizationConflictError, error) {
	preview, err := m.PreviewServerProfileAuthorization(server, desired)
	if err != nil {
		latest, dropped, latestErr := m.latestServerProfilePreviewWithoutMissing(server, desired)
		if latestErr == nil && dropped {
			return ServerProfileAuthorizationApplyResult{}, &ServerProfileAuthorizationConflictError{Latest: latest}, nil
		}
		return ServerProfileAuthorizationApplyResult{}, nil, err
	}
	if preview.Summary != summary {
		latest, latestErr := m.PreviewServerProfileAuthorization(server, desired)
		if latestErr != nil {
			return ServerProfileAuthorizationApplyResult{}, nil, latestErr
		}
		return ServerProfileAuthorizationApplyResult{}, &ServerProfileAuthorizationConflictError{Latest: latest}, nil
	}
	if len(preview.Added) == 0 && len(preview.Removed) == 0 {
		return ServerProfileAuthorizationApplyResult{Preview: preview}, nil, nil
	}
	currentIDs := profileIDs(preview.Current)
	desiredIDs := profileIDs(preview.Desired)
	adjustment, conflict, err := m.st.AdjustServerProfiles(preview.Server.ID, currentIDs, desiredIDs)
	if err != nil {
		return ServerProfileAuthorizationApplyResult{}, nil, err
	}
	if conflict {
		latestIDs, latestErr := m.st.ProfilesForServer(preview.Server.ID)
		if latestErr != nil {
			return ServerProfileAuthorizationApplyResult{}, nil, latestErr
		}
		names, _, nameErr := m.profileNameIndex(nil)
		if nameErr != nil {
			return ServerProfileAuthorizationApplyResult{}, nil, nameErr
		}
		latest := serverProfileAuthorizationPreview(preview.Server, latestIDs, desiredIDs, names)
		return ServerProfileAuthorizationApplyResult{}, &ServerProfileAuthorizationConflictError{Latest: latest}, nil
	}
	if err := m.writeReverseProfileAudits(preview, adjustment, src); err != nil {
		return ServerProfileAuthorizationApplyResult{}, nil, err
	}
	return ServerProfileAuthorizationApplyResult{Preview: preview, Applied: true}, nil, nil
}

// latestServerProfilePreviewWithoutMissing 从目标集合里剔除预览后被删除的
// profile，再用仍存在的目标计算最新差异；没有缺失时 dropped 为假。
func (m *Manager) latestServerProfilePreviewWithoutMissing(
	server string, desired []string,
) (ServerProfileAuthorizationPreview, bool, error) {
	srv, err := m.st.ResolveServer(server)
	if err != nil {
		return ServerProfileAuthorizationPreview{}, false, err
	}
	if srv == nil {
		return ServerProfileAuthorizationPreview{}, false, fmt.Errorf("server %q not found", server)
	}
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return ServerProfileAuthorizationPreview{}, false, err
	}
	names, ids := profileIndexes(profiles)
	remaining := make([]string, 0, len(desired))
	for _, identifier := range desired {
		if id, ok := ids[identifier]; ok {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == len(desired) {
		return ServerProfileAuthorizationPreview{}, false, nil
	}
	currentIDs, err := m.st.ProfilesForServer(srv.ID)
	if err != nil {
		return ServerProfileAuthorizationPreview{}, false, err
	}
	return serverProfileAuthorizationPreview(
		ServerRef{ID: srv.ID, Name: srv.Name}, currentIDs, remaining, names,
	), true, nil
}

func (m *Manager) writeReverseProfileAudits(
	preview ServerProfileAuthorizationPreview,
	adjustment store.ServerProfileSetAdjustment,
	src Source,
) error {
	names := make(map[string]string, len(preview.Desired)+len(preview.Current))
	for _, profile := range append(preview.Desired, preview.Current...) {
		names[profile.ID] = profile.Name
	}
	writeProfileAudit := func(action string, id string) error {
		profileName, ok := names[id]
		if !ok {
			return fmt.Errorf("profile %s not found", id)
		}
		summary := map[string]any{
			"profile": profileName,
			"servers": serverListSummary([]string{preview.Server.Name}),
		}
		return m.writeAudit(action, summary, nil, src)
	}
	for _, id := range adjustment.Added {
		if err := writeProfileAudit("profile.grant", id); err != nil {
			return err
		}
	}
	for _, id := range adjustment.Removed {
		if err := writeProfileAudit("profile.revoke", id); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) profileNameIndex(desired []string) (map[string]string, []string, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, nil, err
	}
	names, ids := profileIndexes(profiles)
	desiredIDs := make([]string, 0, len(desired))
	for _, identifier := range desired {
		id, ok := ids[identifier]
		if !ok {
			return nil, nil, fmt.Errorf("profile %q not found", identifier)
		}
		desiredIDs = append(desiredIDs, id)
	}
	return names, desiredIDs, nil
}

func profileIndexes(profiles []*models.Profile) (namesByID, idsByIdentifier map[string]string) {
	namesByID = make(map[string]string, len(profiles))
	idsByIdentifier = make(map[string]string, len(profiles)*2)
	for _, profile := range profiles {
		namesByID[profile.ID] = profile.Name
		idsByIdentifier[profile.ID] = profile.ID
		idsByIdentifier[profile.Name] = profile.ID
	}
	return namesByID, idsByIdentifier
}

func serverProfileAuthorizationPreview(
	server ServerRef, currentIDs, desiredIDs []string, names map[string]string,
) ServerProfileAuthorizationPreview {
	currentSorted := store.NormalizeIDSet(currentIDs)
	desiredSorted := store.NormalizeIDSet(desiredIDs)
	currentSet := stringSet(currentSorted)
	desiredSet := stringSet(desiredSorted)
	preview := ServerProfileAuthorizationPreview{
		Server:  server,
		Current: profileRefs(currentSorted, names),
		Desired: profileRefs(desiredSorted, names),
	}
	for _, profile := range preview.Desired {
		if !currentSet[profile.ID] {
			preview.Added = append(preview.Added, profile)
		}
	}
	for _, profile := range preview.Current {
		if !desiredSet[profile.ID] {
			preview.Removed = append(preview.Removed, profile)
		}
	}
	preview.Summary = fmt.Sprintf(
		"server=%s;current=%s;desired=%s",
		server.ID, strings.Join(currentSorted, ","), strings.Join(desiredSorted, ","),
	)
	return preview
}

func profileRefs(ids []string, names map[string]string) []ProfileRef {
	out := make([]ProfileRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, ProfileRef{ID: id, Name: names[id]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func profileIDs(refs []ProfileRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.ID)
	}
	return out
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
