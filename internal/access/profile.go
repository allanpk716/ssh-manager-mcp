package access

import (
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ServerRef 是访问管理结果里对一台服务器的引用：标识符加名字。授权行
// 在存储里按标识符保存，页面与命令行输出都按名字展示。
type ServerRef struct {
	ID   string
	Name string
}

// ProjectRef 是引用某个 profile 的项目令牌：名字加生命周期状态。
type ProjectRef struct {
	Name   string
	Status models.ProjectStatus
}

// DeviceCodeRef 是绑定到某个 profile 的设备码：名字加状态。
type DeviceCodeRef struct {
	Name   string
	Status models.CacheTokenStatus
}

// ProfileSummary 是 profile 列表的一行：这个 profile 授权了哪些服务器，
// 以及哪些项目令牌与设备码正在引用它。
type ProfileSummary struct {
	ID          string
	Name        string
	Servers     []ServerRef
	Projects    []ProjectRef
	DeviceCodes []DeviceCodeRef
}

// ListProfiles 按名字顺序列出全部 profile，附上每个 profile 的授权服务
// 器与引用它的项目令牌、设备码。
func (m *Manager) ListProfiles() ([]ProfileSummary, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	servers, err := m.st.ListServers()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]models.Server, len(servers))
	for _, srv := range servers {
		byID[srv.ID] = *srv
	}
	projects, err := m.st.ListProjects()
	if err != nil {
		return nil, err
	}
	devices, err := m.st.ListCacheTokens()
	if err != nil {
		return nil, err
	}
	out := make([]ProfileSummary, 0, len(profiles))
	for _, p := range profiles {
		summary := ProfileSummary{ID: p.ID, Name: p.Name}
		ids, err := m.st.ServersForProfile(p.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if srv, ok := byID[id]; ok {
				summary.Servers = append(summary.Servers, ServerRef{ID: srv.ID, Name: srv.Name})
			}
		}
		sort.Slice(summary.Servers, func(i, j int) bool { return summary.Servers[i].Name < summary.Servers[j].Name })
		for _, proj := range projects {
			if proj.ProfileID == p.ID {
				summary.Projects = append(summary.Projects, ProjectRef{Name: proj.Name, Status: proj.Status})
			}
		}
		for _, ct := range devices {
			if ct.ProfileID == p.ID {
				summary.DeviceCodes = append(summary.DeviceCodes, DeviceCodeRef{Name: ct.Name, Status: ct.Status})
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

// AddProfile 新增一个 profile 并写 profile.add 审计行。名称重复时存储报
// 错，模块把失败行也写进审计后原样返回错误。
func (m *Manager) AddProfile(name string, src Source) (string, error) {
	id, err := m.st.AddProfile(name)
	if aerr := m.writeAudit("profile.add", map[string]any{"name": name}, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return id, err
}

// serverListSummary 生成审计摘要里的服务器清单：条数加调用方输入的名字
// 或标识符，保持命令行原有形状（例如「2台:web1,web2」）。
func serverListSummary(names []string) string {
	return fmt.Sprintf("%d台:%s", len(names), strings.Join(names, ","))
}

// resolveProfile 按 profile 名字或标识符解析（名字在存储里唯一，Web UI
// 的表单与链接用标识符）。找不到时返回已知名单，方便 owner 直接改正拼写。
func (m *Manager) resolveProfile(identifier string) (*models.Profile, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.Name == identifier || p.ID == identifier {
			return p, nil
		}
	}
	known := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		known = append(known, profile.Name)
	}
	return nil, fmt.Errorf("profile %q not found (known: %v)", identifier, known)
}

// resolveServers 把服务器名字或标识符逐个解析成存储里的服务器条目；任
// 意一个找不到就整体报错，不做部分授予。
func (m *Manager) resolveServers(identifiers []string) ([]*models.Server, error) {
	out := make([]*models.Server, 0, len(identifiers))
	for _, identifier := range identifiers {
		srv, err := m.st.ResolveServer(identifier)
		if err != nil {
			return nil, err
		}
		if srv == nil {
			return nil, fmt.Errorf("server %q not found", identifier)
		}
		out = append(out, srv)
	}
	return out, nil
}

// GrantServers 把服务器授予 profile：沿用存储的幂等追加，服务器按名字
// 或标识符解析。请求（无论成败）写一条 profile.grant 审计行，成功行摘
// 要与命令行原有形状一致。
func (m *Manager) GrantServers(profile string, servers []string, src Source) ([]ServerRef, error) {
	summary := map[string]any{"profile": profile, "servers": serverListSummary(servers)}
	var granted []ServerRef
	err := func() error {
		p, err := m.resolveProfile(profile)
		if err != nil {
			return err
		}
		resolved, err := m.resolveServers(servers)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(resolved))
		for _, srv := range resolved {
			ids = append(ids, srv.ID)
			granted = append(granted, ServerRef{ID: srv.ID, Name: srv.Name})
		}
		return m.st.GrantServers(p.ID, ids)
	}()
	if aerr := m.writeAudit("profile.grant", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return granted, err
}

// RevokeResult 是一次撤销请求的结果：真正撤销的服务器，以及本来就没有
// 授权、被跳过的服务器（跳过不是错误，命令行据此提示）。
type RevokeResult struct {
	Revoked []ServerRef
	Skipped []ServerRef
}

// RevokeServers 从 profile 撤销服务器授权：服务器按名字或标识符解析，
// 未授权的跳过不算错误；profile 或服务器不存在时报错。请求写一条
// profile.revoke 审计行，摘要含 profile 名与服务器名。
func (m *Manager) RevokeServers(profile string, servers []string, src Source) (RevokeResult, error) {
	summary := map[string]any{"profile": profile, "servers": serverListSummary(servers)}
	var result RevokeResult
	err := func() error {
		p, err := m.resolveProfile(profile)
		if err != nil {
			return err
		}
		resolved, err := m.resolveServers(servers)
		if err != nil {
			return err
		}
		granted, err := m.st.ServersForProfile(p.ID)
		if err != nil {
			return err
		}
		grantedSet := make(map[string]bool, len(granted))
		for _, id := range granted {
			grantedSet[id] = true
		}
		var revokeIDs []string
		for _, srv := range resolved {
			if grantedSet[srv.ID] {
				revokeIDs = append(revokeIDs, srv.ID)
				result.Revoked = append(result.Revoked, ServerRef{ID: srv.ID, Name: srv.Name})
			} else {
				result.Skipped = append(result.Skipped, ServerRef{ID: srv.ID, Name: srv.Name})
			}
		}
		if len(revokeIDs) > 0 {
			if _, err := m.st.RevokeServers(p.ID, revokeIDs); err != nil {
				return err
			}
		}
		return nil
	}()
	if aerr := m.writeAudit("profile.revoke", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return result, err
}

// ProfileInUseError 表示 profile 仍被未吊销的项目令牌或活跃的设备码引
// 用，不能删除。清单是结构化的，调用方（命令行或 Web UI）直接用来展示
// 「先处理谁」。
type ProfileInUseError struct {
	ProfileName string
	Projects    []string
	DeviceCodes []string
}

func (e *ProfileInUseError) Error() string {
	var parts []string
	if len(e.Projects) > 0 {
		parts = append(parts, fmt.Sprintf("未吊销的项目令牌 %d 个(%s)", len(e.Projects), strings.Join(e.Projects, ",")))
	}
	if len(e.DeviceCodes) > 0 {
		parts = append(parts, fmt.Sprintf("活跃设备码 %d 个(%s)", len(e.DeviceCodes), strings.Join(e.DeviceCodes, ",")))
	}
	return fmt.Sprintf("profile %s 仍被%s引用:先删除或吊销这些引用者,再删除 profile", e.ProfileName, strings.Join(parts, "、"))
}

// DeleteProfile 删除 profile 及其授权行；仍被未吊销项目令牌或活跃设备码
// 引用时返回 ProfileInUseError（引用者清单在错误里）。删除请求写一条
// profile.rm 审计行。
func (m *Manager) DeleteProfile(profile string, src Source) error {
	summary := map[string]any{"name": profile}
	err := func() error {
		p, err := m.resolveProfile(profile)
		if err != nil {
			return err
		}
		blocked := &ProfileInUseError{ProfileName: p.Name}
		projects, err := m.st.ListProjects()
		if err != nil {
			return err
		}
		for _, proj := range projects {
			if proj.ProfileID == p.ID && proj.Status != models.ProjectRevoked {
				blocked.Projects = append(blocked.Projects, proj.Name)
			}
		}
		devices, err := m.st.ListCacheTokens()
		if err != nil {
			return err
		}
		for _, ct := range devices {
			if ct.ProfileID == p.ID && ct.Status == models.CacheTokenActive {
				blocked.DeviceCodes = append(blocked.DeviceCodes, ct.Name)
			}
		}
		if len(blocked.Projects) > 0 || len(blocked.DeviceCodes) > 0 {
			return blocked
		}
		return m.st.DeleteProfile(p.ID)
	}()
	if aerr := m.writeAudit("profile.rm", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return err
}

// SetGrantsResult 是按完整集合调整授权的结果：新增与撤销的服务器清单。
type SetGrantsResult struct {
	Added   []ServerRef
	Removed []ServerRef
}

// GrantSetConflictError 表示打开表单后授权范围已被别的入口修改，本次提
// 交被拒绝；Current 带最新授权集合，调用方用它重新渲染表单。
type GrantSetConflictError struct {
	ProfileName string
	Current     []ServerRef
}

func (e *GrantSetConflictError) Error() string {
	names := make([]string, 0, len(e.Current))
	for _, srv := range e.Current {
		names = append(names, srv.Name)
	}
	return fmt.Sprintf("profile %s 的授权范围已被其他入口修改，已拒绝提交；最新授权:%s", e.ProfileName, strings.Join(names, ","))
}

// serverNameIndex 建立服务器标识符到名字的索引。
func (m *Manager) serverNameIndex() (map[string]string, error) {
	servers, err := m.st.ListServers()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(servers))
	for _, srv := range servers {
		names[srv.ID] = srv.Name
	}
	return names, nil
}

// serverRefs 按名字排序把标识符清单转成服务器引用。
func serverRefs(ids []string, names map[string]string) []ServerRef {
	out := make([]ServerRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, ServerRef{ID: id, Name: names[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetServerGrants 按完整集合调整授权：expected 是打开表单时看到的授权集
// 合，desired 是这次提交的目标集合。expected 与当前集合不一致时返回
// GrantSetConflictError（不写数据、不写审计）；一致时按差异逐项授予或撤
// 销，并把审计拆成一条 profile.grant 和一条 profile.revoke（没有变化的
// 一侧不写）。expected 与 desired 的成员都按服务器名字或标识符解析；
// expected 里解析不到的成员按原样参与比对（表单打开后被删除的服务器会
// 造成冲突而不是报错），desired 里解析不到的成员是提交错误。
func (m *Manager) SetServerGrants(profile string, expected, desired []string, src Source) (SetGrantsResult, error) {
	var result SetGrantsResult
	err := func() error {
		p, err := m.resolveProfile(profile)
		if err != nil {
			return err
		}
		names, err := m.serverNameIndex()
		if err != nil {
			return err
		}
		expectedIDs := make([]string, 0, len(expected))
		for _, identifier := range expected {
			if srv, rerr := m.st.ResolveServer(identifier); rerr != nil {
				return rerr
			} else if srv != nil {
				expectedIDs = append(expectedIDs, srv.ID)
			} else {
				expectedIDs = append(expectedIDs, identifier)
			}
		}
		desiredResolved, err := m.resolveServers(desired)
		if err != nil {
			return err
		}
		desiredIDs := make([]string, 0, len(desiredResolved))
		for _, srv := range desiredResolved {
			desiredIDs = append(desiredIDs, srv.ID)
		}
		adjustment, conflict, err := m.st.AdjustGrantSet(p.ID, expectedIDs, desiredIDs)
		if err != nil {
			return err
		}
		if conflict {
			return &GrantSetConflictError{ProfileName: p.Name, Current: serverRefs(adjustment.Current, names)}
		}
		result.Added = serverRefs(adjustment.Added, names)
		result.Removed = serverRefs(adjustment.Removed, names)
		if len(result.Added) > 0 {
			added := make([]string, 0, len(result.Added))
			for _, srv := range result.Added {
				added = append(added, srv.Name)
			}
			if aerr := m.writeAudit("profile.grant", map[string]any{"profile": p.Name, "servers": serverListSummary(added)}, nil, src); aerr != nil {
				return aerr
			}
		}
		if len(result.Removed) > 0 {
			removed := make([]string, 0, len(result.Removed))
			for _, srv := range result.Removed {
				removed = append(removed, srv.Name)
			}
			if aerr := m.writeAudit("profile.revoke", map[string]any{"profile": p.Name, "servers": serverListSummary(removed)}, nil, src); aerr != nil {
				return aerr
			}
		}
		return nil
	}()
	return result, err
}
