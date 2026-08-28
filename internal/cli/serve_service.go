// Package cli: serve install/uninstall/status — cross-platform service
// registration via github.com/kardianos/service (Plan 16 T7).
//
// This file REPLACES the Windows-only PowerShell/schtasks impl
// (serve_install_windows.go) and the Unix stub (serve_install_other.go) with
// ONE cross-platform code path:
//
//   - Windows  → Windows Service (installed via advapi32 Service Control
//     Manager — same API surface as the old path, minus PowerShell)
//   - Linux    → systemd unit (or upstart / sysv / openrc, whichever kardianos
//     detects on the host)
//   - macOS    → launchd plist (LaunchDaemon when run as root; LaunchAgent
//     when run as a user — kardianos decides via UserService option)
//
// Spec §5.4 (Plan 16): one service-manager abstraction, RestartOnFailure
// expressed as the native concept on each platform (Windows OnFailure=restart,
// systemd Restart=on-failure, launchd KeepAlive=true).
//
// === Service-vs-foreground mode (load-bearing — read before changing) ===
//
// The SAME `ssh-manager serve` binary is invoked in two contexts:
//
//  1. Interactive: the operator types `ssh-manager serve --addr ...` at a
//     shell. The cobra RunE in serve.go runs mcpserver.RunServe directly
//     (foreground; Ctrl-C exits).
//  2. Service-managed: the OS service manager (SCM / systemd / launchd) starts
//     the registered binary with Config.Arguments (`serve --addr ...`). Cobra
//     dispatches to the same RunE. This time the RunE MUST hand control to
//     kardianos via svc.Run() — the service manager owns the process lifecycle
//     and expects the binary to register a Start/Stop callback contract with
//     it (NOT exit when RunServe returns).
//
// The detection seam is service.Interactive() (kardianos). On Windows it calls
// svc.IsWindowsService() (true iff launched by the SCM); on POSIX it checks
// whether the process is session leader / had its stdio set up by launchd or
// systemd. When false (we're service-managed), serve.go's RunE calls svc.Run()
// → our program.Start → mcpserver.RunServe in a goroutine. When true, the
// foreground path runs unchanged.
//
// === Four-signal status (Plan 15 contract preserved) ===
//
// runServeStatus reports four INDEPENDENT signals so a partial failure is
// legible:
//
//   - service: kardianos svc.Status() (StatusUnknown / StatusRunning /
//     StatusStopped — byte enum, NOT localized text). This is the FINDING E
//     fix carried forward: the old PowerShell Get-ScheduledTask.State parser
//     broke on zh-CN /Query text; svc.Status() returns a Go byte that is
//     locale-independent.
//   - process: is a ssh-manager process running? (best-effort, cross-platform)
//   - http:    does the probed addr respond over https? (401/200 = alive +
//     auth gate wired; TLS because serve is TLS-only since auto-TLS — Plan 22 T1)
//   - vault:   is the master.key present, readable, AND a usable 32-byte key?
//     (in-process file probe — no log scraping; see vaultStatusString for the
//     exact failure modes this catches)
//
// The old serve.log marker-scan (vaultUnlockedFromLog) is DROPPED: under
// kardianos the log path is platform-specific (/var/log/sshd... on Linux,
// Console.app on macOS, EventLog on Windows), so a single log-tail strategy
// does not generalize. The master.key probe is a stronger, simpler signal
// anyway: it tests the exact thing we care about (can the running serve read
// the key AND is the key structurally valid — not just "does the file exist").
package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// serveServiceName is the registered service name (single service per host —
// re-running install overwrites it via Uninstall+Install).
const serveServiceName = "ssh-manager-serve"

// serveDisplayName / serveDescription are the human-friendly metadata written
// into the Windows service / systemd unit / launchd plist.
const (
	serveDisplayName = "ssh-manager serve"
	serveDescription = "ssh-manager-mcp MCP broker — authenticated HTTP MCP server for remote (multi-machine) agents"
)

