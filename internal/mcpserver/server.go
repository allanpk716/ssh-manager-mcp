package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/store"
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
// matching mcp.AddTool call in NewServer that indexes the new entry. That keeps
// the safety scorers in lock-step with the registration source: there is ONE
// place that names the tools, and the eval scorer reads it instead of
// re-hardcoding the names.
var BrokerTools = []string{
	"list_servers",    // [0] — enumerate the in-profile servers (no credentials)
	"exec_command",    // [1] — run a shell command on a server (profile-gated)
	"download_file",   // [2] — download a remote file over SFTP (profile-gated, §6-capped)
	"upload_file",     // [3] — push a local file/dir to a server over SFTP (profile-gated, §6-capped)
	"forward_port",    // [4] — open a `ssh -L` tunnel (profile-gated, STATEFUL — held by TunnelManager)
	"close_port",      // [5] — tear down a forward_port tunnel by id (closes listener + SSH client)
	"exec_background", // [6] — start a long-running command in the background (profile-gated, STATEFUL — held by TaskManager; Plan 32 T6)
	"exec_output",     // [7] — poll incremental output of a background task (Plan 32 T7)
	"exec_stop",       // [8] — stop a background task by id (Plan 32 T7)
	"upload_content",  // [9] — write INLINE content (text/base64, decoded ≤ cap) to a remote path over SFTP (profile-gated; Plan 33 T4) — the cross-machine upload path upload_file cannot serve
}

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
	return NewServerFromSource(func() *store.Store { return st }, profileID, projectID)
}

