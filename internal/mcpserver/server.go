package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/store"
)

// BrokerTools is the canonical set of MCP tools the broker exposes (the agent's
// broker-tool surface). NewServer registers exactly these tools, in this order,
// by indexing into this slice (BrokerTools[0] = list_servers, [1] = exec_command,
// [2] = download_file, [3] = upload_file, [4] = forward_port, [5] = close_port).
// Safety scorers in internal/eval (scoreT6 / scoreT8) treat any tool in this set
// as a broker-tool surface — zero-tolerance for credential leaks through them.
//
// Adding a new broker MCP tool means appending to this slice AND adding a
// matching mcp.AddTool call in NewServer that indexes the new entry. That keeps
// the safety scorers in lock-step with the registration source: there is ONE
// place that names the tools, and the eval scorer reads it instead of
// re-hardcoding the names.
var BrokerTools = []string{
	"list_servers",  // [0] — enumerate the in-profile servers (no credentials)
	"exec_command",  // [1] — run a shell command on a server (profile-gated)
	"download_file", // [2] — download a remote file over SFTP (profile-gated, §6-capped)
	"upload_file",   // [3] — push a local file/dir to a server over SFTP (profile-gated, §6-capped)
	"forward_port",  // [4] — open a `ssh -L` tunnel (profile-gated, STATEFUL — held by TunnelManager)
	"close_port",    // [5] — tear down a forward_port tunnel by id (closes listener + SSH client)
}

// NewServer builds an MCP server whose tools are scoped to profileID and
// attribute exec_command / download_file / upload_file / forward_port /
// close_port audit rows to projectID. The returned TunnelManager owns the
// long-lived SSH clients + listeners opened by forward_port; the caller SHOULD
// defer its CloseAll (RunStdio does — MCP-shutdown teardown) so that open
// tunnels are reaped when the agent disconnects.
//
// The tools are bound to a FIXED store. Hot-reloading callers (mcp --cache)
// use NewServerFromSource instead.
func NewServer(st *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, error) {
	return NewServerFromSource(func() *store.Store { return st }, profileID, projectID)
}

// NewServerFromSource is NewServer with a swappable store source: every tool
// closure resolves the store via storeFn() AT CALL TIME, so a hot-reloading
// caller (mcp --cache) can atomically swap the underlying store between calls
// without rebuilding the MCP server or tearing down tunnels. storeFn must be
// safe for concurrent use and must never return nil.
func NewServerFromSource(storeFn func() *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: buildinfo.Version}, nil)
	tunnels := NewTunnelManager()
	tunnels.StartSweeper() // background idle-reaper (closes tunnels idle > forwardIdleTimeout)

	// The tool names below reference BrokerTools by index so the slice above IS
	// the source of truth — adding a broker tool means editing BrokerTools, not
	// copy-pasting a new literal here (and risk the eval scorer drifting).
	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[0], // "list_servers"
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo, plus owner-provided context: role, services (what's deployed), location, hardware, caveats (special handling — read before acting), tags, description. Never includes credentials.",
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
			Description: "Run a shell command on a server. Pass the server's id (from list_servers), not its name. If sudo=true the broker runs `sudo -S` for you — do NOT prepend 'sudo' to the command yourself. sudo=true only works on servers where has_sudo=true. Out-of-profile server ids are rejected. Output is capped at 1 MiB per channel: if truncated=true you received only the PREFIX — read stdout_bytes/stderr_bytes for the true size, then refine your command (tail -n / head -n / grep) and re-run to get the part you need, rather than asking for the whole huge output again.",
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
			Description: "Push a LOCAL file or directory from YOUR machine to a server (the mirror of download_file — direction matters: LocalPath is on your machine, RemotePath is the destination on the server). Pass the server's id (from list_servers), LocalPath (absolute path on your machine of the file or directory to push — a directory is uploaded recursively, preserving relative paths), and RemotePath (absolute destination on the server; its parent directory is created if missing). Returns the file count + total bytes. The 1 MiB cap is a hard per-file bound: a single file larger than 1 MiB is REFUSED before transfer — an error names the file, its size, and the cap, and ZERO bytes of it are sent (in a directory upload, files already completed before the refusal remain); multiple files whose cumulative total crosses 1 MiB (each file within the cap) keep the already-completed files and honestly report truncated=true, with later files not uploaded — retry with smaller payloads. Out-of-profile server ids are rejected. SFTP is used, so sudo is not applicable. Use this to push configs/scripts/artifacts to a server; the broker holds the credentials.",
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
			Description: "Open a local port that forwards to a remote service through a server (the `ssh -L` semantic). Use this to reach a service running ON the server (or reachable from it) from your own machine — e.g. a database, web UI, or metrics endpoint. Pass the server's id (from list_servers), remote_host + remote_port (the host:port to forward to FROM THE SERVER'S PERSPECTIVE — usually 127.0.0.1 + the service's port on the server's own loopback), and an optional local_port (omit / 0 = the broker picks a free port). Returns tunnel_id + local_port: reach the remote service at 127.0.0.1:<local_port> on YOUR machine (e.g. `curl http://127.0.0.1:<local_port>` or pointing your client at it). Out-of-profile server ids are rejected. This holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels also auto-close after ~10 min idle).",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ForwardInput) (*mcp.CallToolResult, ForwardOutput, error) {
			st := storeFn()
			out, err := ForwardForProfile(ctx, st, projectID, profileID, in.ServerID, in.RemoteHost, in.RemotePort, in.LocalPort, tunnels)
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
			Description: "Close a tunnel opened by forward_port. Pass the tunnel_id forward_port returned. Tears down the local listener AND the SSH connection that backed it (frees the resource — the broker was holding it open). Returns ok on success; an error if the tunnel_id is unknown (already closed, or never opened). No server_id / profile needed: the tunnel_id is an opaque handle bound to the broker process that opened it. You SHOULD call this when you are done with a forward rather than waiting for the ~10 min idle timeout.",
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

	return srv, tunnels, nil
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
