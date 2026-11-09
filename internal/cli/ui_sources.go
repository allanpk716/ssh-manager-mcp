package cli

import (
	"encoding/json"
	"io"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/secretmanage"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/updater"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
	"ssh-manager-mcp/internal/webuiops"
	"ssh-manager-mcp/internal/webuisecrets"
	"ssh-manager-mcp/internal/webuiservers"
)

// standaloneUIServerSource 是单机角色的 Web UI 服务器列表来源：每次请求重新
// 打开本机 vault 读取服务器条目，并按共享的「需要注意」规则标记。
type standaloneUIServerSource struct{}

// Servers 读取本机 vault 中的全部服务器并转成 Web UI 展示行。
func (standaloneUIServerSource) Servers(string) ([]webui.ServerRow, error) {
	st, err := openUnlockedStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerRow, 0, len(servers))
	for _, srv := range servers {
		rows = append(rows, webUIServerRow(srv.ID, srv.Name, srv.User, srv.Host, srv.Port, srv.CredentialID, srv.Role, srv.Tags))
	}
	return rows, nil
}

// ServerList 返回带注意原因的服务器列表；供过滤视图使用。单机角色只有
// 一个本机 vault，忽略客户端实例名。
func (standaloneUIServerSource) ServerList(_ string, attentionOnly bool) ([]webui.ServerListEntry, error) {
	st, err := openUnlockedStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return webuiservers.ListEntries(servermanage.New(st), attentionOnly)
}

// ServerDetail 返回一台服务器的详情；单机角色忽略客户端实例名。
func (standaloneUIServerSource) ServerDetail(_ string, identifier string) (*webui.ServerDetailView, error) {
	st, err := openUnlockedStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return webuiservers.DetailView(servermanage.New(st), identifier)
}

// standaloneUIServerWriter 给单机 Web UI 提供服务器写入口。每次写操作重新
// 打开本机 vault，请求结束后关闭。
func standaloneUIServerWriter() webui.ServerWriter {
	return webuiservers.WriterFromOpener(func() (*servermanage.Manager, func(), error) {
		st, err := openUnlockedStore()
		if err != nil {
			return nil, nil, err
		}
		return servermanage.New(st), func() { _ = st.Close() }, nil
	})
}

// standaloneUIAccessStore 给单机 Web UI 提供访问管理入口。每次读写重新打开
// 本机 vault，请求结束后关闭，不长期持有存储句柄。
func standaloneUIAccessStore() webui.AccessStore {
	return webuiaccess.FromOpener(func() (*access.Manager, func(), error) {
		st, err := openUnlockedStore()
		if err != nil {
			return nil, nil, err
		}
		return access.New(st), func() { _ = st.Close() }, nil
	})
}

// standaloneUISecretStore 给单机 Web UI 提供机密项读取来源。每次读取重新
// 打开本机 vault，结束后关闭。
func standaloneUISecretStore() webui.SecretSource {
	return webuisecrets.FromOpener(openSecretManager)
}

// standaloneUISecretWriter 给单机 Web UI 提供机密项写入口。每次写操作重新
// 打开本机 vault，请求结束后关闭。
func standaloneUISecretWriter() webui.SecretWriter {
	return webuisecrets.WriterFromOpener(openSecretManager)
}

func openSecretManager() (*secretmanage.Manager, func(), error) {
	st, err := openUnlockedStore()
	if err != nil {
		return nil, nil, err
	}
	return secretmanage.New(st), func() { _ = st.Close() }, nil
}

// standaloneUIOpsStore 给单机 Web UI 提供运维入口。每次读取重新打开本机
// vault，请求结束后关闭，不长期持有存储句柄。
func standaloneUIOpsStore() webui.OpsStore {
	return webuiops.FromOpener(func() (*ops.Manager, func(), error) {
		st, err := openUnlockedStore()
		if err != nil {
			return nil, nil, err
		}
		return ops.NewWithSettingInputs(st, ops.SettingInputsFromEnv()), func() { _ = st.Close() }, nil
	})
}

// clientUIServerSource 是客户端角色的 Web UI 服务器列表来源：按每次请求传入
// 的实例读取本地加密快照。
type clientUIServerSource struct{}

// clientUISecretSource 是客户端角色的机密项读取来源：按每次请求传入的实
// 例读取本地加密快照的无值视图。
type clientUISecretSource struct{}

