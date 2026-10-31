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
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/updater"
)

// TestMain intercepts the staged-binary helper invocation. The full-chain
// tests build release archives whose payload is this test binary; when the
// update flow executes the extracted copy as `<staged> version`, this branch
// answers with the version the test pinned in the environment — no real
// cross-compile needed. Any other invocation (the normal `go test` run, or a
// child spawned without the env) runs the test suite as usual.
//
// It also points SSHMGR_FILEKEY_PATH at a file that does not exist (unless
// the caller already set it), so a real master.key.plain on the developer
// machine never outranks the SSHMGR_MASTERKEY_HEX each test pins. Without
// this, every test that decrypts a credential fails on a machine where
// sshmgr is installed. Tests that need the file layer set the variable
// themselves with t.Setenv.
func TestMain(m *testing.M) {
	if v := os.Getenv("SSHMGR_TEST_STAGED_VERSION"); v != "" {
		fmt.Println(v)
		os.Exit(0)
	}
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	if _, set := os.LookupEnv("SSHMGR_FILEKEY_PATH"); !set {
		dir, err := os.MkdirTemp("", "sshmgr-cli-test-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		os.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(dir, "no-such-master.key"))
	}
	return m.Run()
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
// surface. It can serve a latest release and several exact tags separately;
// assets and checksums.txt sit in one flat directory, as goreleaser ships
// them.
type fakeUpdateSource struct {
	srv      *httptest.Server
	latest   string
	tag      string
	asset    string
	archive  []byte
	sha      string
	releases map[string]fakeUpdateRelease
}

type fakeUpdateRelease struct {
	tag     string
	asset   string
	archive []byte
	sha     string
}

// startFakeUpdateSourceWithTags starts the fake source and points
// SSHMGR_UPDATE_BASE at it. `latest` is returned by releases/latest; every
// entry in `releases` is also reachable by its exact tag. This lets command
// tests distinguish “GitHub 最新版” from “本地缓存推荐版本”.
func startFakeUpdateSourceWithTags(t *testing.T, latest string, releases map[string][]byte) *fakeUpdateSource {
	t.Helper()
	fs := &fakeUpdateSource{
		latest:   latest,
		releases: make(map[string]fakeUpdateRelease),
	}
	for requestedTag, payload := range releases {
		asset, err := updater.AssetName(requestedTag, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			t.Fatalf("compute asset name for %s: %v", requestedTag, err)
		}
		release := fakeUpdateRelease{
			tag:     requestedTag,
			asset:   asset,
			archive: buildTestArchive(t, asset, payload),
		}
		sum := sha256.Sum256(release.archive)
		release.sha = hex.EncodeToString(sum[:])
		fs.releases[requestedTag] = release
		fs.releases[strings.TrimPrefix(requestedTag, "v")] = release
		fs.releases["v"+strings.TrimPrefix(requestedTag, "v")] = release
	}
	latestRelease := fs.releases[latest]
	if latestRelease.asset == "" {
		t.Fatalf("fake update source has no release %q", latest)
	}
	fs.tag = latestRelease.tag
	fs.asset = latestRelease.asset
	fs.archive = latestRelease.archive
	fs.sha = latestRelease.sha

	releaseJSON := func(release fakeUpdateRelease) string {
		return fmt.Sprintf(`{"tag_name":%q,"assets":[`+
			`{"name":%q,"browser_download_url":"http://origin.invalid/%s"},`+
			`{"name":"checksums.txt","browser_download_url":"http://origin.invalid/checksums.txt"}]}`,
			release.tag, release.asset, release.asset)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+buildinfo.Owner+"/"+buildinfo.Repo+"/releases/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		suffix := strings.TrimPrefix(r.URL.Path, "/repos/"+buildinfo.Owner+"/"+buildinfo.Repo+"/releases/")
		if suffix == "latest" {
			_, _ = io.WriteString(w, releaseJSON(latestRelease))
			return
		}
		release, ok := fs.releases[strings.TrimPrefix(suffix, "tags/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, releaseJSON(release))
	})
	registeredAssets := map[string]bool{}
	for _, release := range fs.releases {
		if registeredAssets[release.asset] {
			continue
		}
		registeredAssets[release.asset] = true
		asset := release
		mux.HandleFunc("/"+asset.asset, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(asset.archive)
		})
	}
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		seen := make(map[string]bool)
		for _, release := range fs.releases {
			if seen[release.asset] {
				continue
			}
			seen[release.asset] = true
			_, _ = fmt.Fprintf(w, "%s  %s\n", release.sha, release.asset)
		}
	})
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	t.Setenv("SSHMGR_UPDATE_BASE", fs.srv.URL)
	return fs
}

