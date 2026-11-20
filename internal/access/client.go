package access

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ClientSummary 是客户端实例列表的一行。它以设备码记录为数据源，但设备
// 码本体绝不进入该结构。
type ClientSummary struct {
	ID            string
	Name          string
	ProfileID     string
	ProfileName   string
	ServerCount   int
	LastPullAt    time.Time
	ClientVersion string
	// RecommendedVersion 与权威端快照携带的推荐版本一致。
	RecommendedVersion string
	MachineName        string
	Paired             bool
	Revoked            bool
}

type ClientMachine struct {
	ID       string
	Name     string
	SitePath string
}

type ClientMachineChoice struct {
	ID        string
	Name      string
	SitePath  string
	Available bool
}

type ClientSoftwareSummary struct {
	LastCollectedAt          time.Time
	ItemCount                int
	UnidentifiedProcessCount int
	TopItems                 []string
}

type ClientResource struct {
	ID         string
	Name       string
	Note       string
	ViaServers []string
}

type ClientDetail struct {
	ClientSummary
	ProfileID          string
	RecommendedVersion string
	TokenPrefix        string
	Servers            []ServerRef
	Machine            *ClientMachine
	Software           *ClientSoftwareSummary
	Secrets            []ClientResource
	Shares             []ClientResource
}

type ClientProfilePreview struct {
	ClientName         string
	CurrentProfileID   string
	CurrentProfileName string
	DesiredProfileID   string
	DesiredProfileName string
	AddedServers       []ServerRef
	LostServers        []ServerRef
	AddedSecrets       []ClientResource
	LostSecrets        []ClientResource
	AddedShares        []ClientResource
	LostShares         []ClientResource
}

