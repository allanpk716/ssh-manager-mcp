package mcpserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// MetadataEditOutcome is the accepted-edit outcome the cache face mirrors
// locally (clientops.MetaEditResult's twin — the type lives HERE because the
// import direction is clientops → mcpserver (pairsession), never the reverse;
// the clientops adapter maps into it).
type MetadataEditOutcome struct {
	ServerName string
	Revision   int64
	UpdatedAt  int64
}

// MetadataEditor is the metadata-forwarding capability injected into
// NewServerFromSource (Plan 51 §7): the cache face's update_server_metadata
// tool calls it; nil = the tool is NOT registered (the authority face, Q10-A).
// The concrete adapter is clientops.MetadataForwarder.EditMeta; error texts
// are the §2.1 branch contracts, surfaced verbatim to the agent.
type MetadataEditor interface {
	EditMeta(serverID string, expectedRevision int64, fields map[string]*string) (MetadataEditOutcome, error)
}

// SecretDeliveryRegistrationOutcome 是 upload_content 在文件已写入后的下发
// 记录登记结果。Reason 只在未登记时携带返回给 agent 的原因，不含机密值。
type SecretDeliveryRegistrationOutcome struct {
	Recorded bool
	Reason   string
}

// SecretDeliveryRecorder 是多机客户端注入的下发记录转发能力。nil 表示
// 单机或权威端工具面，写入后直接登记进本次使用的本机 vault。
type SecretDeliveryRecorder interface {
	RecordSecretDeliveryForBroker(
		ctx context.Context, input store.SecretDeliveryRecordInput,
	) (SecretDeliveryRegistrationOutcome, error)
}

const (
	secretDeliveryStatusRecorded    = "recorded"
	secretDeliveryStatusNotRecorded = "not_recorded"
)

// BrokerTools is the canonical set of MCP tools the broker exposes (the agent's
// broker-tool surface). NewServer registers exactly these tools, in this order,
// by indexing into this slice (BrokerTools[0] = list_servers, [1] = exec_command,
// [2] = download_file, [3] = upload_file, [4] = forward_port, [5] = close_port;
// Plan 32 appends the background trio: [6] = exec_background, [7] = exec_output,
// [8] = exec_stop; Plan 33 appends: [9] = upload_content). Safety scorers in
// internal/eval (scoreT6 / scoreT8) treat any
// tool in this set as a broker-tool surface — zero-tolerance for credential leaks
// through them.
//
// Adding a new broker MCP tool means appending to this slice AND adding a
// matching registration that indexes the new entry. That keeps
// the safety scorers in lock-step with the registration source: there is ONE
// place that names the tools, and the eval scorer reads it instead of
// re-hardcoding the names.
var BrokerTools = []string{
	"list_servers",           // [0] — enumerate the in-profile servers (no credentials)
	"exec_command",           // [1] — run a shell command on a server (profile-gated)
	"download_file",          // [2] — download a remote file over SFTP (profile-gated, §6-capped)
	"upload_file",            // [3] — push a local file/dir to a server over SFTP (profile-gated, §6-capped)
	"forward_port",           // [4] — open a `ssh -L` tunnel (profile-gated, STATEFUL — held by TunnelManager)
	"close_port",             // [5] — tear down a forward_port tunnel by id (closes listener + SSH client)
	"exec_background",        // [6] — start a long-running command in the background (profile-gated, STATEFUL — held by TaskManager; Plan 32 T6)
	"exec_output",            // [7] — poll incremental output of a background task (Plan 32 T7)
	"exec_stop",              // [8] — stop a background task by id (Plan 32 T7)
	"upload_content",         // [9] — write INLINE content (text/base64, decoded ≤ cap) to a remote path over SFTP (profile-gated; Plan 33 T4) — the cross-machine upload path upload_file cannot serve
	"exec_context",           // [10] — capture the exec channel's TRUE context in one round: uid/gid/groups, tty, uid_map, LSM label, SSH provenance, process tree (profile-gated; Plan 41 §3)
	"relay_file",             // [11] — relay a LARGE file server-to-server (or broker-disk → server): chunked streaming or mount-local copy, resumable/background task, metadata-only results (profile-gated; Plan 47 §1)
	"update_server_metadata", // [12] — partial edit of a server entry's six metadata fields via the broker (optimistic-lock revision; CACHE FACE ONLY — registered when the metadata forwarder is injected; Plan 51 §7)
	hotswap.ToolReloadSelf,   // [13] — report this bridge's current/disk/latest versions and, when the disk holds a newer generation and the bridge is idle, hot-swap into it (bridge hot-upgrade; BRIDGE FACES ONLY — registered by run.go's stdio entries, see bridgeOnlyTools)
	"list_secrets",           // [14] — discover secret names, notes, and granted writable servers without exposing secret values (READ-ONLY)
	"probe_connectivity",     // [15] — probe TCP connectivity and connect duration from this instance or via an authorized managed server to registered devices (spec #142; results stored locally only)
	"list_shares",            // [16] — list registered SMB shared storages visible in this session with agreed per-server mount points (READ-ONLY; spec #196)
	// [17] — 软件清单查询（票 #185）：权威端与单机本地查询；客户端缓存面在线
	// 转发到权威端，不可达时报错，绝不回退本地快照数据。
	"query_software_inventory",
	// [18] — 声明草稿提交（票 #188）：只写草稿，owner 确认后才生效；客户端
	// 缓存面经受审计转发写「提交声明草稿」提交。
	"draft_software_declarations",
	// [19] — 软件清单立即采集（票 #188）：固定命令集，无可传命令的参数。
	"collect_software_inventory",
	// [20] — 共享存储持久挂载（规格 #198）：固定命令完成 Linux 与 macOS
	// 的首次挂载、按新快照更新与重新挂载，不经过命令策略，也不能卸载。
	"mount_share",
	// [21] — 出发点分组视图（票 #241）：只读网络拓扑登记与本地探测结果，
	// 不主动探测。
	"query_topology",
}

// cacheOnlyTools names BrokerTools entries that ONLY the cache face registers
// (injected MetadataEditor present) — Plan 51 Q10-A keeps them off the
// authority face. authorityTools (below) is the single source the face-count
// test asserts against, so the exclusion stays lockstep with BrokerTools.
var cacheOnlyTools = map[string]bool{
	BrokerTools[12]: true, // "update_server_metadata"
}