// NewServerFromSource is NewServer with a swappable store source: every tool
// closure resolves the store via storeFn() AT CALL TIME, so a hot-reloading
// caller (mcp --cache) can atomically swap the underlying store between calls
// without rebuilding the MCP server or tearing down tunnels or background
// tasks. storeFn must be safe for concurrent use and must never return nil.
func NewServerFromSource(storeFn func() *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, *TaskManager, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: buildinfo.Version}, nil)
	tunnels := NewTunnelManager()
	tunnels.AttachStore(storeFn, projectID) // mirror pipeline + control-loop store seam (Plan 35 spec §4) — attached BEFORE StartSweeper so the first tick already sees the live store
	tunnels.StartSweeper()                  // background tunnel sweeper (activity-based reclaim, see forwardIdleTimeout)
	tasks, err := NewTaskManager()          // env seam (SSHMGR_BG_*): 非法值/非正数 → 构造失败拒绝启动 (fail-closed)
	if err != nil {
		return nil, nil, nil, err
	}
	tasks.StartSweeper() // 照 tunnels 先例: 构造器不启, 生产接线点在此 (1min tick, spec §3)

	uploadCap, err := resolveUploadContentCap() // env seam (SSHMGR_UPLOAD_CONTENT_MAX): invalid/非正/>1 GiB → construction fails (fail-closed, spec rev3 §3.1)
	if err != nil {
		return nil, nil, nil, err
	}

	// The tool names below reference BrokerTools by index so the slice above IS
	// the source of truth — adding a broker tool means editing BrokerTools, not
	// copy-pasting a new literal here (and risk the eval scorer drifting).
	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[0], // "list_servers"
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo (host is \"hidden\" unless the owner exposed it — address servers via id, never by host), plus owner-provided context: role, services (what's deployed), location, hardware, caveats (special handling — read before acting), tags, description. Never includes credentials.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListServersOutput, error) {
			st := storeFn()
			servers, err := ListServersForProfile(st, profileID)
			if err != nil {
				return nil, ListServersOutput{}, err
			}
			return nil, ListServersOutput{Servers: servers}, nil
		},
	)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[1], // "exec_command"
			Description: "Run a shell command on a server. Pass the server's id (from list_servers), not its name. If sudo=true the broker runs `sudo -S` for you — do NOT prepend 'sudo' to the command yourself. sudo=true only works on servers where has_sudo=true. Out-of-profile server ids are rejected. timeout_seconds defaults to 120 and is hard-capped at 300 (5 min) — the value actually in effect is echoed back as effective_timeout_seconds; anything longer belongs in exec_background. Output is capped at 1 MiB per channel: if truncated=true you received only the PREFIX — read stdout_bytes/stderr_bytes for the true size, then refine your command (tail -n / head -n / grep) and re-run to get the part you need, rather than asking for the whole huge output again.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecOutput, error) {
			st := storeFn()
			out, err := ExecCommandForProfile(ctx, st, projectID, profileID, in.ServerID, in.Command, in.Sudo, time.Duration(in.TimeoutSeconds)*time.Second)
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

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[2], // "download_file"
			Description: "Download a file from a server to read its contents. Pass the server's id (from list_servers) + the absolute remote path. Returns the file content (capped at 1 MiB; if truncated=true you got the PREFIX — read 'bytes' for the true size, then refine: re-download a slice via exec_command head/tail if you need a specific part). Out-of-profile server ids are rejected. Use this for file retrieval; do NOT fabricate file contents.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in DownloadInput) (*mcp.CallToolResult, DownloadOutput, error) {
			st := storeFn()
			out, err := DownloadForProfile(ctx, st, projectID, profileID, in.ServerID, in.Path)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, DownloadOutput{}, nil
			}
			return nil, out, nil
		},
	)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[3], // "upload_file"
			Description: "Push a LOCAL file or directory to a server (the mirror of download_file — direction matters: LocalPath is read from the machine the broker runs on — with a stdio MCP that is your machine; with a remote serve broker it is the serve host — and RemotePath is the destination on the server). Pass the server's id (from list_servers), LocalPath (absolute path on the broker host of the file or directory to push — a directory is uploaded recursively, preserving relative paths; a symlinked directory as the upload root is resolved to its target, while symlinked sub-directories inside it are refused (upload the target directly)), and RemotePath (absolute destination on the server; its parent directory is created if missing). Returns the file count + total bytes. The 1 MiB cap is a hard per-file bound: a single file larger than 1 MiB is REFUSED before transfer — an error names the file, its size, and the cap, and ZERO bytes of it is sent (in a directory upload, files already completed before the refusal remain); multiple files whose cumulative total crosses 1 MiB (each file within the cap) keep the already-completed files and honestly report truncated=true, with later files not uploaded — retry with smaller payloads. Out-of-profile server ids are rejected. SFTP is used, so sudo is not applicable. Use this to push configs/scripts/artifacts to a server; the broker holds the credentials.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in UploadInput) (*mcp.CallToolResult, UploadOutput, error) {
			st := storeFn()
			out, err := UploadForProfile(ctx, st, projectID, profileID, in.ServerID, in.LocalPath, in.RemotePath)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, UploadOutput{}, nil
			}
			return nil, out, nil
		},
	)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[4], // "forward_port"
			Description: "Open a local port that forwards to a remote service through a server (the `ssh -L` semantic). Use this to reach a service running ON the server (or reachable from it) from the machine the broker runs on — e.g. a database, web UI, or metrics endpoint. Pass the server's id (from list_servers), remote_host + remote_port (the host:port to forward to FROM THE SERVER'S PERSPECTIVE — usually 127.0.0.1 + the service's port on the server's own loopback), an optional local_port (omit / 0 = the broker picks a free port), and an optional listen_host (IP literal only — the local address to bind; default 127.0.0.1; loopback is always allowed, a non-loopback address must be owner-approved). Returns tunnel_id + local_port + listen_host: the forward listens on listen_host:<local_port> ON THE MACHINE THE BROKER RUNS ON — with a stdio MCP that is your machine; with a remote serve broker it is the serve host, so reach it from there (e.g. curl on that host) — it is NOT reachable from a different machine. Out-of-profile server ids are rejected. This holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels auto-close after ~10 minutes of INACTIVITY — a tunnel carrying traffic stays alive).",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ForwardInput) (*mcp.CallToolResult, ForwardOutput, error) {
			st := storeFn()
			out, err := ForwardForProfile(ctx, st, projectID, profileID, in.ServerID, in.RemoteHost, in.RemotePort, in.LocalPort, in.ListenHost, tunnels)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, ForwardOutput{}, nil
			}
			return nil, out, nil
		},
	)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[5], // "close_port"
			Description: "Close a tunnel opened by forward_port. Pass the tunnel_id forward_port returned. Tears down the local listener AND the SSH connection that backed it (frees the resource — the broker was holding it open). Returns ok on success; an error if the tunnel_id is unknown (already closed, or never opened). No server_id / profile needed: the tunnel_id is an opaque handle bound to the broker process that opened it. You SHOULD call this when you are done with a forward rather than waiting for the ~10-minutes-of-inactivity auto-close.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in CloseForwardInput) (*mcp.CallToolResult, any, error) {
			st := storeFn()
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

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[6], // "exec_background" (Plan 32 T6)
			Description: "Start a LONG-RUNNING command on a server in the background (builds, training, log tails, watches — anything that would outlive one exec_command call) and return immediately with a task_id. Poll incremental output with exec_output(task_id); stop early with exec_stop(task_id). Pass the server's id (from list_servers), not its name. If sudo=true the broker runs `sudo -S` for you — do NOT prepend 'sudo' to the command yourself; sudo=true only works on servers where has_sudo=true. timeout_seconds defaults to 24h and is capped at 24h; the effective value is echoed back as effective_timeout_seconds. At most 32 background tasks exist per project (running + finished retained); when the limit is hit and every task is running, new starts are refused — wait for a task to finish or call exec_stop. Finished tasks keep their tail output readable for ~1h, then are evicted. Task records live only in the broker process memory: a broker restart loses them all (no recovery) — treat a restart as 'every task died'. No env/workdir/stdin parameters: compose the command line yourself, e.g. 'cd /var/log && tail -f app.log' or 'VAR=x make build'. Rule of thumb: short commands (under ~5 min, output needed at once) → exec_command; long-lived or incremental → exec_background + exec_output polling.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecBackgroundInput) (*mcp.CallToolResult, BgStartOutput, error) {
			st := storeFn()
			out, err := ExecBackgroundForProfile(ctx, st, projectID, profileID, in.ServerID, in.Command, in.Sudo, in.TimeoutSeconds, tasks)
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

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[7], // "exec_output" (Plan 32 T7)
			Description: "Read incremental output from a background task started with exec_background. Pass the task_id it returned. stdout_offset/stderr_offset are ABSOLUTE byte offsets into each stream (0/omit = stream start): pass back the next_stdout_offset/next_stderr_offset you received to continue where you left off — poll repeatedly to tail a task (tail -f / journalctl -f style). encoding: 'text' (default — raw bytes as UTF-8; invalid sequences become U+FFFD and a multi-byte character can be split at a read boundary) or 'base64' (exact bytes — decode it yourself; use it for binary or non-UTF-8 e.g. GBK output). Offsets are byte-based in BOTH modes. wait_seconds (0-60, omit = 0) long-polls: the call returns once either stream has new bytes past your offsets, the task leaves running, or the budget expires — keep it under ~30 to stay below your own client timeout. If an offset fell behind the retained 1 MiB tail window you get truncated=true plus lost_stdout_bytes/lost_stderr_bytes — those bytes are gone; continue from next_*_offset instead of retrying the old offset. An offset past the stream end is pulled back to the end. Finished tasks keep their tail readable for ~1h, then the id fails with the unknown-task error (never existed / expired retention / evicted for capacity / broker restarted — records are in-process only).",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecOutputInput) (*mcp.CallToolResult, BgReadOutput, error) {
			st := storeFn()
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

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[8], // "exec_stop" (Plan 32 T7)
			Description: "Stop a background task. Pass the task_id exec_background returned. Returns immediately with the task's status AT the moment you triggered the stop: a running task answers 'running' — the stop was set in motion, the terminal 'stopped' state shows up on your next exec_output call (this call never blocks waiting for the task to die). Stopping an already-finished task is idempotent: it just returns that terminal status. Kill semantics, honestly: there is no signal ladder — stopping closes the SSH session, which delivers SIGHUP remotely (the same thing killing a real ssh session does); processes the command started with nohup/setsid on the server survive it. Unknown task_id → error (never existed / expired after the ~1h retention window / evicted for capacity / broker restarted — task records are in-process only).",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecStopInput) (*mcp.CallToolResult, BgStopOutput, error) {
			st := storeFn()
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

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[9], // "upload_content"
			Description: fmt.Sprintf("Upload inline content as a file on a server — the cross-machine path (upload_file reads from the broker's own filesystem; use upload_content to push content YOU hold). Pass the server's id (from list_servers) + the content + the absolute destination path (must start with / or a Windows drive root like C:/; parent directories are created; an existing file is overwritten). encoding: 'text' (default, UTF-8 — invalid sequences are replaced with U+FFFD, not byte-exact) or 'base64' (exact bytes — SINGLE-LINE standard base64 with padding; use it for binary, non-UTF-8 or byte-exact content). Capped at %d bytes decoded — larger payloads are refused before transfer; for bigger files place them where the broker can reach and use upload_file. No sudo: root-owned paths are not writable. Concurrent writes to the same path are not atomic — avoid racing another upload. On failure the remote file may be left partially written — verify and clean up yourself.", uploadCap),
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in UploadContentInput) (*mcp.CallToolResult, UploadContentOutput, error) {
			st := storeFn()
			out, err := UploadContentForProfile(ctx, st, projectID, profileID, in.ServerID, in.Content, in.RemotePath, in.Encoding, uploadCap)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, UploadContentOutput{}, nil
			}
			return nil, out, nil
		},
	)

	return srv, tunnels, tasks, nil
}

// ListServersOutput is the list_servers tool output.
type ListServersOutput struct {
	Servers []ServerInfo `json:"servers" jsonschema:"servers you are authorized to use"`
}

// ExecCommandInput is the exec_command tool input.
type ExecCommandInput struct {
	ServerID       string `json:"server_id" jsonschema:"server id from list_servers"`
	Command        string `json:"command" jsonschema:"shell command to run on the server"`
	Sudo           bool   `json:"sudo,omitempty" jsonschema:"true to run with sudo (broker handles sudo -S; do not prepend sudo). Requires has_sudo=true."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"optional max seconds; defaults to 120"`
}