// program implements service.Interface. Start must return quickly, so it
// spawns a goroutine that opens the vault and runs RunServe (which blocks).
// Stop cancels the goroutine's ctx, which makes RunServe shut the HTTP server
// down and return — kardianos then unblocks svc.Run and the binary exits.
//
// The addr / tlsCert / tlsKey are captured at svc.Run time (NOT at install
// time) because the program{} value passed to service.New when running in
// service-managed mode is constructed inside serve.go's RunE from the cobra
// flags — which carry the args the service manager passed (Config.Arguments).
type program struct {
	addr    string
	tlsCert string
	tlsKey  string

	cancel context.CancelFunc
	doneCh chan struct{} // closed when RunServe has returned
}

// Start is invoked by kardianos (via svc.Run) when the service manager signals
// "start". It MUST return quickly — the service manager counts Start's return
// as "service started successfully". The actual serve loop runs in a goroutine.
func (p *program) Start(s service.Service) error {
	p.doneCh = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.run(ctx)
	return nil
}

// serveLogRotateBytes is the serve.log rotation threshold: an existing log
// larger than this is rotated (renamed to serve.log.1) when the sink opens.
const serveLogRotateBytes = 5 << 20 // 5 MiB

// openServeLog returns the serve.log file sink for the SERVICE path, rotating
// first: a >5MiB serve.log is renamed to serve.log.1 (ONE generation kept —
// the previous .1 is overwritten; os.Rename replaces an existing destination
// on every platform), then the fresh serve.log is opened O_APPEND 0o600.
//
// ANY failure returns nil — serve must never fail because logging failed. The
// caller (program.run) falls back to stderr-only, which the platform service
// manager still captures (EventLog / journald / syslog).
//
// Why a file sink at all (Plan 22 T4): stderr under a service manager goes to
// Windows EventLog, which is painful to inspect on a headless production box
// and records NOTHING on normal operation — a plain, greppable serve.log next
// to the vault makes the NUC10 broker debuggable.
func openServeLog() *os.File {
	p, err := paths.ServeLogPath()
	if err != nil {
		return nil
	}
	if fi, err := os.Stat(p); err == nil && fi.Size() > serveLogRotateBytes {
		_ = os.Rename(p, p+".1") // best-effort; a failed rename still appends below
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// run opens the vault and calls mcpserver.RunServe. Output goes to stderr
// (the service manager captures stderr per-platform: EventLog on Windows,
// journald on systemd, syslog on macOS) AND, when the rotating file sink
// opens, to serve.log (Plan 22 T4 — see openServeLog). The ctx is cancelled
// by Stop.
func (p *program) run(ctx context.Context) {
	defer close(p.doneCh)
	var w io.Writer = os.Stderr
	if f := openServeLog(); f != nil {
		w = io.MultiWriter(os.Stderr, f)
		defer f.Close()
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		fmt.Fprintf(w, "ssh-manager serve (service): open vault: %v\n", err)
		return
	}
	defer st.Close()
	// Post-auto-TLS: RunServe always serves TLS (self-signed when p.tlsCert is
	// empty), so the old "plaintext on non-loopback" warning no longer applies.
	//
	// Startup line for serve.log: RunServe prints its own post-bind
	// "listening on" line to stderr, but offers no injectable writer (its
	// internals are out of scope for this task), so we emit an honest
	// PRE-bind line through w — the file sink records every serve start with
	// the addr + TLS mode. tlsLabel mirrors RunServe's computation: "auto" =
	// self-signed auto-TLS (tlsCert empty), "true" = explicit --tls-cert.
	tlsLabel := "true"
	if p.tlsCert == "" {
		tlsLabel = "auto"
	}
	fmt.Fprintf(w, "ssh-manager serve (service): starting serve on %s (tls=%s)\n", p.addr, tlsLabel)
	if err := mcpserver.RunServe(ctx, st, p.addr, p.tlsCert, p.tlsKey); err != nil {
		fmt.Fprintf(w, "ssh-manager serve (service): %v\n", err)
	}
}

// Stop is invoked by kardianos when the service manager signals "stop". It
// cancels the RunServe ctx and waits briefly for the goroutine to return so
// the HTTP server's Shutdown completes cleanly before the process exits.
// Stop must not block for more than a few seconds (service managers force-
// kill after a platform-specific timeout — Windows ~20s WaitToKillServiceTimeout).
func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.doneCh != nil {
		select {
		case <-p.doneCh:
		case <-time.After(5 * time.Second):
			// Service manager's force-kill timeout will reap us; don't block
			// Stop indefinitely on a hung RunServe.
		}
	}
	return nil
}

// newServeInstallCmd builds `serve install` (cross-platform: registers a
// kardianos-managed service — Windows Service / systemd unit / launchd plist).
func newServeInstallCmd() *cobra.Command {
	var addr, tlsCert, tlsKey string
	c := &cobra.Command{
		Use:   "install",
		Short: "Register serve as a boot-started, auto-restarting background service",
		Long: `Register the foreground 'serve' command as an OS-managed background service:

  Windows:   Windows Service (auto-start at boot, RestartOnFailure=restart)
  Linux:     systemd unit    (Restart=on-failure, WantedBy=multi-user.target)
  macOS:     launchd plist   (RunAtLoad + KeepAlive — LaunchDaemon under sudo,
                              LaunchAgent as the current user)

The registered service runs the SAME ssh-manager binary (resolved at install
time via os.Executable) with ` + "`serve --addr ...`" + ` — so the service
re-uses this exact build. RestartOnFailure is expressed as each platform's
native concept (spec §5.4): no PowerShell, no schtasks, no hand-rolled XML.

master.key must already exist (run 'ssh-manager unlock' first). On Windows the
service runs under the LocalSystem account by default (UserName="" →
LocalSystem); the vault master.key must therefore be readable by that account
(machine-scope DPAPI blob or a plaintext FileKeyProvider file under a
SYSTEM-readable path). For a single-user NUC-style deployment this is the
common case.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeInstall(cmd, addr, tlsCert, tlsKey)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "listen address the registered service will bind (use 0.0.0.0:port or a VLAN IP for remote agents)")
	c.Flags().StringVar(&tlsCert, "tls-cert", "", "path to a TLS cert the registered service will use (optional; if omitted, the service auto-generates a self-signed cert on first start)")
	c.Flags().StringVar(&tlsKey, "tls-key", "", "path to a TLS key the registered service will use (required only when --tls-cert is set)")
	return c
}

func newServeUninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the registered serve service",
		Long: `Stop and remove the serve service (Windows Service / systemd unit /
launchd plist). Vault data (master.key, store.db) is NOT touched — only the
service registration is removed. Re-running install later re-registers.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeUninstall(cmd)
		},
	}
	return c
}

func newServeStatusCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "status",
		Short: "Report whether the serve service is registered, running, listening, and vault-unlocked",
		Long: `Report four INDEPENDENT signals so a partial failure is legible:

  service: kardianos svc.Status() — Running / Stopped / Unknown / NOT INSTALLED
  process: is a ssh-manager serve process running?
  http:    does the bound addr respond over https (TLS)? (401/200 = auth gate wired)
  vault:   is master.key present, readable, AND a usable 32-byte key?
           (in-process file probe — catches missing / corrupt / wrong-length
           master.key that the running serve would crash-loop on at boot)

Each signal diagnoses a different failure mode (registered-but-crashed,
running-but-not-listening, listening-but-vault-locked). overall is HEALTHY
only when all four pass.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeStatus(cmd, addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "serve address to probe (if serve was installed/started with a non-default --addr, pass the same value here)")
	return c
}

// runServeInstall registers the kardianos service — a thin cobra shell over
// installServeService (the programmatic core the Plan 19 role wizard calls via
// tui.SetServeInstaller; tui cannot import cli — cli imports tui).
//
// Precheck rationale (codex #2, FINDING B defense — retained in spirit):
// master.key must exist and decrypt. The old Windows path additionally required
// a machine-scope DPAPI sentinel because the boot Password-logon session could
// not read a user-scope blob; under kardianos the service runs as LocalSystem
// (or root on POSIX), and the master.key is a plaintext FileKeyProvider file
// (Plan 16), so the sentinel concept is gone — the readability check below
// is the simpler, equivalent gate.
func runServeInstall(cmd *cobra.Command, addr, tlsCert, tlsKey string) error {
	return installServeService(addr, tlsCert, tlsKey, cmd.OutOrStdout())
}

// installServeService is the programmatic install core (Plan 19 T4 抽核):
// precheck master.key → resolve own executable → build the service Config →
// best-effort vault-dir ACL harden → idempotent Uninstall+Install+Start.
// All output (including best-effort WARNINGS) goes to out — the CLI passes
// stdout, the wizard passes io.Discard and renders only the error + the manual
// elevated command.
func installServeService(addr, tlsCert, tlsKey string, out io.Writer) error {
	// 1. Precheck: master.key must exist + be structurally usable. The service
	//    host (often LocalSystem / root) needs to read it; if it's missing or
	//    corrupt the service will crash-loop at boot — the worst time to find
	//    out. Get() alone returns nil error for ANY readable file (including
	//    zero-byte / truncated / wrong-length), so we additionally validate the
	//    length via store.ValidMasterKeyLen — exactly the failure mode
	//    vaultStatusString (below) catches on the same key. A bad key here =
	//    a crash-looping service at next boot.
	mk, err := (store.FileKeyProvider{}).Get()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("master key not found: run 'ssh-manager unlock' in an interactive session first (see docs/backup-restore.md)")
		}
		return fmt.Errorf("master key present but undecryptable: %w (if admin-reset password, restore from backup or re-init vault)", err)
	}
	if !store.ValidMasterKeyLen(mk) {
		return fmt.Errorf("master key is %d bytes, expected 32 — corrupt or wrong file; run `ssh-manager unlock` to regenerate, or restore from backup", len(mk))
	}

	// 2. Resolve the binary the service will run. os.Executable returns the
	//    absolute path of THIS ssh-manager build — the service re-runs exactly
	//    the same binary.
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}
	if exePath, err = filepath.Abs(exePath); err != nil {
		return fmt.Errorf("abs executable path: %w", err)
	}

	// 3. Build the service Config. Arguments are what the service manager
	//    passes to the binary when it starts the service — i.e. `serve` with
	//    the same flags the operator just used. TLS flags are preserved
	//    verbatim (codex #5).
	args := []string{"serve", "--addr", addr}
	if tlsCert != "" && tlsKey != "" {
		args = append(args, "--tls-cert", tlsCert, "--tls-key", tlsKey)
	}
	cfg := &service.Config{
		Name:        serveServiceName,
		DisplayName: serveDisplayName,
		Description: serveDescription,
		Executable:  exePath,
		Arguments:   args,
		Option:      platformServiceOptions(),
	}

	// 4. Wire HardenACL (T6) on the vault dir if it exists. install runs with
	//    admin/root privileges (the only context where Install succeeds), so
	//    this is the right place to lock the directory's ACL — the master.key
	//    file ACL is already set by FileKeyProvider.Set, but the parent dir
	//    inherits a potentially-broad DACL on Windows (T6 report concern #1).
	//    Best-effort: a failure here does NOT block install (the file ACL is
	//    the load-bearing protection; the dir ACL is defense-in-depth).
	hardenVaultDirACLBestEffort(out)

	// 5. Create + Install + Start. kardianos returns "service already exists"
	//    on a double-install; we make install idempotent by Uninstall-ing any
	//    prior registration first (a re-install over an existing service is
	//    the common operator flow — e.g. after a binary upgrade).
	s, err := service.New(&program{addr: addr, tlsCert: tlsCert, tlsKey: tlsKey}, cfg)
	if err != nil {
		if errors.Is(err, service.ErrNoServiceSystemDetected) {
			return fmt.Errorf("no service manager detected on this host (err=%w). On Linux this usually means systemd is unavailable (containerized CI runner); on macOS try `sudo`. See docs/multi-machine.md", err)
		}
		return fmt.Errorf("configure service: %w", err)
	}
	// Idempotent re-install: best-effort Uninstall of any prior registration,
	// then Install. A "not installed" error from Uninstall is the normal
	// first-install case (silently ignored).
	if uerr := s.Uninstall(); uerr != nil && !isServiceNotInstalled(uerr) {
		fmt.Fprintf(out, "warning: could not remove prior registration before re-install: %v (continuing)\n", uerr)
	}
	if err := s.Install(); err != nil {
		return fmt.Errorf("install service: %w", err)
	}
	fmt.Fprintf(out, "installed service %q (platform=%s; args=%v)\n", serveServiceName, s.Platform(), args)
	// Start immediately so the service is running now (not only at next boot).
	// Best-effort: if Start fails we do NOT roll back — the service is
	// registered and will start at boot; the user can diagnose via
	// 'serve status'.
	if err := s.Start(); err != nil {
		fmt.Fprintf(out, "warning: service installed but Start failed: %v (registered; will start at boot — check 'ssh-manager serve status')\n", err)
	} else {
		fmt.Fprintln(out, "service started. Use 'ssh-manager serve status' to verify it is listening.")
	}
	return nil
}

// runServeUninstall stops + removes the service — thin shell over
// uninstallServeService (the programmatic core Task 7's `clear` reuses).
// Vault data is NOT deleted.
func runServeUninstall(cmd *cobra.Command) error {
	return uninstallServeService(cmd.OutOrStdout())
}

// uninstallServeService stops + removes the service registration. Vault data
// is NOT deleted. Idempotent: "not installed" is reported to out, not an error.
func uninstallServeService(out io.Writer) error {
	cfg := &service.Config{Name: serveServiceName, Option: platformServiceOptions()}
	s, err := service.New(&program{}, cfg)
	if err != nil {
		if errors.Is(err, service.ErrNoServiceSystemDetected) {
			fmt.Fprintf(out, "no service manager detected on this host (nothing to uninstall)\n")
			return nil
		}
		return fmt.Errorf("configure service: %w", err)
	}
	// Best-effort Stop before Uninstall so the running serve releases the
	// port. A "not installed" error from Stop is ignored.
	if serr := s.Stop(); serr != nil && !isServiceNotInstalled(serr) {
		fmt.Fprintf(out, "warning: service Stop before uninstall: %v (continuing)\n", serr)
	}
	if err := s.Uninstall(); err != nil {
		if isServiceNotInstalled(err) {
			fmt.Fprintf(out, "service %q is not installed (nothing to uninstall)\n", serveServiceName)
			return nil
		}
		return fmt.Errorf("uninstall service: %w", err)
	}
	fmt.Fprintf(out, "uninstalled service %q\n", serveServiceName)
	return nil
}

// runServeStatus reports four independent signals (spec §5.8: process-alive ≠
// vault-unlocked). Each is printed with its own line so a partial failure is
// legible (e.g. service Running but HTTP down = serve crashed mid-init).
//
// addr is the address the http signal probes (--addr flag, default
// 127.0.0.1:7878). The registered service's ACTUAL bind addr lives in
// Config.Arguments; reading it back per-platform is brittle, so the flag is
// the explicit, minimal seam: an operator who installed with a non-default
// --addr passes the same value here.
//
// The "vault" signal here is a file-level probe of master.key (present,
// readable, AND a usable 32-byte key) — NOT a probe of the running serve's
// in-memory vault state. See vaultStatusString for rationale + failure modes.
func runServeStatus(cmd *cobra.Command, addr string) error {
	out := cmd.OutOrStdout()
	cfg := &service.Config{Name: serveServiceName, Option: platformServiceOptions()}
	s, err := service.New(&program{}, cfg)
	if err != nil {
		if errors.Is(err, service.ErrNoServiceSystemDetected) {
			fmt.Fprintf(out, "service:   NOT INSTALLED (no service manager detected on this host)\n")
			fmt.Fprintf(out, "process:   %s\n", boolStr(serveProcessRunning(), "running", "not running"))
			fmt.Fprintf(out, "http:      %s\n", boolStr(probeServeHTTP(addr), "responding", "not responding"))
			fmt.Fprintf(out, "vault:     %s\n", vaultStatusString())
			fmt.Fprintln(out, "overall:   DEGRADED (no service manager)")
			return nil
		}
		return fmt.Errorf("configure service: %w", err)
	}

	// (a) Service State via kardianos svc.Status() — byte enum, NOT localized
	//     text (FINDING E fix carried forward). Unknown + ErrNotInstalled →
	//     NOT INSTALLED.
	stateStr := "Unknown"
	status, serr := s.Status()
	switch {
	case serr != nil && errors.Is(serr, service.ErrNotInstalled):
		stateStr = "NOT INSTALLED"
	case serr != nil:
		stateStr = "Unknown (" + serr.Error() + ")"
	case status == service.StatusRunning:
		stateStr = "Running"
	case status == service.StatusStopped:
		stateStr = "Stopped"
	case status == service.StatusUnknown:
		stateStr = "Unknown"
	}
	fmt.Fprintf(out, "service:   %s\n", stateStr)

	// (b) Process-alive: is a ssh-manager serve process running?
	alive := serveProcessRunning()
	fmt.Fprintf(out, "process:   %s\n", boolStr(alive, "running", "not running"))

	// (c) HTTP-alive: does the probed addr respond over https (401/200 =
	//     alive + auth works)? Probe the --addr flag value (default
	//     127.0.0.1:7878) — the registered service's actual addr is in
	//     Config.Arguments, but reading it back per-platform is brittle; the
	//     HTTP probe is best-effort anyway. An operator who installed with a
	//     non-default --addr passes the same value to status.
	listening := probeServeHTTP(addr)
	fmt.Fprintf(out, "http:      %s\n", boolStr(listening, "responding (401/200 = auth working)", "not responding"))

	// (d) Vault-unlocked: is master.key present, readable, AND a usable key?
	//     Direct file probe — no log scraping (the old serve.log marker-scan
	//     was Windows-specific and does not generalize across platform log
	//     sinks). See vaultStatusString for the exact failure modes this
	//     catches (missing / unreadable / wrong-length key).
	vaultStr := vaultStatusString()
	fmt.Fprintf(out, "vault:     %s\n", vaultStr)

	// Overall verdict. We treat NOT INSTALLED as DEGRADED (not HEALTHY) even
	// if a stray serve process happens to be running, so the operator is
	// nudged to either install or kill the stray.
	overall := "HEALTHY"
	if stateStr != "Running" || !alive || !listening || strings.Contains(vaultStr, "LOCKED") {
		overall = "DEGRADED (see above)"
	}
	fmt.Fprintf(out, "overall:   %s\n", overall)
	return nil
}

// platformServiceOptions returns the platform-specific Option keys that
// express spec §5.4's "RestartOnFailure, expressed natively":
//
//   - Windows: OnFailure=restart, OnFailureDelayDuration=1s,
//     OnFailureResetPeriod=10 (Windows Service recovery actions).
//   - Linux (systemd): Restart=on-failure (kardianos default is "always";
//     "on-failure" matches spec §5.4 intent + avoids restart loops on clean
//     Stop).
//   - macOS (launchd): KeepAlive=true (default), RunAtLoad=true so the
//     service starts as soon as the plist is loaded (not only at next boot /
//     login).
//
// The non-applicable keys are silently ignored by kardianos on the other
// platforms (KeyValue is a map; each platform reads only its own keys).
func platformServiceOptions() service.KeyValue {
	kv := service.KeyValue{}
	switch runtime.GOOS {
	case "windows":
		kv["OnFailure"] = "restart"
		kv["OnFailureDelayDuration"] = "1s"
		kv["OnFailureResetPeriod"] = 10
		kv["StartType"] = "automatic"
	case "linux", "freebsd":
		kv["Restart"] = "on-failure"
	case "darwin":
		kv["KeepAlive"] = true
		kv["RunAtLoad"] = true
	}
	return kv
}

// hardenVaultDirACLBestEffort calls store.HardenACL on the vault directory if
// it exists. install runs with admin/root privileges, so this is the right
// moment to lock the dir ACL (the master.key file is already ACL-locked by
// FileKeyProvider.Set, but the parent dir inherits a broad DACL on Windows —
// T6 report concern #1). On non-Windows, HardenACL is a no-op (file mode
// bits are the protection). Best-effort: warnings go to out, never fatal.
func hardenVaultDirACLBestEffort(out io.Writer) {
	// Resolve the vault dir via paths.MasterKeyPath (SSHMGR_FILEKEY_PATH
	// override-aware) so we ACL the EXACT dir the vault lives in.
	mkPath, err := paths.MasterKeyPath()
	if err != nil || mkPath == "" {
		return
	}
	dir := filepath.Dir(mkPath)
	if _, err := os.Stat(dir); err != nil {
		// Dir doesn't exist yet — nothing to ACL. install before any vault
		// exists is unusual (the precheck above gates on master.key existing)
		// but defended anyway.
		return
	}
	if err := store.HardenACL(dir); err != nil {
		fmt.Fprintf(out, "warning: HardenACL on vault dir %q: %v (file ACL is the load-bearing protection; dir ACL is defense-in-depth)\n", dir, err)
	}
}

// isServiceNotInstalled reports whether err is kardianos's "service not
// installed" signal (used to make Uninstall + Stop idempotent). The Windows
// backend returns ErrNotInstalled via Status(); Uninstall surfaces it as a
// fmt.Errorf wrapper containing "not installed". The POSIX backends return a
// plain error whose text mentions "not installed" / "does not exist". Match
// both shapes so idempotent re-installs work everywhere.
func isServiceNotInstalled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, service.ErrNotInstalled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not installed") ||
		strings.Contains(msg, "not exist") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "does not exist")
}

// --- cross-platform process + HTTP + vault probes (status) ------------------

// serveProcessRunning reports whether a ssh-manager process is currently
// running. Cross-platform: on Windows it shells tasklist (existing impl);
// on POSIX it scans /proc for a matching ssh-manager comm. Best-effort — a
// false negative does not change the overall DEGRADED verdict when the other
// three signals are also off, and a false positive is bounded by the other
// three signals.
func serveProcessRunning() bool {
	switch runtime.GOOS {
	case "windows":
		return serveProcessRunningWindows()
	default:
		return serveProcessRunningPOSIX()
	}
}

// probeServeHTTP does a 1-second GET to the serve address over TLS; a 401 or
// 200 means serve is up and the auth gate is wired. Any other response or
// timeout = not-responding. We deliberately accept 401 (Plan 10 bearer-token
// gate — it is the CORRECT answer for an unauthenticated probe).
//
// The probe targets /snapshot, not the root (Plan 42 批1 T1, spec F2): since
// the ②a removal the root mux answers 404 to everything except /snapshot, so
// a root probe would false-negative a perfectly healthy serve. An
// unauthenticated GET /snapshot is rejected by the cache-token gate at the
// auth layer — 401 before any snapshot serialization or touch (zero side
// effects) — and 401 is the alive signal.
//
// https, not http (Plan 22 T1): since auto-TLS, serve is TLS-ONLY (a self-
// signed cert is generated on first start when no --tls-cert is given), so a
// plaintext probe could never complete a TLS handshake — `serve status`
// reported http "not responding" forever on a healthy production serve.
//
// InsecureSkipVerify is deliberate: this is a LIVENESS probe against the
// configured addr, and auto-TLS certs are self-signed (untrusted by the local
// trust store by construction). Skipping verification here is safe because
// nothing is transferred — no credentials are sent over the connection, and
// identity is NOT what this signal asserts (identity is pinned separately via
// the cert fingerprint, `serve cert-info`). A mis-pointed probe that reaches
// the wrong HTTPS host still only learns "something answered"; the 401/200
// status filter does the rest.
func probeServeHTTP(addr string) bool {
	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			// localhost liveness: self-signed cert — see the rationale above.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get("https://" + addr + "/snapshot")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized
}

// vaultStatusString probes whether the running serve would be able to read the
// master key — i.e. it verifies the master.key the running serve uses is
// present, readable, AND structurally a usable key. Returns "ok" on success,
// or "LOCKED (<reason>)" on missing / unreadable / wrong-length key.
//
// What this probe catches (Plan 16 T7 review, Important finding 1):
//
//   - master.key MISSING  → "LOCKED (master.key not found — ...)" (the operator
//     has not run `ssh-manager unlock` yet; serve would hard-fail at boot).
//   - master.key UNREADABLE (FS permission error, etc.) → "LOCKED (<fs error>)".
//   - master.key WRONG-LENGTH / corrupt / truncated / garbage → "LOCKED (master.key
//     is 4 bytes, expected 32 — corrupt or wrong file; restore from backup)".
//     A truncated / zero-byte / wrong-file master.key file exists on disk but is
//     NOT a usable AES-256 key; serve would crash-loop at boot trying to use it.
//     The old probe (FileKeyProvider.Get alone, which only does os.ReadFile)
//     returned "ok" for these — masking the boot-time failure.
//
// Why NOT a full store.Open decrypt probe: store.Open has side effects (it
// creates store.db + runs the migration on the path it's given), which is
// unacceptable for a *diagnostic* probe invoked by `serve status`. The master
// key is only validated lazily inside GetCredential anyway, so Open would not
// catch a wrong-length key either. The length check via store.ValidMasterKeyLen
// (== keyLen, 32 bytes per GenerateMasterKey) is the lightest faithful proxy
// that catches the real on-disk failure modes without creating vault artifacts.
//
// Note: this probes the FILE, not the running serve's in-memory state. A serve
// that crashed after reading the key still shows ok here — which is why this
// signal is INTENTIONALLY paired with the process + http signals in
// runServeStatus (a dead serve shows up as process=not running + http=not
// responding even if vault=ok).
func vaultStatusString() string {
	mk, err := (store.FileKeyProvider{}).Get()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "LOCKED (master.key not found — run `ssh-manager unlock`)"
		}
		return "LOCKED (" + err.Error() + ")"
	}
	if !store.ValidMasterKeyLen(mk) {
		return fmt.Sprintf("LOCKED (master.key is %d bytes, expected 32 — corrupt or wrong file; restore from backup or re-run `ssh-manager unlock`)", len(mk))
	}
	return "ok"
}

// servicePlatform exposes service.Platform() for the integration test's log
// line (returns "" if no service system was detected on this host).
func servicePlatform() string {
	return service.Platform()
}

func boolStr(b bool, ifTrue, ifFalse string) string {
	if b {
		return ifTrue
	}
	return ifFalse
}