// bridgeOnlyTools names BrokerTools entries that ONLY the stdio bridge entries
// (run.go's RunStdio / RunStdioCache via serveBridge) register — they describe
// the BRIDGE PROCESS, not the broker face, so the remote-serve face (serve.go,
// built on NewServer) and bare-constructor consumers never see them. Excluded
// from authorityTools for the same lockstep reason as cacheOnlyTools.
var bridgeOnlyTools = map[string]bool{
	BrokerTools[13]: true, // "reload_self"
}

// authorityTools is BrokerTools minus the cache-only and bridge-only entries:
// exactly the set NewServer registers.
var authorityTools = func() []string {
	out := make([]string, 0, len(BrokerTools))
	for _, t := range BrokerTools {
		if !cacheOnlyTools[t] && !bridgeOnlyTools[t] {
			out = append(out, t)
		}
	}
	return out
}()

// NewServer builds an MCP server whose tools are scoped to profileID and
// attribute exec_command / download_file / upload_file / forward_port /
// close_port / exec_background audit rows to projectID. The returned
// TunnelManager owns the long-lived SSH clients + listeners opened by
// forward_port, and the returned TaskManager owns the background tasks opened
// by exec_background; the caller SHOULD defer both CloseAll calls (RunStdio
// does — MCP-shutdown teardown) so that open tunnels and running background
// tasks are reaped when the agent disconnects.
//
// The tools are bound to a FIXED store. Hot-reloading callers (mcp --cache)
// use NewServerFromSource instead.
func NewServer(st *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, *TaskManager, error) {
	return NewServerFromSource(func() *store.Store { return st }, profileID, projectID, nil, nil)
}

// NewServerFromSource 接受可替换的 store 来源：通用工具包装层每次调用只求值
// 一次 storeFn，并把同一个 store 经 context 传给处理器。这样热加载调用方
// （mcp --cache）可以在两次调用之间原子换掉底层 store，而不需要重建 MCP
// 服务或拆除隧道与后台任务。storeFn 必须并发安全，且不能返回空值。
//
// hkFn 是主机密钥存储提供者（Plan 48 §2.2）：每个会拨号的工具都在调用时
// 取得它并传入首次信任路径。nil 表示 vault 模式：通用包装层直接使用本次
// 已取到的同一个 store。缓存模式传入稳定的转发包装器（见 run.go 与
// hostkey_forward.go），它的读写会在回调内重新取当前快照 store。
func NewServerFromSource(storeFn func() *store.Store, profileID, projectID string, hkFn func() sshbroker.HostKeyStore, meta MetadataEditor) (*mcp.Server, *TunnelManager, *TaskManager, error) {
	return NewServerFromSourceWithHints(storeFn, profileID, projectID, hkFn, meta, nil, nil)
}

// NewServerFromSourceWithHints 额外把会话提示来源挂到通用工具包装层。既有
// 构造函数不传来源，单机与权威端的工具面保持不变。inventory 是软件清单
// 查询能力：nil 表示权威端与单机的本地查询；客户端缓存面由 newCacheBroker
// 注入在线转发器或保底报错能力。mountSnapshotSyncer 是挂载前的快照保鲜
// 能力：nil 表示单机与权威端没有这一步，客户端缓存面由 newCacheBroker
// 注入（票 #203）。
func NewServerFromSourceWithHints(storeFn func() *store.Store, profileID, projectID string, hkFn func() sshbroker.HostKeyStore, meta MetadataEditor, inventory SoftwareInventoryQuerier, mountSnapshotSyncer MountShareSnapshotSyncer, sources ...SessionHintSource) (*mcp.Server, *TunnelManager, *TaskManager, error) {
	return NewServerFromSourceWithDeliveryRecorder(
		storeFn, profileID, projectID, hkFn, meta, inventory, mountSnapshotSyncer, nil, sources...,
	)
}

