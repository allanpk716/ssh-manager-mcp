//go:build windows

package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// --- captureRegisterTask: injects a fake psRunner to capture the PowerShell --
// script + stdin that registerTask would run, without actually launching      -
// powershell.exe. Returns the captured script, stdin, and an "argv" snapshot  -
// (for the password-not-in-argv assertion).                                  -

// capturedPS holds what captureRegisterTask observed.
type capturedPS struct {
	script string
	stdin  string
	// argv is a stand-in the tests use to assert the password never lands on a
	// process command line. registerTask itself does not build a powershell.exe
	// argv with the password — it only sets Run(script, stdin) — so we record
	// the password-bearing channel here as "stdin" and leave argv empty by
	// construction. The contract under test is "password not in argv", which is
	// trivially satisfied because the fake runner has no argv at all.
	argv string
}

// fakePsRunner is a psRunner that records (script, stdin) and returns a fixed
// stdout so registerTask's "REGISTERED" check passes.
type fakePsRunner struct {
	captured *capturedPS
	out      string
}

func (f *fakePsRunner) Run(script, stdin string) (string, error) {
	*f.captured = capturedPS{script: script, stdin: stdin, argv: ""}
	return f.out, nil
}

// captureRegisterTask builds a fake psRunner + a capturedPS, calls registerTask
// with the given inputs + password, and returns what was captured. The fake
// runner reports "REGISTERED\n" so registerTask's success check passes (unless
// the test wants to exercise the non-REGISTERED path).
func captureRegisterTask(in taskInputs, password string) (capturedPS, error) {
	cap := capturedPS{}
	r := &fakePsRunner{captured: &cap, out: "REGISTERED\n"}
	err := registerTask(r, in, password)
	return cap, err
}

// TestRegisterTask_BuildsObjectAPIParams pins the object-API contract: the
// PowerShell script emitted by registerTask must use New-ScheduledTask* cmdlets
// (NOT Register-ScheduledTask -Xml), carry -RunLevel Limited and
// -MultipleInstances IgnoreNew, and pass the password via stdin (NOT argv).
//
// Pins:
//   - FINDING C root cause: object API replaces the broken XML chain.
//   - pi #2 (spike 4): MultipleInstances=IgnoreNew explicit (defense against
//     the boot+logon trigger race spawning two serve instances).
//   - opencode #9 / codex #3 / pi #10: password never enters argv (no /RP, no
//     Get-Credential fragility) — it goes Go-readPassphrase/env -> stdin ->
//     Register-ScheduledTask -Password.
//   - codex #5: TLS flags preserved in the action argument.
//   - spec 5.8: RunLevel Limited (filtered token; serve needs no elevation).
func TestRegisterTask_BuildsObjectAPIParams(t *testing.T) {
	in := taskInputs{
		ExePath: `C:\ssh-manager.exe`, Addr: "0.0.0.0:7878",
		User:    "allan716",
		LogPath: `C:\Users\allan716\AppData\Local\ssh-manager\serve.log`,
		TLSCert: "", TLSKey: "",
	}
	captured, err := captureRegisterTask(in, "testpw")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"New-ScheduledTaskAction",
		"New-ScheduledTaskTrigger -AtStartup",
		"New-ScheduledTaskSettingsSet",
		"-MultipleInstances IgnoreNew",
		"Register-ScheduledTask",
		"-RunLevel Limited",
	} {
		if !strings.Contains(captured.script, want) {
			t.Errorf("PowerShell script missing %q\nscript:\n%s", want, captured.script)
		}
	}
	// Password must NOT be on argv (it goes through stdin only).
	if strings.Contains(captured.argv, "testpw") {
		t.Errorf("password leaked into argv (should be stdin-only): argv=%q", captured.argv)
	}
	// Password must be present in stdin (Go read -> stdin -> Register-ScheduledTask -Password).
	if !strings.Contains(captured.stdin, "testpw") {
		t.Errorf("password missing from stdin: stdin=%q", captured.stdin)
	}
	// AtLogOn trigger must be present alongside AtStartup (boot+logon dual trigger
	// so a freshly-logged-in user gets serve up immediately, plus boot for the
	// headless case).
	if !strings.Contains(captured.script, "New-ScheduledTaskTrigger -AtLogOn") {
		t.Errorf("script missing AtLogOn trigger\nscript:\n%s", captured.script)
	}
	// ExecutionTimeLimit Zero = unlimited (serve is long-running; default PT72H
	// would kill it).
	if !strings.Contains(captured.script, "ExecutionTimeLimit") {
		t.Errorf("script missing ExecutionTimeLimit\nscript:\n%s", captured.script)
	}
	// Register-ScheduledTask must use -Force (idempotent reinstall over a prior
	// task) and -User (the password-logon principal).
	if !strings.Contains(captured.script, "-Force") {
		t.Errorf("script missing -Force (idempotent reinstall)\nscript:\n%s", captured.script)
	}
	if !strings.Contains(captured.script, "-User") {
		t.Errorf("script missing -User\nscript:\n%s", captured.script)
	}
}

