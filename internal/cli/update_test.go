package cli

// Plan 44 T8 tests: `sshmgr update` command assembly (spec
// 2026-08-29-plan-44-self-update-rename §4.4/§4.5/§4.6). Everything runs
// through cobra (runUpdate), against an httptest loopback fake release
// source (SSHMGR_UPDATE_BASE, the §4.6 env seam) with the package-level
// function seams swapped for stand-ins. No SCM/systemd/launchd is ever
// touched: service existence is faked via the probeService seam and the
// restart handle via the serviceNew seam — the real SCM read-back is
// verified on real machines (spec §7 G10/G12), not here.
//
// The full-chain tests need a staged binary that actually executes and
// answers `version`: the archive payload IS this test binary, and TestMain
// below intercepts the child invocation when SSHMGR_TEST_STAGED_VERSION is
// set, printing that version — the standard helper-process pattern.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kardianos/service"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/updater"
)

// TestMain intercepts the staged-binary helper invocation. The full-chain
// tests build release archives whose payload is this test binary; when the
// update flow executes the extracted copy as `<staged> version`, this branch
// answers with the version the test pinned in the environment — no real
// cross-compile needed. Any other invocation (the normal `go test` run, or a
// child spawned without the env) runs the test suite as usual.
func TestMain(m *testing.M) {
	if v := os.Getenv("SSHMGR_TEST_STAGED_VERSION"); v != "" {
		fmt.Println(v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// --- fixtures and seams -----------------------------------------------------

// fakeOldBinaryBytes is the content of the fixture "currently installed"
// binary. Nothing in the update flow executes self (only the staged binary
// runs), so the old image can be a tiny marker — and a byte-identical
// assertion after the swap proves the flip unambiguously.
func fakeOldBinaryBytes() []byte { return []byte("fake old sshmgr binary (fixture v0.0.1)\n") }

// selfFileName mirrors the release binary name for the running platform.
func selfFileName() string {
	if runtime.GOOS == "windows" {
		return "sshmgr.exe"
	}
	return "sshmgr"
}

// writeSelfFixture creates a temp HOME with a fake installed binary.
func writeSelfFixture(t *testing.T) (home, self string, oldBytes []byte) {
	t.Helper()
	home = t.TempDir()
	self = filepath.Join(home, selfFileName())
	oldBytes = fakeOldBinaryBytes()
	if err := os.WriteFile(self, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, self, oldBytes
}

// testBinBytes reads this test binary — the archive payload for full-chain
// tests (the extracted staged copy must execute and answer `version`).
func testBinBytes(t *testing.T) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildTestArchive packs payload as the flat single-entry release archive
// shape ExtractBinary expects: root entry `sshmgr.exe` (zip) on windows,
// `sshmgr` (tar.gz) elsewhere — format chosen from the asset name extension.
// The Store method keeps member bytes == payload byte-for-byte.
func buildTestArchive(t *testing.T, assetName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(assetName, ".zip") {
		zw := zip.NewWriter(&buf)
		zh := &zip.FileHeader{Name: "sshmgr.exe", Method: zip.Store}
		zh.SetMode(0o755)
		w, err := zw.CreateHeader(zh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "sshmgr", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeUpdateSource is an httptest loopback fake of the GitHub Releases
// surface (latest + tags/<tag> return the same payload; asset and
// checksums.txt sit in one flat directory, as goreleaser ships them).
type fakeUpdateSource struct {
	srv     *httptest.Server
	tag     string
	asset   string
	archive []byte
	sha     string
}

// startFakeUpdateSource starts the fake source and points
// SSHMGR_UPDATE_BASE at it (loopback http — the §4.2(4) exception).
// browser_download_url deliberately names an off-whitelist host: with a
// custom base the updater must rebuild the URL onto the base (spec
// §4.2(4)), so the fake only answers on its own paths.
func startFakeUpdateSource(t *testing.T, tag string, payload []byte) *fakeUpdateSource {
	t.Helper()
	asset, err := updater.AssetName(tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("compute asset name: %v", err)
	}
	fs := &fakeUpdateSource{tag: tag, asset: asset, archive: buildTestArchive(t, asset, payload)}
	sum := sha256.Sum256(fs.archive)
	fs.sha = hex.EncodeToString(sum[:])

	relJSON := fmt.Sprintf(`{"tag_name":%q,"assets":[`+
		`{"name":%q,"browser_download_url":"http://origin.invalid/%s"},`+
		`{"name":"checksums.txt","browser_download_url":"http://origin.invalid/checksums.txt"}]}`,
		fs.tag, fs.asset, fs.asset)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+buildinfo.Owner+"/"+buildinfo.Repo+"/releases/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, relJSON)
	})
	mux.HandleFunc("/"+fs.asset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fs.archive)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", fs.sha, fs.asset)
	})
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	t.Setenv("SSHMGR_UPDATE_BASE", fs.srv.URL)
	return fs
}

// withStagedVersion pins what the staged-binary helper (TestMain) answers.
func withStagedVersion(t *testing.T, v string) {
	t.Helper()
	t.Setenv("SSHMGR_TEST_STAGED_VERSION", v)
}

// fakeRestarter is the serviceNew-seam stand-in.
type fakeRestarter struct {
	status     service.Status
	statusErr  error
	restartErr error
	restartReq int
}

func (f *fakeRestarter) Status() (service.Status, error) { return f.status, f.statusErr }
func (f *fakeRestarter) Restart() error                  { f.restartReq++; return f.restartErr }

// updateSeamSet captures every seam runUpdate consults, so one cleanup
// restores them all (save/restore discipline of updater's replace_test.go).
type updateSeamSet struct {
	resolveSelf          func() (string, error)
	currentVersionStr    func() string
	stdinIsTTY           func() bool
	probeService         func(string) updater.ProbeResult
	registeredBinaryPath func(string) (string, error)
	serveHTTPProbe       func(string) bool
	readConfirmLine      func() (string, error)
	detectHeal           func() (string, bool)
	serviceNew           func(service.Interface, *service.Config) (serviceRestarter, error)
}

func updateSeams() updateSeamSet {
	return updateSeamSet{
		resolveSelf:          resolveSelf,
		currentVersionStr:    currentVersionStr,
		stdinIsTTY:           stdinIsTTY,
		probeService:         probeService,
		registeredBinaryPath: registeredBinaryPath,
		serveHTTPProbe:       serveHTTPProbe,
		readConfirmLine:      readConfirmLine,
		detectHeal:           detectHeal,
		serviceNew:           serviceNew,
	}
}

func restoreUpdateSeams(s updateSeamSet) {
	resolveSelf = s.resolveSelf
	currentVersionStr = s.currentVersionStr
	stdinIsTTY = s.stdinIsTTY
	probeService = s.probeService
	registeredBinaryPath = s.registeredBinaryPath
	serveHTTPProbe = s.serveHTTPProbe
	readConfirmLine = s.readConfirmLine
	detectHeal = s.detectHeal
	serviceNew = s.serviceNew
}

// seamUpdateDefaults swaps every update seam for a deterministic stand-in:
// self at the fixture path, current version curVer, non-TTY stdin, both
// services not installed, registered path == self, HTTP probe green. All
// seams are restored on cleanup. Tests then override individual seams.
func seamUpdateDefaults(t *testing.T, self, curVer string) {
	t.Helper()
	orig := updateSeams()
	t.Cleanup(func() { restoreUpdateSeams(orig) })

	resolveSelf = func() (string, error) { return self, nil }
	currentVersionStr = func() string { return curVer }
	stdinIsTTY = func() bool { return false }
	probeService = func(string) updater.ProbeResult {
		return updater.ProbeResult{State: updater.ProbeNotInstalled, Desc: service.ErrNotInstalled.Error()}
	}
	registeredBinaryPath = func(string) (string, error) { return self, nil }
	serveHTTPProbe = func(string) bool { return true }
	readConfirmLine = func() (string, error) { return "", errors.New("tests: no interactive input") }
	// detectHeal and serviceNew keep their production defaults here: the real
	// DetectHeal inspects the pristine test binary and reports nothing, and
	// serviceNew is only reached in restart tests, which stub it explicitly.
}

// seamProbeByName installs a probeService stand-in answering per name
// (unknown names → not installed). Call after seamUpdateDefaults.
func seamProbeByName(t *testing.T, byName map[string]updater.ProbeResult) {
	t.Helper()
	probeService = func(name string) updater.ProbeResult {
		if pr, ok := byName[name]; ok {
			return pr
		}
		return updater.ProbeResult{State: updater.ProbeNotInstalled, Desc: "not installed"}
	}
}

// runUpdate drives the root command with `update <args>` and returns stdout
// + the Execute error (nil on success).
func runUpdate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"update"}, args...))
	err := root.Execute()
	return out.String(), err
}

