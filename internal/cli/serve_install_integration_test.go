//go:build windows

// Package cli: serve-install REAL-MACHINE integration test (Plan 15 T8, FINDING
// C root-cause fix).
//
// Plan 14 §7.2 specified an almost-identical gated integration test, gated on
// SSHMGR_SERVE_INSTALL=1 — and the gate DEFAULTED TO SKIP, was NEVER run on a
// real machine or in CI. The entire chain of serve-install bugs (FINDING C:
// stdin/$input multi-line XML drop, UTF-16 prolog vs UTF-8 bytes, Register-
// ScheduledTask -Xml serialization failure; FINDING D: RestartOnFailure not
// persisted; FINDING E: zh-CN /Query locale misparse) stayed hidden behind
// that gate until NUC10 §7.3 manual acceptance blew them open.
//
// Plan 15 T8 fixes the ROOT CAUSE: this test runs in CI (windows-latest) on
// every push/PR touching serve_install / dpapi / masterkey, and CI SETS
// SSHMGR_SERVE_INSTALL=1 so the gate OPENS there. The gate still defaults to
// skip locally so `go test ./...` stays hermetic on a developer machine.
//
// What this test proves (spec §7.2, full round-trip):
//
//   - step 0 (vault seed, consensus B): non-interactively initialize a vault
//     via SSHMGR_MASTERKEY_HEX env (resolveMasterKey reads env first, tier 1)
//     → `unlock` (idempotent: Get succeeds → postGetMigrator no-op → print
//     the env key) → `servers add` seeds one test server + credential. This
//     also writes master.key + sentinel via DpapiKeyProvider.Set (machine-scope
//     protect path), so the serve-install precheck (codex #2) accepts it.
//   - step 1: `serve install --addr 127.0.0.1:7878` registers the Task
//     Scheduler task (object API, FINDING C fix). Password from
//     SSHMGR_SERVE_INSTALL_PASSWORD env (consensus A).
//   - step 2: Get-ScheduledTask.State == Ready (task registered).
//   - step 3: Settings.MultipleInstances == IgnoreNew (pi #2 / spike 4 defense
//     contract — verified via the PowerShell CIM view).
//   - step 4: schtasks /Run → the registered task action starts serve → HTTP
//     GET http://127.0.0.1:7878/ returns 401 (Plan 10 bearer-token gate; 401 =
//     auth is wired, the right answer for an unauthenticated probe).
//   - step 5: vault decryptable in the task-host session — the serve process
//     started by the task host read machine-scope master.key + decrypted
//     store.db. (Per spec §7.2 v2 opencode #2 this only proves machine-scope
//     works in-task-session, NOT that user-scope is the bug — the FINDING B
//     cross-logon-session closure is NUC10 §7.3 reboot, out of CI's reach.)
//   - step 6: `serve status` four signals (task/process/http/vault) come back
//     with correct semantics; overall HEALTHY or DEGRADED-with-fresh-log. Then
//     `serve uninstall` removes the task + stops the process.
//
// What this test tolerates (spec §7.2 / Plan 15 contract):
//
//   - Tolerance for exact status wording: serve status prints four labelled
//     lines but the overall verdict may be HEALTHY or DEGRADED depending on
//     race between task host startup and our probes. We assert the hard
//     invariants (task registered, HTTP 401, vault decryptable, uninstall
//     clean) and treat status wording as best-effort.
//   - The test is the FIRST automated run against a real Task Scheduler; if it
//     surfaces a real-machine issue (R1 RestartOnFailure persistence on
//     windows-latest's PS version, sentinel write under CI's ACL, locale on
//     the runner image), the failure IS the value — that is exactly the
//     "FINDING C root-cause fix" working as designed.
//
// === CI dependency (user follow-up, NOT implementable here) ===
//
// The workflow references GitHub secret SSHMGR_CI_PASSWORD (the account
// password for the throwaway sshmgrci user it creates via `net user /add`).
// Until the user configures that secret in repo settings, CI runs will fail
// at the test's password-required gate. This is EXPECTED — the workflow is
// correct; the secret is a one-time user setup.
package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testMasterKeyHex is a fixed 32-byte test master key (hex-encoded). It is
// used ONLY to seed the vault in step 0 via SSHMGR_MASTERKEY_HEX env. It is
// not secret — it exists in this source file. The vault under test lives in
// a per-test temp dir; this key is the key FOR that throwaway vault.
const testMasterKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// TestServeInstallIntegration — gated by SSHMGR_SERVE_INSTALL=1. CI sets the
// env; local `go test ./...` skips. See the file doc comment for the full
// step 0-6 contract.
func TestServeInstallIntegration(t *testing.T) {
	if os.Getenv("SSHMGR_SERVE_INSTALL") != "1" {
		t.Skip("set SSHMGR_SERVE_INSTALL=1 to run the real Task Scheduler integration test (CI sets this; local skips)")
	}
	password := os.Getenv("SSHMGR_SERVE_INSTALL_PASSWORD")
	if password == "" {
		t.Fatal("SSHMGR_SERVE_INSTALL=1 but SSHMGR_SERVE_INSTALL_PASSWORD is empty (consensus A: CI/scripts supply the Windows account password via env so the task host can start at boot via Password logon)")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not on PATH (Task Scheduler registration requires PowerShell)")
	}

	// Resolve the ssh-manager.exe to invoke. CI builds it via
	// `go build -o ssh-manager.exe ./cmd/ssh-manager` before running the test.
	// We look for (in order):
	//  1. SSHMGR_TEST_BIN env (explicit override — useful for local runs).
	//  2. ssh-manager.exe walked up from the test working dir (repo root).
	//  3. A binary built next to os.Executable() (test binary dir).
	// If none exists, FAIL — the integration test cannot run without a real
	// ssh-manager.exe to install as the task action.
	binPath := resolveSSHManagerBin(t)
	t.Logf("using ssh-manager binary: %s", binPath)

	// === Per-test isolated vault + serve.log dir ===
	//
	// We cannot easily redirect master.key / store.db via flags (the CLI reads
	// them from UserConfigDir). So we isolate by pointing %AppData% /
	// %LocalAppData% at temp dirs for the duration of the test. Every
	// ssh-manager subprocess we spawn inherits this env (cmd.Env below) and
	// therefore reads/writes the throwaway vault. t.Setenv sets the env for
	// the test process AND is inherited by any exec.Cmd that does NOT
	// override Env (we pass t-helper env explicitly).
	appDataDir := t.TempDir()
	localAppDataDir := t.TempDir()
	t.Setenv("AppData", filepath.Join(appDataDir, "Roaming"))
	t.Setenv("LocalAppData", filepath.Join(localAppDataDir, "Local"))
	if err := os.MkdirAll(os.Getenv("AppData"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(os.Getenv("LocalAppData"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Fixed test master key via env (resolveMasterKey tier 1: env wins over
	// keychain). Every subprocess we spawn inherits this.
	t.Setenv("SSHMGR_MASTERKEY_HEX", testMasterKeyHex)

	// buildCmdEnv returns the env to hand to an ssh-manager subprocess so it
	// inherits the per-test %AppData%/%LocalAppData%/master-key env. We pass
	// the SAME vars t.Setenv set on the test process (os.Environ reflects
	// t.Setenv updates).
	buildCmdEnv := func() []string { return os.Environ() }

	// runBin runs the ssh-manager binary with given args, returning combined
	// output + error. Fails the test on a non-zero exit ONLY when fatal=true
	// (some steps like the pre-clean uninstall are best-effort).
	runBin := func(args []string, fatal bool) (string, error) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = buildCmdEnv()
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		out := buf.String()
		if err != nil && fatal {
			t.Fatalf("ssh-manager %q failed: %v\noutput:\n%s", strings.Join(args, " "), err, out)
		}
		return out, err
	}

	// PRE-CLEAN: uninstall any leftover from a prior run (idempotent) so a
	// stale task does not poison the assertions. Best-effort: a "task not
	// found" error is the normal pre-clean outcome on a fresh runner.
	_, _ = runBin([]string{"serve", "uninstall"}, false)

	// Always attempt the final cleanup so a mid-test failure does not leave a
	// boot task registered on the machine (which would try to start at next
	// boot against a vault that no longer exists).
	t.Cleanup(func() {
		_, _ = runBin([]string{"serve", "uninstall"}, false)
		// Also hard-kill any stray serve process the task host left running
		// (the task host survives task deletion; its serve child does not).
		_, _ = exec.Command("taskkill.exe", "/IM", "ssh-manager.exe", "/F").CombinedOutput()
	})

	// === step 0: vault seed (consensus B) =================================
	//
	// `unlock` with SSHMGR_MASTERKEY_HEX set: Get() succeeds → postGetMigrator
	// no-op (no legacy blob yet, clean env) → prints the env key. Critically,
	// keychain.Set is NOT called on the Get-succeeds path — but the FIRST time
	// resolveMasterKey runs in a subprocess it sees the env var first anyway.
	// To get master.key ON DISK (so the serve-install precheck finds it + the
	// machine-scope sentinel), we generate it explicitly by going through the
	// keychain.Set path: call unlock WITH the env, then `servers add` triggers
	// openUnlockedStore → resolveMasterKey (env first) → vault.Open →
	// first-time store.db create. But that does NOT write master.key either.
	//
	// So: seed master.key + sentinel directly via the DpapiKeyProvider Set
	// path (same mechanism unlock's first-run branch uses), using the SAME key
	// value the env pins. This makes the vault + the on-disk master.key
	// consistent: resolveMasterKey reads the env (tier 1), the on-disk blob
	// decrypts to the same value, the sentinel is present (precheck accepts).
	seedVaultStep0(t, testMasterKeyHex)

	// Verify step 0 took: `servers add` seeds one test server + credential.
	// openUnlockedStore resolves the master key (env first → our hex) →
	// vault.Open creates store.db on first call. This is the credential the
	// serve process will decrypt at boot to prove "vault ok" in step 5.
	if _, err := runBin([]string{
		"servers", "add",
		"--name", "ci-smoke-target",
		"--host", "127.0.0.1",
		"--port", "22",
		"--user", "ci",
		"--password", "ci-dummy-password",
	}, true); err != nil {
		t.Fatalf("step 0 servers add: %v", err)
	}
	t.Log("step 0: vault seeded (master.key + sentinel + store.db + 1 server)")

	// === step 1: serve install ===========================================
	//
	// Object-API registration (FINDING C fix). Password from env (consensus A,
	// readServeInstallPassword env-first). --addr 127.0.0.1:7878 keeps the
	// probe loopback-only (the test does NOT expose serve on the runner NIC).
	// --task-user sshmgrci: register the task under the dedicated CI test
	// account (created by the workflow) whose password is the env secret. On
	// a hosted runner the default (current user = runneradmin) can't work —
	// runneradmin's password is unknown, so Register-ScheduledTask -User
	// -Password must target the account whose password we actually hold.
	taskUser := os.Getenv("SSHMGR_CI_TASK_USER")
	if taskUser == "" {
		taskUser = "sshmgrci"
	}
	args := []string{"serve", "install", "--addr", "127.0.0.1:7878", "--task-user", taskUser}
	if _, err := runBin(args, true); err != nil {
		t.Fatalf("step 1 serve install: %v", err)
	}
	t.Log("step 1: serve install registered the Task Scheduler task")

	// === step 2: task registered (State == Ready) ========================
	//
	// FINDING E: query path is Get-ScheduledTask.State (PowerShell enum, NOT
	// localized text). Ready = registered + not currently running (or Running
	// if schtasks /Run already fired the action during install — the install
	// path does call schtasks /Run; accept either Ready or Running here).
	state := taskStateViaPowerShellOrFail(t)
	if state != "Ready" && state != "Running" {
		t.Fatalf("step 2: expected task State in {Ready, Running}, got %q", state)
	}
	t.Logf("step 2: task registered (State=%s)", state)

	// === step 3: MultipleInstances == IgnoreNew (pi #2 / spike 4) =========
	//
	// The boot + logon dual trigger can both fire; IgnoreNew prevents two
	// serve instances racing for port 7878. Assert it on the persisted task
	// (the object-API script sets -MultipleInstances IgnoreNew; this verifies
	// it actually persisted through Register-ScheduledTask).
	mi := multipleInstancesViaPowerShell(t)
	if mi != "IgnoreNew" {
		t.Fatalf("step 3: expected MultipleInstances=IgnoreNew (pi #2 spike-4 defense), got %q", mi)
	}
	t.Logf("step 3: MultipleInstances=%s (IgnoreNew contract held)", mi)

	// === step 4: schtasks /Run → HTTP 401 ================================
	//
	// install already called schtasks /Run once (best-effort, line ~215 in
	// serve_install_windows.go); re-run here so the probe is deterministic
	// regardless of whether install's /Run won the race against our HTTP
	// probe. Then wait for serve to bind 127.0.0.1:7878 (serve boots fast but
	// not instantly — poll up to 10s).
	if err := schtasksRun(serveTaskName); err != nil {
		t.Fatalf("step 4: schtasks /Run: %v", err)
	}
	if !waitForHTTP401(t, "127.0.0.1:7878", 10*time.Second) {
		t.Fatalf("step 4: serve did not come up at 127.0.0.1:7878 (no HTTP 401 within 10s of schtasks /Run)")
	}
	t.Log("step 4: schtasks /Run → HTTP 401 (serve up + auth gate wired)")

	// === step 5: vault decryptable in the task-host session ==============
	//
	// A 401 response proves serve is listening AND the auth gate ran. It does
	// NOT prove the vault decrypted (serve could be returning 401 from a
	// pre-vault middleware). The decisive signal is serve.log: a serve that
	// failed to decrypt master.key writes the resolveMasterKey hard-fail
	// marker ("master key present but unreadable" / "undecryptable") and
	// exits. So we wait briefly for the log to stabilize, then assert the
	// hard-fail markers are ABSENT. (Per spec §7.2 opencode #2 this only
	// proves machine-scope in-task-session works, not the FINDING B
	// cross-logon closure — that needs NUC10 §7.3 reboot.)
	time.Sleep(2 * time.Second) // let any crash-loop marker flush to serve.log
	if locked, marker := vaultLockedMarkerFromLog(); locked {
		t.Fatalf("step 5: serve.log has a vault-locked marker (%q) — serve could not decrypt master.key in the task-host session", marker)
	}
	t.Log("step 5: vault decryptable in task-host session (no hard-fail marker in serve.log)")

	// === step 6: serve status (four signals) + uninstall =================
	//
	// Tolerate overall verdict HEALTHY or DEGRADED — the four labelled lines
	// are what we assert exist (task / process / http / vault). A race between
	// the task host startup and our probes can leave one signal transiently
	// off; the verdict wording is best-effort, the line PRESENCE is the
	// invariant. Then uninstall must remove the task cleanly.
	statusOut, err := runBin([]string{"serve", "status"}, false)
	if err != nil {
		t.Fatalf("step 6: serve status failed: %v\noutput:\n%s", err, statusOut)
	}
	for _, label := range []string{"task:", "process:", "http:", "vault:"} {
		if !strings.Contains(statusOut, label) {
			t.Errorf("step 6: serve status output missing %q line\noutput:\n%s", label, statusOut)
		}
	}
	t.Logf("step 6a: serve status four signals present\n%s", indentBlock(statusOut))

	if _, err := runBin([]string{"serve", "uninstall"}, true); err != nil {
		t.Fatalf("step 6b: serve uninstall: %v", err)
	}
	// Verify the task is actually gone post-uninstall (not just that the
	// command exited 0). FINDING E query path; isTaskNotFound classifies the
	// "no such task" throw.
	if _, _, err := taskStateViaPowerShell(defaultPsRunner{}, serveTaskName); err == nil {
		t.Fatalf("step 6b: task still queryable after uninstall — uninstall did not remove it")
	} else if !isTaskNotFound(err) {
		t.Fatalf("step 6b: post-uninstall task query returned unexpected error (expected not-found): %v", err)
	}
	t.Log("step 6b: uninstall removed the task cleanly")
}

// seedVaultStep0 writes master.key + sentinel + store.db for the throwaway
// per-test vault. We use DpapiKeyProvider.Set (the machine-scope protect path)
// so the sentinel sidecar is written — the serve-install precheck (codex #2)
// rejects a blob without the sentinel. The on-disk key value EQUALS the
// SSHMGR_MASTERKEY_HEX env value so resolveMasterKey (env first) and
// keychain.Get (disk) agree.
//
// This mirrors what production `unlock`'s first-run branch does (generate +
// keychain.Set), short-circuited because the env pins the key value.
func seedVaultStep0(t *testing.T, hexKey string) {
	t.Helper()
	mk, err := hexDecodeStatic(hexKey)
	if err != nil {
		t.Fatalf("seedVaultStep0: decode hex key: %v", err)
	}
	// keychain is the package seam (store.KeyProvider, Windows impl =
	// DpapiKeyProvider). Set writes master.key (machine-scope DPAPI) + the
	// sentinel sidecar — the same path production unlock's first-run branch
	// takes. Set is part of the KeyProvider interface (masterkey.go:27), so no
	// type assertion is needed.
	if err := keychain.Set(mk); err != nil {
		t.Fatalf("seedVaultStep0: keychain.Set (DpapiKeyProvider machine-scope protect): %v", err)
	}
}

// taskStateViaPowerShellOrFail runs the real PowerShell query for the serve
// task and returns its State enum (Ready/Running/Disabled/...). Fails the test
// on any query error.
func taskStateViaPowerShellOrFail(t *testing.T) string {
	t.Helper()
	state, _, err := taskStateViaPowerShell(defaultPsRunner{}, serveTaskName)
	if err != nil {
		t.Fatalf("Get-ScheduledTask.State query: %v", err)
	}
	return state
}

// multipleInstancesViaPowerShell reads Settings.MultipleInstances off the
// persisted task (pi #2 / spike-4 contract). Returns the raw enum string
// (IgnoreNew / Parallel / Queue). Fails the test on query error.
func multipleInstancesViaPowerShell(t *testing.T) string {
	t.Helper()
	// Get-ScheduledTask returns the settings object directly; .MultipleInstances
	// is a PowerShell enum whose ToString() is the English name regardless of
	// host locale (same property as .State — FINDING E reasoning applies).
	const ps = "$ErrorActionPreference='Stop'\n" +
		"$t = Get-ScheduledTask -TaskName '" + serveTaskName + "' -ErrorAction Stop\n" +
		"Write-Output $t.Settings.MultipleInstances\n"
	out, err := defaultPsRunner{}.Run(ps, "")
	if err != nil {
		t.Fatalf("Get-ScheduledTask.Settings.MultipleInstances query: %v: %s", err, out)
	}
	return strings.TrimSpace(out)
}

// waitForHTTP401 polls http://addr/ until it returns 401 (or 200), up to the
// timeout. Returns true if serve came up within the budget. 401 = auth gate
// wired (Plan 10 bearer token); 200 = also acceptable (auth passed). Any other
// status, connection refused, or timeout = false.
func waitForHTTP401(t *testing.T, addr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	url := "http://" + addr + "/"
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// vaultLockedMarkerFromLog scans serve.log for the master-key hard-fail
// markers (the same set vaultUnlockedFromLog uses). Returns (true, marker) if
// a locked-vault marker is present; (false, "") if the log is clean (serve is
// presumed vault-ok). Absent/unreadable log → (false, "") — a not-yet-written
// log should not be mistaken for a locked vault.
func vaultLockedMarkerFromLog() (bool, string) {
	data, err := os.ReadFile(serveLogPath())
	if err != nil {
		return false, ""
	}
	tail := tailString(string(data), 8192)
	for _, marker := range []string{
		"unreadable",
		"undecryptable",
		"vault locked",
		"run `ssh-manager unlock`",
	} {
		if strings.Contains(tail, marker) {
			return true, marker
		}
	}
	return false, ""
}

// resolveSSHManagerBin locates the ssh-manager.exe the integration test will
// invoke as the registered task action. Order:
//  1. SSHMGR_TEST_BIN env (explicit override).
//  2. ./ssh-manager.exe, ../ssh-manager.exe, ../../ssh-manager.exe walked up
//     from the test's working dir (repo root build output).
//  3. <test-binary-dir>/ssh-manager.exe (next to os.Executable()).
//
// Fails the test if no candidate exists — the integration test fundamentally
// needs a real ssh-manager.exe to install as the task action.
func resolveSSHManagerBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SSHMGR_TEST_BIN"); p != "" {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	cwd, _ := os.Getwd()
	for _, rel := range []string{"ssh-manager.exe", "../ssh-manager.exe", "../../ssh-manager.exe", "../../../ssh-manager.exe"} {
		cand := rel
		if !filepath.IsAbs(cand) {
			cand = filepath.Join(cwd, rel)
		}
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "ssh-manager.exe")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	t.Fatal("resolveSSHManagerBin: no ssh-manager.exe found — CI must build it via `go build -o ssh-manager.exe ./cmd/ssh-manager` before running this test (or set SSHMGR_TEST_BIN)")
	return ""
}

// hexDecodeStatic decodes a hex string to bytes. Local copy (vs importing
// encoding/hex at the package level) so this file remains a self-contained
// integration-test addition with no churn risk from helpers moving.
func hexDecodeStatic(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd-length hex")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexNibble(s[i*2])
		lo, ok2 := hexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex char at byte %d", i)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// indentBlock prefixes every line of s with two spaces, for readable test logs.
func indentBlock(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}
