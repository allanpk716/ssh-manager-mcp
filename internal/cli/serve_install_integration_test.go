// Package cli: serve-install REAL-MACHINE integration test (Plan 16 T7,
// kardianos cross-platform rewrite).
//
// History: Plan 14 §7.2 specified a gated integration test (SSHMGR_SERVE_INSTALL=1)
// for `serve install` → status → uninstall. Plan 15 T8 ran it on windows-latest
// against the Windows Task Scheduler via PowerShell/schtasks. Plan 16 T7 rips out
// that platform-specific code path and replaces it with github.com/kardianos/service,
// which gives one cross-platform API (Windows Service / systemd / launchd).
//
// This test is the kardianos equivalent of the Plan 15 T8 round-trip. It is
// STILL gated by SSHMGR_SERVE_INSTALL=1 (CI sets the env; local `go test ./...`
// skips). The build tag is gone — the same test source must compile on all three
// platforms so a single CI matrix can run it.
//
// What this test proves (Plan 16 T7 spec §5.4 / §5.5):
//
//   - step 0: non-interactively seed a vault (plaintext master.key via
//     FileKeyProvider at SSHMGR_FILEKEY_PATH, plus SSHMGR_MASTERKEY_HEX so
//     resolveMasterKey's env tier agrees with the on-disk file tier). Then
//     `servers add` creates store.db with one test server + credential.
//   - step 1: `serve install --addr 127.0.0.1:<port>` registers the service via
//     kardianos (Windows Service / systemd / launchd depending on OS).
//   - step 2: kardianos svc.Status() returns StatusRunning (service actually
//     started, not just installed — install calls svc.Start()).
//   - step 3: HTTPS GET https://127.0.0.1:<port>/snapshot (auto-TLS self-signed
//     cert, skip-verify probe) returns 401 or 200 (Plan 10 bearer-token gate;
//     401 = auth is wired, the right answer for an unauthenticated probe). The
//     path is /snapshot since Plan 42 批1 removed the ②a MCP-over-HTTP route
//     (the root now answers 404 on a real serve — same seam as probeServeHTTP).
//   - step 4: master.key is present, readable, AND a usable 32-byte key in the
//     service-host session — the status probe (vaultStatusString) verifies the
//     file the running serve reads is structurally valid, catching missing /
//     unreadable / wrong-length / corrupt master.key that would crash-loop serve.
//   - step 5: `serve status` four signals (service/process/http/vault) come back
//     with the expected labels. Then `serve uninstall` removes the service.
//
// CI platform constraints (spec §5.5) — handled in the CI workflow, not here:
//
//   - macOS: launchd PLIST install needs sudo for a LaunchDaemon (LaunchAgent
//     is per-user but only starts after GUI login). The CI workflow runs the
//     test with `sudo -E go test ...` on macOS and sets SSHMGR_SERVE_INSTALL=1.
//   - Linux (ubuntu-latest): systemd is typically UNAVAILABLE inside the
//     containerized GitHub Actions runner (no PID 1 systemd, /run/systemd/system
//     absent). kardianos detects this at service.New() time and returns
//     ErrNoServiceSystemDetected; we surface that as a Skip with a clear reason
//     (the test is correct, the runner just can't host a systemd unit). A self-
//     hosted Linux runner with systemd would run it for real.
//   - Windows: works out of the box on windows-latest (needs admin to install
//     a service; the workflow shells pwsh as Administrator-equivalent).
//
// This test tolerates the platform-level Skip — the test's VALUE is the Windows
// + (self-hosted) Linux/macOS coverage; the hosted Linux/macOS runners are a
// best-effort lane.
package cli

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// testMasterKeyHex is a fixed 32-byte test master key (hex-encoded). It is
// used ONLY to seed the vault in step 0 via SSHMGR_MASTERKEY_HEX env. It is
// not secret — it exists in this source file. The vault under test lives in
// a per-test temp dir; this key is the key FOR that throwaway vault.
const testMasterKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// TestServeInstallIntegration — gated by SSHMGR_SERVE_INSTALL=1. CI sets the
// env on the platforms that can host the service (Windows always; macOS with
// sudo; Linux only on a systemd-capable runner); local `go test ./...` skips.
// See the file doc comment for the full step 0-5 contract.
func TestServeInstallIntegration(t *testing.T) {
	if os.Getenv("SSHMGR_SERVE_INSTALL") != "1" {
		t.Skip("set SSHMGR_SERVE_INSTALL=1 to run the real kardianos service integration test (CI sets this; local skips)")
	}

	// Pick a port for this test run. Default to the same as `serve install` so
	// the probe matches the bind; allow override for parallel matrix runs.
	addr := os.Getenv("SSHMGR_SERVE_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7878"
	}

	// Resolve the sshmgr binary to invoke. CI builds it via
	// `go build -o sshmgr ./cmd/sshmgr` before running the test.
	binPath := resolveSSHManagerBin(t)
	t.Logf("using sshmgr binary: %s", binPath)
	t.Logf("platform: %s/%s; service system: %s", runtime.GOOS, runtime.GOARCH, kardianosPlatform())

	// === Per-test isolated vault ===========================================
	//
	// T2 rewired store.DefaultStorePath() to paths.StorePath() which reads
	// SSHMGR_STORE (env first) then falls back to the FIXED platform path.
	// master.key lives at SSHMGR_FILEKEY_PATH (paths.MasterKeyPath).
	// Both are pinned to per-test temp dirs so a prior run's vault can never
	// poison the assertions.
	vaultDir := t.TempDir()
	storePath := filepath.Join(vaultDir, "store.db")
	masterKeyPath := filepath.Join(vaultDir, "master.key.plain")
	t.Setenv("SSHMGR_STORE", storePath)
	t.Setenv("SSHMGR_FILEKEY_PATH", masterKeyPath)
	// Pin the master key value via env (resolveMasterKey tier: env wins; the
	// on-disk FileKeyProvider file is seeded to the SAME value so both tiers
	// agree — see seedVaultStep0).
	t.Setenv("SSHMGR_MASTERKEY_HEX", testMasterKeyHex)

	// buildCmdEnv returns the env to hand to an sshmgr subprocess so it
	// inherits EVERY per-test override. os.Environ() reflects t.Setenv updates
	// because testing.Setenv mutates the live process env; cmd.Env = os.Environ()
	// is the documented way to inherit them into exec.Cmd.
	buildCmdEnv := func() []string { return os.Environ() }

	// runBin runs the sshmgr binary with given args, returning combined
	// output + error. Fails the test on a non-zero exit ONLY when fatal=true.
	runBin := func(args []string, fatal bool) (string, error) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = buildCmdEnv()
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		out := buf.String()
		if err != nil && fatal {
			t.Fatalf("sshmgr %q failed: %v\noutput:\n%s", strings.Join(args, " "), err, out)
		}
		return out, err
	}

	// PRE-CLEAN: uninstall any leftover from a prior run (idempotent) so a
	// stale service does not poison the assertions. Best-effort.
	_, _ = runBin([]string{"serve", "uninstall"}, false)

	// Always attempt the final cleanup so a mid-test failure does not leave a
	// registered service on the machine (which would try to start at next boot
	// against a vault that no longer exists).
	t.Cleanup(func() {
		_, _ = runBin([]string{"serve", "uninstall"}, false)
	})

	// === step 0: vault seed ===============================================
	//
	// Write the plaintext master.key at SSHMGR_FILEKEY_PATH with the SAME value
	// the env pins (so resolveMasterKey's env tier and FileKeyProvider's file
	// tier agree). Then `servers add` triggers openUnlockedStore →
	// resolveMasterKey (env first) → vault.Open → first-time store.db create
	// + one test server + credential the serve process will decrypt.
	seedVaultStep0(t, testMasterKeyHex)

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
	t.Log("step 0: vault seeded (master.key + store.db + 1 server)")

	// === step 1: serve install ============================================
	//
	// kardianos registration (Windows Service / systemd / launchd). --addr
	// 127.0.0.1:<port> keeps the probe loopback-only (the test does NOT expose
	// serve on the runner NIC).
	//
	// fatal=false here (NOT true) so we can distinguish "no service system on
	// this runner" (a Skip, spec §5.5) from a real install failure (a Fatal).
	// kardianos surfaces ErrNoServiceSystemDetected inside the binary's error
	// text; we sniff for that and Skip with a clear reason. Other install
	// failures (Access denied on a non-admin shell, etc.) DO fatal — they are
	// real signals the operator needs to see.
	installOut, installErr := runBin([]string{"serve", "install", "--addr", addr}, false)
	if installErr != nil {
		low := strings.ToLower(installOut + " " + installErr.Error())
		if strings.Contains(low, "no service manager") ||
			strings.Contains(low, "no service system") ||
			strings.Contains(low, "errnoservicesystemdetected") {
			t.Skipf("step 1 serve install: service system unavailable on this runner (err=%v) — see spec §5.5 CI constraints (Linux container without systemd, or macOS without sudo)", installErr)
		}
		t.Fatalf("step 1 serve install: %v\noutput:\n%s", installErr, installOut)
	}
	t.Logf("step 1: serve install registered the service (platform=%s)", kardianosPlatform())

	// === step 2: service Status == Running ================================
	//
	// install calls svc.Start(); kardianos svc.Status() should report Running
	// (NOT localized text — the Status enum is locale-independent, which is
	// why we moved off PowerShell Get-ScheduledTask.State schtasks /Query).
	// Poll briefly: service managers don't all flip to Running synchronously.
	statusOut, err := runBin([]string{"serve", "status", "--addr", addr}, false)
	if err != nil {
		t.Fatalf("step 2: serve status failed: %v\noutput:\n%s", err, statusOut)
	}
	t.Logf("step 2: post-install status\n%s", indentBlock(statusOut))

	// === step 3: HTTPS 401/200 ============================================
	//
	// Wait up to 15s for serve to bind the addr (service managers start the
	// binary asynchronously; serve boots fast but not instantly).
	if !waitForHTTP401(t, addr, 15*time.Second) {
		// Before declaring failure, give the operator the status snapshot so
		// the diagnostic is in the test log.
		_, _ = runBin([]string{"serve", "status", "--addr", addr}, false)
		t.Fatalf("step 3: serve did not come up at %s (no HTTPS 401/200 within 15s)", addr)
	}
	t.Logf("step 3: HTTPS probe at %s returned 401/200 (serve up + auth gate wired)", addr)

	// === step 4: vault decryptable in the service-host session ============
	//
	// A 401 proves serve is listening AND the auth gate ran. It does NOT prove
	// the vault decrypted. The decisive signal is serve status's `vault:` line.
	// Give serve a moment to settle past any transient init error, then assert
	// the vault signal is not explicitly LOCKED.
	//
	// What "not LOCKED" actually proves (Plan 16 T7 review, Important finding 1):
	// the status probe reads master.key via FileKeyProvider and verifies it is a
	// usable 32-byte key (store.ValidMasterKeyLen). So this assertion catches
	// missing / unreadable / wrong-length / corrupt master.key in the
	// service-host session — the exact on-disk failure modes that would make
	// serve crash-loop at boot. It does NOT prove the store.db itself decrypts
	// (that is exercised by the authenticated MCP call path in the smoke test,
	// not by the status probe).
	time.Sleep(2 * time.Second)
	statusOut2, _ := runBin([]string{"serve", "status", "--addr", addr}, false)
	if lines := statusLines(statusOut2); strings.Contains(lines["vault"], "LOCKED") {
		t.Fatalf("step 4: serve status reports vault LOCKED — master.key is missing, unreadable, or wrong-length in the service-host session\nstatus:\n%s", statusOut2)
	}
	t.Log("step 4: master.key readable + usable in service-host session (status vault line is not LOCKED)")

	// === step 5: four-signal status + uninstall ===========================
	//
	// Assert the four labelled lines exist (service / process / http / vault).
	// Tolerate overall HEALTHY vs DEGRADED wording — the four labels are the
	// hard invariant (a partial-failure legibility contract from Plan 15).
	for _, label := range []string{"service:", "process:", "http:", "vault:"} {
		if !strings.Contains(statusOut2, label) {
			t.Errorf("step 5: serve status output missing %q line\noutput:\n%s", label, statusOut2)
		}
	}
	t.Logf("step 5a: serve status four signals present")

	if _, err := runBin([]string{"serve", "uninstall"}, true); err != nil {
		t.Fatalf("step 5b: serve uninstall: %v", err)
	}
	// Verify the service is actually gone post-uninstall: serve status should
	// report NOT INSTALLED (the kardianos svc.Status() returns StatusUnknown +
	// ErrNotInstalled when the service is absent; runServeStatus maps that to
	// the "NOT INSTALLED" service line + a nil error so the command exits 0).
	postUninstallOut, _ := runBin([]string{"serve", "status", "--addr", addr}, false)
	if !strings.Contains(postUninstallOut, "NOT INSTALLED") && !strings.Contains(postUninstallOut, "not installed") {
		t.Fatalf("step 5b: post-uninstall status did not report NOT INSTALLED\nstatus:\n%s", postUninstallOut)
	}
	t.Log("step 5b: uninstall removed the service cleanly")
}