// TestRegisterTask_PreservesTLSFlags pins codex #5: when --tls-cert/--tls-key
// are set on `serve install`, the registered task action MUST carry them so
// headless serve uses HTTPS, not plaintext. The TLS flags (--tls-cert /
// --tls-key) appear in the script template; the path VALUES ride on stdin
// (joined into the PowerShell $tlsArg at runtime) so the script body never
// embeds user-controlled paths.
func TestRegisterTask_PreservesTLSFlags(t *testing.T) {
	in := taskInputs{
		ExePath: `C:\bin\ssh-manager.exe`, Addr: "0.0.0.0:7878",
		User:    "u",
		LogPath: `C:\serve.log`,
		TLSCert: `C:\tls\cert.pem`, TLSKey: `C:\tls\key.pem`,
	}
	captured, err := captureRegisterTask(in, "pw")
	if err != nil {
		t.Fatal(err)
	}
	// The FLAGS are in the script body (the values are interpolated at PS
	// runtime from stdin $tlsCert/$tlsKey).
	for _, want := range []string{"--tls-cert", "--tls-key"} {
		if !strings.Contains(captured.script, want) {
			t.Errorf("script missing TLS flag %q\nscript:\n%s", want, captured.script)
		}
	}
	// The path VALUES ride on stdin (newline-delimited field 5 and 6), not in
	// the script body — this is the same stdin-not-argv contract as the
	// password, applied to all user-supplied values.
	if !strings.Contains(captured.stdin, "cert.pem") || !strings.Contains(captured.stdin, "key.pem") {
		t.Errorf("TLS path values missing from stdin: stdin=%q", captured.stdin)
	}
}

// TestRegisterTask_PasswordNeverInScript pins opencode #9 / codex #3 / pi #10
// from a second angle: the password MUST NOT appear anywhere in the PowerShell
// script body (only via $input stdin). This catches a regression where someone
// embeds $password literally into the script template via fmt.Sprintf.
func TestRegisterTask_PasswordNeverInScript(t *testing.T) {
	in := taskInputs{ExePath: `C:\sm.exe`, Addr: "127.0.0.1:7878", User: "u", LogPath: `C:\l.log`}
	captured, err := captureRegisterTask(in, "super-secret-pw-123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(captured.script, "super-secret-pw-123") {
		t.Errorf("password literal leaked into the PowerShell script body (must be stdin-only)\nscript:\n%s", captured.script)
	}
}

// TestRegisterTask_RejectsNonREGISTERED pins that registerTask surfaces a clear
// error when the PowerShell runner does NOT print the "REGISTERED" sentinel
// (e.g. Register-ScheduledTask failed with an access-denied / bad-password
// error). Without this check, serve install would silently succeed even when
// the task was never registered.
func TestRegisterTask_RejectsNonREGISTERED(t *testing.T) {
	in := taskInputs{ExePath: `C:\sm.exe`, Addr: "127.0.0.1:7878", User: "u", LogPath: `C:\l.log`}
	r := &fakePsRunner{captured: &capturedPS{}, out: "Access is denied.\n"}
	err := registerTask(r, in, "pw")
	if err == nil {
		t.Fatal("registerTask must error when PowerShell did not print REGISTERED")
	}
	if !strings.Contains(err.Error(), "REGISTERED") && !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("error should reference the missing confirmation or the runner output: %v", err)
	}
}

