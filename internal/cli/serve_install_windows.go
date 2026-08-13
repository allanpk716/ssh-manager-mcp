//go:build windows

// Package cli: serve install/uninstall/status — Windows Task Scheduler.
//
// Registers `serve` as a per-user Task Scheduler task that starts at boot and
// at logon, and self-recovers from crashes (RestartOnFailure via CIM Set after
// Register — T5 R1; here we set MultipleInstances=IgnoreNew and omit
// -RestartCount/-RestartInterval because spike-3 showed the object API does not
// persist them). The task runs
// the SAME ssh-manager.exe binary (resolved at install time via os.Executable)
// under the current user account at filtered token level (RunLevel Limited —
// NOT Highest; spec 5.8: serve only reads the user profile + listens on a port;
// elevation is unnecessary and widens blast radius).
//
// === Plan 15 T4 rewrite (object API + Go password read + machine-scope precheck) ===
//
// Plan 14's `serve install` shipped three stacked bugs in the XML chain
// (FINDING C: stdin/$input loses multi-line XML on PS 5.1; UTF-16 prolog vs
// UTF-8 bytes; Register-ScheduledTask -Xml serialization failure) and was
// never usable on a real machine. Plan 15 T4 replaces the XML chain with
// PowerShell's object API (New-ScheduledTaskAction / -Trigger / -SettingsSet /
// Register-ScheduledTask), reads the Windows account password on the Go side
// (consensus A: bypasses Get-Credential / ConvertTo-SecureString headless
// fragility — spike 1), and adds a machine-scope precheck (codex #2) that
// refuses to install when master.key is a legacy user-scope blob (which would
// crash-loop at boot under FINDING B).
//
// Password handling (consensus A; supersedes Plan 14's Get-Credential path):
//
//	`Get-Credential` and `ConvertTo-SecureString` are unreliable in headless /
//	non-interactive PowerShell sessions (spike 1: the Microsoft.PowerShell.Security
//	module + TypeData load is flaky under -NonInteractive). We instead read the
//	password from the Go side — env SSHMGR_SERVE_INSTALL_PASSWORD first (CI /
//	scripts), else TTY no-echo via golang.org/x/term (reused from unlock.go's
//	readPassphrase) — and feed it to Register-ScheduledTask via stdin (PowerShell
//	$input → $password). The password therefore NEVER enters the powershell.exe
//	argv (no Windows 4688 audit-log exposure), and Get-Credential's fragility is
//	bypassed entirely.
//
// The build-tag (windows) keeps this file off Linux/macOS builds; those
// platforms see serve_install_other.go (not-yet-supported stub, spec 5.8 v2).
package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

// serveTaskName is the registered Task Scheduler task name (single task per
// user — re-running install overwrites it).
const serveTaskName = "ssh-manager-serve"

// serveLogRelSubdir / serveLogFile are where the task's stdout/stderr lands,
// relative to %LocalAppData%. Full path: %LocalAppData%\ssh-manager\serve.log.
// The directory is created by the wrapper cmd before serve starts. Kept as
// separate consts (not filepath.Join — that's not constant); the path is
// assembled at use sites with the OS separator.
const (
	serveLogRelSubdir = "ssh-manager"
	serveLogFile      = "serve.log"
)

// serveLogPath returns the absolute serve.log path under %LocalAppData%.
func serveLogPath() string {
	return filepath.Join(os.Getenv("LocalAppData"), serveLogRelSubdir, serveLogFile)
}