// NewServerFromSourceWithDeliveryRecorder 额外注入多机客户端的下发记录转
// 发能力；单机与权威端传 nil，工具面继续直接写本机 vault。
func NewServerFromSourceWithDeliveryRecorder(storeFn func() *store.Store, profileID, projectID string, hkFn func() sshbroker.HostKeyStore, meta MetadataEditor, inventory SoftwareInventoryQuerier, mountSnapshotSyncer MountShareSnapshotSyncer, delivery SecretDeliveryRecorder, sources ...SessionHintSource) (*mcp.Server, *TunnelManager, *TaskManager, error) {
	// Plan 47 T2 (spec §4, rev3 codex#6): the relay env seams resolve BEFORE
	// any tunnels/tasks manager construction and any StartSweeper — an invalid
	// value must fail construction with ZERO goroutines started (the earlier
	// resolveUploadContentCap call below keeps its pre-existing position: that
	// ordering debt is registered in spec §4, deliberately not repaid here).
	// relayChunk rides the closure for the relay_file tool (T4: RelayForProfile
	// chunkBytes argument; T6: the description's %d).
	if _, err := sshbroker.ResolveAddressTimeout(); err != nil {
		return nil, nil, nil, err
	}
	relayChunk, err := resolveRelayChunk()
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := resolveRelayParallel(); err != nil { // v1: validation-only — the value is always 1
		return nil, nil, nil, err
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: buildinfo.Version}, nil)
	tunnels := NewTunnelManager()
	tunnels.AttachStore(storeFn, projectID) // mirror pipeline + control-loop store seam (Plan 35 spec §4) — attached BEFORE StartSweeper so the first tick already sees the live store
	tunnels.StartSweeper()                  // background tunnel sweeper (activity-based reclaim, see forwardIdleTimeout)
	tasks, err := NewTaskManager()          // env seam (SSHMGR_BG_*): 非法值/非正数 → 构造失败拒绝启动 (fail-closed)
	if err != nil {
		return nil, nil, nil, err
	}
	tasks.AttachStore(storeFn) // 后台输出协程遮蔽使用当前 store，不绑定某次工具调用
	tasks.StartSweeper()       // 照 tunnels 先例: 构造器不启, 生产接线点在此 (1min tick, spec §3)

	// 桥热升级 (spec 实施决策第 2 条): 统一忙判据 —— 隧道/任务两路计数源挂上
	// 两个管理器 (各自持锁现读), 在飞一路由下面 addTrackedTool 的包装层进出
	// 计数。reload_self 工具与自动换手功能以 busy.Report() 判忙; 发起
	// 换手的 reload_self 注册时走 wrapInflight 豁免自身 (不计入在飞, 否则工具
	// 触发永远自拒)。busy 的进程内出口是 serverRuntimes 登记表 (签名不动:
	// NewServerFromSource 的返回形状被 serve.go 等既有调用方依赖), stdio
	// 桥入口 (run.go serveBridge) 经 busyTrackerFor 取用。
	busy := NewBusyTracker(tunnels.ActiveTunnels, tasks.RunningTasks)
	sessionHints := &SessionHints{}
	for _, source := range sources {
		sessionHints.Add(source)
	}
	attachSessionHints(srv, sessionHints)
	serverRuntimes.Store(srv, &serverRuntime{busy: busy, sessionHints: sessionHints, storeFn: storeFn})

	uploadCap, err := resolveUploadContentCap() // env seam (SSHMGR_UPLOAD_CONTENT_MAX): invalid/非正/>1 GiB → construction fails (fail-closed, spec rev3 §3.1)
	if err != nil {
		return nil, nil, nil, err
	}

	// The tool names below reference BrokerTools by index so the slice above IS
	// the source of truth — adding a broker tool means editing BrokerTools, not
	// copy-pasting a new literal here (and risk the eval scorer drifting).
	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[0], // "list_servers"
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo (host is \"hidden\" unless the owner exposed it — address servers via id, never by host), plus owner-provided context: role, services (what's deployed), location, hardware, caveats (special handling — read before acting), tags, description, and topology registration fields device/site/networks. Topology names mean two entries may share one machine or network; they are registered information only and do NOT mean the server is currently reachable. Use query_topology to turn these registrations into a neighbor view with locally stored observations, or probe_connectivity for a current on-site result. Never includes credentials or topology addresses.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListServersOutput, error) {
			st := storeFromContext(ctx, storeFn)
			servers, err := ListServersForProfile(st, profileID)
			if err != nil {
				return nil, ListServersOutput{}, err
			}
			return nil, ListServersOutput{Servers: servers}, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[1], // "exec_command"
			Description: "Run a shell command on a server. Pass the server's id (from list_servers), not its name. If sudo=true the broker handles the sudo password for you — do NOT prepend 'sudo' to the command yourself. sudo=true only works on servers where has_sudo=true. Out-of-profile server ids are rejected. timeout_seconds defaults to 120 and is hard-capped at 300 (5 min) — the value actually in effect is echoed back as effective_timeout_seconds; anything longer belongs in exec_background. On timeout the broker sends a SIGKILL request and tears down the SSH connection, and the call returns on time (timeout plus a short grace) with timed_out=true and whatever output was already received; a few servers never clean up the remote process — after a timeout check with ps/pgrep and clean up manually if needed; on high-latency links raise the grace window with SSHMGR_EXEC_KILL_GRACE. Output is capped at 1 MiB per channel: if truncated=true you received only the PREFIX — read stdout_bytes/stderr_bytes for the true size, then refine your command (tail -n / head -n / grep) and re-run to get the part you need, rather than asking for the whole huge output again. If a call comes back 'confirmation required by command policy' with a confirmation code, you MUST first ask the user for consent to run this exact command on this server, then call exec_command again with the SAME command plus confirmation_code and confirmation_reason (a short note of what the user consented to); the code is single-use, expires after 5 minutes, and matches only this exact command on this server. Windows servers authorized for SMB shared storage: Windows network drives are mapped before each command in that command's SSH session with WNetAddConnection2; the mapping password never appears in a target process command line. Desktop sessions, Windows services, scheduled tasks, upload_file, upload_content, download_file, streaming relay sessions, port forwarding, and processes that keep running after the command ends cannot see those drives; relay_file Windows local-copy tasks are the exception because they run in one injected command session. Windows sudo execution also does not inject them. injected_network_drives lists successful mappings, while mapping failures append a warning and never block the command. This tool does not rewrite command text; to translate a shared address (\\\\server\\share\\path or smb://server/share/path) to the server-local path, call query_topology with from=<server id> and to=<shared address>.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ExecCommandForProfile(ctx, st, projectID, profileID, in.ServerID, in.Command, in.Sudo, time.Duration(in.TimeoutSeconds)*time.Second, ExecConfirmation{Code: in.ConfirmationCode, Reason: in.ConfirmationReason}, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, ExecOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[14], // "list_secrets"
			Description: "Read-only list of secrets you may reference. Each output line is: name | note | environment variable name | server names. Optional query matches case-insensitively against the name or note; optional server takes a server ID (exact match, preferred) or a server NAME from list_servers and filters to secrets that may write to it. This tool never returns secret values, value lengths, hashes, timestamps, or servers outside your authorization. Use the reference form {{sshmgr-secret:NAME}} in upload_content content; sshmgr fills in the secret value before writing it to the server.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ListSecretsInput) (*mcp.CallToolResult, any, error) {
			st := storeFromContext(ctx, storeFn)
			text, err := ListSecretsTextForProfile(st, profileID, in.Query, in.Server)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}, nil, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[15], // "probe_connectivity"
			Description: "Probe TCP connectivity and connect duration from this instance (from=\"self\") or via an authorized managed server (from=<server id>) to registered devices. Each target is a server id or device name, optionally name:port; the default port is the server entry's port, or 22 for devices. TCP connect only (no bandwidth); only addresses in common registered networks are probed (the fallback host is never probed); at most 10 deduplicated targets, 5 probed concurrently, 1.5s per address (SSHMGR_SSH_ADDRESS_TIMEOUT), 30s total. Results are compact text grouped by target and are stored locally on this instance only — never sent back to the authority.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ProbeConnectivityInput) (*mcp.CallToolResult, any, error) {
			st := storeFromContext(ctx, storeFn)
			text, err := ProbeConnectivityForProfile(ctx, st, projectID, profileID, in.From, in.Targets, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}, nil, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[16], // "list_shares"
			Description: "Read-only list of the SMB shared storages visible in your session. Each share carries its name, the registered provider device name, owner note, directory agreements (purpose = subdirectory inside the share), Windows drive letter, registration version, owner change note, and the servers in your session scope that may mount it — each with the agreed mount point (PowerShell/cmd servers use <drive>:, others use /mnt/sshmgr/<share name>). Optional server takes a server id or name and narrows every share to that one server. Optional live_check=true concurrently runs fixed read-only on-site mount checks on visible Linux and macOS servers (each server with its own timeout) and annotates each server entry with mounted / not-mounted / config-missing / unreachable, plus safe follow-up notes such as a stale configuration version or a macOS system-configuration reset; Windows entries are marked not-checked because their fixed drive letter is not a persistent mount. For Windows servers the fixed drive letter is mapped before each exec_command or exec_background command in that command's SSH session; relay_file Windows local-copy tasks create the same kind of injected command session. Desktop sessions, Windows services, scheduled tasks, separate file-transfer sessions, and processes that outlive the command cannot see it. Optional check_server (a server id or name; requires live_check) narrows the on-site check to that one server. Without live_check the tool performs no network or SSH action and mount points are agreements only. It never writes audit records for listing or checking and never includes mount passwords, addresses, or any credentials; no agent tool can register or modify shared storages.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ListSharesInput) (*mcp.CallToolResult, ListSharesOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ListSharesForProfile(ctx, st, profileID, in, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, emptyListSharesOutput(), nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[21], // "query_topology"
			Description: "Read-only network topology query. With only `from`, return that origin device's neighbor groups; with `to`, return pairwise connectivity details from the origin to those targets. `from` is \"self\" (the device where this sshmgr instance runs) or a server id from list_servers; each `to` item is a server id for a managed device, or a device name for an unmanaged device or client instance; detail=true expands addresses, registered network bandwidth, metering, and device roles. Alternatively, set `from` to an authorized server id and `to` to exactly one SMB shared address (\\\\server\\share\\path or smb://server/share/path) to translate it to that server's agreed local path and report whether that server may mount the shared storage; mixed normal targets and multiple shared addresses are rejected. Reads only topology registrations, shared-storage registrations, the dial success address cache, and this instance's locally stored probe observations; it never probes and never opens an SSH connection. For a current on-site connectivity result, call probe_connectivity.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in QueryTopologyInput) (*mcp.CallToolResult, any, error) {
			st := storeFromContext(ctx, storeFn)
			var to []string
			if in.To != nil {
				to = *in.To
			}
			text, err := QueryTopologyForProfile(st, profileID, in.From, to, in.Detail)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}, nil, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[2], // "download_file"
			Description: "Download a file from a server to read its contents. Pass the server's id (from list_servers) + the absolute remote path. On a Windows server, Windows drive-path backslashes are converted to forward slashes; on a Linux or macOS server, backslashes are kept as file-name characters. Returns the file content (capped at 1 MiB; if truncated=true you got the PREFIX — read 'bytes' for the true size, then refine: re-download a slice via exec_command head/tail if you need a specific part). Out-of-profile server ids are rejected. Use this for file retrieval; do NOT fabricate file contents. This separate file transfer session cannot see Windows network drives mapped for exec_command or exec_background; copy files into the command session if you need to move them through a shared drive.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in DownloadInput) (*mcp.CallToolResult, DownloadOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := DownloadForProfile(ctx, st, projectID, profileID, in.ServerID, in.Path, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, DownloadOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[20], // "mount_share"
			Description: "Mount an SMB shared storage on a Linux or macOS server by fixed commands. Input is only a share name from list_shares and a server id from list_servers; no command or path can be supplied. The broker selects a registered common-network address from the target server, writes the persistent Linux or macOS configuration and the root-only mount secret, makes it effective, and verifies the mount point can list content. The operation is intended to be idempotent and safe to call again after share changes, but this tool cannot unmount or roll back a mount. A matching owner revocation marker rejects the call until a newer shared-storage authorization is synced. Windows servers do not need it: their fixed drive letter is injected into each command session. Mount passwords are never included in command text, results, or audit records.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in MountShareInput) (*mcp.CallToolResult, MountShareOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := MountShareForProfile(
				ctx, st, projectID, profileID, in.Share, in.ServerID,
				toolHostKeyStoreFromContext(ctx), mountSnapshotSyncer, storeFn,
			)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, emptyMountShareOutput(), nil
			}
			return nil, *out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[3], // "upload_file"
			Description: "Push a LOCAL file or directory to a server (the mirror of download_file — direction matters: LocalPath is read from the machine the broker runs on — with a stdio MCP that is your machine; with a remote serve broker it is the serve host — and RemotePath is the destination on the server). Pass the server's id (from list_servers), LocalPath (absolute path on the broker host of the file or directory to push — a directory is uploaded recursively, preserving relative paths; a symlinked directory as the upload root is resolved to its target, while symlinked sub-directories inside it are refused (upload the target directly)), and RemotePath (absolute destination on the server; its parent directory is created if missing). On a Windows server, Windows drive-path backslashes are converted to forward slashes; on a Linux or macOS server, backslashes are kept as file-name characters. Returns the file count + total bytes. Secret placeholders such as {{sshmgr-secret:NAME}} are transferred byte-for-byte as literal text; only upload_content fills them. This original-byte upload neither registers nor deletes a secret delivery record. The 1 MiB cap is a hard per-file bound: a single file larger than 1 MiB is REFUSED before transfer — an error names the file, its size, and the cap, and ZERO bytes of it is sent (in a directory upload, files already completed before the refusal remain); multiple files whose cumulative total crosses 1 MiB (each file within the cap) keep the already-completed files and honestly report truncated=true, with later files not uploaded — retry with smaller payloads. Out-of-profile server ids are rejected. SFTP is used, so sudo is not applicable. Use this to push configs/scripts/artifacts to a server; the broker holds the credentials. This separate file transfer session cannot see Windows network drives mapped for exec_command or exec_background; copy files into the command session if you need to move them through a shared drive.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in UploadInput) (*mcp.CallToolResult, UploadOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := UploadForProfile(ctx, st, projectID, profileID, in.ServerID, in.LocalPath, in.RemotePath, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, UploadOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[4], // "forward_port"
			Description: "Open a local port that forwards to a remote service through a server (the `ssh -L` semantic). Use this to reach a service running ON the server (or reachable from it) from the machine the broker runs on — e.g. a database, web UI, or metrics endpoint. Pass the server's id (from list_servers), remote_host + remote_port (the host:port to forward to FROM THE SERVER'S PERSPECTIVE — usually 127.0.0.1 + the service's port on the server's own loopback), an optional local_port (omit / 0 = the broker picks a free port), and an optional listen_host (IP literal only — the local address to bind; default 127.0.0.1; loopback is always allowed, a non-loopback address must be owner-approved). Returns tunnel_id + local_port + listen_host: the forward listens on listen_host:<local_port> ON THE MACHINE THE BROKER RUNS ON — with a stdio MCP that is your machine; with a remote serve broker it is the serve host, so reach it from there (e.g. curl on that host) — it is NOT reachable from a different machine. Out-of-profile server ids are rejected. This holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels auto-close after ~10 minutes of INACTIVITY — a tunnel carrying traffic stays alive).",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ForwardInput) (*mcp.CallToolResult, ForwardOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ForwardForProfile(ctx, st, projectID, profileID, in.ServerID, in.RemoteHost, in.RemotePort, in.LocalPort, in.ListenHost, tunnels, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, ForwardOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[5], // "close_port"
			Description: "Close a tunnel opened by forward_port. Pass the tunnel_id forward_port returned. Tears down the local listener AND the SSH connection that backed it (frees the resource — the broker was holding it open). Returns ok on success; an error if the tunnel_id is unknown (already closed, or never opened). No server_id / profile needed: the tunnel_id is an opaque handle bound to the broker process that opened it. You SHOULD call this when you are done with a forward rather than waiting for the ~10-minutes-of-inactivity auto-close.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in CloseForwardInput) (*mcp.CallToolResult, any, error) {
			st := storeFromContext(ctx, storeFn)
			if err := CloseForwardForProfile(ctx, st, projectID, in.TunnelID, tunnels); err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "closed"}},
			}, nil, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[6], // "exec_background" (Plan 32 T6)
			Description: "Start a LONG-RUNNING command on a server in the background (builds, training, log tails, watches — anything that would outlive one exec_command call) and return immediately with a task_id. Poll incremental output with exec_output(task_id); stop early with exec_stop(task_id). Pass the server's id (from list_servers), not its name. If sudo=true the broker handles the sudo password for you — do NOT prepend 'sudo' to the command yourself; sudo=true only works on servers where has_sudo=true. timeout_seconds defaults to 24h and is capped at 24h; the effective value is echoed back as effective_timeout_seconds. On timeout or exec_stop the task's SSH connection is torn down and the task is guaranteed to end promptly; but the terminal state is a broker-side verdict, not proof the remote process is dead — some servers ignore the teardown (see exec_stop). At most 32 background tasks exist per project (running + finished retained); when the limit is hit and every task is running, new starts are refused — wait for a task to finish or call exec_stop. Finished tasks keep their tail output readable for ~1h, then are evicted. Task records live only in the broker process memory: a broker restart loses them all (no recovery) — treat a restart as 'every task died'. No env/workdir/stdin parameters: compose the command line yourself, e.g. 'cd /var/log && tail -f app.log' or 'VAR=x make build'. Rule of thumb: short commands (under ~5 min, output needed at once) → exec_command; long-lived or incremental → exec_background + exec_output polling. If a call comes back 'confirmation required by command policy' with a confirmation code, you MUST first ask the user for consent to run this exact command on this server, then call exec_background again with the SAME command plus confirmation_code and confirmation_reason (a short note of what the user consented to); the code is single-use, expires after 5 minutes, and matches only this exact command on this server. Windows servers authorized for SMB shared storage: Windows network drives are mapped before each command in that command's SSH session with WNetAddConnection2; the mapping password never appears in a target process command line. Desktop sessions, Windows services, scheduled tasks, upload_file, upload_content, download_file, streaming relay sessions, port forwarding, and processes that keep running after the command ends cannot see those drives; relay_file Windows local-copy tasks are the exception because they run in one injected command session. Windows sudo execution also does not inject them. Mapping failures are appended to the task output when it finishes and never block the command. This tool does not rewrite command text; to translate a shared address (\\\\server\\share\\path or smb://server/share/path) to the server-local path, call query_topology with from=<server id> and to=<shared address>.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecBackgroundInput) (*mcp.CallToolResult, BgStartOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ExecBackgroundForProfile(ctx, st, projectID, profileID, in.ServerID, in.Command, in.Sudo, in.TimeoutSeconds, tasks, ExecConfirmation{Code: in.ConfirmationCode, Reason: in.ConfirmationReason}, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, BgStartOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[7], // "exec_output" (Plan 32 T7)
			Description: "Read incremental output from a background task started with exec_background. Pass the task_id it returned. stdout_offset/stderr_offset are ABSOLUTE byte offsets into each stream (0/omit = stream start): pass back the next_stdout_offset/next_stderr_offset you received to continue where you left off — poll repeatedly to tail a task (tail -f / journalctl -f style). encoding: 'text' (default — raw bytes as UTF-8; invalid sequences become U+FFFD and a multi-byte character can be split at a read boundary) or 'base64' (exact bytes — decode it yourself; use it for binary or non-UTF-8 e.g. GBK output). Offsets are byte-based in BOTH modes. wait_seconds (0-60, omit = 0) long-polls: the call returns once either stream has new bytes past your offsets, the task leaves running, or the budget expires — keep it under ~30 to stay below your own client timeout. If an offset fell behind the retained 1 MiB tail window you get truncated=true plus lost_stdout_bytes/lost_stderr_bytes — those bytes are gone; continue from next_*_offset instead of retrying the old offset. An offset past the stream end is pulled back to the end. Finished tasks keep their tail readable for ~1h, then the id fails with the unknown-task error (never existed / expired retention / evicted for capacity / broker restarted — records are in-process only). timeout/stopped are broker-side verdicts — on servers that ignore the teardown the remote process may still be running; verify with exec_command (ps/pgrep) when needed.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecOutputInput) (*mcp.CallToolResult, BgReadOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ExecOutputForProfile(ctx, st, projectID, in.TaskID, in.WaitSeconds, in.StdoutOffset, in.StderrOffset, in.Encoding, tasks)
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, BgReadOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[8], // "exec_stop" (Plan 32 T7)
			Description: "Stop a background task. Pass the task_id exec_background returned. Returns immediately with the task's status AT the moment you triggered the stop: a running task answers 'running' — the stop was set in motion, the terminal 'stopped' state shows up on your next exec_output call (this call never blocks waiting for the task to die). Stopping an already-finished task is idempotent: it just returns that terminal status. Kill semantics, honestly: there is no signal ladder — stopping closes the SSH session, which delivers SIGHUP remotely (the same thing killing a real ssh session does); processes the command started with nohup/setsid on the server survive it. Some servers completely ignore the session teardown — the stop still terminates the task and its connection promptly, but the remote process may keep running (check with exec_command ps/pgrep). Unknown task_id → error (never existed / expired after the ~1h retention window / evicted for capacity / broker restarted — task records are in-process only).",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecStopInput) (*mcp.CallToolResult, BgStopOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ExecStopForProfile(ctx, st, projectID, in.TaskID, tasks)
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, BgStopOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[9], // "upload_content"
			Description: fmt.Sprintf("Upload inline content as a file on a server — the cross-machine path (upload_file reads from the broker's own filesystem; use upload_content to push content YOU hold). Pass the server's id (from list_servers) + the content + the absolute destination path (must start with /; on a Windows server it may also start with a Windows drive root like C:/, and backslashes are converted to forward slashes there; parent directories are created; an existing file is overwritten). encoding: 'text' (default, UTF-8 — invalid sequences are replaced with U+FFFD, not byte-exact) or 'base64' (exact bytes — SINGLE-LINE standard base64 with padding; use it for binary, non-UTF-8 or byte-exact content). Secret placeholders: only upload_content fills {{sshmgr-secret:NAME}} placeholders with the current secret values. After a filled file is written, sshmgr tries its best to register a secret delivery record for the owner; registration can be missed while the authority is unreachable, and failure never rolls back or retries the file. For filled writes, delivery_record_status is 'recorded' when that record was registered and 'not_recorded' otherwise, with delivery_record_reason explaining the latter. Content without a placeholder returns no delivery-record fields and is not registered; neither it nor upload_file/relay_file deletes an older record for the path. Capped at %d bytes decoded — larger payloads are refused before transfer; for bigger files place them where the broker can reach and use upload_file. No sudo: root-owned paths are not writable. Concurrent writes to the same path are not atomic — avoid racing another upload. On failure the remote file may be left partially written — verify and clean up yourself. This separate file transfer session cannot see Windows network drives mapped for exec_command or exec_background.", uploadCap),
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in UploadContentInput) (*mcp.CallToolResult, UploadContentOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := uploadContentForProfile(ctx, st, projectID, profileID, in.ServerID, in.Content, in.RemotePath, in.Encoding, uploadCap, delivery, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, UploadContentOutput{}, nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[10], // "exec_context" (Plan 41 §3)
			Description: "Capture the exec channel's TRUE execution context in one round: uid/gid/groups, tty (exec channels have no PTY — 'no-tty' is expected, not an anomaly), user-namespace mapping ('0 0 4294967295' = initial namespace, real root — not userns fakeroot), LSM label, SSH provenance (client/connection — captured BEFORE elevation: sudo's env_reset empties them in the privileged layer), and the process tree position. Use this BEFORE hypothesizing about identities, interception layers, or 'mystery permission denied' — e.g. when exec_command(sudo=true) returns uid=0 yet a path stays EACCES, one call tells you whether the channel, the namespace, or the LSM label explains it. Pass sudo=true to probe the privileged channel (uid should be 0; requires has_sudo=true). The result also lists the environment variable names this exact layer would inject and variables that cannot be injected with reasons; it never includes secret values. For Windows servers, planned_network_drives and uninjected_network_drives preview the drive mappings for the selected execution layer without probing connectivity or changing cached address state.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecContextInput) (*mcp.CallToolResult, ExecContextOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := ExecContextForProfile(ctx, st, projectID, profileID, in.ServerID, in.Sudo, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport
				// error. The typed output must stay schema-valid: the SDK serializes and
				// validates it even alongside an error result.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, emptyExecContextOutput(), nil
			}
			return nil, out, nil
		},
	)

	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[11], // "relay_file" (Plan 47 §1.2)
			Description: fmt.Sprintf(`Relay a LARGE file server-to-server, or from the broker's own disk to a server — the zero-context big-file path (streaming bytes stay in broker memory; shared-storage local-copy bytes stay on the selected server; neither the tool result nor exec_output ever contains file content). Use it when a file is too big for upload_file's 1 MiB per-file cap (e.g. model weights, GB-scale artifacts), or when the file lives on one server and must land on another (e.g. downloaded on an internet-facing server, delivered to an air-gapped one). Pass to_server_id + to_path (absolute), and either from_server_id + from_path (absolute path on that server) or just from_path (absolute path on the broker host). Exactly one endpoint may instead use from_share_address or to_share_address with an SMB shared address (\\\\host\\share\\subpath or smb://host/share/subpath); when the other endpoint is an authorized server that can mount that shared storage, sshmgr runs a local file copy on that server and file bytes do not travel through the broker; Linux and macOS must already be actually mounted, while Windows does not need a persistent mount. Linux and macOS use cp; Windows maps the fixed drive with WNetAddConnection2 in the same SSH command session and then runs robocopy, whose exit codes 0 through 7 are treated as success and 8 or higher as failure. The Windows drive letter is visible only for that copy session and leaves no persistent mapping. Every local copy keeps the in-progress object at <destination>.sshmgr-partial, creates no chunk manifest, and exposes the final destination name only after the copy succeeds; every interruption, stop, failure, or rerun starts from byte 0 and overwrites the old partial object. relay_file does not auto-mount Linux or macOS: call mount_share first when asked. When the other endpoint is outside the shared storage's mountable range, sshmgr selects an authorized, mountable, actually mounted Linux or macOS server to expose the shared address, then reuses the normal streaming relay; automatic order prefers a candidate on a physical local network with the provider device, then server name in ascending order, and via overrides that choice. Windows cannot serve as that streaming fallback endpoint because its mapped drive letter exists only in a command session and a separate file-transfer session cannot see it. Both endpoints being shared addresses remains unsupported. Secret placeholders such as {{sshmgr-secret:NAME}} are transferred byte-for-byte as literal text; only upload_content fills them. relay_file neither registers nor deletes a secret delivery record. Chunk size %d bytes for streaming relays. Returns task_id immediately — poll with exec_output(task_id) for progress and the final digests, stop with exec_stop(task_id); mode, restart_after_interruption, and share_endpoint_server_id identify which transfer mode started and which selected server exposes a shared address. A STREAMING RELAY IS RESUMABLE: interrupted/stopped/failed transfers leave <to_path>.sshmgr-partial + a manifest on the destination; re-running relay_file with the same paths completes only the missing chunks. fresh=true discards them and restarts. VERIFICATION: any streaming transfer that moved the full file from byte 0 to EOF in this one run (a fresh start or an empty-manifest self-heal) reports file_sha256 — compare with exec sha256sum <to_path> on the destination; a transfer that resumed completed chunks reports the chunk-merkle root instead (per-chunk integrity was verified against the manifest as each chunk was written; independently re-verifying such a file requires splitting it into %d-byte chunks, hashing each, and hashing the concatenated digests). Directories are NOT supported — tar on the source first (exec tar czf), relay the tarball, untar on the destination. No sudo: root-owned destination paths are not writable. Space pre-flight: for streaming_relay, 'space_check'='unavailable' means the destination could not report free space but proceeding is safe because a full disk stops at a chunk boundary and resumes later; for local_copy it means the local copy command may fail on a full disk and a rerun starts from byte 0. Complete story for an offline server: exec_background on the internet-facing server to download, relay_file to the air-gapped one, then exec sha256sum there and compare file_sha256 from exec_output. Separate streaming file-transfer sessions cannot see Windows network drives mapped for exec_command or exec_background; a Windows local copy creates its own injected command session and is therefore the exception.`, relayChunk, relayChunk),
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in RelayInput) (*mcp.CallToolResult, RelayOutput, error) {
			st := storeFromContext(ctx, storeFn)
			out, err := RelayForProfile(ctx, st, tasks, projectID, profileID, in, relayChunk, toolHostKeyStoreFromContext(ctx))
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, RelayOutput{}, nil
			}
			return nil, out, nil
		},
	)

	// update_server_metadata (Plan 51 §7) — CACHE FACE ONLY: registered iff
	// the metadata forwarder was injected (mcp --cache builds it from the
	// instance's cache.auth.json; NewServer/RunStdio pass nil → the tool does
	// not exist on the authority face, Q10-A). The broker does ALL gating
	// (device-code auth, profile grant, revision CAS); this side forwards and
	// then mirrors the accepted write onto the CURRENT store generation
	// (resolved AFTER the HTTP call — the Plan 48 T5b discipline).
	if meta != nil {
		addTrackedTool(srv, busy, storeFn, hkFn,
			&mcp.Tool{
				Name:        BrokerTools[12], // "update_server_metadata" (Plan 51)
				Description: `Edit a server entry's descriptive metadata (role / services / location / hardware / caveats / description) in the shared vault, via the broker. Pass server_id (from list_servers) and expected_revision — the revision value you saw in list_servers (the optimistic-lock token). Each of the six fields is optional: OMIT a field to leave it unchanged; pass an EMPTY STRING to clear it. Record what you actually verified (e.g. after probing hardware or discovering deployed services) — this keeps list_servers accurate for every future session. On conflict (409, someone else edited first) the error carries the current revision AND current values of all six fields: merge your intent with those values, then retry once with the new revision. Connection details (host/port/user/credentials) and tags are NOT editable here — the owner manages those.`,
			},
			func(ctx context.Context, req *mcp.CallToolRequest, in UpdateServerMetadataInput) (*mcp.CallToolResult, UpdateServerMetadataOutput, error) {
				fields := metadataFieldsFromInput(&in)
				if len(fields) == 0 {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: "update_server_metadata: provide at least one of the six metadata fields (role, services, location, hardware, caveats, description) — omit a field to keep its value, pass an empty string to clear it"}},
					}, UpdateServerMetadataOutput{}, nil
				}
				res, err := meta.EditMeta(in.ServerID, in.ExpectedRevision, fields)
				if err != nil {
					// Branch text is the verbatim §2.1 contract (includes the
					// 409 current-values merge input).
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
					}, UpdateServerMetadataOutput{}, nil
				}
				out := UpdateServerMetadataOutput{
					ServerID:   in.ServerID,
					ServerName: res.ServerName,
					Revision:   res.Revision,
					UpdatedAt:  res.UpdatedAt,
					Mirrored:   true,
				}
				// Mirror onto the CURRENT generation, re-resolved AFTER the
				// HTTP call (T5b). The broker already committed — a mirror
				// failure is reported, never fatal.
				if aerr := storeFn().ApplyForwardedMetadata(in.ServerID, fields, res.Revision, res.UpdatedAt); aerr != nil {
					out.Mirrored = false
					out.Warning = "applied on the broker, but the local cache could not mirror it — run cache pull to refresh (" + aerr.Error() + ")"
				}
				return nil, out, nil
			},
		)
	}

	// query_software_inventory（票 #185）：两个面都注册。权威端与单机在
	// inventory 为 nil 时走本地适配层；客户端缓存面注入在线转发器，未注
	// 入时由保底能力固定报错（见 newCacheBroker）。
	registerSoftwareInventoryTool(srv, busy, storeFn, hkFn, profileID, inventory)
	// draft_software_declarations 与 collect_software_inventory（票 #188）
	// 同样两个面都注册：本地面直接写 vault / 立即采集；客户端面复用同一
	// 个注入能力，未实现对应方法时保底报错。
	registerSoftwareDeclarationDraftTool(srv, busy, storeFn, hkFn, profileID, inventory)
	registerSoftwareInventoryCollectionTool(srv, busy, storeFn, hkFn, profileID, inventory)

	return srv, tunnels, tasks, nil
}

