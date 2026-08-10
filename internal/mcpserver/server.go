package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
)

// BrokerTools is the canonical set of MCP tools the broker exposes (the agent's
// broker-tool surface). NewServer registers exactly these tools, in this order,
// by indexing into this slice (BrokerTools[0] = list_servers, [1] = exec_command,
// [2] = download_file, [3] = upload_file). Safety scorers in internal/eval
// (scoreT6 / scoreT8) treat any tool in this set as a broker-tool surface —
// zero-tolerance for credential leaks through them.
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
}

// NewServer builds an MCP server whose tools are scoped to profileID and
// attribute exec_command / download_file audit rows to projectID.
func NewServer(st *store.Store, profileID, projectID string) (*mcp.Server, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: "v0.1.0"}, nil)

	// The tool names below reference BrokerTools by index so the slice above IS
	// the source of truth — adding a broker tool means editing BrokerTools, not
	// copy-pasting a new literal here (and risk the eval scorer drifting).
	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        BrokerTools[0], // "list_servers"
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo — never credentials.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListServersOutput, error) {
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
			Description: "Push a LOCAL file or directory from YOUR machine to a server (the mirror of download_file — direction matters: LocalPath is on your machine, RemotePath is the destination on the server). Pass the server's id (from list_servers), LocalPath (absolute path on your machine of the file or directory to push — a directory is uploaded recursively, preserving relative paths), and RemotePath (absolute destination on the server; its parent directory is created if missing). Returns the file count + total bytes (capped at 1 MiB total; if truncated=true the cap was hit mid-upload and only a PARTIAL tree landed — retry with a smaller payload). Out-of-profile server ids are rejected. SFTP is used, so sudo is not applicable. Use this to push configs/scripts/artifacts to a server; the broker holds the credentials.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in UploadInput) (*mcp.CallToolResult, UploadOutput, error) {
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

	return srv, nil
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