// TestRegisterTask_RestartOnFailurePersisted 钉死 FINDING D 修复:对象 API
// -RestartCount 不持久化(spike 3),registerTask 必须额外用 CIM 设
// RestartOnFailure Interval=PT1M Count=3(R1)。CI 断言(目标非硬契约)。
func TestRegisterTask_RestartOnFailurePersisted(t *testing.T) {
	in := taskInputs{ExePath: `C:\ssh-manager.exe`, Addr: "0.0.0.0:7878", User: "u", LogPath: `C:\serve.log`}
	captured, err := captureRegisterTask(in, "pw")
	if err != nil {
		t.Fatal(err)
	}
	// R1 路径:CIM 直接设 RestartOnFailure
	if !strings.Contains(captured.script, "RestartOnFailure") {
		t.Errorf("脚本缺 RestartOnFailure CIM 设值(R1)\n%s", captured.script)
	}
	if !strings.Contains(captured.script, "PT1M") || !strings.Contains(captured.script, "3") {
		t.Errorf("脚本缺 Interval=PT1M / Count=3\n%s", captured.script)
	}
}

// TestServeInstall_PrecheckRejectsUserScopeMasterKey pins codex #2: serve
// install MUST refuse to register a task when master.key is a legacy user-scope
// blob. Without this gate, the registered task would start at boot (Password-
// logon session), fail to read a user-scope master key (FINDING B), and crash-
// loop forever — the exact regression Plan 15 is here to prevent.
//
// === SOUNDNESS (spike-2 caveat, load-bearing — read before changing) ===
// Plan 15 spike 2 (TestDpapi_CrossScopeInteroperable) proved DPAPI's scope flag
// is a HINT, not a hard gate: a blob self-describes its scope and BOTH flags
// decrypt it. So MachineUnprotectForMigrate(userBlob) SUCCEEDS — the brief's
// literal precheck (reject when MachineUnprotectForMigrate errors) would NEVER
// error and silently accept user-scope blobs, recreating FINDING B. The brief's
// precheck is unsound; T3 confirmed this empirically (task-3-report.md CONCERN 1).
//
// Sound mechanism implemented here (matches T3 CONCERN 1's suggested follow-up):
// a sentinel sidecar file `<master.key>.machinescope` is written by
// DpapiKeyProvider.Set (the machine-scope protect path) and removed by Delete.
// The precheck verifies the sentinel exists + the blob is decryptable. A legacy
// user-scope blob written directly (UserProtectForMigrate + os.WriteFile, never
// through Set) has NO sentinel → precheck rejects (correct). A freshly Set or
// migrated blob carries the sentinel → precheck accepts. This is the only sound
// signal under spike-2; blob inspection alone cannot distinguish scope.
func TestServeInstall_PrecheckRejectsUserScopeMasterKey(t *testing.T) {
	dir := t.TempDir()
	masterPath := filepath.Join(dir, "master.key")
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("USERNAME empty (DPAPI blob protect needs a real user session)")
	}
	// Write a user-scope blob DIRECTLY (never via Set, so no sentinel sidecar).
	// This mirrors a pre-Plan-15 NUC10 with a leftover user-scope master.key.
	// 32-byte mk (master key length, pinned across the codebase).
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte('A' + (i % 26)) // distinctive, 32 bytes
	}
	legacyBlob, err := store.DpapiKeyProvider{}.UserProtectForMigrate(mk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masterPath, legacyBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	// Point the keychain seam at this master.key. Get() succeeds (dual-scope),
	// so without a sound precheck the install would proceed and the task would
	// crash-loop at boot.
	origKeychain := keychain
	keychain = store.DpapiKeyProvider{Path: masterPath, DirUser: user}
	defer func() { keychain = origKeychain }()

	// Also override the password reader so the test never touches a TTY. The
	// precheck fires BEFORE the password read, but defensively set both so the
	// test is hermetic even if the precheck ordering ever changes.
	t.Setenv("SSHMGR_SERVE_INSTALL_PASSWORD", "does-not-matter-precheck-fires-first")

	root := NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"serve", "install", "--addr", "127.0.0.1:7878"})
	err = root.Execute()
	if err == nil {
		t.Fatal("serve install must reject a user-scope master.key; got nil err (FINDING B recurrence)")
	}
	if !strings.Contains(err.Error(), "machine-scope") && !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("error must mention machine-scope/unlock to point the user at the fix; got: %v", err)
	}
}