// metadataFieldsFromInput collects the present (non-nil) metadata params into
// the wire map: key present = write (empty string = clear); key absent =
// keep. Pointer nil-ness IS the three-way distinction the schema carries.
func metadataFieldsFromInput(in *UpdateServerMetadataInput) map[string]*string {
	fields := map[string]*string{}
	if in.Role != nil {
		fields["role"] = in.Role
	}
	if in.Services != nil {
		fields["services"] = in.Services
	}
	if in.Location != nil {
		fields["location"] = in.Location
	}
	if in.Hardware != nil {
		fields["hardware"] = in.Hardware
	}
	if in.Caveats != nil {
		fields["caveats"] = in.Caveats
	}
	if in.Description != nil {
		fields["description"] = in.Description
	}
	return fields
}

// serverRuntime 把互相独立的忙判据库与会话提示记录挂在同一个服务运行对象
// 上；二者生命周期相同，职责不混在同一个结构里。
type serverRuntime struct {
	busy         *BusyTracker
	sessionHints *SessionHints
	storeFn      func() *store.Store
}

// serverRuntimes 登记每个已构造 *mcp.Server 的运行对象(NewServerFromSource
// 构造时写入;stdio 桥入口 serveBridge 经 busyTrackerFor 读出,供
// reload_self 的忙检查与豁免注册)。加法出口而非改 NewServerFromSource
// 签名:返回形状被 serve.go 等既有调用方依赖。
var serverRuntimes sync.Map // *mcp.Server → *serverRuntime

