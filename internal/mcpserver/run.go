package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/vault"
)

// RunStdio resolves the token to a project+profile, builds the scoped server, and runs it over stdio.
// Returns an error if the store is locked or the token is unknown (caller prints to stderr + exits).
func RunStdio(token string) error {
	st, err := vault.OpenStore()
	if err != nil {
		return err
	}
	defer st.Close()
	project, err := st.VerifyToken(token)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("invalid or unknown token")
	}
	srv, tunnels, err := NewServer(st, project.ProfileID, project.ID)
	if err != nil {
		return err
	}
	// MCP-shutdown teardown: when the agent disconnects (stdin closes) srv.Run
	// returns and the deferred CloseAll tears down every open forward_port
	// tunnel — listener + owning ssh.Client — so the process exits with no
	// leaked SSH connections. The idle sweeper goroutine is also stopped. (The
	// go-sdk MCP server has no per-server shutdown hook on the Run path — its
	// session onClose fires per-session; we teardown at Run-return instead,
	// which is the single-session stdio case.)
	defer tunnels.CloseAll()
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}
