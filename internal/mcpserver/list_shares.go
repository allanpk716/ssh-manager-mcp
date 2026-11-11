package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

const sharedStorageMountPointNotice = "挂载点是约定路径，本工具不检测是否已挂载"

// sharedStorageLiveCheckNotice 是开启现场检测后的固定说明。
const sharedStorageLiveCheckNotice = "已按服务器标注现场检测的挂载状态；检测命令只读，不修改远端文件"

// 每台服务器的现场检测状态值。
const (
	shareMountStatusMounted       = "mounted"
	shareMountStatusNotMounted    = "not-mounted"
	shareMountStatusConfigMissing = "config-missing"
	shareMountStatusUnreachable   = "unreachable"
	shareMountStatusNotChecked    = "not-checked"
)

// 现场检测的安全说明文本，不包含密码、地址或挂载账号。
const (
	sharedStorageWindowsNote     = "Windows 不做持久挂载；固定盘符由每条 exec_command 或 exec_background 命令前的会话内注入提供，桌面会话、Windows 服务、计划任务、文件传输和命令结束后仍在运行的进程看不到；密码不出现在目标机进程命令行中"
	sharedStorageDarwinResetNote = "系统配置被重置，再调用挂载工具即可恢复"
	sharedStorageRestrictedNote  = "服务器条目没有配置 sudo 凭据，只能确认挂载点是否可访问，未读取持久配置比较版本"
	sharedStorageRemoteNewerNote = "远端配置版本比本进程快照新，请先同步快照再调用挂载工具"
)

// sharedStorageUnreachableNote 把现场检测失败原因映射为「不可达」的固定
// 说明；未知原因按 SSH 连接失败呈现。
func sharedStorageUnreachableNote(reason string) string {
	switch reason {
	case sharemount.CheckFailureSudo:
		return "提权失败"
	case sharemount.CheckFailureUnsupportedSystem:
		return "系统不受支持"
	default:
		return "SSH 连接失败"
	}
}