// busyTrackerFor 返回 serverRuntimes 里登记的忙判据库;未登记(裸构造、
// 或登记被测试清理)返回 nil——调用方据此退化为「无忙判据」行为。
func busyTrackerFor(srv *mcp.Server) *BusyTracker {
	if v, ok := serverRuntimes.Load(srv); ok {
		if runtime, ok := v.(*serverRuntime); ok {
			return runtime.busy
		}
	}
	return nil
}

// storeSourceFor 返回构造服务面时登记的「调用时求值 store 获取函数」，
// 供不经过 addTrackedTool 的豁免注册形态复用同一个输出遮蔽层。
func storeSourceFor(srv *mcp.Server) func() *store.Store {
	if v, ok := serverRuntimes.Load(srv); ok {
		if runtime, ok := v.(*serverRuntime); ok {
			return runtime.storeFn
		}
	}
	return nil
}

// addExemptTool 是豁免形态的注册帮手(票 02 评审建议):与 addTrackedTool
// 一样走 wrapInflight,只是 exempt=true——发起换手的 reload_self 必须豁免
// 自身,否则它做忙检查时永远看见自己忙碌(规格 D5)。保住「全部注册走
// wrapInflight 包装」的 grep 不变量:豁免注册也在这里,不走裸 mcp.AddTool。
func addExemptTool[In, Out any](srv *mcp.Server, busy *BusyTracker, storeFn func() *store.Store, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	withMaskingDescription(t)
	mcp.AddTool(srv, t, maskToolOutput(storeFn, nil, wrapInflight(busy, h, true)))
}