// assertSelfUntouched asserts the fixture binary is byte-identical and the
// temp HOME holds nothing but it (no tmpdir residue, no .old generations).
func assertSelfUntouched(t *testing.T, home, self string, oldBytes []byte) {
	t.Helper()
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, oldBytes) {
		t.Error("self bytes changed on an abort/no-op path")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(self) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("home dir residue: want only %q, got %v", filepath.Base(self), names)
	}
}

// assertSelfFlippedTo asserts self now carries payload and the previous
// image survives as the generational .old backup.
func assertSelfFlippedTo(t *testing.T, home, self string, payload, oldBytes []byte) {
	t.Helper()
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, payload) {
		t.Errorf("self was not flipped to the staged payload (%d vs %d bytes)", len(b), len(payload))
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := ""
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old.") {
			oldPath = filepath.Join(home, e.Name())
		}
	}
	if oldPath == "" {
		t.Fatal("no generational .old backup created")
	}
	ob, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, oldBytes) {
		t.Error(".old backup does not carry the previous binary image")
	}
}

// wantServiceCmd renders the platform manual service command the update flow
// prints (op: "restart" or "start") — mirrors the implementation's switch
// (spec §4.4: Windows restart is `sc stop X && sc start X`, sc has no restart).
func wantServiceCmd(op string) string {
	switch runtime.GOOS {
	case "windows":
		if op == "restart" {
			return "sc stop " + buildinfo.ServeServiceName + " && sc start " + buildinfo.ServeServiceName
		}
		return "sc " + op + " " + buildinfo.ServeServiceName
	case "darwin":
		if op == "restart" {
			return "sudo launchctl kickstart -k system/" + buildinfo.ServeServiceName
		}
		return "sudo launchctl bootstrap system/" + buildinfo.ServeServiceName
	default:
		return "sudo systemctl " + op + " " + buildinfo.ServeServiceName
	}
}