// startFakeUpdateSource starts a one-release fake source. browser_download_url
// deliberately names an off-whitelist host: with a custom base the updater
// must rebuild the URL onto the base (spec §4.2(4)), so the fake only answers
// on its own paths.
func startFakeUpdateSource(t *testing.T, tag string, payload []byte) *fakeUpdateSource {
	t.Helper()
	return startFakeUpdateSourceWithTags(t, tag, map[string][]byte{tag: payload})
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
	registeredServeAddr  func(string) (string, error)
	serveHTTPProbe       func(string) bool
	readConfirmLine      func() (string, error)
	detectHeal           func() (string, bool)
	serviceNew           func(service.Interface, *service.Config) (serviceRestarter, error)
	resolveMachineRole   func(string) (roles.Launch, error)
}

func updateSeams() updateSeamSet {
	return updateSeamSet{
		resolveSelf:          resolveSelf,
		currentVersionStr:    currentVersionStr,
		stdinIsTTY:           stdinIsTTY,
		probeService:         probeService,
		registeredBinaryPath: registeredBinaryPath,
		registeredServeAddr:  registeredServeAddr,
		serveHTTPProbe:       serveHTTPProbe,
		readConfirmLine:      readConfirmLine,
		detectHeal:           detectHeal,
		serviceNew:           serviceNew,
		resolveMachineRole:   resolveMachineRole,
	}
}

func restoreUpdateSeams(s updateSeamSet) {
	resolveSelf = s.resolveSelf
	currentVersionStr = s.currentVersionStr
	stdinIsTTY = s.stdinIsTTY
	probeService = s.probeService
	registeredBinaryPath = s.registeredBinaryPath
	registeredServeAddr = s.registeredServeAddr
	serveHTTPProbe = s.serveHTTPProbe
	readConfirmLine = s.readConfirmLine
	detectHeal = s.detectHeal
	serviceNew = s.serviceNew
	resolveMachineRole = s.resolveMachineRole
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
	registeredServeAddr = func(string) (string, error) { return "", nil } // hermetic: no registered addr → default probe target
	serveHTTPProbe = func(string) bool { return true }
	readConfirmLine = func() (string, error) { return "", errors.New("tests: no interactive input") }
	// detectHeal and serviceNew keep their production defaults here: the real
	// DetectHeal inspects the pristine test binary and reports nothing, and
	// serviceNew is only reached in restart tests, which stub it explicitly.
	resolveMachineRole = func(string) (roles.Launch, error) {
		return roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleStandalone}, nil
	}
}

// seamUpdateRole pins the launch decision without consulting this developer
// machine's role.json. Client-specific tests combine it with a seeded local
// cache, so the command still exercises the public update behavior end to end.
func seamUpdateRole(role roles.Role) {
	kind := roles.LaunchBroker
	if role == roles.RoleClient {
		kind = roles.LaunchClient
	}
	resolveMachineRole = func(string) (roles.Launch, error) {
		return roles.Launch{Kind: kind, Role: role}, nil
	}
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

// runUpdateWithStderr 也返回标准错误输出，用于断言不改变命令结果的提示。
func runUpdateWithStderr(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	errw := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errw)
	root.SetArgs(append([]string{"update"}, args...))
	err := root.Execute()
	return out.String(), errw.String(), err
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
	if runtime.GOOS != "windows" {
		// The unix branch is the atomic rename (spec §4.3): NO generational
		// backup exists there — assert the residue stays clean instead of
		// asserting the windows-only .old shape.
		if oldPath != "" {
			t.Errorf("unix replace must not leave a generational backup: %s", oldPath)
		}
		return
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
		return "sudo launchctl kickstart system/" + buildinfo.ServeServiceName
	default:
		return "sudo systemctl " + op + " " + buildinfo.ServeServiceName
	}
}

