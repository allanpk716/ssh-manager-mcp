//go:build windows

// Package cli: serve install/uninstall/status — Windows Task Scheduler.
//
// Registers `serve` as a per-user Task Scheduler task that starts at boot and
// self-recovers from crashes. The task runs the SAME ssh-manager.exe binary
// (resolved at install time via os.Executable) under the current user account
// (filtered token, NOT RunLevel Highest — spec 5.8: serve only reads the user
// profile + listens on a port; elevation is unnecessary and widens blast
// radius).
//
// Password handling (spec 5.8; review codex#3 / pi#10 / opencode#9):
//
//	`schtasks /Create /RU <user> /RP <password>` puts the password on the
//	process command line (visible in Process Explorer / Task Manager /
//	Windows 4688 process-creation audit logs). We avoid that path entirely.
//	Instead we shell into PowerShell's Register-ScheduledTask cmdlet, which
//	reads the password interactively via Get-Credential. Get-Credential
//	prompts through the Windows credential dialog (or the host console) and
//	returns a PSCredential whose password lives ONLY in PowerShell process
//	memory — it never crosses the ssh-manager.exe argv, never touches the
//	4688 log. Register-ScheduledTask stores it in the Task Scheduler LSA
//	secret store (the standard, hardened path). schtasks /Create /RP is the
//	documented fallback only (not used here).
//
// The build-tag (windows) keeps this file off Linux/macOS builds; those
// platforms see serve_install_other.go (not-yet-supported stub, spec 5.8 v2).
package cli