// --- flag validation --------------------------------------------------------

func TestUpdateFlagMutualExclusion(t *testing.T) {
	badHex := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		args []string
		want string // substring expected in the error
	}{
		{"--file with --version", []string{"--file", "pkg.zip", "--version", "v1.0.0"}, "--version"},
		{"--sha256 with --no-verify", []string{"--file", "pkg.zip", "--sha256", badHex, "--no-verify"}, "--no-verify"},
		{"--sha256 without --file", []string{"--sha256", badHex}, "--file"},
		{"--no-verify without --file", []string{"--no-verify"}, "--file"},
		{"--file without verify choice", []string{"--file", "pkg.zip"}, "--no-verify"},
		{"--check with --file", []string{"--check", "--file", "pkg.zip", "--no-verify"}, "--check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runUpdate(t, tc.args...)
			if err == nil {
				t.Fatalf("args %v: want error, got success", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// --- dry run / up-to-date / confirm gate ------------------------------------

func TestUpdateCheckDryRunNoSideEffects(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	src := startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")

	out, err := runUpdate(t, "--check")
	if err != nil {
		t.Fatalf("update --check: %v\nout:\n%s", err, out)
	}
	for _, want := range []string{
		"update base", // 证据行:base(httptest base 非默认 → 醒目标记)
		"非默认",
		"0.0.1",   // 当前版本
		"v0.0.2",  // 最新版本
		src.asset, // 资产名
		"干跑",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--check output missing %q:\n%s", want, out)
		}
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateAlreadyLatestExitZero(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.1", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")

	out, err := runUpdate(t) // no --yes needed: up-to-date exits before any confirm
	if err != nil {
		t.Fatalf("update (already latest): %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "已是最新") {
		t.Errorf("output missing 已是最新:\n%s", out)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateConfirmGateRequiresYesOnNonTTY(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2") // the gate sits AFTER the staged self-check

	out, err := runUpdate(t)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("want non-TTY refusal naming --yes, got %v\nout:\n%s", err, out)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

// --- full chain (--yes): download → staged check → replace → flip -----------

func TestUpdateFullChainYesFlipsVersion(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	src := startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("update --yes: %v\nout:\n%s", err, out)
	}
	for _, want := range []string{
		"update base",   // 证据行:base
		"0.0.1",         // 版本对(当前)
		"v0.0.2",        // 版本对(目标)
		src.asset,       // 资产名
		src.sha[:16],    // SHA256 命中(证据行含哈希)
		"staged 自检",     // staged 结果
		"替换: " + self,   // 替换路径
		"下次 agent 会话生效", // 未装服务
	} {
		if !strings.Contains(out, want) {
			t.Errorf("evidence output missing %q:\n%s", want, out)
		}
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

// --- service gates (seam-faked; SCM read-back is real-machine G10/G12) ------

func TestUpdateLegacyServiceInstalledAborts(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	seamProbeByName(t, map[string]updater.ProbeResult{
		updater.LegacyServiceName: {State: updater.ProbeInstalled, Desc: "installed (stopped)"},
	})

	out, err := runUpdate(t)
	if err == nil || !strings.Contains(err.Error(), "中止") {
		t.Fatalf("want migration abort, got %v\nout:\n%s", err, out)
	}
	for _, want := range []string{
		updater.LegacyServiceName,  // 迁移块点名旧服务
		"先迁 client 后升 serve",       // runbook 顺序铁律
		"docs/deployment-modes.md", // 总册指针
	} {
		if !strings.Contains(out, want) {
			t.Errorf("migration block missing %q:\n%s", want, out)
		}
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateDualServiceAbort(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	seamProbeByName(t, map[string]updater.ProbeResult{
		buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (running)"},
		updater.LegacyServiceName:  {State: updater.ProbeInstalled, Desc: "installed (running)"},
	})

	out, err := runUpdate(t)
	if err == nil || !strings.Contains(err.Error(), "中止") {
		t.Fatalf("want migration abort with both services present, got %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, updater.LegacyServiceName) {
		t.Errorf("migration block not printed:\n%s", out)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateMechanismErrAbortsUnconditionally(t *testing.T) {
	t.Run("new name mechanism error", func(t *testing.T) {
		home, self, oldBytes := writeSelfFixture(t)
		seamUpdateDefaults(t, self, "0.0.1")
		seamProbeByName(t, map[string]updater.ProbeResult{
			buildinfo.ServeServiceName: {State: updater.ProbeMechanismErr, Desc: "scm: rpc unavailable"},
		})
		_, err := runUpdate(t)
		if err == nil || !strings.Contains(err.Error(), "fail-closed") {
			t.Fatalf("want fail-closed abort, got %v", err)
		}
		assertSelfUntouched(t, home, self, oldBytes)
	})
	t.Run("legacy name mechanism error", func(t *testing.T) {
		home, self, oldBytes := writeSelfFixture(t)
		seamUpdateDefaults(t, self, "0.0.1")
		seamProbeByName(t, map[string]updater.ProbeResult{
			updater.LegacyServiceName: {State: updater.ProbeMechanismErr, Desc: "systemctl: dbus refused"},
		})
		_, err := runUpdate(t)
		if err == nil || !strings.Contains(err.Error(), "fail-closed") {
			t.Fatalf("want fail-closed abort (erratum: no skip branch), got %v", err)
		}
		assertSelfUntouched(t, home, self, oldBytes)
	})
}

func TestUpdateRegisteredPathMismatchAborts(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere", selfFileName())
	registeredBinaryPath = func(string) (string, error) { return elsewhere, nil }
	seamProbeByName(t, map[string]updater.ProbeResult{
		buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (running)"},
	})

	out, err := runUpdate(t)
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("want mismatch abort, got %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, elsewhere) || !strings.Contains(out, self) {
		t.Errorf("mismatch output must print BOTH paths (registered %q + self %q):\n%s", elsewhere, self, out)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

// --- restart branch (seam-faked) ---------------------------------------------

// restartInstalledSeam marks the NEW-name service installed and hands the
// flow a fake restart handle. Call after seamUpdateDefaults.
func restartInstalledSeam(t *testing.T, fr *fakeRestarter) {
	t.Helper()
	seamProbeByName(t, map[string]updater.ProbeResult{
		buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (running)"},
	})
	serviceNew = func(service.Interface, *service.Config) (serviceRestarter, error) { return fr, nil }
}

func TestUpdateRestartFailureExitCode3(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	fr := &fakeRestarter{status: service.StatusRunning, restartErr: errors.New("access denied")}
	restartInstalledSeam(t, fr)

	out, err := runUpdate(t, "--yes")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("want ExitCodeError(3), got %v\nout:\n%s", err, out)
	}
	if fr.restartReq != 1 {
		t.Errorf("Restart() called %d times, want 1", fr.restartReq)
	}
	if !strings.Contains(out, "重启将断开活动隧道") {
		t.Errorf("missing pre-restart warning line:\n%s", out)
	}
	if !strings.Contains(out, wantServiceCmd("restart")) {
		t.Errorf("missing manual restart command %q:\n%s", wantServiceCmd("restart"), out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes) // replace DID succeed
}

func TestUpdateRestartSuccessHealthProbe(t *testing.T) {
	_, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	fr := &fakeRestarter{status: service.StatusRunning}
	restartInstalledSeam(t, fr)

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("update --yes with healthy restart: %v\nout:\n%s", err, out)
	}
	if fr.restartReq != 1 {
		t.Errorf("Restart() called %d times, want 1", fr.restartReq)
	}
	for _, want := range []string{"重启", "健康回探"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUpdateStoppedServicePrintsStartOnly(t *testing.T) {
	_, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	fr := &fakeRestarter{status: service.StatusStopped}
	restartInstalledSeam(t, fr)

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("update --yes with stopped service: %v\nout:\n%s", err, out)
	}
	if fr.restartReq != 0 {
		t.Errorf("Restart() called %d times, want 0 (stopped → start command only)", fr.restartReq)
	}
	if !strings.Contains(out, wantServiceCmd("start")) {
		t.Errorf("missing start command %q:\n%s", wantServiceCmd("start"), out)
	}
}

// --- downgrade / dev current version ----------------------------------------

func TestUpdateDowngradeWarningEvenWithYes(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.5.0")
	withStagedVersion(t, "0.0.2")

	out, err := runUpdate(t, "--yes", "--version", "v0.0.2")
	if err != nil {
		t.Fatalf("downgrade --yes: %v\nout:\n%s", err, out)
	}
	for _, want := range []string{"降级至", "回滚通道"} {
		if !strings.Contains(out, want) {
			t.Errorf("downgrade output missing %q:\n%s", want, out)
		}
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateDevVersionGenericWarning(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "dev")
	withStagedVersion(t, "0.0.2")

	out, err := runUpdate(t, "--yes", "--version", "v0.0.2")
	if err != nil {
		t.Fatalf("dev + --version: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "无法判定升降级") {
		t.Errorf("missing generic upgrade-direction warning:\n%s", out)
	}
	if strings.Contains(out, "降级至") {
		t.Errorf("dev must NOT trigger the downgrade-specific line:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateDevVersionRequiredWithoutFlag(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "dev")

	_, err := runUpdate(t, "--check")
	if err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("want error demanding explicit --version, got %v", err)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

// --- --file mode --------------------------------------------------------------

func TestUpdateFileNoVerifyWarnsAndExecutes(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	asset, err := updater.AssetName("v0.0.2", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), asset)
	if err := os.WriteFile(pkg, buildTestArchive(t, asset, testBinBytes(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runUpdate(t, "--file", pkg, "--no-verify", "--yes")
	if err != nil {
		t.Fatalf("--file --no-verify: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "未校验") {
		t.Errorf("missing unverified-package warning:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateFileSha256(t *testing.T) {
	asset, err := updater.AssetName("v0.0.2", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	payload := testBinBytes(t)
	archive := buildTestArchive(t, asset, payload)
	sum := sha256.Sum256(archive)

	t.Run("matching --sha256 executes", func(t *testing.T) {
		home, self, oldBytes := writeSelfFixture(t)
		seamUpdateDefaults(t, self, "0.0.1")
		withStagedVersion(t, "0.0.2")
		pkg := filepath.Join(t.TempDir(), asset)
		if err := os.WriteFile(pkg, archive, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := runUpdate(t, "--file", pkg, "--sha256", hex.EncodeToString(sum[:]), "--yes")
		if err != nil {
			t.Fatalf("--file --sha256 hit: %v\nout:\n%s", err, out)
		}
		if !strings.Contains(out, "SHA256") {
			t.Errorf("missing SHA256 evidence line:\n%s", out)
		}
		assertSelfFlippedTo(t, home, self, payload, oldBytes)
	})

	t.Run("mismatching --sha256 aborts", func(t *testing.T) {
		home, self, oldBytes := writeSelfFixture(t)
		seamUpdateDefaults(t, self, "0.0.1")
		pkg := filepath.Join(t.TempDir(), asset)
		if err := os.WriteFile(pkg, archive, 0o644); err != nil {
			t.Fatal(err)
		}
		wrong := strings.Repeat("ab", 32)
		out, err := runUpdate(t, "--file", pkg, "--sha256", wrong, "--yes")
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "sha256") {
			t.Fatalf("want sha256 mismatch abort, got %v\nout:\n%s", err, out)
		}
		assertSelfUntouched(t, home, self, oldBytes)
	})
}

// --- crash-window self-heal (confirm is interactive-only, --yes NOT exempt) --

func TestUpdateHealYesIsNotExempt(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	detectHeal = func() (string, bool) {
		return "self missing but backup self.old.42 exists; recover with: move /y ...", true
	}
	_, err := runUpdate(t, "--yes")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "自愈") {
		t.Fatalf("want --yes-not-exempt heal refusal, got %v", err)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateHealNonTTYRefused(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	detectHeal = func() (string, bool) { return "backup exists; recover with: mv ...", true }
	_, err := runUpdate(t)
	if err == nil || !strings.Contains(err.Error(), "非交互") {
		t.Fatalf("want non-TTY heal refusal, got %v", err)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestPerformHealRestoresNewestGeneration(t *testing.T) {
	home := t.TempDir()
	self := filepath.Join(home, selfFileName())
	// canonical missing (the crash-window precondition), two generations present
	if err := os.WriteFile(self+".old.100", []byte("older image"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self+".old.200", []byte("newer image"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := performHeal(self)
	if err != nil {
		t.Fatalf("performHeal: %v", err)
	}
	if got != self {
		t.Errorf("performHeal = %q, want %q", got, self)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "newer image" {
		t.Errorf("healed self = %q, want the NEWEST generation", b)
	}
	if _, err := os.Stat(self + ".old.200"); !os.IsNotExist(err) {
		t.Errorf("healed backup still present: %v", err)
	}
}

// --- version-pair edge: explicit --version equal to current is a refusal -----

func TestUpdateExplicitSameVersionRefused(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.1", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")

	_, err := runUpdate(t, "--yes", "--version", "v0.0.1")
	if err == nil || !strings.Contains(err.Error(), "已安装该版本") {
		t.Fatalf("want installed-version refusal, got %v", err)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

// --- T8-R1 hardening (review findings 3 + 4a-4d) ------------------------------

// seamInteractiveConfirm makes stdin a TTY answering confirmations from a
// scripted sequence ("y"/"n"), one line per call.
func seamInteractiveConfirm(t *testing.T, answers ...string) {
	t.Helper()
	stdinIsTTY = func() bool { return true }
	readConfirmLine = func() (string, error) {
		if len(answers) == 0 {
			return "", errors.New("tests: confirmation answer sequence exhausted")
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
}

func TestUpdateRestartDeclineExitCode3(t *testing.T) {
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	fr := &fakeRestarter{status: service.StatusRunning}
	restartInstalledSeam(t, fr)
	seamInteractiveConfirm(t, "y", "n") // update confirm = yes, restart confirm = NO

	out, err := runUpdate(t) // no --yes: the decline must be reachable
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("restart decline: want ExitCodeError(3), got %v\nout:\n%s", err, out)
	}
	if fr.restartReq != 0 {
		t.Errorf("Restart() called %d times, want 0 after a decline", fr.restartReq)
	}
	if !strings.Contains(out, wantServiceCmd("restart")) {
		t.Errorf("missing manual restart command after decline:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes) // replace DID succeed
}

func TestUpdateRestartConstructionFailureExitCode3(t *testing.T) {
	_, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	restartInstalledSeam(t, &fakeRestarter{status: service.StatusRunning})
	serviceNew = func(service.Interface, *service.Config) (serviceRestarter, error) {
		return nil, errors.New("no service system detected")
	}

	out, err := runUpdate(t, "--yes")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("serviceNew failure: want ExitCodeError(3), got %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, wantServiceCmd("restart")) {
		t.Errorf("missing manual restart command after construction failure:\n%s", out)
	}
}

// TestUpdateFileLegacyServiceAborts locks deviation note #1: --file takes the
// SAME unconditional service gates as the GitHub path — a legacy service in
// any state aborts with the migration block before the local file is touched
// (the pkg path is deliberately nonexistent: the gates fire first).
func TestUpdateFileLegacyServiceAborts(t *testing.T) {
	cases := []struct {
		name   string
		byName map[string]updater.ProbeResult
	}{
		{
			name: "legacy installed only",
			byName: map[string]updater.ProbeResult{
				updater.LegacyServiceName: {State: updater.ProbeInstalled, Desc: "installed (running)"},
			},
		},
		{
			name: "dual service",
			byName: map[string]updater.ProbeResult{
				buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (running)"},
				updater.LegacyServiceName:  {State: updater.ProbeInstalled, Desc: "installed (stopped)"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, self, oldBytes := writeSelfFixture(t)
			seamUpdateDefaults(t, self, "0.0.1")
			seamProbeByName(t, tc.byName)

			out, err := runUpdate(t, "--file", "no-such-pkg-anywhere.zip", "--no-verify")
			if err == nil || !strings.Contains(err.Error(), "中止") {
				t.Fatalf("--file: want migration abort, got %v\nout:\n%s", err, out)
			}
			if !strings.Contains(out, updater.LegacyServiceName) {
				t.Errorf("migration block not printed:\n%s", out)
			}
			assertSelfUntouched(t, homeOf(t, self), self, oldBytes)
		})
	}
}

// TestUpdateDualServiceMismatchPrefersMigrationBlock locks review finding 3:
// with BOTH services present, the migration block wins over the "path
// mismatch" message (spec §3.2: 旧名存在任何态,无论新名状态 → 迁移块中止).
func TestUpdateDualServiceMismatchPrefersMigrationBlock(t *testing.T) {
	_, self, oldBytes := writeSelfFixture(t)
	seamUpdateDefaults(t, self, "0.0.1")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere", selfFileName())
	registeredBinaryPath = func(string) (string, error) { return elsewhere, nil }
	seamProbeByName(t, map[string]updater.ProbeResult{
		buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (running)"},
		updater.LegacyServiceName:  {State: updater.ProbeInstalled, Desc: "installed (running)"},
	})

	out, err := runUpdate(t)
	if err == nil || !strings.Contains(err.Error(), "中止") {
		t.Fatalf("want migration abort, got %v\nout:\n%s", err, out)
	}
	if strings.Contains(err.Error(), "不一致") {
		t.Errorf("mismatch message must yield to the migration block, got: %v", err)
	}
	if !strings.Contains(out, updater.LegacyServiceName) || !strings.Contains(out, "docs/deployment-modes.md") {
		t.Errorf("migration block not printed:\n%s", out)
	}
	assertSelfUntouched(t, homeOf(t, self), self, oldBytes)
}

// TestUpdateHealInteractiveYesContinuesFullChain covers finding 4d: answering
// "y" at the heal prompt performs the recovery and the run continues through
// the whole chain to a successful replace (two confirmations answered: heal,
// then update).
func TestUpdateHealInteractiveYesContinuesFullChain(t *testing.T) {
	home, self, _ := writeSelfFixture(t)
	healedImage := []byte("healed image (was the newest .old generation)\n")
	if err := os.WriteFile(self+".old.100", healedImage, 0o755); err != nil {
		t.Fatal(err)
	}
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	detectHeal = func() (string, bool) { return "backup self.old.100 exists; recover with: mv ...", true }
	seamInteractiveConfirm(t, "y", "y") // heal confirm = yes, update confirm = yes

	out, err := runUpdate(t)
	if err != nil {
		t.Fatalf("heal-yes full chain: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "自愈完成") {
		t.Errorf("missing heal-completed evidence line:\n%s", out)
	}
	// self was healed from the .old.100 image, then replaced by the staged
	// binary; the replace's own generational backup carries the healed image.
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, testBinBytes(t)) {
		t.Error("self not flipped to the staged payload after heal+update")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := ""
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old.") && e.Name() != selfFileName()+".old.100" {
			oldPath = filepath.Join(home, e.Name())
		}
	}
	if oldPath == "" {
		t.Fatal("no generational .old backup from the replace step")
	}
	ob, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, healedImage) {
		t.Errorf("replace backup = %q, want the healed image", ob)
	}
}

// homeOf derives the fixture HOME from the self path (dir of self).
func homeOf(t *testing.T, self string) string {
	t.Helper()
	return filepath.Dir(self)
}