// newServeInstallCmd builds `serve install` (Windows: Task Scheduler).
func newServeInstallCmd() *cobra.Command {
	var addr, tlsCert, tlsKey, taskUser string
	c := &cobra.Command{
		Use:   "install",
		Short: "Register serve as a boot-started, auto-restarting background task (Windows Task Scheduler)",
		Long: `Register the foreground 'serve' command as a Windows Task Scheduler task that:

  - starts at boot AND at user logon (LogonType=Password via Register-
    ScheduledTask -User/-Password, so it runs whether or not anyone is
    interactively logged in),
  - collapses concurrent starts via MultipleInstances=IgnoreNew (the boot +
    logon triggers can both fire; IgnoreNew prevents two serve instances),
  - redirects stdout+stderr to %LocalAppData%\ssh-manager\serve.log so a
    headless startup failure is diagnosable,
  - runs as the CURRENT user with a filtered token (RunLevel Limited).

The Windows account password is read by ssh-manager (env
SSHMGR_SERVE_INSTALL_PASSWORD for CI / non-interactive; else a no-echo TTY
prompt) and passed to PowerShell via stdin — it never appears on any process
command line or in the Windows 4688 audit log. The task runs the SAME
ssh-manager.exe binary that you used to run 'serve install'.

master.key must already exist AND be machine-scope DPAPI (run 'ssh-manager
unlock' in an interactive session first — it migrates a legacy user-scope
master.key to machine-scope so the Password-logon task host at boot can read
it). Linux/macOS report 'not yet supported'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeInstall(cmd, addr, tlsCert, tlsKey, taskUser)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "listen address the registered task will bind (use 0.0.0.0:port or a VLAN IP for remote agents)")
	c.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS cert the registered task will use (enables HTTPS)")
	c.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS key the registered task will use")
	c.Flags().StringVar(&taskUser, "task-user", "", "Windows account the registered task runs as (default: current user; accept 'user' or 'DOMAIN\\user'). The password (env SSHMGR_SERVE_INSTALL_PASSWORD or TTY) must be for THIS account.")
	return c
}

func newServeUninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the registered serve Task Scheduler task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeUninstall(cmd)
		},
	}
	return c
}

func newServeStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Report whether the serve task is registered, running, listening, and vault-unlocked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeStatus(cmd)
		},
	}
	return c
}

// runServeInstall registers the Task Scheduler task.
//
// Plan 15 T4: object-API registration replaces Plan 14's broken XML chain
// (FINDING C). The password is read on the Go side (consensus A) and the
// machine-scope precheck (codex #2) refuses to install when master.key is a
// legacy user-scope blob (which would crash-loop at boot — FINDING B).
func runServeInstall(cmd *cobra.Command, addr, tlsCert, tlsKey, taskUser string) error {
	// 1. Precheck: master.key must exist, be decryptable, AND be machine-scope
	//    (codex #2). The boot task host runs under a Password-logon session
	//    that cannot read a user-scope DPAPI blob — installing a task against
	//    such a key would boot-loop the serve (FINDING B). The decryptable +
	//    exists half is covered by keychain.Get (dual-scope under spike-2);
	//    the machine-scope half is verified by a sentinel sidecar file written
	//    by DpapiKeyProvider.Set (see verifyMachineScopeForBoot's doc comment
	//    for why blob inspection alone is unsound under spike-2).
	if _, err := keychain.Get(); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("master key not found: run 'ssh-manager unlock' in an interactive session first (see docs/backup-restore.md)")
		}
		return fmt.Errorf("master key present but undecryptable: %w (if admin-reset password, restore from backup or re-init vault)", err)
	}
	if err := verifyMachineScopeForBoot(); err != nil {
		return err
	}

	// 2. Resolve the binary that will run as the task action. os.Executable
	//    returns the absolute path of THIS ssh-manager.exe — the task re-runs
	//    exactly the same build.
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("abs executable path: %w", err)
	}

	// 3. Resolve %LocalAppData% for the log redirect target.
	localAppData := os.Getenv("LocalAppData")
	if localAppData == "" {
		return fmt.Errorf("%%LocalAppData%% is not set; cannot compute serve.log path")
	}
	logPath := serveLogPath()

	// 4. Read the Windows account password (consensus A). Env first (CI /
	//    scripts), else TTY no-echo. The password NEVER enters argv.
	password, err := readServeInstallPassword(cmd)
	if err != nil {
		return err
	}

	// 5. Register via the PowerShell object API (FINDING C fix). Password +
	//    parameters go through stdin (not argv, not Get-Credential). TLS flags
	//    are preserved in the action argument (codex #5). MultipleInstances=
	//    IgnoreNew is set explicitly (pi #2 / spike-4 defense against the boot
	//    + logon dual trigger spawning two serves).
	// Resolve the Windows account the task runs as. --task-user flag wins
	// (lets CI register under a dedicated test account + lets an owner pin a
	// specific account); else default to the current user (NUC10 single-user:
	// allan716 installs + allan716 runs the task).
	user := taskUser
	if user == "" {
		user = currentUserForTask()
	}
	if user == "" {
		return fmt.Errorf("could not resolve task user: --task-user is empty and USERNAME env is unset")
	}

	in := taskInputs{
		ExePath: exePath,
		Addr:    addr,
		User:    user,
		LogPath: logPath,
		TLSCert: tlsCert,
		TLSKey:  tlsKey,
	}
	if err := registerTask(defaultPsRunnerFactory(), in, password); err != nil {
		return fmt.Errorf("register scheduled task: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "registered task %q (boot+logon trigger, MultipleInstances=IgnoreNew, log -> %s)\n", serveTaskName, logPath)

	// 6. Immediately /Run once so the task is started now (not only at next
	//    boot) and so serve.log is generated. Best-effort: if /Run fails we do
	//    NOT roll back — the task is registered and will start at boot; the
	//    user can diagnose via serve.log + 'serve status'.
	if err := schtasksRun(serveTaskName); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: schtasks /Run failed: %v (task is registered; check 'ssh-manager serve status' and %s)\n", err, logPath)
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "task started. Use 'ssh-manager serve status' to verify it is listening.")
	}
	return nil
}

// runServeUninstall deletes the task. Best-effort kills a still-running serve
// process tree (the task host survives an uninstall; the serve child must be
// stopped explicitly).
func runServeUninstall(cmd *cobra.Command) error {
	if err := schtasksDelete(serveTaskName); err != nil {
		// Distinguish "nothing to uninstall" from a real delete failure so
		// idempotent re-runs after a missing install don't look like errors.
		if isSchtasksNotFound(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "task %q is not registered (nothing to uninstall)\n", serveTaskName)
			return nil
		}
		return fmt.Errorf("delete scheduled task: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "deleted task %q\n", serveTaskName)
	// Best-effort: stop any serve instance the task host left running.
	if err := stopServeProcesses(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: post-uninstall serve stop failed: %v\n", err)
	}
	return nil
}

// runServeStatus reports four independent signals (spec 5.8: process-alive ≠
// vault-unlocked). Each is printed with its own line so a partial failure is
// legible (e.g. task registered but HTTP down = serve crashed mid-boot).
func runServeStatus(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	// (a) Task state via Get-ScheduledTask.State (English enum, not localized
	//     text — FINDING E). The previous schtasks /Query parser scanned for
	//     "Status:"/"任务状态:" prefixes which break on zh-CN and other non-
	//     English hosts; Get-ScheduledTask.State returns a PowerShell enum
	//     value ("Ready"/"Running"/"Disabled") that is NOT localized.
	state, lastResult, qerr := taskStateViaPowerShell(defaultPsRunner{}, serveTaskName)
	if qerr != nil {
		if isTaskNotFound(qerr) {
			fmt.Fprintf(out, "task:      NOT REGISTERED (run 'ssh-manager serve install')\n")
			return nil
		}
		return fmt.Errorf("query scheduled task: %w", qerr)
	}
	fmt.Fprintf(out, "task:      registered (%s, last result %s)\n", state, lastResult)

	// (b) Process-alive: is a ssh-manager serve process running?
	alive := serveProcessRunning()
	fmt.Fprintf(out, "process:   %s\n", boolStr(alive, "running", "not running"))

	// (c) HTTP-alive: does localhost:<port> respond (401 = alive + auth works)?
	listening := probeServeHTTP(addrFromRunningOrDefault())
	fmt.Fprintf(out, "http:      %s\n", boolStr(listening, "responding (401/200 = auth working)", "not responding"))

	// (d) Vault-unlocked: scan serve.log for the hard-fail marker. A live
	//     process with an undecryptable master key is NOT a usable serve.
	unlocked, logNote := vaultUnlockedFromLog()
	fmt.Fprintf(out, "vault:     %s%s\n", boolStr(unlocked, "ok", "LOCKED"), logNote)

	if state == "Running" && alive && listening && unlocked {
		fmt.Fprintln(out, "overall:   HEALTHY")
		return nil
	}
	fmt.Fprintln(out, "overall:   DEGRADED (see above; check serve.log)")
	return nil
}

// --- Object-API registration (Plan 15 T4) --------------------------------
//
// Plan 14's XML chain (buildServeTaskXML + registerTaskViaPowerShell -Xml) had
// three stacked bugs (FINDING C): PS 5.1's $input drops multi-line stdin, the
// UTF-16 prolog collided with UTF-8 bytes, and Register-ScheduledTask -Xml
// serialization failed. The object API (New-ScheduledTask* + Register-
// ScheduledTask) sidesteps all three — no XML document crosses the Go/PS
// boundary, only a small newline-delimited parameter bundle on stdin.

// taskInputs is the data passed to the PowerShell object-API registration.
type taskInputs struct {
	ExePath string // absolute path to ssh-manager.exe (task action)
	Addr    string // --addr the registered task will bind
	User    string // Windows account the task runs as (LogonType=Password)
	LogPath string // %LocalAppData%\ssh-manager\serve.log (stdout+stderr redirect)
	TLSCert string // optional --tls-cert path (preserved verbatim; codex #5)
	TLSKey  string // optional --tls-key path (preserved verbatim; codex #5)
}

// psRunner runs a PowerShell command with a captured stdin + script, returning
// combined stdout+stderr. It is the testable seam for registerTask: tests
// inject a fake psRunner to capture (script, stdin) without launching
// powershell.exe. The production impl is defaultPsRunner.
type psRunner interface {
	Run(script string, stdin string) (stdout string, err error)
}

// defaultPsRunnerFactory is the production psRunner factory. It is a package-
// level var (not a const) so tests can swap it for a fake that captures the
// script + stdin, exercising runServeInstall's wiring (precheck → password read
// → registerTask) end-to-end without launching powershell.exe.
var defaultPsRunnerFactory = func() psRunner { return defaultPsRunner{} }

// defaultPsRunner is the production psRunner: powershell.exe -NoProfile -Command
// <script>, stdin piped in, combined output returned. -NonInteractive is
// intentionally NOT set: PS reads the password from $input (stdin), which
// requires the interactive input stream; -NonInteractive would close it.
type defaultPsRunner struct{}

func (defaultPsRunner) Run(script string, stdin string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readServeInstallPassword reads the Windows account password the task host
// needs to start the serve task at boot (Password logon type). Env
// SSHMGR_SERVE_INSTALL_PASSWORD wins (CI / scripts / non-interactive); otherwise
// the password is read from the TTY with echo off via unlock.go's readPassphrase
// (golang.org/x/term). This is consensus A: Get-Credential + ConvertTo-
// SecureString are unreliable in headless / -NonInteractive PowerShell sessions
// (spike 1), so the password is read on the Go side and handed to PowerShell
// via stdin ($input → $password). It never enters the powershell.exe argv.
func readServeInstallPassword(cmd *cobra.Command) (string, error) {
	if p := os.Getenv("SSHMGR_SERVE_INSTALL_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Enter Windows password for the serve task (stored by Task Scheduler so it can start at boot; not echoed): ")
	b, err := readPassphrase("")
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

// registerTask builds the object-API PowerShell script and runs it via r.
//
// The script reads 8 newline-delimited fields from $input (stdin): exe, addr,
// user, logPath, logDir, tlsCert, tlsKey, password. It then constructs the
// task via New-ScheduledTaskAction / New-ScheduledTaskTrigger (boot + logon) /
// New-ScheduledTaskSettingsSet, and Register-ScheduledTask with -RunLevel
// Limited, -MultipleInstances IgnoreNew, -User, -Password, -Force. The
// REGISTERED sentinel on stdout is the success signal (absent ⇒ the
// Register-ScheduledTask call failed and we surface the runner's combined
// output).
//
// Contracts pinned by tests (see serve_install_windows_test.go):
//   - FINDING C: object API (no XML).
//   - pi #2 / spike 4: -MultipleInstances IgnoreNew explicit (boot+logon race).
//   - opencode #9 / codex #3 / pi #10: password via stdin ONLY, never in argv,
//     never interpolated into the script template (the $password variable is
//     read from $input at runtime).
//   - codex #5: TLS flags preserved in the action argument when both set.
//   - spec 5.8 / opencode #6: -RunLevel Limited (filtered token).
//
// RestartOnFailure persistence (Plan 15 T5, R1): spike 3 proved the object API
// silently drops -RestartCount / -RestartInterval (Count persists as 0). The
// New-ScheduledTaskSettingsSet call here therefore OMITS those flags, and AFTER
// Register-ScheduledTask we set RestartOnFailure via CIM (Get-ScheduledTask →
// mutate $t.Settings.RestartOnFailure → Set-ScheduledTask -InputObject $t).
// Real-machine persistence of the CIM Set on PS 5.1 is UNVERIFIED in the
// worktree (no Task Scheduler available) — deferred to T8 (CI windows-latest)
// and T9 (NUC10 §7.3). Fallback if R1 silently no-ops on PS 5.1: R2
// (schtasks /Change /XML <RestartOnFailure-only fragment>) or best-effort
// degrade (document that RestartOnFailure is unreliable on PS 5.1; rely on the
// boot trigger for crash recovery). This is a target contract, not a hard
// one (consensus C).
func registerTask(r psRunner, in taskInputs, password string) error {
	// PowerShell script. The password + every parameter is read from $input
	// (stdin) — NOTHING is interpolated into this template except the static
	// task name. In particular the password is $p[7], read at runtime, so a
	// grep of the script body cannot find it.
	//
	// NOTE on the string assembly: Go raw strings are delimited by backticks,
	// so a literal PS backtick (the PowerShell line-continuation / escape
	// char, e.g. inside "`n" for newline) cannot appear inside one. We split
	// the script at those points and concatenate via Go double-quoted strings
	// ("`" + "n"). The PS -split operator takes a regex; for a literal newline
	// we pass [Environment]::NewLine-free `n via "`n" OR just "`r`n" — here we
	// split on [char]10 (LF) via the regex '\n' which PS interprets as a
	// newline under -split, sidestepping the backtick entirely.
	const ps = `$ErrorActionPreference='Stop'
$raw = [Console]::In.ReadToEnd()
$p = $raw -split "\r?\n"
$exe=$p[0]; $addr=$p[1]; $user=$p[2]; $logPath=$p[3]; $logDir=$p[4]; $tlsCert=$p[5]; $tlsKey=$p[6]; $password=$p[7]
if (-not $user) { Write-Error "registerTask: user is empty (stdin parse failed; raw len=$($raw.Length))"; exit 3 }
$tlsArg = ''
if ($tlsCert -ne '' -and $tlsKey -ne '') { $tlsArg = ' --tls-cert "' + $tlsCert + '" --tls-key "' + $tlsKey + '"' }
$actionArg = '/C if not exist "' + $logDir + '" mkdir "' + $logDir + '" & "' + $exe + '" serve --addr "' + $addr + '"' + $tlsArg + ' >> "' + $logPath + '" 2>&1'
$action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $actionArg
$trigBoot = New-ScheduledTaskTrigger -AtStartup
$trigLogon = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -DontStopIfGoingOnBatteries -AllowStartIfOnBatteries
Register-ScheduledTask -TaskName '` + serveTaskName + `' -Action $action -Trigger @($trigBoot,$trigLogon) -Settings $settings -RunLevel Limited -User $user -Password $password -Force | Out-Null
$t = Get-ScheduledTask -TaskName '` + serveTaskName + `'
$t.Settings.RestartOnFailure.Interval = 'PT1M'
$t.Settings.RestartOnFailure.Count = 3
Set-ScheduledTask -InputObject $t | Out-Null
Write-Output "REGISTERED"
`
	logDir := filepath.Dir(in.LogPath)
	// stdin fields joined by CRLF: PowerShell 5.1 reads stdin line-by-line and
	// treats CRLF as the line separator (LF-only makes the whole stdin ONE line
	// → $input has 1 element → $p[2]/$p[7] are null → "User argument is null"
	// + password lost). CRLF makes each field a distinct $input element.
	stdin := strings.Join([]string{in.ExePath, in.Addr, in.User, in.LogPath, logDir, in.TLSCert, in.TLSKey, password}, "\r\n")
	out, err := r.Run(ps, stdin)
	if err != nil {
		return fmt.Errorf("powershell: %w: %s", err, out)
	}
	if !strings.Contains(out, "REGISTERED") {
		return fmt.Errorf("powershell did not confirm registration (no REGISTERED sentinel): %s", out)
	}
	return nil
}

// --- machine-scope precheck (codex #2, FINDING B prevention) -------------
//
// verifyMachineScopeForBoot refuses to install a boot task if master.key is not
// provably machine-scope DPAPI. Without this gate, a legacy user-scope blob
// (readable in an interactive / RDP session but NOT in the Password-logon
// session the task host uses at boot) would let the task start at boot, fail to
// read the master key, and crash-loop — exactly the FINDING B regression Plan
// 15 exists to fix.
//
// === SOUNDNESS (spike-2 caveat, load-bearing — read before changing) ===
// Plan 15 spike 2 (TestDpapi_CrossScopeInteroperable) proved DPAPI's scope flag
// is a HINT, not a hard gate: a blob self-describes its scope and BOTH flags
// decrypt it. So store.DpapiKeyProvider.MachineUnprotectForMigrate(userBlob)
// SUCCEEDS — the brief's literal precheck (reject when MachineUnprotectForMigrate
// errors) would NEVER error and silently accept user-scope blobs, recreating
// FINDING B. The brief's precheck is unsound; T3 confirmed this empirically
// (task-3-report.md CONCERN 1).
//
// Sound mechanism implemented here (matches T3 CONCERN 1's suggested follow-up):
// a sentinel sidecar file machineScopeSentinelPath(masterKeyPath) — written by
// DpapiKeyProvider.Set (the machine-scope protect path) — is the ONLY reliable
// signal that the blob was protected with the machine flag. A legacy user-scope
// blob written directly (UserProtectForMigrate + os.WriteFile, never through
// Set) has NO sentinel → precheck rejects. A freshly Set or migrated blob
// carries the sentinel → precheck accepts. Blob inspection alone cannot
// distinguish scope under spike-2; the sentinel is the sound signal.
func verifyMachineScopeForBoot() error {
	masterPath, ok, err := currentMasterKeyPath()
	if err != nil {
		// Could not resolve the path (e.g. %AppData% unset). The keychain.Get
		// sanity already succeeded above, so a failure here is unusual; surface
		// it rather than silently passing the precheck on a config anomaly.
		return fmt.Errorf("locate master.key for machine-scope precheck: %w", err)
	}
	if !ok || masterPath == "" {
		// Non-DpapiKeyProvider keychain (Unix builds don't compile this file,
		// but a future Windows provider might). No machine-scope concept →
		// nothing to verify. Do NOT block install on a non-DPAPI keychain.
		return nil
	}
	blob, rErr := os.ReadFile(masterPath)
	if rErr != nil {
		// keychain.Get already succeeded, so a read failure here is a race or
		// ACL anomaly. Surface it; do NOT silently pass.
		return fmt.Errorf("read master.key for machine-scope precheck: %w (if admin-reset password, restore from backup or re-init vault)", rErr)
	}
	// Sanity: the blob must be decryptable (at least one scope). Under spike-2
	// this nearly always succeeds; it only fails on a genuinely corrupt blob.
	// We do NOT use this for scope detection (spike-2 makes it non-discriminating);
	// it is a defense-in-depth corruption check.
	//
	// Parenthesize the DpapiKeyProvider{} composite literals — Go's parser
	// mis-parses an unparenthesized composite literal at the start of an if
	// condition's init statement (it reads the {} as a statement block).
	dpapi := store.DpapiKeyProvider{}
	if _, mErr := dpapi.MachineUnprotectForMigrate(blob); mErr != nil {
		if _, uErr := dpapi.UserUnprotectForMigrate(blob); uErr != nil {
			return fmt.Errorf("master.key is not decryptable under either DPAPI scope (corrupt or admin-reset): machine=%v user=%v. Restore from backup (docs/backup-restore.md) or re-init vault", mErr, uErr)
		}
	}
	// THE SOUND SCOPE CHECK: sentinel sidecar. Written by DpapiKeyProvider.Set
	// (T4 wiring in masterkey_windows.go). Absent ⇒ legacy user-scope blob ⇒
	// reject with the migration runbook pointer.
	if _, sErr := os.Stat(machineScopeSentinelPath(masterPath)); sErr != nil {
		return fmt.Errorf("master.key is not machine-scope DPAPI (no machine-scope sentinel: %v). boot auto-start needs machine-scope so the Password-logon task host can read it. Run 'ssh-manager unlock' in an interactive session (it migrates a legacy user-scope master.key to machine-scope), then re-run 'serve install'", sErr)
	}
	return nil
}

// machineScopeSentinelPath returns the path to the machine-scope sentinel
// sidecar for a given master.key path (e.g. .../master.key -> .../master.key.machinescope).
// The sentinel is written by DpapiKeyProvider.Set and removed by Delete; it is
// the ONLY sound signal of machine-scope protection under spike-2.
func machineScopeSentinelPath(masterKeyPath string) string {
	return masterKeyPath + ".machinescope"
}

// currentMasterKeyPath resolves the master.key path from the active keychain
// seam. Returns (path, isDpapiProvider, err). Non-DPAPI providers (e.g. Unix
// keyring) return ("", false, nil) — callers treat that as "no machine-scope
// concept, skip the precheck".
func currentMasterKeyPath() (string, bool, error) {
	dkp, ok := keychain.(store.DpapiKeyProvider)
	if !ok {
		return "", false, nil
	}
	pp, err := dkp.PathOrEmpty()
	return pp, true, err
}

// --- schtasks subprocess helpers (status / run / delete) -----------------

func schtasksRun(taskName string) error {
	out, err := exec.Command("schtasks.exe", "/Run", "/TN", taskName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func schtasksDelete(taskName string) error {
	out, err := exec.Command("schtasks.exe", "/Delete", "/TN", taskName, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// taskStateViaPowerShell returns (State, LastTaskResult) for the task via the
// PowerShell object API (FINDING E fix). The previous schtasks /Query parser
// scanned localized text prefixes ("Status:" on en-US, "任务状态:" on zh-CN,
// "計劃工作狀態:" on zh-TW, …) which silently broke `serve status` on every
// non-English host — the State line came back blank and the status command
// showed "registered (Unknown, last result Unknown)".
//
// Get-ScheduledTask.State is a PowerShell enum value (Microsoft.PowerShell.
// ScheduledTask.ScheduledTaskState) that is ALWAYS one of {Unknown, Disabled,
// Queued, Ready, Running} regardless of host UI locale — enum .ToString() is
// not localized the way schtasks /Query's "Status:" label is. Get-
// ScheduledTaskInfo.LastTaskResult is the integer HRESULT (0 = success,
// 0x41301 = "currently running", etc.) — also locale-independent.
//
// The function takes a psRunner so tests can inject a fake returning a fixed
// "Ready\n0" stdout without launching powershell.exe (the brief's
// TestTaskStateViaPowerShell_ParsesEnumState).
//
// Error path: Get-ScheduledTask -ErrorAction Stop throws when the task does
// not exist; the wrapping error includes the runner's combined output so
// isTaskNotFound can classify it (the runServeStatus caller reports NOT
// REGISTERED on that branch).
func taskStateViaPowerShell(r psRunner, taskName string) (state, lastResult string, err error) {
	// NOTE: this string cannot be a raw string literal (backticks) with taskName
	// interpolation — Go raw strings are constant, but the concat makes it non-
	// constant, which the parser rejects inside a function. Use a double-quoted
	// literal with \n escapes + taskName concat instead. The script body is
	// static apart from the task name; nothing user-controlled is interpolated
	// (and taskName is the package const serveTaskName, not user input).
	ps := "$ErrorActionPreference='Stop'\n" +
		"$t = Get-ScheduledTask -TaskName '" + taskName + "' -ErrorAction Stop\n" +
		"$ti = Get-ScheduledTaskInfo -TaskName '" + taskName + "'\n" +
		"Write-Output $t.State\n" +
		"Write-Output $ti.LastTaskResult\n"
	out, err := r.Run(ps, "")
	if err != nil {
		return "", "", fmt.Errorf("powershell: %w: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) >= 1 {
		state = strings.TrimSpace(lines[0])
	}
	if len(lines) >= 2 {
		lastResult = strings.TrimSpace(lines[1])
	}
	return state, lastResult, nil
}

// isTaskNotFound reports whether err came from a Get-ScheduledTask call that
// threw because the named task does not exist. PowerShell emits the thrown
// error record on stderr (UTF-8, NOT the OEM code page schtasks uses), so we
// match English + zh-CN wording directly without OEM decoding. Substring
// matching is locale-robust enough for the status branch's "NOT REGISTERED"
// report (no task ever registered vs. transient query error).
func isTaskNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	return strings.Contains(low, "cannot find") ||
		strings.Contains(low, "no scheduled task") ||
		strings.Contains(low, "was not found") ||
		strings.Contains(msg, "找不到") || // zh-CN: "cannot find"
		strings.Contains(msg, "不存在") // zh-CN: "does not exist"
}

// isSchtasksNotFound reports whether err came from a schtasks call that
// returned "the task does not exist". schtasks emits localized text in the
// console's OEM code page (GBK on zh-CN hosts, CP1252 on en-US), so we first
// decode the raw bytes to UTF-8 via the OEM code page, then match English +
// Chinese wording. Substring matching is locale-robust enough for the
// uninstall + status branches.
func isSchtasksNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := decodeOEM(err.Error())
	low := strings.ToLower(msg)
	return strings.Contains(low, "cannot find") ||
		strings.Contains(low, "does not exist") ||
		strings.Contains(low, "no scheduled tasks") ||
		strings.Contains(msg, "找不到") || // zh-CN: "cannot find"
		strings.Contains(msg, "不存在") // zh-CN: "does not exist"
}

// --- process + HTTP + log probes (status) --------------------------------

// serveProcessRunning reports whether the ssh-manager.exe serve process is
// currently running (best-effort via tasklist).
//
// opencode #7 fix: the previous version did a substring match on
// "ssh-manager.exe" which FALSE-POSITIVES on any process whose name contains
// that substring (e.g. "my-ssh-manager.exe", "ssh-manager.exe-helper"). We now
// match the FIRST CSV field EXACTLY (case-insensitive equal), which is the
// process image name in tasklist's CSV output.
//
// Process-alive is INTENTIONALLY separate from HTTP-alive (probeServeHTTP):
// the two signals diagnose different failures (a running serve that is NOT
// listening = serve crashed mid-init or port conflict; a listening port with
// no process = stale socket / different binary bound the port). Merging them
// would collapse that diagnostic distinction.
func serveProcessRunning() bool {
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq ssh-manager.exe", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		// tasklist prints "INFO: No tasks are running ..." (non-zero exit) when
		// nothing matches; that is the not-running case, not an error.
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		// CSV rows look like: "ssh-manager.exe","1234","Console","1","12,345 K"
		// The first field is the image name; match it EXACTLY (case-insensitive).
		fields := strings.Split(line, ",")
		if len(fields) >= 1 {
			name := strings.Trim(strings.TrimSpace(fields[0]), `"`)
			if strings.EqualFold(name, "ssh-manager.exe") {
				return true
			}
		}
	}
	return false
}

// stopServeProcesses kills any running ssh-manager.exe (taskkill /IM). Used by
// uninstall so the task host's orphaned serve child does not keep the port.
func stopServeProcesses() error {
	out, err := exec.Command("taskkill.exe", "/IM", "ssh-manager.exe", "/F").CombinedOutput()
	if err != nil {
		// Not running is a success here, not an error.
		if strings.Contains(strings.ToLower(string(out)), "not found") ||
			strings.Contains(strings.ToLower(string(out)), "no tasks") {
			return nil
		}
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// probeServeHTTP does a 1-second GET to the serve address; a 401 or 200 means
// serve is up and the auth gate is wired. Any other response or timeout =
// not-responding. We deliberately accept 401 (Plan 10 bearer-token gate).
func probeServeHTTP(addr string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized
}

// addrFromRunningOrDefault returns the --addr the running serve is bound to
// if discoverable, else the install default. We do not yet parse the task XML
// for the addr (keep the status path robust); we default to the same install
// default. The HTTP probe is best-effort anyway.
func addrFromRunningOrDefault() string { return "127.0.0.1:7878" }

// vaultUnlockedFromLog scans the tail of serve.log for the master-key
// hard-fail marker ("master key present but undecryptable" / ErrNotFound text
// from resolveMasterKey, spec 5.6). Returns (true, "") when no marker found
// (process is presumed vault-ok); (false, note) when a marker is found.
//
// Absent or unreadable log → (true, " (no log yet)") — a not-yet-started task
// should not be reported as LOCKED.
//
// === Stale-log degradation hint (consensus E; heartbeat lands in T7) ===
//
// If serve.log's mtime is older than 5 minutes, the log's "no marker" answer
// is no longer trustworthy: a serve that stalled (deadlocked, OOM-quiet, or
// crashed after the last heartbeat write) would leave a marker-free stale log
// that we'd incorrectly report as vault-ok. The 5-minute threshold gives 4x
// margin over the T7 heartbeat cadence (~1min), so this only fires when serve
// is ACTUALLY stalled, not merely idle between heartbeats.
//
// Stale → return (false, " (log stale >5min; current state unknown)") as a
// DEGRADATION HINT, NOT a hard negation: the task/process/http three other
// signals can still drive overall to HEALTHY (a fresh serve that is busy and
// hasn't written the heartbeat in the last 5min should not be marked LOCKED).
// The overall-HEALTHY check in runServeStatus treats this (false) as
// vault-not-ok, which is the intended conservative behavior for a STALLED
// serve. (Re-evaluate if T7 heartbeat proves the 5min threshold too tight.)
func vaultUnlockedFromLog() (bool, string) {
	logPath := serveLogPath()
	if logPath == "" || logPath == string(filepath.Separator) {
		return true, " (no %LocalAppData%; cannot read log)"
	}
	info, err := os.Stat(logPath)
	if err != nil {
		return true, " (no log yet)"
	}
	// Stale check (consensus E): log mtime > 5min → serve heartbeat (T7)
	// should write every ~1min, so 5min is 4x margin. Stale is a degradation
	// HINT, not a hard negation (task/process/http three-way can still be
	// HEALTHY if serve is merely between heartbeats).
	if time.Since(info.ModTime()) > 5*time.Minute {
		return false, " (log stale >5min; current state unknown)"
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return true, " (log unreadable)"
	}
	tail := tailString(string(data), 8192)
	// Markers from the two hard-fail sites: resolveMasterKey (vault.go) emits
	// "master key present but unreadable" (DPAPI/keychain decrypt failure);
	// serveInstall's own precheck (line ~134) emits "undecryptable". Both
	// mean the task is crash-looping with a locked vault. Match distinctive
	// substrings so a future reword stays detected.
	for _, marker := range []string{
		"unreadable",
		"undecryptable",
		"vault locked",
		"run `ssh-manager unlock`",
	} {
		if strings.Contains(tail, marker) {
			return false, fmt.Sprintf(" (locked: serve.log has %q)", marker)
		}
	}
	return true, ""
}

// tailString returns the last maxBytes of s (or all of s if shorter).
func tailString(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:]
}

// --- misc helpers --------------------------------------------------------

func currentUserForTask() string {
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return ""
}

func boolStr(b bool, ifTrue, ifFalse string) string {
	if b {
		return ifTrue
	}
	return ifFalse
}

// decodeOEM best-effort decodes a string whose bytes are in the host's OEM
// console code page (CP936/GBK on zh-CN, CP437/CP1252 on en-US) into UTF-8.
// schtasks/tasklist emit localized text in this code page; without decoding,
// the zh-CN "找不到" / "不存在" not-found markers come back as mojibake and
// isSchtasksNotFound cannot match them. Uses kernel32!GetOEMCP +
// MultiByteToWideChar via syscall (pure stdlib; no x/sys dependency).
//
// On any decoding error the input is returned unchanged (the caller's English
// substring match still catches en-US hosts).
func decodeOEM(s string) string {
	oemCP := getOEMCP()
	utf16, ok := multiByteToWideChar(oemCP, []byte(s))
	if !ok {
		return s
	}
	return utf16ToString(utf16)
}

var (
	kernel32Lazy        = syscall.NewLazyDLL("kernel32.dll")
	procGetOEMCP        = kernel32Lazy.NewProc("GetOEMCP")
	procMultiByteToWide = kernel32Lazy.NewProc("MultiByteToWideChar")
)

// getOEMCP returns the system OEM code page (e.g. 936 for zh-CN, 437 for en-US).
func getOEMCP() uint32 {
	r1, _, _ := procGetOEMCP.Call()
	return uint32(r1)
}

// multiByteToWideChar converts a multi-byte (code-page) byte slice to UTF-16.
// Returns the UTF-16 uint16 slice + ok=false on any API failure. Flags=0
// (MB_ERR_INVALID_CHARS not set) so invalid bytes map to U+FFFD-ish rather
// than failing — callers want best-effort decoding.
func multiByteToWideChar(codePage uint32, b []byte) ([]uint16, bool) {
	if len(b) == 0 {
		return []uint16{}, true
	}
	// First call: query required wide-char count.
	r1, _, _ := procMultiByteToWide.Call(uintptr(codePage), 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		0, 0)
	n := int(r1)
	if n == 0 {
		return nil, false
	}
	out := make([]uint16, n)
	r1, _, _ = procMultiByteToWide.Call(uintptr(codePage), 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&out[0])), uintptr(n))
	if r1 == 0 {
		return nil, false
	}
	return out[:r1], true
}

// utf16ToString converts a UTF-16 uint16 slice (no terminator) to a Go string.
func utf16ToString(u []uint16) string {
	return syscall.UTF16ToString(u)
}