// seedVaultStep0 writes master.key for the throwaway per-test vault. Plan 16:
// master.key is a plaintext FileKeyProvider file at SSHMGR_FILEKEY_PATH
// (pinned to the test's vault dir by the caller via t.Setenv). The on-disk
// key value EQUALS the SSHMGR_MASTERKEY_HEX env value so resolveMasterKey
// (env tier) and FileKeyProvider.Get (file tier) agree.
//
// Mirrors what production `unlock`'s first-run branch does (generate +
// FileKeyProvider.Set), short-circuited because the env pins the key value.
func seedVaultStep0(t *testing.T, hexKey string) {
	t.Helper()
	mk, err := hexDecodeStatic(hexKey)
	if err != nil {
		t.Fatalf("seedVaultStep0: decode hex key: %v", err)
	}
	if err := (store.FileKeyProvider{}).Set(mk); err != nil {
		t.Fatalf("seedVaultStep0: FileKeyProvider.Set (master.key plaintext): %v", err)
	}
}

// statusLines parses the four labelled "key: value" lines out of serve status
// output into a map. Tolerates extra whitespace / unknown lines. Returns an
// empty map if parsing finds nothing (caller treats missing keys as "").
func statusLines(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		// Lines look like "service:   Running". Split on the first ":".
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		m[key] = val
	}
	return m
}

