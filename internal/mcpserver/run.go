package mcpserver

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// RunStdio resolves the token to a project+profile, builds the scoped server, and runs it over stdio.
// Returns an error if the store is locked or the token is unknown (caller prints to stderr + exits).
//
// The platform master-key KeyProvider is INJECTED by the caller (the cli/keychain
// seam) so this package stays OS-agnostic and doesn't import cli. vault.OpenStore
// resolves env → kp → FileProvider (3-tier, spec §5.6).
func RunStdio(token string, kp store.KeyProvider) error {
	st, err := vault.OpenStore(kp)
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

// RunStdioCache hydrates a Snapshot into a fresh TEMPORARY read-only store, verifies the SAME
// project token against the cached projects (iron rule + profile scoping intact offline), and
// runs the broker over stdio — identical agent surface to RunStdio. Offline audit lands in
// auditPath (a JSONL sidecar); every mutation is refused (ErrReadOnly). Unknown host keys are
// rejected (SaveHostKey returns ErrReadOnly → HostKeyTOFU fails closed). The temp store is
// deleted on exit; creds in it are sealed under a throwaway master key.
//
// Agent-surface invariant: NewServer is called UNCHANGED — the broker reads the cache via the
// exact same list_servers / exec_command / download_file / upload_file / forward_port / close_port
// tools, gated by the SAME profile scoping (profileID from the verified project) and attributing
// audit to the SAME project id. The only difference from RunStdio is that the store is read-only
// (mutations refused) and audit is sidecar'd (per-machine, single-direction, zero-merge).
func RunStdioCache(token string, snap *store.Snapshot, auditPath string) error {
	mk, err := store.GenerateMasterKey()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "sshmgr-cache-*.db")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	st, err := store.Open(tmpPath, mk)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.ImportSnapshot(snap); err != nil {
		return err
	}

	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer af.Close()
	st.SetReadOnly(af) // AFTER ImportSnapshot: every subsequent mutation → ErrReadOnly

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
	defer tunnels.CloseAll()
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}