// --- help copy pins (勘误2 confirm order; launchd start shape) ----------------

// TestUpdateHelpPinsConfirmOrder locks spec §4.3 勘误2: the Long help must
// teach the confirm point AFTER download/verify/staged self-check (confirm
// sits between the staged check and the replace), never the pre-erratum
// "check → confirm → download" order. The help wraps the sequence across two
// lines, so the assertion runs on whitespace-normalized text.
func TestUpdateHelpPinsConfirmOrder(t *testing.T) {
	root := NewRootCmd()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetArgs([]string{"update", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --help: %v", err)
	}
	norm := strings.Join(strings.Fields(buf.String()), " ")
	want := "check → download → verify → staged self-check → confirm → replace → restart"
	if !strings.Contains(norm, want) {
		t.Fatalf("help missing the pinned post-erratum order %q:\n%s", want, buf.String())
	}
	if strings.Contains(norm, "check → confirm → download") {
		t.Fatalf("help still teaches the pre-erratum order (confirm before download):\n%s", buf.String())
	}
}

// TestManualServiceCommandDarwinStartUsesKickstart locks the final-review fix:
// the launchd START command must be `kickstart` (executable as-is — the
// service is already registered; kickstart without -k just starts it), not
// `bootstrap` (needs an explicit plist path, so the printed command could
// never run). Parameterized renderer lets this darwin shape be asserted on
// every platform.
func TestManualServiceCommandDarwinStartUsesKickstart(t *testing.T) {
	start := manualServiceCommandFor("darwin", "start")
	if want := "sudo launchctl kickstart system/" + buildinfo.ServeServiceName; start != want {
		t.Errorf("darwin start = %q, want %q", start, want)
	}
	if strings.Contains(start, "bootstrap") {
		t.Errorf("darwin start must not use bootstrap (needs a plist path): %q", start)
	}
	restart := manualServiceCommandFor("darwin", "restart")
	if want := "sudo launchctl kickstart -k system/" + buildinfo.ServeServiceName; restart != want {
		t.Errorf("darwin restart = %q, want %q", restart, want)
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
		"update base", // 证据行:base
		"0.0.1",       // 版本对(当前)
		"v0.0.2",      // 版本对(目标)
		src.asset,     // 资产名
		src.sha[:16],  // SHA256 命中(证据行含哈希)
		"staged 自检",   // staged 结果
		"替换: " + self, // 替换路径
		"自动热升级",       // 未装服务(v0.20.0 起运行中的桥空闲时自动热升级)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("evidence output missing %q:\n%s", want, out)
		}
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

// --- 客户端推荐版本 -----------------------------------------------------------

// seedUpdateRecommendedCache 复用既有的真实本地加密缓存夹具，并把 HOME 指到
// 临时目录，避免测试读写这台开发机的 macOS 用户配置目录。
func seedUpdateRecommendedCache(t *testing.T, recommendedVersion string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	seedRootVersionCache(t, recommendedVersion)
}

func TestUpdateClientInstallsRecommendedVersionFromCache(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)
	withStagedVersion(t, "0.0.2")

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("客户端 update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "版本: 0.0.1 → v0.0.2\n") {
		t.Fatalf("客户端 update 应安装缓存推荐版本 v0.0.2：\n%s", out)
	}
	if strings.Contains(out, "v0.0.3") {
		t.Fatalf("客户端 update 不应回退到 GitHub 最新版本 v0.0.3：\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateClientWithoutRecommendedVersionRefuses(t *testing.T) {
	seedUpdateRecommendedCache(t, "")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.3", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)

	out, err := runUpdate(t, "--yes")
	if err == nil || !strings.Contains(err.Error(), "无法确定推荐版本，请先升级权威端，或用 `--version` 指定") {
		t.Fatalf("缺少推荐版本时应该拒绝执行，错误为 %v", err)
	}
	if strings.Contains(out, "v0.0.3") {
		t.Fatalf("缺少推荐版本时不应查询或安装 GitHub 最新版本：\n%s", out)
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateClientVersionFlagOverridesRecommendedVersion(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.4", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
		"v0.0.4": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)
	withStagedVersion(t, "0.0.3")

	out, err := runUpdate(t, "--yes", "--version", "v0.0.3")
	if err != nil {
		t.Fatalf("--version 覆盖推荐版本失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "版本: 0.0.1 → v0.0.3\n") {
		t.Fatalf("--version 应优先于推荐版本，应安装 v0.0.3：\n%s", out)
	}
	if strings.Contains(out, "v0.0.2") || strings.Contains(out, "v0.0.4") {
		t.Fatalf("--version 指定目标以外不应安装推荐版本或最新版本：\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateClientUsesRoleInsteadOfServiceInstallationState(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)
	withStagedVersion(t, "0.0.2")
	seamProbeByName(t, map[string]updater.ProbeResult{
		buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (stopped)"},
	})
	serviceNew = func(service.Interface, *service.Config) (serviceRestarter, error) {
		return &fakeRestarter{status: service.StatusStopped}, nil
	}

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("客户端 update 不应按服务安装状态进入服务分支: %v\n%s", err, out)
	}
	if !strings.Contains(out, "未安装服务(客户端)") {
		t.Fatalf("客户端 update 应按客户端角色输出热升级提示:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateClientExplicitVersionUsesRoleInsteadOfServiceInstallationState(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)
	withStagedVersion(t, "0.0.3")
	seamProbeByName(t, map[string]updater.ProbeResult{
		buildinfo.ServeServiceName: {State: updater.ProbeInstalled, Desc: "installed (stopped)"},
	})
	serviceNew = func(service.Interface, *service.Config) (serviceRestarter, error) {
		return &fakeRestarter{status: service.StatusStopped}, nil
	}

	out, err := runUpdate(t, "--yes", "--version", "v0.0.3")
	if err != nil {
		t.Fatalf("客户端显式指定版本时不应按服务安装状态进入服务分支: %v\n%s", err, out)
	}
	if !strings.Contains(out, "未安装服务(客户端)") {
		t.Fatalf("客户端显式指定版本也应按客户端角色输出热升级提示:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateRoleResolutionFailureKeepsGitHubLatestBehavior(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.3", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	resolveMachineRole = func(string) (roles.Launch, error) {
		return roles.Launch{}, errors.New("role state unreadable")
	}
	withStagedVersion(t, "0.0.3")

	out, errw, err := runUpdateWithStderr(t, "--yes")
	if err != nil {
		t.Fatalf("角色判定失败时应保持改动前行为并继续更新: %v\n%s%s", err, out, errw)
	}
	if !strings.Contains(out, "版本: 0.0.1 → v0.0.3\n") {
		t.Fatalf("角色判定失败时应安装 GitHub 最新版本:\n%s", out)
	}
	if !strings.Contains(errw, "判定本机角色失败") || !strings.Contains(errw, "按非客户端处理") {
		t.Fatalf("角色判定失败应在标准错误输出说明回退行为:%q", errw)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateClientReadsOnlyNamedCacheInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRootVersionCacheForInstance(t, "0.0.2", "laptop")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)
	withStagedVersion(t, "0.0.2")

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("只有一个命名实例时客户端 update 应读取它的推荐版本: %v\n%s", err, out)
	}
	if !strings.Contains(out, "版本: 0.0.1 → v0.0.2\n") {
		t.Fatalf("客户端 update 应安装唯一命名实例的推荐版本:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateClientMultipleNamedInstancesRequireInstanceFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRootVersionCacheForInstance(t, "0.0.2", "laptop")
	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(configRoot, "ssh-manager", "instances", "laptop")
	target := filepath.Join(configRoot, "ssh-manager", "instances", "desk")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cache.bin", "cache.meta.json", "cache.auth.json"} {
		blob, err := os.ReadFile(filepath.Join(source, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, name), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)
	withStagedVersion(t, "0.0.2")

	out, errw, err := runUpdateWithStderr(t, "--yes")
	if err == nil || !strings.Contains(err.Error(), "无法确定唯一实例") || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("多个命名实例应要求 --instance，错误为 %v", err)
	}
	if strings.Contains(out, "v0.0.3") || strings.Contains(errw, "v0.0.3") {
		t.Fatalf("未选定实例时不应查询 GitHub 最新版本：stdout=%s stderr=%s", out, errw)
	}
	assertSelfUntouched(t, home, self, oldBytes)

	out, err = runUpdate(t, "--yes", "--instance", "laptop")
	if err != nil {
		t.Fatalf("--instance 选定实例后应继续更新: %v\n%s", err, out)
	}
	if !strings.Contains(out, "版本: 0.0.1 → v0.0.2\n") {
		t.Fatalf("--instance 应读取指定实例的推荐版本:\n%s", out)
	}
	assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
}

func TestUpdateClientCheckListsRecommendedAndLatestVersions(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "0.0.1")
	seamUpdateRole(roles.RoleClient)

	out, err := runUpdate(t, "--check")
	if err != nil {
		t.Fatalf("客户端 update --check: %v\n%s", err, out)
	}
	for _, want := range []string{
		"当前版本: 0.0.1",
		"推荐版本: 0.0.2",
		"GitHub 最新版本: v0.0.3",
		"干跑:未下载、未替换、未重启任何东西",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateClientCheckListsVersionsWhenCurrentVersionIsDev(t *testing.T) {
	seedUpdateRecommendedCache(t, "0.0.2")
	home, self, oldBytes := writeSelfFixture(t)
	startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
		"v0.0.2": testBinBytes(t),
		"v0.0.3": testBinBytes(t),
	})
	seamUpdateDefaults(t, self, "dev")
	seamUpdateRole(roles.RoleClient)

	out, err := runUpdate(t, "--check")
	if err != nil {
		t.Fatalf("开发构建客户端 update --check: %v\n%s", err, out)
	}
	for _, want := range []string{
		"当前版本: dev",
		"推荐版本: 0.0.2",
		"GitHub 最新版本: v0.0.3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	assertSelfUntouched(t, home, self, oldBytes)
}

func TestUpdateBrokerRolesKeepGitHubLatest(t *testing.T) {
	for _, role := range []roles.Role{roles.RoleStandalone, roles.RoleServer} {
		t.Run(string(role), func(t *testing.T) {
			seedUpdateRecommendedCache(t, "0.0.2")
			home, self, oldBytes := writeSelfFixture(t)
			startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
				"v0.0.2": testBinBytes(t),
				"v0.0.3": testBinBytes(t),
			})
			seamUpdateDefaults(t, self, "0.0.1")
			seamUpdateRole(role)
			withStagedVersion(t, "0.0.3")

			out, err := runUpdate(t, "--yes")
			if err != nil {
				t.Fatalf("%s update: %v\n%s", role, err, out)
			}
			if !strings.Contains(out, "版本: 0.0.1 → v0.0.3\n") {
				t.Fatalf("%s update 应继续安装 GitHub 最新版本：\n%s", role, out)
			}
			if strings.Contains(out, "v0.0.2") {
				t.Fatalf("%s 不应读取推荐版本：\n%s", role, out)
			}
			assertSelfFlippedTo(t, home, self, testBinBytes(t), oldBytes)
		})
	}
}

func TestUpdateBrokerRolesCheckOnlyListsGitHubLatest(t *testing.T) {
	for _, role := range []roles.Role{roles.RoleStandalone, roles.RoleServer} {
		t.Run(string(role), func(t *testing.T) {
			seedUpdateRecommendedCache(t, "0.0.2")
			home, self, oldBytes := writeSelfFixture(t)
			startFakeUpdateSourceWithTags(t, "v0.0.3", map[string][]byte{
				"v0.0.2": testBinBytes(t),
				"v0.0.3": testBinBytes(t),
			})
			seamUpdateDefaults(t, self, "0.0.1")
			seamUpdateRole(role)

			out, err := runUpdate(t, "--check")
			if err != nil {
				t.Fatalf("%s update --check: %v\n%s", role, err, out)
			}
			if !strings.Contains(out, "最新版本: v0.0.3\n") {
				t.Fatalf("%s --check 应列出 GitHub 最新版本：\n%s", role, out)
			}
			if strings.Contains(out, "推荐版本") || strings.Contains(out, "0.0.2") {
				t.Fatalf("%s --check 不应列出推荐版本：\n%s", role, out)
			}
			assertSelfUntouched(t, home, self, oldBytes)
		})
	}
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

// TestUpdateProbeUsesRegisteredAddr (backlog #17): the post-restart health
// probe must target the --addr the service was REGISTERED with — read back
// from the registration records, wildcard bind normalized to loopback —
// instead of the hardcoded default (which misreported "not responding" on
// every non-default install).
func TestUpdateProbeUsesRegisteredAddr(t *testing.T) {
	_, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	fr := &fakeRestarter{status: service.StatusRunning}
	restartInstalledSeam(t, fr)
	registeredServeAddr = func(string) (string, error) { return "0.0.0.0:9000", nil }
	probed := ""
	serveHTTPProbe = func(addr string) bool {
		probed = addr
		return true
	}

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("update --yes: %v\nout:\n%s", err, out)
	}
	if probed != "127.0.0.1:9000" {
		t.Fatalf("probe target = %q, want the registered addr normalized to loopback %q", probed, "127.0.0.1:9000")
	}
	if !strings.Contains(out, "健康回探(127.0.0.1:9000)") {
		t.Fatalf("output must name the real probe target:\n%s", out)
	}
}

// TestUpdateProbeFallsBackWhenAddrUnreadable: a registration read failure
// keeps the install default as the probe target (best-effort evidence, not
// an abort) — and the output still names what was probed.
func TestUpdateProbeFallsBackWhenAddrUnreadable(t *testing.T) {
	_, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.0.2", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.0.1")
	withStagedVersion(t, "0.0.2")
	fr := &fakeRestarter{status: service.StatusRunning}
	restartInstalledSeam(t, fr)
	registeredServeAddr = func(string) (string, error) { return "", errors.New("records unreadable") }
	probed := ""
	serveHTTPProbe = func(addr string) bool {
		probed = addr
		return false
	}

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("update --yes: %v\nout:\n%s", err, out)
	}
	if probed != defaultProbeAddr {
		t.Fatalf("probe target = %q, want fallback default %q", probed, defaultProbeAddr)
	}
	if !strings.Contains(out, "健康回探("+defaultProbeAddr+")") {
		t.Fatalf("output must name the fallback probe target:\n%s", out)
	}
	// The read failure itself must be VISIBLE (review M1): without this line
	// the operator reads "not responding" as a slow listener and chases the
	// wrong cause while the code already knows the registration is broken.
	if !strings.Contains(out, "读取注册 --addr 失败") {
		t.Fatalf("output must warn about the failed registered-addr read:\n%s", out)
	}
}

// TestLoopbackProbeAddr pins the wildcard→loopback rewrite table.
func TestLoopbackProbeAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:7878":      "127.0.0.1:7878",
		"[::]:7878":         "127.0.0.1:7878",
		":7878":             "127.0.0.1:7878",    // empty host
		"192.168.1.10:7878": "192.168.1.10:7878", // concrete LAN bind: probe the real address
		"[fe80::1]:7878":    "[fe80::1]:7878",    // concrete IPv6: dial the real registered address
		"127.0.0.1:9000":    "127.0.0.1:9000",
		"not-a-hostport":    "not-a-hostport", // unparsable: unchanged (probe reports not-responding)
	}
	for in, want := range cases {
		if got := loopbackProbeAddr(in); got != want {
			t.Errorf("loopbackProbeAddr(%q) = %q, want %q", in, got, want)
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
	if runtime.GOOS != "windows" {
		// Unix replace is the atomic branch — no backup from the replace step.
		if oldPath != "" {
			t.Errorf("unix replace must not leave a generational backup: %s", oldPath)
		}
	} else if oldPath == "" {
		t.Fatal("no generational .old backup from the replace step")
	} else {
		ob, err := os.ReadFile(oldPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ob, healedImage) {
			t.Errorf("replace backup = %q, want the healed image", ob)
		}
	}
}

// homeOf derives the fixture HOME from the self path (dir of self).
func homeOf(t *testing.T, self string) string {
	t.Helper()
	return filepath.Dir(self)
}