// ListClients 聚合客户端实例、绑定 profile、profile 授权服务器数量与所在
// 机器。来源通过配对流程生成的同名访问身份推导；没有这条记录时按 owner
// 手动签发展示。
func (m *Manager) ListClients() ([]ClientSummary, error) {
	tokens, err := m.st.ListCacheTokens()
	if err != nil {
		return nil, err
	}
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	profileNames := make(map[string]string, len(profiles))
	serverCounts := make(map[string]int, len(profiles))
	for _, profile := range profiles {
		profileNames[profile.ID] = profile.Name
		ids, err := m.st.ServersForProfile(profile.ID)
		if err != nil {
			return nil, err
		}
		serverCounts[profile.ID] = len(ids)
	}
	devices, err := m.st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	machines := make(map[string]string, len(devices))
	for _, device := range devices {
		if device.ClientTokenID != "" {
			machines[device.ClientTokenID] = device.Name
		}
	}
	projects, err := m.st.ListProjects()
	if err != nil {
		return nil, err
	}
	paired := make(map[string]bool, len(projects))
	for _, project := range projects {
		if project.PairGenerated {
			paired[project.Name] = true
		}
	}
	out := make([]ClientSummary, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, ClientSummary{
			ID: token.ID, Name: token.Name,
			ProfileID:   token.ProfileID,
			ProfileName: profileNames[token.ProfileID], ServerCount: serverCounts[token.ProfileID],
			LastPullAt: token.LastPullAt, ClientVersion: token.ClientVersion,
			RecommendedVersion: buildinfo.Version,
			MachineName:        machines[token.ID],
			Paired:             paired[clientPairProjectName(token.Name)], Revoked: token.Status == models.CacheTokenRevoked,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ClientDetail 按标识符读取一个客户端实例，并组装 profile 可见服务器、
// 所在机器、软件清单摘要与经 profile 服务器可拿到的资源。
func (m *Manager) ClientDetail(identifier string) (*ClientDetail, error) {
	tokens, err := m.st.ListCacheTokens()
	if err != nil {
		return nil, err
	}
	var token *models.CacheToken
	for i := range tokens {
		if tokens[i].ID == identifier || tokens[i].Name == identifier {
			token = tokens[i]
			break
		}
	}
	if token == nil {
		return nil, nil
	}
	summaries, err := m.clientSummaries(tokens)
	if err != nil {
		return nil, err
	}
	var summary ClientSummary
	for _, row := range summaries {
		if row.ID == token.ID {
			summary = row
			break
		}
	}
	servers, err := m.clientServers(token.ProfileID)
	if err != nil {
		return nil, err
	}
	detail := &ClientDetail{
		ClientSummary: summary, ProfileID: token.ProfileID,
		RecommendedVersion: buildinfo.Version, TokenPrefix: token.TokenPrefix,
		Servers: servers,
	}
	detail.ServerCount = len(servers)
	detail.Machine, err = m.clientMachine(token.ID)
	if err != nil {
		return nil, err
	}
	if detail.Machine != nil {
		detail.Software, err = m.clientSoftware(detail.Machine.Name)
		if err != nil {
			return nil, err
		}
	}
	serverIDs := make([]string, 0, len(servers))
	for _, server := range servers {
		serverIDs = append(serverIDs, server.ID)
	}
	detail.Secrets, err = m.clientSecrets(serverIDs)
	if err != nil {
		return nil, err
	}
	detail.Shares, err = m.clientShares(serverIDs)
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// PreviewClientProfileChange 计算确认前的完整影响预览：服务器、机密项与
// 共享存储都同时给出新增与失去两个方向。
func (m *Manager) PreviewClientProfileChange(identifier, desiredProfile string) (*ClientProfilePreview, error) {
	detail, err := m.ClientDetail(identifier)
	if err != nil || detail == nil {
		return nil, err
	}
	profile, err := m.resolveProfile(desiredProfile)
	if err != nil {
		return nil, err
	}
	desiredServers, err := m.clientServers(profile.ID)
	if err != nil {
		return nil, err
	}
	currentIDs := make([]string, 0, len(detail.Servers))
	for _, server := range detail.Servers {
		currentIDs = append(currentIDs, server.ID)
	}
	desiredIDs := make([]string, 0, len(desiredServers))
	for _, server := range desiredServers {
		desiredIDs = append(desiredIDs, server.ID)
	}
	currentSecrets, err := m.clientSecrets(currentIDs)
	if err != nil {
		return nil, err
	}
	desiredSecrets, err := m.clientSecrets(desiredIDs)
	if err != nil {
		return nil, err
	}
	currentShares, err := m.clientShares(currentIDs)
	if err != nil {
		return nil, err
	}
	desiredShares, err := m.clientShares(desiredIDs)
	if err != nil {
		return nil, err
	}
	currentProfile, err := m.resolveProfile(detail.ProfileID)
	if err != nil {
		return nil, err
	}
	return &ClientProfilePreview{
		ClientName: detail.Name, CurrentProfileID: currentProfile.ID, CurrentProfileName: currentProfile.Name,
		DesiredProfileID: profile.ID, DesiredProfileName: profile.Name,
		AddedServers: addedServers(detail.Servers, desiredServers),
		LostServers:  lostServers(detail.Servers, desiredServers),
		AddedSecrets: addedResources(currentSecrets, desiredSecrets),
		LostSecrets:  lostResources(currentSecrets, desiredSecrets),
		AddedShares:  addedResources(currentShares, desiredShares),
		LostShares:   lostResources(currentShares, desiredShares),
	}, nil
}

// ChangeClientProfile 更换客户端实例绑定的 profile 并写审计。存储层会同
// 步换绑配对流程生成的访问身份，保证客户端完成一次同步后仍能启动本地
// MCP 进程。
func (m *Manager) ChangeClientProfile(identifier, expectedProfileID, desiredProfileID string, src Source) error {
	detail, err := m.ClientDetail(identifier)
	if err != nil {
		return err
	}
	if detail == nil {
		return fmt.Errorf("client %q not found", identifier)
	}
	oldProfileID := expectedProfileID
	if expectedProfileID != "" {
		oldProfile, err := m.resolveProfile(expectedProfileID)
		if err != nil {
			return err
		}
		oldProfileID = oldProfile.ID
	}
	newProfile, err := m.resolveProfile(desiredProfileID)
	if err != nil {
		return err
	}
	summary := map[string]any{"name": detail.Name, "profile": newProfile.Name}
	change, err := m.st.ChangeClientProfile(detail.ID, oldProfileID, newProfile.ID)
	_ = change
	if aerr := m.writeAudit("cache-token.bind", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return err
}

// ListClientMachineChoices 返回可选择机器，并标记已被其他客户端实例占用
// 的机器。
func (m *Manager) ListClientMachineChoices(clientID string) ([]ClientMachineChoice, error) {
	devices, err := m.st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	sites, err := m.st.ListSites()
	if err != nil {
		return nil, err
	}
	tree := models.NewSiteTree(sites)
	out := make([]ClientMachineChoice, 0, len(devices))
	for _, device := range devices {
		out = append(out, ClientMachineChoice{
			ID: device.ID, Name: device.Name, SitePath: tree.PathString(device.SiteID),
			Available: device.ClientTokenID == "" || device.ClientTokenID == clientID,
		})
	}
	return out, nil
}

// LinkClientMachine 把一个客户端实例关联到既有机器，复用设备编辑的唯一
// 一对一校验与审计。
func (m *Manager) LinkClientMachine(identifier, machineID string, src Source) error {
	detail, err := m.ClientDetail(identifier)
	if err != nil {
		return err
	}
	if detail == nil {
		return fmt.Errorf("client %q not found", identifier)
	}
	device, err := m.st.GetDeviceByID(machineID)
	if err != nil {
		return err
	}
	if device == nil {
		return fmt.Errorf("机器 %q 不存在", machineID)
	}
	_, err = m.st.EditDeviceWithSource(device.Name, store.DeviceChanges{Client: &detail.Name}, accessStoreWriteSource(src))
	return err
}

// CreateAndLinkClientMachine 新建一台机器并直接关联客户端实例。
func (m *Manager) CreateAndLinkClientMachine(identifier, machineName, sitePath string, src Source) error {
	detail, err := m.ClientDetail(identifier)
	if err != nil {
		return err
	}
	if detail == nil {
		return fmt.Errorf("client %q not found", identifier)
	}
	_, err = m.st.AddDeviceAtSiteWithSource(machineName, "", detail.Name, sitePath, accessStoreWriteSource(src))
	return err
}

// RemoveClient 移除客户端实例，并写一条不含任何凭据本体的审计行。
func (m *Manager) RemoveClient(identifier string, src Source) error {
	detail, err := m.ClientDetail(identifier)
	if err != nil {
		return err
	}
	if detail == nil {
		return fmt.Errorf("client %q not found", identifier)
	}
	removal, err := m.st.RemoveClient(detail.ID)
	summary := map[string]any{"client": detail.Name}
	if removal.MachineName != "" {
		summary["machine"] = removal.MachineName
	}
	if err == nil && removal.RevokedPairProject {
		summary["paired_project"] = removal.RevokedPairProject
	}
	if aerr := m.writeAudit("client.remove", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return err
}

func accessStoreWriteSource(src Source) store.WriteSource {
	if src.kind == store.AuditSourceWebUI {
		return store.WebUISource(src.addr)
	}
	return store.CLISource()
}

// clientSummaries 抽出列表与详情共用的客户端基础字段组装。
func (m *Manager) clientSummaries(tokens []*models.CacheToken) ([]ClientSummary, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	profileNames := make(map[string]string, len(profiles))
	serverCounts := make(map[string]int, len(profiles))
	for _, profile := range profiles {
		profileNames[profile.ID] = profile.Name
		ids, err := m.st.ServersForProfile(profile.ID)
		if err != nil {
			return nil, err
		}
		serverCounts[profile.ID] = len(ids)
	}
	devices, err := m.st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	machines := make(map[string]string, len(devices))
	for _, device := range devices {
		if device.ClientTokenID != "" {
			machines[device.ClientTokenID] = device.Name
		}
	}
	projects, err := m.st.ListProjects()
	if err != nil {
		return nil, err
	}
	paired := make(map[string]bool, len(projects))
	for _, project := range projects {
		if project.PairGenerated {
			paired[project.Name] = true
		}
	}
	out := make([]ClientSummary, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, ClientSummary{
			ID: token.ID, Name: token.Name, ProfileName: profileNames[token.ProfileID],
			ServerCount: serverCounts[token.ProfileID], LastPullAt: token.LastPullAt,
			ClientVersion: token.ClientVersion, RecommendedVersion: buildinfo.Version,
			MachineName: machines[token.ID],
			Paired:      paired[clientPairProjectName(token.Name)], Revoked: token.Status == models.CacheTokenRevoked,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) clientServers(profileID string) ([]ServerRef, error) {
	ids, err := m.st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	servers, err := m.st.ListServers()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]models.Server, len(servers))
	for _, server := range servers {
		byID[server.ID] = *server
	}
	out := make([]ServerRef, 0, len(ids))
	for _, id := range ids {
		if server, ok := byID[id]; ok {
			out = append(out, ServerRef{ID: server.ID, Name: server.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) clientMachine(clientID string) (*ClientMachine, error) {
	devices, err := m.st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	var device *models.Device
	for _, row := range devices {
		if row.ClientTokenID == clientID {
			device = row
			break
		}
	}
	if device == nil {
		return nil, nil
	}
	sites, err := m.st.ListSites()
	if err != nil {
		return nil, err
	}
	return &ClientMachine{
		ID: device.ID, Name: device.Name,
		SitePath: models.NewSiteTree(sites).PathString(device.SiteID),
	}, nil
}

func (m *Manager) clientSoftware(machineName string) (*ClientSoftwareSummary, error) {
	status, err := m.st.SoftwareCollectionStatusForDevice(machineName)
	if err != nil {
		return nil, err
	}
	items, err := m.st.SoftwareObservedItemsForDevice(machineName)
	if err != nil {
		return nil, err
	}
	summary := &ClientSoftwareSummary{LastCollectedAt: status.LastSuccessAt, ItemCount: len(items)}
	for _, item := range items {
		if len(summary.TopItems) < 3 {
			label := item.Name
			if item.Version != "" {
				label += " " + item.Version
			}
			summary.TopItems = append(summary.TopItems, label)
		}
		if item.Category == models.SoftwareCategoryUnidentified {
			summary.UnidentifiedProcessCount++
		}
	}
	return summary, nil
}

func (m *Manager) clientSecrets(serverIDs []string) ([]ClientResource, error) {
	rows, err := m.st.ListSecretsInScope(store.ScopeToServers(serverIDs...))
	if err != nil {
		return nil, err
	}
	out := make([]ClientResource, 0, len(rows))
	for _, row := range rows {
		out = append(out, ClientResource{
			ID: row.ID, Name: row.Name, Note: firstNoteLine(row.Note), ViaServers: row.AllowedServers,
		})
	}
	return out, nil
}

func (m *Manager) clientShares(serverIDs []string) ([]ClientResource, error) {
	shares, err := m.st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(serverIDs))
	for _, id := range serverIDs {
		allowed[id] = true
	}
	out := make([]ClientResource, 0, len(shares))
	for _, share := range shares {
		via := make([]string, 0, len(share.MountableServers))
		for _, server := range share.MountableServers {
			if allowed[server.ID] {
				via = append(via, server.Name)
			}
		}
		if len(via) == 0 {
			continue
		}
		out = append(out, ClientResource{
			ID: share.ID, Name: share.Name, Note: firstNoteLine(share.Note), ViaServers: via,
		})
	}
	return out, nil
}

func firstNoteLine(note string) string {
	if line, _, ok := strings.Cut(note, "\n"); ok {
		return line
	}
	return note
}

func addedServers(current, desired []ServerRef) []ServerRef {
	known := make(map[string]bool, len(current))
	for _, row := range current {
		known[row.ID] = true
	}
	out := make([]ServerRef, 0, len(desired))
	for _, row := range desired {
		if !known[row.ID] {
			out = append(out, row)
		}
	}
	return out
}

func lostServers(current, desired []ServerRef) []ServerRef {
	return addedServers(desired, current)
}

func addedResources(current, desired []ClientResource) []ClientResource {
	known := make(map[string]bool, len(current))
	for _, row := range current {
		known[row.ID] = true
	}
	out := make([]ClientResource, 0, len(desired))
	for _, row := range desired {
		if !known[row.ID] {
			out = append(out, row)
		}
	}
	return out
}

func lostResources(current, desired []ClientResource) []ClientResource {
	return addedResources(desired, current)
}

// clientPairProjectName 与配对流程生成的访问身份名字保持一致。这里不能
// 调用存储包的私有函数，测试会钉住这个名字。
func clientPairProjectName(client string) string {
	return "pair-" + client
}