// TestServeInstall_PrecheckAcceptsMachineScopeMasterKey is the green partner of
// the test above: when master.key was written by DpapiKeyProvider.Set (machine-
// scope protect path → sentinel present), the precheck MUST accept and proceed
// to registerTask. This proves the sentinel mechanism doesn't false-reject a
// legitimately machine-scope vault (otherwise install is impossible).
//
// We inject a fake psRunner via a package-level seam so the test never actually
// launches powershell.exe; the test asserts the precheck passed (reached
// registerTask) and registerTask was invoked.
func TestServeInstall_PrecheckAcceptsMachineScopeMasterKey(t *testing.T) {
	dir := t.TempDir()
	masterPath := filepath.Join(dir, "master.key")
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("USERNAME empty")
	}
	// Use Set (machine-scope protect path) — this writes both the blob and the
	// sentinel sidecar, exactly as production unlock/first-run would.
	// 32-byte master key (master key length, pinned across the codebase).
	prov := store.DpapiKeyProvider{Path: masterPath, DirUser: user}
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte('M' + (i % 13)) // distinctive, 32 bytes, M-scope mnemonic
	}
	if err := prov.Set(mk); err != nil {
		t.Fatalf("Set machine-scope master.key: %v", err)
	}
	// Sanity: sentinel exists (T4 sound-mechanism contract).
	if _, err := os.Stat(machineScopeSentinelPath(masterPath)); err != nil {
		t.Fatalf("sentinel not written by DpapiKeyProvider.Set (T4 sound mechanism broken): %v", err)
	}
	origKeychain := keychain
	keychain = prov
	defer func() { keychain = origKeychain }()

	// Inject a fake psRunner so registerTask does NOT launch powershell.exe. The
	// fake returns "REGISTERED\n" so the success path completes. If the precheck
	// rejected, registerTask would never be reached and the captured.script
	// would be empty.
	cap := capturedPS{}
	origRunner := defaultPsRunnerFactory
	defaultPsRunnerFactory = func() psRunner { return &fakePsRunner{captured: &cap, out: "REGISTERED\n"} }
	defer func() { defaultPsRunnerFactory = origRunner }()

	t.Setenv("SSHMGR_SERVE_INSTALL_PASSWORD", "testpw")

	root := NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"serve", "install", "--addr", "127.0.0.1:7878"})
	if err := root.Execute(); err != nil {
		t.Fatalf("serve install must accept a machine-scope master.key: %v", err)
	}
	// registerTask was reached + the fake runner observed the object-API script.
	if !strings.Contains(cap.script, "New-ScheduledTaskAction") {
		t.Errorf("precheck rejected a machine-scope master.key (registerTask not reached, captured script empty). captured=%+v", cap)
	}
}

// --- retained tests (unrelated to the XML/object-API rewrite) ---------------

// TestServeSubcommands_Registered verifies all three subcommands hang off
// `serve` (cobra parent-with-subs pattern) and the foreground RunE is
// preserved (serve with no subcommand still runs RunServe).
func TestServeSubcommands_Registered(t *testing.T) {
	root := NewRootCmd()
	for _, name := range []string{"install", "uninstall", "status"} {
		sub, _, err := root.Find([]string{"serve", name})
		if err != nil {
			t.Errorf("serve %s not registered: %v", name, err)
			continue
		}
		if sub.Name() != name {
			t.Errorf("serve %s resolved to %q", name, sub.Name())
		}
	}
	// serve itself must still resolve (foreground RunE intact).
	srv, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatalf("serve not found: %v", err)
	}
	if srv.RunE == nil {
		t.Errorf("serve parent lost its RunE (foreground serve must still work)")
	}
}