// ReloadSelfOutput is the reload_self tool output. busy (the active list) is
// present only when the handover was declined because the bridge was busy.
type ReloadSelfOutput struct {
	MaskedOutput
	Version          string      `json:"version" jsonschema:"version of THIS bridge process"`
	DiskGeneration   int64       `json:"disk_generation" jsonschema:"generation recorded on disk next to the binary (sshmgr update writes it); 0 = no signal"`
	DiskVersion      string      `json:"disk_version,omitempty" jsonschema:"version recorded in the on-disk generation signal; empty when no signal"`
	Latest           string      `json:"latest,omitempty" jsonschema:"tag of the latest GitHub release (queried live once per call)"`
	LatestError      string      `json:"latest_error,omitempty" jsonschema:"why the latest-release query failed, verbatim; empty on success"`
	Handover         string      `json:"handover" jsonschema:"handover outcome: started | declined_busy | no_new_generation | not_first_generation | already_armed | failed"`
	SuccessorVersion string      `json:"successor_version,omitempty" jsonschema:"version the ready successor reported (handover=started)"`
	Error            string      `json:"error,omitempty" jsonschema:"why the handover failed (handover=failed)"`
	Busy             *BusyReport `json:"busy,omitempty" jsonschema:"active list present when handover=declined_busy"`
}

