package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

func newServeCmd() *cobra.Command {
	var addr, tlsCert, tlsKey string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the SSH MCP server over HTTP for remote (multi-machine) agents",
		Long: `Run the broker as an authenticated HTTP MCP server so agents on other
machines can share one authoritative vault.

Each request must carry 'Authorization: Bearer <project-token>'. The token
resolves to a project and its profile scope (same gate as 'ssh-manager mcp').

Default bind is loopback (127.0.0.1:7878) — safe. For multi-machine use, set
--addr to 0.0.0.0:7878 or a VLAN IP, and prefer --tls-cert/--tls-key: without
TLS the bearer token travels in cleartext on the network.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := vault.OpenStore(store.FileKeyProvider{})
			if err != nil {
				return err
			}
			defer st.Close()

			if tlsCert == "" && !isLoopback(addr) {
				fmt.Fprintln(os.Stderr, "WARNING: serving plaintext HTTP on a non-loopback address — the bearer token is sniffable. Use --tls-cert/--tls-key.")
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			// The "listening on" line is emitted by RunServe AFTER net.Listen
			// succeeds, so a bind failure no longer prints a misleading line.
			return mcpserver.RunServe(ctx, st, addr, tlsCert, tlsKey)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "listen address (use 0.0.0.0:port or a VLAN IP for remote agents)")
	c.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS cert (enables HTTPS)")
	c.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS key")

	// Subcommands (install/uninstall/status) wrap the foreground RunE above as
	// a managed background service. Windows implements Task Scheduler
	// registration; Linux/macOS build a stub that reports not-yet-supported
	// (see serve_install_windows.go / serve_install_other.go). Cobra allows a
	// parent command with its own RunE to also have subcommands.
	c.AddCommand(newServeInstallCmd(), newServeUninstallCmd(), newServeStatusCmd())
	return c
}

// isLoopback reports whether addr's host part is loopback (best-effort parse).
// Used to suppress the cleartext warning when serving on loopback only.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
