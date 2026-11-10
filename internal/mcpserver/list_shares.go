package mcpserver

import (
	"errors"

	"ssh-manager-mcp/internal/store"
)

const sharedStorageMountPointNotice = "挂载点是约定路径，本工具不检测是否已挂载"

// errSharedStorageAuthorityTooOld 是新客户端连着旧权威端时的固定报错
// （ADR 0014：快照没有推荐版本字段时，网络拓扑与共享存储工具整体降级）。
var errSharedStorageAuthorityTooOld = errors.New(
	"权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端",
)

// emptyListSharesOutput 返回能通过输出结构校验的空结果。报错路径也需要
// 它：零值里的空指针数组会被序列化成 JSON null，工具输出校验要求的是
// 数组。
func emptyListSharesOutput() ListSharesOutput {
	return ListSharesOutput{Shares: []SharedStorageInfo{}}
}

// ListSharesInput 是 list_shares 工具的输入。
type ListSharesInput struct {
	Server string `json:"server,omitempty" jsonschema:"optional server id or name; when given, only shares this server may mount are returned, each carrying only that server's mount point"`
}

// SharedStorageDirectoryInfo 是 owner 登记的一条目录约定：用途对应共享
// 内部的子路径。
type SharedStorageDirectoryInfo struct {
	Purpose string `json:"purpose" jsonschema:"owner-registered purpose of the subdirectory"`
	Path    string `json:"path" jsonschema:"subdirectory path inside the SMB share"`
}

// SharedStorageServerInfo 是一台可挂载这处共享的服务器条目，携带按这台
// 服务器的 shell 类型推算出的约定挂载点。
type SharedStorageServerInfo struct {
	ServerID   string `json:"server_id" jsonschema:"stable server id — reuse this across tools"`
	Name       string `json:"name" jsonschema:"server entry name"`
	MountPoint string `json:"mount_point" jsonschema:"agreed mount point for this server's shell type"`
}

// SharedStorageInfo 是 agent 可见的一处共享存储登记。挂载密码的值、挂载
// 账号用户名与候选访问地址都刻意不在其中。
type SharedStorageInfo struct {
	Name        string                       `json:"name" jsonschema:"share name"`
	Device      string                       `json:"provider_device" jsonschema:"name of the registered device that provides the share"`
	Note        string                       `json:"note" jsonschema:"owner note"`
	Directories []SharedStorageDirectoryInfo `json:"directories" jsonschema:"owner-registered directory agreements"`
	Drive       string                       `json:"drive" jsonschema:"fixed Windows drive letter"`
	Version     int64                        `json:"version" jsonschema:"share registration version; it only ever increases"`
	ChangeNote  string                       `json:"change_note" jsonschema:"owner note for the current version"`
	Servers     []SharedStorageServerInfo    `json:"servers" jsonschema:"servers in your session scope that may mount this share"`
}

// ListSharesOutput 是 list_shares 工具的输出。
type ListSharesOutput struct {
	MaskedOutput
	Shares []SharedStorageInfo `json:"shares" jsonschema:"shares visible in this session"`
	Notice string              `json:"notice" jsonschema:"fixed notice about mount points"`
}

// ListSharesForProfile 返回当前会话可见的共享存储。每个共享的可挂载服务
// 器列表先与 profile 授予的服务器取交集，交集为空的共享不返回。这里只读
// 登记数据，不做任何网络或 SSH 动作。
func ListSharesForProfile(st *store.Store, profileID, serverIdentifier string) (ListSharesOutput, error) {
	if st.IsReadOnly() && st.CacheRecommendedVersion() == "" {
		return emptyListSharesOutput(), errSharedStorageAuthorityTooOld
	}
	visible, err := sessionVisibleServerIDs(st, profileID, serverIdentifier)
	if err != nil {
		return emptyListSharesOutput(), err
	}
	shares, err := st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return emptyListSharesOutput(), err
	}
	out := ListSharesOutput{Shares: []SharedStorageInfo{}, Notice: sharedStorageMountPointNotice}
	for _, share := range shares {
		servers := []SharedStorageServerInfo{}
		for _, mountable := range share.MountableServers {
			if !visible[mountable.ID] {
				continue
			}
			servers = append(servers, SharedStorageServerInfo{
				ServerID: mountable.ID, Name: mountable.Name, MountPoint: mountable.MountPoint,
			})
		}
		if len(servers) == 0 {
			continue
		}
		directories := make([]SharedStorageDirectoryInfo, 0, len(share.Directories))
		for _, directory := range share.Directories {
			directories = append(directories, SharedStorageDirectoryInfo{
				Purpose: directory.Purpose, Path: directory.Path,
			})
		}
		out.Shares = append(out.Shares, SharedStorageInfo{
			Name: share.Name, Device: share.DeviceName, Note: share.Note,
			Directories: directories, Drive: share.Drive, Version: share.Version,
			ChangeNote: share.ChangeNote, Servers: servers,
		})
	}
	return out, nil
}

// sessionVisibleServerIDs 返回 profile 授予的服务器编号集合。serverIdentifier
// 非空（服务器编号或名字）时收窄成这一台；解析不到授权范围内的服务器时，
// 使用全部工具共用的 profile 门禁报错。
func sessionVisibleServerIDs(st *store.Store, profileID, serverIdentifier string) (map[string]bool, error) {
	if serverIdentifier != "" {
		target, err := st.ResolveGrantedServer(profileID, serverIdentifier)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, ErrNotInProfile
		}
		return map[string]bool{target.ID: true}, nil
	}
	ids, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