// registerReloadSelfTool 注册 reload_self(工具面第 14 把,BrokerTools[13];
// 只由 stdio 桥入口注册,见 bridgeOnlyTools)。处理器经 wrapInflight 豁免
// 自身在飞计数(规格 D5)。重复调用是幂等的同义替换——继任接管后由
// serveBridge 再调一次,借 SDK 的「工具集变更」路径向已领养会话补发一次
// tools/list_changed 通知(规格 D3:继任接管后无条件发一次)。
func registerReloadSelfTool(srv *mcp.Server, busy *BusyTracker, rs *hotswap.ReloadService) {
	addExemptTool(srv, busy, storeSourceFor(srv),
		&mcp.Tool{
			Name:        BrokerTools[13], // "reload_self" (bridge hot-upgrade)
			Description: "Report THIS bridge process's version status — current version, the on-disk generation+version (written by sshmgr update; a NEWER GENERATION, upgrade or downgrade alike, is what triggers a handover), and the latest GitHub release tag (one live read-only query; a query failure is reported verbatim in latest_error and changes nothing else). Takes no arguments. When the disk holds a newer generation AND the bridge is idle (no active tunnels, running background tasks, or in-flight requests — this very call is exempt), it hot-swaps into the new binary without dropping the connection: the response you are reading is written by the OLD process, the successor then takes over the SAME session (no re-initialize) and emits one tools/list_changed. If the successor fails to become ready the handover is rolled back and the failure reason is returned here — the old bridge keeps serving. When busy, returns the active list instead of swapping. Known host quirk: a tool first used in the same turn it was registered may be reported missing once — retry next turn.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ReloadSelfOutput, error) {
			return nil, reloadSelf(ctx, busy, rs), nil
		},
	)
}

