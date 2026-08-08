package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
)

// NewServer builds an MCP server whose two tools are scoped to profileID and
// attribute exec_command audit rows to projectID.
func NewServer(st *store.Store, profileID, projectID string) (*mcp.Server, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: "v0.1.0"}, nil)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        "list_servers",
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
			Name:        "exec_command",
			Description: "Run a shell command on a server. Pass the server's id (from list_servers), not its name. If sudo=true the broker runs `sudo -S` for you — do NOT prepend 'sudo' to the command yourself. sudo=true only works on servers where has_sudo=true. Out-of-profile server ids are rejected.",
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
			return nil, ExecOutput{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode, TimedOut: out.TimedOut}, nil
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