import (
	"encoding/xml"
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
	var addr, tlsCert, tlsKey string
	c := &cobra.Command{
		Use:   "install",
		Short: "Register serve as a boot-started, auto-restarting background task (Windows Task Scheduler)",
		Long: `Register the foreground 'serve' command as a Windows Task Scheduler task that:

  - starts at user logon / boot (LogonType=Password, runs whether or not anyone
    is interactively logged in),
  - restarts on crash (RestartOnFailure: 3 attempts, 1 minute apart),
  - redirects stdout+stderr to %LocalAppData%\ssh-manager\serve.log so a
    headless startup failure is diagnosable,
  - runs as the CURRENT user with a filtered token (no RunLevel Highest).

The password is read INTERACTIVELY by PowerShell Get-Credential and never
appears on any process command line or in the Windows 4688 audit log. The task
runs the SAME ssh-manager.exe binary that you used to run 'serve install'.

master.key must already exist (run 'ssh-manager unlock' in an interactive
session first). Linux/macOS report 'not yet supported'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeInstall(cmd, addr, tlsCert, tlsKey)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "listen address the registered task will bind (use 0.0.0.0:port or a VLAN IP for remote agents)")
	c.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS cert the registered task will use (enables HTTPS)")
	c.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS key the registered task will use")
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
func runServeInstall(cmd *cobra.Command, addr, tlsCert, tlsKey string) error {
	// 1. Pre-check: master.key must exist. Without it the task would start,
	//    fail to resolve the master key, and loop-restart. Spec 5.8: confirm
	//    keychain.Get does NOT return ErrNotFound; a decryption error also
	//    blocks install (corrupt master.key is a security event, not a
	//    "please unlock" condition — do NOT route the user through unlock).
	if _, err := keychain.Get(); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("master key not found: run 'ssh-manager unlock' in an interactive session first (see docs/backup-restore.md)")
		}
		return fmt.Errorf("master key present but undecryptable: %w (if admin-reset password, restore from backup or re-init vault)", err)
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

	// 4. Build the schtasks XML definition (at-startup trigger + restart +
	//    log redirect, NO RunLevel Highest). buildServeTaskXML is pure and
	//    unit-tested without registration.
	xmlDef := buildServeTaskXML(serveTaskInputs{
		ExePath: exePath,
		Addr:    addr,
		TLSCert: tlsCert,
		TLSKey:  tlsKey,
		LogPath: logPath,
		User:    currentUserForTask(),
	})

	// 5. Register via PowerShell. Register-ScheduledTask is passed the XML
	//    (via -InputObject from New-ScheduledTask from the inline XML) and
	//    -User <user>; the password is read interactively by Get-Credential
	//    INSIDE PowerShell. Neither the password nor a /RP-style argv is ever
	//    constructed by Go. TaskPassword (PSCredential) lives only in the
	//    PowerShell process.
	if err := registerTaskViaPowerShell(xmlDef); err != nil {
		return fmt.Errorf("register scheduled task: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "registered task %q (at-startup, RestartOnFailure PT1M x3, log -> %s)\n", serveTaskName, logPath)
	fmt.Fprintln(cmd.OutOrStdout(), "starting task once now to verify (schtasks /Run)…")

	// 6. Immediately /Run once so the task is started now (not only at next
	//    boot) and so serve.log is generated. Best-effort: if /Run fails we
	//    do NOT roll back — the task is registered and will start at boot;
	//    the user can diagnose via serve.log + 'serve status'.
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

	// (a) Task state from schtasks /Query /FO LIST.
	state, lastResult, qerr := schtasksQuery(serveTaskName)
	if qerr != nil {
		if isSchtasksNotFound(qerr) {
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

// --- XML generation (pure, unit-tested) ---------------------------------

// serveTaskInputs carries the resolved inputs to buildServeTaskXML. Splitting
// it into a struct keeps the generator signature stable as fields grow and
// makes the unit test declarative.
type serveTaskInputs struct {
	ExePath string // absolute path to ssh-manager.exe
	Addr    string
	TLSCert string
	TLSKey  string
	LogPath string // %LocalAppData%\ssh-manager\serve.log
	User    string // USERNAME; used only for the XML <UserId> comment field
}

// taskXML mirrors the subset of the Task Scheduler schema we emit. Field
// names are upper-cased to match the schema exactly (xml marshalling uses
// the struct field name unless `xml:` is given).
type taskXML struct {
	XMLName        xml.Name           `xml:"Task"`
	Version        string             `xml:"RegistrationInfo>Version"`
	URI            string             `xml:"RegistrationInfo>URI"`
	Settings       taskSettings       `xml:"Settings"`
	Principals     taskPrincipals     `xml:"Principals"`
	Triggers       taskTriggers       `xml:"Triggers"`
	ActionsContext taskActionsContext `xml:"Actions"`
}

type taskSettings struct {
	// RunOnlyIfNetworkAvailable + the restart policy + execution limits.
	// RunLevel is intentionally OMITTED (defaults to LeastPrivilege / filtered
	// token) — spec 5.8 review opencode#6: serve needs no elevation.
	DisallowStartIfOnBatteries bool              `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool              `xml:"StopIfGoingOnBatteries"`
	ExecutionTimeLimit         string            `xml:"ExecutionTimeLimit"`
	RestartOnFailure           taskRestartPolicy `xml:"RestartOnFailure"`
}

type taskRestartPolicy struct {
	Interval string `xml:"Interval,attr"`
	Count    string `xml:"Count,attr"`
}

type taskPrincipals struct {
	Principal taskPrincipal `xml:"Principal"`
}

// taskPrincipal: run as the current user, with a stored password (so the task
// runs at boot before interactive logon), at filtered token level.
type taskPrincipal struct {
	UserId    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type taskTriggers struct {
	BootTrigger struct {
		Enabled string `xml:"Enabled"`
	} `xml:"BootTrigger"`
	LogonTrigger struct {
		Enabled string `xml:"Enabled"`
	} `xml:"LogonTrigger"`
}

// taskActionsContext exists only so we can emit the schema-required Exec
// inside <Actions Context="Author">. The wrapper struct pattern lets us
// attach the attribute to <Actions>.
type taskActionsContext struct {
	Context string   `xml:"Context,attr"`
	Exec    taskExec `xml:"Exec"`
}

type taskExec struct {
	Command   string `xml:"Command"`
	Arguments string `xml:"Arguments"`
}

// buildServeTaskXML returns the Task Scheduler 1.3+ XML for the serve task.
//
// Properties pinned by spec 5.8 + reviews:
//   - RestartOnFailure Interval=PT1M Count=3 (review codex#4 — crash recovery).
//   - LogonType=Password (task runs at boot, before interactive logon).
//   - RunLevel=LeastPrivilege (NOT Highest — review opencode#6 — filtered token
//     is enough to read the user profile + bind a port).
//   - Exec wraps cmd.exe with a /C line that mkdirs the log dir, appends
//     stdout+stderr to serve.log, then runs ssh-manager.exe serve (review
//     opencode#5 — headless failure must be diagnosable). The 2>&1 redirect
//     is what makes a boot-time DPAPI failure visible in the log.
//
// Pure: no env, no fs, no process. Unit-tested by TestBuildServeTaskXML.
func buildServeTaskXML(in serveTaskInputs) string {
	// Build the serve argv. The wrapper runs in cmd.exe context, so we
	// quote each token defensively (paths may contain spaces).
	serveArgv := []string{quoteCmd(in.ExePath), "serve", "--addr", quoteCmd(in.Addr)}
	if in.TLSCert != "" {
		serveArgv = append(serveArgv, "--tls-cert", quoteCmd(in.TLSCert))
	}
	if in.TLSKey != "" {
		serveArgv = append(serveArgv, "--tls-key", quoteCmd(in.TLSKey))
	}
	// cmd.exe /C pipeline: (mkdir log dir) && (append serve's combined output
	// to serve.log). mkdir is idempotent. `>>` appends across restarts so the
	// log accumulates boot-loop evidence. `2>&1` folds stderr in.
	logDir := filepath.Dir(in.LogPath)
	cmdLine := fmt.Sprintf(
		`if not exist %s mkdir %s & %s >> %s 2>&1`,
		quoteCmd(logDir), quoteCmd(logDir),
		strings.Join(serveArgv, " "),
		quoteCmd(in.LogPath),
	)

	task := taskXML{
		Version: "1.0",
		URI:     "\\ssh-manager-serve",
		Settings: taskSettings{
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			ExecutionTimeLimit:         "PT0S", // no limit (serve is long-running)
			RestartOnFailure: taskRestartPolicy{
				Interval: "PT1M",
				Count:    "3",
			},
		},
		Principals: taskPrincipals{
			Principal: taskPrincipal{
				UserId:    in.User,
				LogonType: "Password",
				RunLevel:  "LeastPrivilege",
			},
		},
		ActionsContext: taskActionsContext{
			Context: "Author",
			Exec: taskExec{
				Command:   "cmd.exe",
				Arguments: "/C " + cmdLine,
			},
		},
	}
	// Boot trigger starts at system boot (needs Password logon type, which we
	// set). Logon trigger additionally catches interactive logon for the
	// common case "I just rebooted and want serve up before opening a shell".
	task.Triggers.BootTrigger.Enabled = "true"
	task.Triggers.LogonTrigger.Enabled = "true"

	b, err := xml.MarshalIndent(task, "", "  ")
	if err != nil {
		// Struct is fixed-shape; marshalling cannot fail in practice. If it
		// ever does, surface it loudly instead of emitting a half document.
		return fmt.Sprintf("<!-- xml marshal error: %v -->", err)
	}
	return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + string(b) + "\n"
}

// quoteCmd wraps s in double quotes for cmd.exe consumption. Embedded quotes
// are not expected (paths/tokens we emit don't contain them) but are escaped
// with the cmd.exe caret idiom for safety.
func quoteCmd(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `^"`) + `"`
}

// --- PowerShell registration (password stays out of argv) ----------------

// registerTaskViaPowerShell runs a single `powershell -NoProfile -Command`
// invocation that:
//  1. writes the XML to an in-memory XmlDocument (no temp file on disk),
//  2. prompts for the password via Get-Credential (GUI dialog or host prompt;
//     the password lives only inside the PSCredential object),
//  3. calls Register-ScheduledTask with the XML + the credential.
//
// The password NEVER enters the Go process, the powershell.exe argv, or the
// Windows 4688 audit log. This is the spec 5.8 primary path.
func registerTaskViaPowerShell(xmlDef string) error {
	// We pipe the XML to PowerShell via stdin (encoded) and read it via
	// $input. This avoids both a disk temp file and any argv length ceiling.
	// The here-string terminator "END_OF_TASK_XML" is chosen to be unlikely
	// in real XML.
	const ps = `$ErrorActionPreference='Stop';
$xml = [string]::Join("` + "\n" + `", $input);
$doc = New-Object System.Xml.XmlDocument;
$doc.LoadXml($xml);
$user = $env:USERNAME;
$domain = $env:USERDOMAIN;
$cred = Get-Credential -UserName "$domain\$user" -Message "ssh-manager serve install: enter your Windows password (stored by Task Scheduler so the task can start at boot). The password is NOT echoed to any process command line or audit log.";
if ($null -eq $cred) { Write-Error "no credential entered; aborting install"; exit 2 };
Register-ScheduledTask -TaskName '` + serveTaskName + `' -Xml $doc -User $cred.UserName -Password $cred.GetNetworkCredential().Password -Force | Out-Null;
Write-Output "REGISTERED";
`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
	// stdin gets the XML; PowerShell reads it via $input.
	cmd.Stdin = strings.NewReader(xmlDef)
	// surface PowerShell's own streams for diagnostics.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("powershell: %w: %s", err, out)
	}
	if !strings.Contains(string(out), "REGISTERED") {
		return fmt.Errorf("powershell did not confirm registration: %s", out)
	}
	return nil
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

// schtasksQuery returns (State, LastResult) lines from `schtasks /Query /FO
// LIST /V` for the task. Returns an error wrapping the not-found case so
// callers can branch on isSchtasksNotFound.
func schtasksQuery(taskName string) (state, lastResult string, err error) {
	out, oerr := exec.Command("schtasks.exe", "/Query", "/TN", taskName, "/FO", "LIST").CombinedOutput()
	if oerr != nil {
		return "", "", fmt.Errorf("%w: %s", oerr, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Status:"):
			state = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		case strings.HasPrefix(line, "任务状态:"): // localized "Status:" on zh-CN hosts
			state = strings.TrimSpace(strings.TrimPrefix(line, "任务状态:"))
		case strings.HasPrefix(line, "Schedule:"):
			// "Schedule:" line varies; ignore.
		}
	}
	if state == "" {
		state = "Unknown"
	}
	// Last result requires /V (verbose). Query again with /V to get it.
	vout, verr := exec.Command("schtasks.exe", "/Query", "/TN", taskName, "/FO", "LIST", "/V").CombinedOutput()
	if verr == nil {
		for _, line := range strings.Split(string(vout), "\n") {
			line = strings.TrimSpace(line)
			for _, prefix := range []string{"Last Result:", "上次结果:"} {
				if strings.HasPrefix(line, prefix) {
					lastResult = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				}
			}
		}
	}
	if lastResult == "" {
		lastResult = "Unknown"
	}
	return state, lastResult, nil
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

// serveProcessRunning reports whether any ssh-manager.exe process is currently
// running (best-effort via tasklist).
func serveProcessRunning() bool {
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq ssh-manager.exe", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		return false
	}
	// tasklist prints "INFO: No tasks are running ..." (non-zero exit) when
	// nothing matches; otherwise CSV rows. Any line containing ssh-manager.exe
	// with a PID counts.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), "ssh-manager.exe") {
			return true
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
func vaultUnlockedFromLog() (bool, string) {
	logPath := serveLogPath()
	if logPath == "" || logPath == string(filepath.Separator) {
		return true, " (no %LocalAppData%; cannot read log)"
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return true, " (no log yet)"
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