// reloadSelf 是 reload_self 的处理器主体(从注册闭包拆出便于单测)。
func reloadSelf(ctx context.Context, busy *BusyTracker, rs *hotswap.ReloadService) ReloadSelfOutput {
	out := ReloadSelfOutput{Handover: "no_new_generation"}
	st := rs.Report(ctx)
	out.Version = st.CurrentVersion
	out.DiskGeneration = st.DiskGeneration
	out.DiskVersion = st.DiskVersion
	out.Latest = st.Latest
	out.LatestError = st.LatestError

	switch {
	case rs.Adopted():
		// Adopted 语义(v0.20.0 起):仅指本桥取不到自身可执行路径、无力拉继任
		// 的形态——被领养的后续代照常换手(见 reload.go 的代次分叉)。
		// 这里如实说明而不冒充换手。
		out.Handover = "not_first_generation"
	case st.DiskGeneration <= rs.BirthGen():
		// 盘上无新代际(无信号=0,或仍是出生代际):只报情报。
	default:
		if busy != nil {
			if rep := busy.Report(); rep.Busy {
				out.Handover = "declined_busy"
				out.Busy = &rep
				return out
			}
		}
		switch arm := rs.Arm(); arm.State {
		case hotswap.ArmArmed:
			out.Handover = "started"
			out.SuccessorVersion = arm.SuccessorVersion
		case hotswap.ArmAlreadyArmed:
			out.Handover = "already_armed"
		default: // ArmFailed
			out.Handover = "failed"
			out.Error = arm.Err.Error()
		}
	}
	return out
}

// ListServersOutput is the list_servers tool output.
type ListServersOutput struct {
	MaskedOutput
	Servers []ServerInfo `json:"servers" jsonschema:"servers you are authorized to use"`
}

// ExecCommandInput is the exec_command tool input.
type ExecCommandInput struct {
	ServerID           string `json:"server_id" jsonschema:"server id from list_servers"`
	Command            string `json:"command" jsonschema:"shell command to run on the server"`
	Sudo               bool   `json:"sudo,omitempty" jsonschema:"true to run with sudo (broker handles the sudo password; do not prepend sudo). Requires has_sudo=true."`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty" jsonschema:"optional max seconds; defaults to 120"`
	ConfirmationCode   string `json:"confirmation_code,omitempty" jsonschema:"optional one-time confirmation code from a 'confirmation required by command policy' response; ask the user for consent first, then call again with the SAME command plus this code and confirmation_reason"`
	ConfirmationReason string `json:"confirmation_reason,omitempty" jsonschema:"optional short note of what the user consented to; required together with confirmation_code"`
}