// errTopologyAndSharedStorageAuthorityTooOld 是新客户端连着旧权威端时的固
// 定报错（ADR 0014：快照没有推荐版本字段时，网络拓扑与共享存储工具整体
// 降级）。
var errTopologyAndSharedStorageAuthorityTooOld = errors.New(
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
	Server      string `json:"server,omitempty" jsonschema:"optional server id or name; when given, only shares this server may mount are returned, each carrying only that server's mount point"`
	LiveCheck   bool   `json:"live_check,omitempty" jsonschema:"optional; when true, concurrently run fixed read-only on-site mount checks on visible Linux and macOS servers (each server with its own timeout) and annotate every server entry with its mount status"`
	CheckServer string `json:"check_server,omitempty" jsonschema:"optional server id or name; requires live_check=true and narrows the on-site check to this one server"`
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
	// MountStatus 与 MountNotes 只在现场检测开启时填写。
	MountStatus string   `json:"mount_status,omitempty" jsonschema:"present only with live_check: mounted, not-mounted, config-missing, unreachable, or not-checked"`
	MountNotes  []string `json:"mount_notes,omitempty" jsonschema:"safe follow-up notes for the mount status; never an IP address, domain name, or secret"`
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
// 器列表先与 profile 授予的服务器取交集，交集为空的共享不返回。默认只读
// 登记数据，不做任何网络或 SSH 动作；只有调用方开启现场检测时才并发检
// 测 Linux 与 macOS 服务器。
func ListSharesForProfile(
	ctx context.Context, st *store.Store, profileID string, input ListSharesInput,
	hk sshbroker.HostKeyStore,
) (ListSharesOutput, error) {
	if st.IsReadOnly() && st.CacheRecommendedVersion() == "" {
		return emptyListSharesOutput(), errTopologyAndSharedStorageAuthorityTooOld
	}
	if input.CheckServer != "" && !input.LiveCheck {
		return emptyListSharesOutput(), errors.New("只检测指定服务器时必须开启现场检测，请同时设置 live_check 为 true")
	}
	visible, err := sessionVisibleServerIDs(st, profileID, input.Server)
	if err != nil {
		return emptyListSharesOutput(), err
	}
	shares, err := st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return emptyListSharesOutput(), err
	}
	out := ListSharesOutput{Shares: []SharedStorageInfo{}}
	views := []sharedStorageShareView{}
	for _, share := range shares {
		servers := []SharedStorageServerInfo{}
		entries := []models.SharedStorageServer{}
		for _, mountable := range share.MountableServers {
			if !visible[mountable.ID] {
				continue
			}
			servers = append(servers, SharedStorageServerInfo{
				ServerID: mountable.ID, Name: mountable.Name, MountPoint: mountable.MountPoint,
			})
			entries = append(entries, mountable)
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
		views = append(views, sharedStorageShareView{share: share, entries: entries})
	}
	if !input.LiveCheck {
		out.Notice = sharedStorageMountPointNotice
		return out, nil
	}
	out.Notice = sharedStorageLiveCheckNotice
	if err := annotateSharedStorageMountChecks(
		ctx, st, profileID, input.CheckServer, hk, &out, views,
	); err != nil {
		return emptyListSharesOutput(), err
	}
	return out, nil
}

// sharedStorageShareView 把一个进入输出的共享与它实际可见的服务器条目
// 放在一起；现场检测的索引必须基于裁剪后的输出顺序，不能基于登记里的
// 全部可挂载服务器。
type sharedStorageShareView struct {
	share   *models.SharedStorage
	entries []models.SharedStorageServer
}

// sharedStorageCheckTarget 是一台待现场检测的服务器：它需要检测的共享与
// 这些共享在输出里的位置。
type sharedStorageCheckTarget struct {
	server *models.Server
	shares []*models.SharedStorage
	refs   []sharedStorageCheckRef
}

type sharedStorageCheckRef struct {
	shareIndex int
	entryIndex int
}

// annotateSharedStorageMountChecks 对输出里的 Linux 与 macOS 服务器并发
// 执行只读现场检测；Windows 服务器直接标注不检测。检测不写审计，某台
// 失败只影响它自己的状态。
func annotateSharedStorageMountChecks(
	ctx context.Context, st *store.Store, profileID, checkServer string,
	hk sshbroker.HostKeyStore, out *ListSharesOutput, views []sharedStorageShareView,
) error {
	checkOnlyID := ""
	if checkServer != "" {
		target, err := st.ResolveGrantedServer(profileID, checkServer)
		if err != nil {
			return err
		}
		if target == nil {
			return ErrNotInProfile
		}
		checkOnlyID = target.ID
	}
	targets := map[string]*sharedStorageCheckTarget{}
	order := []string{}
	annotatedWindows := false
	for shareIndex, view := range views {
		for entryIndex, mountable := range view.entries {
			if checkOnlyID != "" && mountable.ID != checkOnlyID {
				continue
			}
			if models.NormalizeShellType(string(mountable.ShellType)).IsWindows() {
				entry := &out.Shares[shareIndex].Servers[entryIndex]
				entry.MountStatus = shareMountStatusNotChecked
				entry.MountNotes = []string{sharedStorageWindowsNote}
				annotatedWindows = true
				continue
			}
			target := targets[mountable.ID]
			if target == nil {
				server, err := st.GetServer(mountable.ID)
				if err != nil {
					return err
				}
				if server == nil {
					return fmt.Errorf("服务器条目 %s 不存在", mountable.ID)
				}
				target = &sharedStorageCheckTarget{server: server}
				targets[mountable.ID] = target
				order = append(order, mountable.ID)
			}
			target.shares = append(target.shares, view.share)
			target.refs = append(target.refs, sharedStorageCheckRef{
				shareIndex: shareIndex, entryIndex: entryIndex,
			})
		}
	}
	if checkOnlyID != "" && len(order) == 0 && !annotatedWindows {
		return errors.New("指定的检测服务器不在当前可见共享的可挂载范围内")
	}

	service := &sharemount.Service{Store: st, Instance: bridgeCacheInstance(), HostKeys: hk}
	results := make([]*sharemount.CheckResult, len(order))
	failures := make([]*sharemount.CheckFailure, len(order))
	var wg sync.WaitGroup
	for index, id := range order {
		target := targets[id]
		wg.Add(1)
		go func(index int, target *sharedStorageCheckTarget) {
			defer wg.Done()
			result, err := service.Check(ctx, sharemount.CheckRequest{
				Server: target.server, Shares: target.shares,
			})
			if err != nil {
				var checkFailure *sharemount.CheckFailure
				if errors.As(err, &checkFailure) {
					failures[index] = checkFailure
				}
				fmt.Fprintf(os.Stderr, "sshmgr: list_shares 现场检测 %s 失败：%v\n",
					target.server.Name, err)
				return
			}
			results[index] = result
		}(index, target)
	}
	wg.Wait()

	for index, id := range order {
		result := results[index]
		for stateIndex, ref := range targets[id].refs {
			state := sharemount.CheckShareState{}
			if result != nil && stateIndex < len(result.States) {
				state = result.States[stateIndex]
			}
			status, notes := sharedStorageMountPresentation(views[ref.shareIndex].share, state, result)
			if result == nil && status == shareMountStatusUnreachable {
				note := sharedStorageUnreachableNote("")
				if failures[index] != nil {
					note = sharedStorageUnreachableNote(failures[index].Reason)
				}
				notes = []string{note}
			}
			entry := &out.Shares[ref.shareIndex].Servers[ref.entryIndex]
			entry.MountStatus = status
			entry.MountNotes = notes
		}
	}
	return nil
}

// sharedStorageMountPresentation 把一台服务器上一个共享的检测结论映射成
// 安全输出：状态只取固定枚举，说明只包含版本号与下一步动作。
func sharedStorageMountPresentation(
	share *models.SharedStorage, state sharemount.CheckShareState, result *sharemount.CheckResult,
) (string, []string) {
	if result == nil {
		return shareMountStatusUnreachable, nil
	}
	if result.Restricted {
		status := shareMountStatusNotMounted
		if state.Mounted {
			status = shareMountStatusMounted
		}
		return status, []string{sharedStorageRestrictedNote}
	}
	if !state.Managed {
		return shareMountStatusConfigMissing, nil
	}
	var notes []string
	darwinReset := result.System == sharemount.CheckSystemDarwin && !result.AutoMasterPresent
	if darwinReset {
		notes = append(notes, sharedStorageDarwinResetNote)
	}
	switch {
	case state.Version < share.Version:
		notes = append(notes, fmt.Sprintf(
			"配置版本落后：远端版本 %d，快照版本 %d，再调用挂载工具即可更新",
			state.Version, share.Version))
	case state.Version > share.Version:
		notes = append(notes, sharedStorageRemoteNewerNote)
	}
	if !state.Mounted || darwinReset {
		return shareMountStatusNotMounted, notes
	}
	return shareMountStatusMounted, notes
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
