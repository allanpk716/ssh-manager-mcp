//go:build !windows

// Package cli: serve install/uninstall/status — Linux/macOS placeholder.
//
// Plan 14 v2 narrowed serve install to Windows Task Scheduler (spec §3.4 /
// §5.8; review codex#9). Linux systemd --user and macOS launchd each have
// platform-specific traps that are out of scope here:
//
//   - Linux: systemd --user requires loginctl enable-linger (else the user
//     manager exits with the session), and D-Bus session discovery differs
//     across distros / containers.
//   - macOS: a LaunchAgent only starts after GUI login, not at boot; a
//     LaunchDaemon runs as root (different account domain from the owner,
//     breaking user-scope master key access — the same reason spec §3.1
//     rejected LocalSystem on Windows).
//
// Rather than ship untested daemons, the three subcommands are present in
// the command tree (so users get a clear "not yet supported" message instead
// of cobra's misleading "unknown command") and defer to a dedicated plan
// (tracked in docs/multi-machine.md, spec §9).
//
// serve.go's AddCommand references these constructors unconditionally; the
// build-tag on this file keeps them off Windows builds (where
// serve_install_windows.go provides the real implementations).
package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newServeInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Register serve as a background service (not yet supported on this OS)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveInstallUnsupported()
		},
	}
}

func newServeUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Unregister the serve background service (not yet supported on this OS)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveInstallUnsupported()
		},
	}
}

func newServeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report serve background-service status (not yet supported on this OS)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveInstallUnsupported()
		},
	}
}

// serveInstallUnsupported returns the canonical "deferred to a follow-up
// plan" error. Same wording across install/uninstall/status.
func serveInstallUnsupported() error {
	return fmt.Errorf("serve install/uninstall/status is not yet supported on %s; see docs/multi-machine.md (tracked for a follow-up plan — Windows Task Scheduler is the only implemented path)", runtime.GOOS)
}