// Servers 读取当前实例本地快照中的服务器并转成 Web UI 展示行。
func (clientUIServerSource) Servers(instance string) ([]webui.ServerRow, error) {
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerRow, 0, len(snap.Servers))
	for i := range snap.Servers {
		srv := &snap.Servers[i]
		rows = append(rows, webUIServerRow(srv.ID, srv.Name, srv.User, srv.Host, srv.Port, srv.CredentialID, srv.Role, snapshotServerTags(srv.TagsRaw)))
	}
	return rows, nil
}

// webUIServerRow 把服务器连接信息与标签转换成 Web UI 的展示行。
func webUIServerRow(id, name, user, host string, port int, credentialID, role string, tags []string) webui.ServerRow {
	return webui.ServerRow{
		ID:             id,
		Name:           name,
		User:           user,
		Host:           host,
		Port:           port,
		NeedsAttention: models.ServerNeedsAttention(credentialID, role, tags),
	}
}

// snapshotServerTags 解析快照服务器条目里按 JSON 数组字符串存放的标签；解析
// 失败时按无标签处理，不影响列表展示。
func snapshotServerTags(raw string) []string {
	if raw == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil
	}
	return tags
}

// ServerList 按传入客户端实例的本地快照返回带注意原因的服务器列表。
func (s clientUIServerSource) ServerList(instance string, attentionOnly bool) ([]webui.ServerListEntry, error) {
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerListEntry, 0, len(snap.Servers))
	for i := range snap.Servers {
		srv := &snap.Servers[i]
		tags := snapshotServerTags(srv.TagsRaw)
		reasons := servermanage.ReasonLabels(models.ServerAttentionReasons(srv.CredentialID, srv.Role, tags))
		if attentionOnly && len(reasons) == 0 {
			continue
		}
		rows = append(rows, webui.ServerListEntry{
			ID: srv.ID, Name: srv.Name, User: srv.User, Host: srv.Host, Port: srv.Port,
			AttentionReasons: reasons,
		})
	}
	return rows, nil
}

// ServerDetail 按传入客户端实例的本地快照返回一台服务器的详情；凭据状态
// 也以快照内容为准，不显示任何凭据内容。
func (s clientUIServerSource) ServerDetail(instance, identifier string) (*webui.ServerDetailView, error) {
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil {
		return nil, err
	}
	credentialType := map[string]string{}
	hasPassphrase := map[string]bool{}
	for _, cred := range snap.Credentials {
		credentialType[cred.ID] = cred.Type
		hasPassphrase[cred.ID] = len(cred.Passphrase) > 0
	}
	deviceNames := map[string]string{}
	for _, device := range snap.Devices {
		deviceNames[device.ID] = device.Name
	}
	for i := range snap.Servers {
		srv := &snap.Servers[i]
		if srv.ID != identifier && srv.Name != identifier {
			continue
		}
		tags := snapshotServerTags(srv.TagsRaw)
		return &webui.ServerDetailView{
			ID: srv.ID, Name: srv.Name, User: srv.User, Host: srv.Host, Port: srv.Port,
			Shell:          string(models.NormalizeShellType(srv.ShellType)),
			CredentialType: credentialType[srv.CredentialID], HasKeyPassphrase: hasPassphrase[srv.CredentialID],
			HasSudoPassword: srv.SudoCredentialID != "", Description: srv.Description,
			Location: srv.Location, Hardware: srv.Hardware, Services: srv.Services,
			Role: srv.Role, Caveats: srv.Caveats, Tags: tags, ExposeHost: srv.ExposeHost,
			DeviceName: deviceNames[srv.DeviceID], HasDevice: deviceNames[srv.DeviceID] != "",
			AttentionReasons: servermanage.ReasonLabels(models.ServerAttentionReasons(srv.CredentialID, srv.Role, tags)),
			Revision:         srv.Revision,
		}, nil
	}
	return nil, nil
}

// ListSecrets 读取当前实例快照中的机密项可见字段；客户端没有写入口。
func (clientUISecretSource) ListSecrets(instance, query, server string) ([]webui.SecretRow, error) {
	return webuisecrets.SnapshotSource(clientops.LoadCacheSnapshotFor).ListSecrets(instance, query, server)
}

// uiVersionHintSource 给 Web UI 提供客户端版本提示文字，复用命令行结尾提示
// 的判定与文案；空串表示不显示（非客户端、无快照、版本一致或不可解析）。
func uiVersionHintSource(instance string) string {
	state, err := readClientVersionState(instance, io.Discard)
	if err != nil || !state.IsClient || state.Snapshot == nil {
		return ""
	}
	return updater.VersionMismatchHint(buildinfo.Version, state.Snapshot.RecommendedVersion)
}
