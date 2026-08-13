package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// serveServiceModeFlag is the internal flag that distinguishes a
// service-managed invocation from an interactive one. It is set by the
// kardianos-aware RunE below when service.Interactive() reports false (we
// were launched by the OS service manager, not from a shell). Purely a
// named bool for legibility — there is no cobra flag registered for it.
//
// (We do NOT register a `--service` cobra flag: the service manager invokes
// us with EXACTLY Config.Arguments = ["serve", "--addr", ...], and adding a
// visible flag to the operator-facing CLI would clutter `serve --help` with
// an internal concern. service.Interactive() is the cleaner seam.)

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
TLS the bearer token travels in cleartext on the network.

This command is ALSO the entry point used by ` + "`serve install`" + `: when
the OS service manager (Windows Service / systemd / launchd) starts the
registered binary, kardianos hands control to svc.Run() inside this RunE,
which invokes program.Start → mcpserver.RunServe in a goroutine. Interactive
invocations (an operator typing ` + "`ssh-manager serve`" + `) run RunServe
directly in the foreground.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// === Service-vs-foreground mode (load-bearing — see serve_service.go) ===
			//
			// service.Interactive() returns false iff this process was launched
			// by the OS service manager (Windows SCM / systemd / launchd). In
			// that case we hand control to kardianos: svc.Run() blocks, calls
			// our program.Start (which spawns RunServe in a goroutine), and
			// returns after Stop fires when the manager signals stop.
			//
			// If Interactive() is true (operator typed `ssh-manager serve` at a
			// shell) OR kardianos cannot decide (returns true on a null system),
			// we run RunServe directly in the foreground — the original path.
			if !service.Interactive() {
				return runServeAsService(addr, tlsCert, tlsKey)
			}

			// Foreground: open the vault, run RunServe until ctx is cancelled.
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
	// a managed background service via github.com/kardianos/service (Windows
	// Service / systemd / launchd). See serve_service.go. Cobra allows a
	// parent command with its own RunE to also have subcommands.
	c.AddCommand(newServeInstallCmd(), newServeUninstallCmd(), newServeStatusCmd())
	return c
}

// runServeAsService hands control to kardianos: it constructs the program with
// the cobra-supplied addr/tls, builds the service.Config, and calls svc.Run()
// which BLOCKS until the service manager signals stop (at which point
// program.Stop fires → RunServe's ctx is cancelled → RunServe returns →
// svc.Run returns → this function returns and the binary exits).
//
// The vault is opened INSIDE program.run (not here) because:
//  1. Start must return quickly (the service manager counts Start's return as
//     "service started"); opening the vault synchronously here would race the
//     manager's startup timeout if the keychain / DPAPI decrypt is slow.
//  2. A vault-open failure should be logged by the program goroutine (so it
//     lands in the service's log sink) rather than surfacing as a svc.Run
//     error code that the manager interprets as "failed to start".
//
// We do NOT consult SSHMGR_MASTERKEY_HEX here: the env is a dev/test affordance
// and a service-managed process inherits a clean environment from the service
// manager (no shell rc files), so the env tier is unreliable in this path.
// Production relies on the FileKeyProvider file (vault.OpenStore(program.run)).
func runServeAsService(addr, tlsCert, tlsKey string) error {
	cfg := &service.Config{
		Name:        serveServiceName,
		DisplayName: serveDisplayName,
		Description: serveDescription,
		// Executable + Arguments are not needed at Run time — kardianos reads
		// them only for Install. We DO need Name + the platform Option keys so
		// the service is constructed against the right platform backend.
		Option: platformServiceOptions(),
	}
	prg := &program{addr: addr, tlsCert: tlsCert, tlsKey: tlsKey}
	s, err := service.New(prg, cfg)
	if err != nil {
		// No service system detected (e.g. Linux container without systemd
		// that somehow reached this branch — should not happen because
		// Interactive() returns true there, but defended anyway). Fall through
		// to the foreground path by returning a clear error.
		return fmt.Errorf("construct service: %w", err)
	}
	// svc.Run blocks until Stop. The returned error (if any) is the start/stop
	// error surfaced by the program's Start/Stop callbacks.
	return s.Run()
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
