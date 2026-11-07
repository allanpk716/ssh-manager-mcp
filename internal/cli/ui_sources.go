package cli

import (
	"encoding/json"
	"io"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/updater"
	"ssh-manager-mcp/internal/webui"
)

// standaloneUIServerSource 是单机角色的 Web UI 服务器列表来源：每次请求重新
// 打开本机 vault 读取服务器条目，并按共享的「需要注意」规则标记。
type standaloneUIServerSource struct{}

// Servers 读取本机 vault 中的全部服务器并转成 Web UI 展示行。
func (standaloneUIServerSource) Servers() ([]webui.ServerRow, error) {
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
		rows = append(rows, webui.ServerRow{
			ID:             srv.ID,
			Name:           srv.Name,
			User:           srv.User,
			Host:           srv.Host,
			Port:           srv.Port,
			NeedsAttention: models.ServerNeedsAttention(srv.CredentialID, srv.Role, srv.Tags),
			Role:           srv.Role,
			Description:    srv.Description,
		})
	}
	return rows, nil
}

// clientUIServerSource 是客户端角色的 Web UI 服务器列表来源：读取指定实例的
// 本地加密快照，实例由命令行按既有规则解析后传入。
type clientUIServerSource struct {
	instance string
}

// Servers 读取当前实例本地快照中的服务器并转成 Web UI 展示行。
func (s clientUIServerSource) Servers() ([]webui.ServerRow, error) {
	snap, err := clientops.LoadCacheSnapshotFor(s.instance)
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerRow, 0, len(snap.Servers))
	for i := range snap.Servers {
		srv := &snap.Servers[i]
		rows = append(rows, webui.ServerRow{
			ID:             srv.ID,
			Name:           srv.Name,
			User:           srv.User,
			Host:           srv.Host,
			Port:           srv.Port,
			NeedsAttention: models.ServerNeedsAttention(srv.CredentialID, srv.Role, snapshotServerTags(srv.TagsRaw)),
			Role:           srv.Role,
			Description:    srv.Description,
		})
	}
	return rows, nil
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

// uiVersionHintSource 给 Web UI 提供客户端版本提示文字，复用命令行结尾提示
// 的判定与文案；空串表示不显示（非客户端、无快照、版本一致或不可解析）。
func uiVersionHintSource(instance string) string {
	state, err := readClientVersionState(instance, io.Discard)
	if err != nil || !state.IsClient || state.Snapshot == nil {
		return ""
	}
	return updater.VersionMismatchHint(buildinfo.Version, state.Snapshot.RecommendedVersion)
}