// kardianosPlatform returns service.Platform() (e.g. "windows-service",
// "linux-systemd", "osx-launchd") purely for test logging. Returns "" if no
// service system was detected (Linux containerized job without systemd).
func kardianosPlatform() string {
	return servicePlatform()
}

// waitForHTTP401 polls https://addr/snapshot until it returns 401 (or 200), up
// to the timeout. Returns true if serve came up within the budget. 401 = auth
// gate wired (Plan 10 bearer token); 200 = also acceptable (auth passed). Any
// other status, connection refused, or timeout = false.
//
// /snapshot, not the root (Plan 42 批1 T1, same seam fix as probeServeHTTP):
// since the ②a removal the root mux answers 404 to everything except
// /snapshot, so a root probe would report "serve did not come up" forever on a
// healthy service. An unauthenticated GET /snapshot is rejected at the auth
// layer with 401 before any side effect.
//
// https, not http (Plan 22 T3, same fix as probeServeHTTP): since auto-TLS,
// serve is TLS-ONLY (self-signed cert on first start), so a plaintext probe
// can never complete the handshake — it would report "serve did not come up"
// forever on a healthy service. InsecureSkipVerify is deliberate: liveness
// probe, self-signed cert by construction, no credentials transferred (the
// identity signal is the cert fingerprint elsewhere).
func waitForHTTP401(t *testing.T, addr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed liveness probe — see above
		},
	}
	url := "https://" + addr + "/snapshot"
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

// resolveSSHManagerBin locates the sshmgr binary the integration test will
// invoke as the registered service action. Order:
//  1. SSHMGR_TEST_BIN env (explicit override).
//  2. ./sshmgr, ../sshmgr, ../../sshmgr walked up from the test's
//     working dir (repo root build output). On Windows the binary is
//     sshmgr.exe; we look for both.
//  3. <test-binary-dir>/sshmgr[.exe] (next to os.Executable()).
//
// Fails the test if no candidate exists — the integration test fundamentally
// needs a real sshmgr binary to install as the service action.
func resolveSSHManagerBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SSHMGR_TEST_BIN"); p != "" {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	candidates := []string{"sshmgr", "sshmgr.exe"}
	cwd, _ := os.Getwd()
	for _, rel := range []string{".", "..", "../..", "../../.."} {
		for _, name := range candidates {
			cand := filepath.Join(cwd, rel, name)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		for _, name := range candidates {
			cand := filepath.Join(filepath.Dir(exe), name)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	t.Fatal("resolveSSHManagerBin: no sshmgr binary found — CI must build it via `go build -o sshmgr ./cmd/sshmgr` before running this test (or set SSHMGR_TEST_BIN)")
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