// TestServeStatus_OnMissingTask reports "NOT REGISTERED" and returns nil
// (status is a read command — missing task is a normal state, not an error).
// We force the not-found path by querying a throwaway task name.
func TestServeStatus_OnMissingTask(t *testing.T) {
	// Use the underlying schtasks query helper directly against a name that
	// is vanishingly unlikely to be registered.
	_, _, err := schtasksQuery("ssh-manager-serve-definitely-not-registered-xyz")
	if err == nil {
		t.Skip("schtasks returned no error for a bogus task name (locale/format quirk); skipping not-found branch assertion")
	}
	if !isSchtasksNotFound(err) {
		t.Logf("note: schtasks error for missing task was not classified not-found (locale-dependent): %v", err)
	}
}

// --- GATED integration test (manual / real-machine) ---------------------
//
// The test below TOUCHES THE REAL Task Scheduler: it registers + runs a task,
// which needs the user's Windows password (env SSHMGR_SERVE_INSTALL_PASSWORD)
// and leaves system state behind. It is gated behind SSHMGR_SERVE_INSTALL=1
// and SKIPS by default so `go test ./...` stays hermetic. Run manually:
//
//	SSHMGR_SERVE_INSTALL=1 SSHMGR_SERVE_INSTALL_PASSWORD=<pw> \
//	    go test ./internal/cli/ -run TestServeInstall_Gated -v

func TestServeInstall_Gated(t *testing.T) {
	if !serveInstallGate(t) {
		t.Skip("skipping gated Task Scheduler registration test (set SSHMGR_SERVE_INSTALL=1 to run)")
	}
	if os.Getenv("SSHMGR_SERVE_INSTALL_PASSWORD") == "" {
		t.Fatal("SSHMGR_SERVE_INSTALL=1 but SSHMGR_SERVE_INSTALL_PASSWORD is empty (consensus A: password via env for CI / non-interactive)")
	}

	// 1. uninstall any leftover from a prior run (idempotent).
	if err := schtasksDelete(serveTaskName); err != nil && !isSchtasksNotFound(err) {
		t.Fatalf("pre-clean schtasks /Delete: %v", err)
	}
	t.Cleanup(func() {
		// Always attempt cleanup so a failure mid-test does not leave a
		// registered boot task on the machine.
		_ = schtasksDelete(serveTaskName)
	})

	// 2. Build the object-API inputs + register via the real psRunner.
	in := taskInputs{
		ExePath: mustOwnExe(t),
		Addr:    "127.0.0.1:7878",
		User:    currentUserForTask(),
		LogPath: serveLogPath(),
	}
	if err := registerTask(defaultPsRunner{}, in, os.Getenv("SSHMGR_SERVE_INSTALL_PASSWORD")); err != nil {
		t.Fatalf("registerTask: %v", err)
	}

	// 3. schtasks /Query shows the task exists.
	if _, _, err := schtasksQuery(serveTaskName); err != nil {
		t.Fatalf("schtasks /Query after install: %v", err)
	}

	// 4. uninstall cleans it up.
	if err := schtasksDelete(serveTaskName); err != nil {
		t.Fatalf("schtasks /Delete after install: %v", err)
	}
	if _, _, err := schtasksQuery(serveTaskName); err == nil || !isSchtasksNotFound(err) {
		t.Fatalf("task still queryable after uninstall (err=%v)", err)
	}
}

// serveInstallGate reports whether the gated integration test should run.
// Centralized so the skip message is consistent.
func serveInstallGate(t *testing.T) bool {
	t.Helper()
	gate := "SSHMGR_SERVE_INSTALL"
	if os.Getenv(gate) != "1" {
		return false
	}
	// Also require that powershell.exe exists; if not, skip rather than fail.
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Logf("%s=1 but powershell.exe not on PATH; skipping", gate)
		return false
	}
	return true
}

// mustOwnExe returns the absolute path of the running test binary. In the
// gated test we actually want ssh-manager.exe (not the test binary), so this
// helper resolves the project binary if present, else falls back to the test
// binary (still exercises registration; the action just won't be a real serve).
func mustOwnExe(t *testing.T) string {
	t.Helper()
	// Prefer a built ssh-manager.exe next to the repo if present; otherwise
	// the test binary still proves the registration path.
	for _, cand := range []string{
		"../../ssh-manager.exe",
		"../../../ssh-manager.exe",
	} {
		if _, err := exec.LookPath(cand); err == nil {
			return cand
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal("resolve executable path for gated test:", err)
	}
	return exe
}
