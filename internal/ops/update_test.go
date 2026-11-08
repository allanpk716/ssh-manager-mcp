package ops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/updater"
)

// stagedVersionEnv 让升级服务测试里的假发布包回答指定版本：归档负载就是本
// 测试二进制，子进程看到这个环境变量时只打印版本号后退出。
const stagedVersionEnv = "SSHMGR_OPS_UPDATE_STAGED_VERSION"

func TestMain(m *testing.M) {
	if v := os.Getenv(stagedVersionEnv); v != "" {
		fmt.Println(v)
		os.Exit(0)
	}
	site = startFakeUpdateSite()
	os.Exit(m.Run())
}

// fakeUpdateSite 是共享的本地 HTTPS 假发布服务器。真实更新链会校验传输层证
// 书，因此服务器只创建一次，证书写入 SSL_CERT_FILE 指向的文件，让默认
// HTTP 客户端在测试进程里信任它。
var site *fakeUpdateSite

type fakeUpdateSite struct {
	srv *httptest.Server

	mu       sync.Mutex
	latest   string
	releases map[string]fakeRelease
}

type fakeRelease struct {
	tag        string
	asset      string
	archive    []byte
	sha        string
	sumPayload string // 覆盖 checksums.txt 内容；空串按真实哈希生成
}

func startFakeUpdateSite() *fakeUpdateSite {
	s := &fakeUpdateSite{releases: map[string]fakeRelease{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+buildinfo.Owner+"/"+buildinfo.Repo+"/releases/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		suffix := strings.TrimPrefix(r.URL.Path, "/repos/"+buildinfo.Owner+"/"+buildinfo.Repo+"/releases/")
		tag := ""
		if suffix == "latest" {
			tag = s.latest
		} else {
			tag = strings.TrimPrefix(suffix, "tags/")
		}
		release, ok := s.releases[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[`+
			`{"name":%q,"browser_download_url":"https://origin.invalid/%s"},`+
			`{"name":"checksums.txt","browser_download_url":"https://origin.invalid/checksums.txt"}]}`,
			release.tag, release.asset, release.asset)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt") || r.URL.Path == "checksums.txt":
			for _, line := range s.checksumLines() {
				_, _ = io.WriteString(w, line)
			}
		default:
			name := strings.TrimPrefix(r.URL.Path, "/")
			if index := strings.LastIndex(name, "/"); index >= 0 {
				name = name[index+1:]
			}
			release, ok := s.releaseByAsset(name)
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(release.archive)
		}
	})
	s.srv = httptest.NewTLSServer(mux)

	// 让默认 HTTP 传输信任这台假发布服务器：更新器内部创建的客户端不指定
	// Transport，最终复用 http.DefaultTransport。只改测试进程内的传输配置，
	// 不动系统证书存储。
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	trusted := transport.Clone()
	roots := x509.NewCertPool()
	roots.AddCert(s.srv.Certificate())
	trusted.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = trusted
	return s
}

// addRelease 注册一个可下载的版本；payload 是归档里的二进制内容。
func (s *fakeUpdateSite) addRelease(t *testing.T, tag string, payload []byte) string {
	t.Helper()
	asset, err := updater.AssetName(tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archive := buildFakeUpdateArchive(t, asset, payload)
	sum := sha256.Sum256(archive)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[tag] = fakeRelease{tag: tag, asset: asset, archive: archive, sha: fmt.Sprintf("%x", sum)}
	return asset
}

// corruptChecksum 把某个版本的校验和行换成错误哈希，用于校验失败的用例。
func (s *fakeUpdateSite) corruptChecksum(t *testing.T, tag string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	release := s.releases[tag]
	release.sumPayload = strings.Repeat("0", 64) + "  " + release.asset + "\n"
	s.releases[tag] = release
}

func (s *fakeUpdateSite) checksumLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var lines []string
	for _, release := range s.releases {
		if seen[release.asset] {
			continue
		}
		seen[release.asset] = true
		if release.sumPayload != "" {
			lines = append(lines, release.sumPayload)
			continue
		}
		lines = append(lines, fmt.Sprintf("%s  %s\n", release.sha, release.asset))
	}
	return lines
}

func (s *fakeUpdateSite) releaseByAsset(name string) (fakeRelease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, release := range s.releases {
		if release.asset == name {
			return release, true
		}
	}
	return fakeRelease{}, false
}

func (s *fakeUpdateSite) setLatest(t *testing.T, tag string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = tag
}

func (s *fakeUpdateSite) baseURL(t *testing.T) string {
	t.Helper()
	t.Setenv("SSHMGR_UPDATE_BASE", s.srv.URL)
	return s.srv.URL
}

func buildFakeUpdateArchive(t *testing.T, assetName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(assetName, ".zip") {
		zw := zip.NewWriter(&buf)
		header := &zip.FileHeader{Name: "sshmgr.exe", Method: zip.Store}
		header.SetMode(0o755)
		w, err := zw.CreateHeader(header)
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

// testBinaryPayload 返回本测试二进制内容；配合 stagedVersionEnv 让替换前的
// 试运行能回答指定版本。
func testBinaryPayload(t *testing.T) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func withStagedVersion(t *testing.T, version string) {
	t.Helper()
	t.Setenv(stagedVersionEnv, version)
}

// writeFakeSelf 在临时目录里放一个假二进制，返回路径与原始内容。
func writeFakeSelf(t *testing.T) (dir, self string, original []byte) {
	t.Helper()
	dir = t.TempDir()
	name := "sshmgr"
	if runtime.GOOS == "windows" {
		name = "sshmgr.exe"
	}
	self = filepath.Join(dir, name)
	original = []byte("fake sshmgr binary for update service test\n")
	if err := os.WriteFile(self, original, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, self, original
}

func TestUpdateServiceCheckReportsVersionsForNonClient(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.2.0")
	_, self, _ := writeFakeSelf(t)

	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState:    func(string) (UpdateClientState, error) { return UpdateClientState{}, nil },
	})
	status, err := service.Check(context.Background(), "")
	if err != nil {
		t.Fatalf("检查更新失败：%v", err)
	}
	if status.CurrentVersion != "0.1.0" || status.RecommendedVersion != "0.1.0" ||
		status.LatestVersion != "v0.2.0" || status.TargetVersion != "v0.2.0" {
		t.Fatalf("版本信息不符：%+v", status)
	}
	if !status.CanUpgrade || status.Reason != "" {
		t.Fatalf("可升级状态不符：%+v", status)
	}
}

func TestUpdateServiceCheckClientReadsRecommendedVersion(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.3.0", testBinaryPayload(t))
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.3.0")
	_, self, _ := writeFakeSelf(t)

	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState: func(instance string) (UpdateClientState, error) {
			if instance != "laptop" {
				t.Fatalf("应按传入实例读取推荐版本，实际 %q", instance)
			}
			return UpdateClientState{IsClient: true, RecommendedVersion: "v0.2.0"}, nil
		},
	})
	status, err := service.Check(context.Background(), "laptop")
	if err != nil {
		t.Fatalf("检查更新失败：%v", err)
	}
	if status.TargetVersion != "v0.2.0" || !status.IsClient || status.RecommendedVersion != "v0.2.0" ||
		status.LatestVersion != "v0.3.0" {
		t.Fatalf("客户端应以推荐版本为目标：%+v", status)
	}
	if !status.CanUpgrade || status.Reason != "" {
		t.Fatalf("客户端落后于推荐版本时应可升级：%+v", status)
	}
}

func TestUpdateServiceCheckClientWithoutRecommendedVersionRefuses(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.3.0", testBinaryPayload(t))
	site.setLatest(t, "v0.3.0")
	_, self, _ := writeFakeSelf(t)

	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState: func(string) (UpdateClientState, error) {
			return UpdateClientState{IsClient: true}, nil
		},
	})
	status, err := service.Check(context.Background(), "")
	if err != nil {
		t.Fatalf("检查不应报错，页面仍要渲染：%v", err)
	}
	if status.CanUpgrade {
		t.Fatalf("读不到推荐版本时不能升级：%+v", status)
	}
	if !strings.Contains(status.Reason, "先同步或先升级权威端") {
		t.Fatalf("拒绝原因应提示先同步或先升级权威端：%+v", status)
	}
	if status.TargetVersion != "" || status.LatestVersion != "v0.3.0" {
		t.Fatalf("读不到推荐版本时不应给出目标版本：%+v", status)
	}
}

func TestUpdateServiceCheckClientNewerThanRecommendedVersion(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.2.0")
	_, self, _ := writeFakeSelf(t)

	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.3.0" },
		ClientState: func(string) (UpdateClientState, error) {
			return UpdateClientState{IsClient: true, RecommendedVersion: "v0.2.0"}, nil
		},
	})
	status, err := service.Check(context.Background(), "")
	if err != nil {
		t.Fatalf("检查更新失败：%v", err)
	}
	if !status.NewerThanAuthority || !strings.Contains(status.Reason, "比权威端新") {
		t.Fatalf("当前版本高于推荐版本时应提示比权威端新：%+v", status)
	}
}

// recordingAudit 记录升级服务写下的审计行，供断言动作名、来源与摘要。
type recordingUpdateAudit struct {
	mu      sync.Mutex
	entries []recordedUpdateAudit
}

type recordedUpdateAudit struct {
	source string
	ip     string
	entry  UpdateAuditEntry
}

func (r *recordingUpdateAudit) record(source Source, entry UpdateAuditEntry) error {
	sourceName, sourceIP := source.AuditFields()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, recordedUpdateAudit{source: sourceName, ip: sourceIP, entry: entry})
	return nil
}

func (r *recordingUpdateAudit) snapshot() []recordedUpdateAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedUpdateAudit(nil), r.entries...)
}

func TestUpdateServiceApplyReplacesBinaryWritesSignalAndAudit(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.2.0")
	withStagedVersion(t, "0.2.0")
	_, self, original := writeFakeSelf(t)

	var steps []UpdateStep
	audit := &recordingUpdateAudit{}
	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState:    func(string) (UpdateClientState, error) { return UpdateClientState{}, nil },
		Audit:          audit.record,
	})
	result, err := service.Apply(context.Background(), UpdateApplyRequest{
		Source:   WebUISource("127.0.0.1"),
		Progress: func(step UpdateStep) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("升级失败：%v", err)
	}
	if result.OldVersion != "0.1.0" || result.NewVersion != "v0.2.0" || result.BinaryPath != self {
		t.Fatalf("升级结果不符：%+v", result)
	}

	replaced, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(replaced, original) {
		t.Fatal("升级后二进制应被替换")
	}
	signal, ok := updater.ReadGenerationSignal(self)
	if !ok || signal.Version != "v0.2.0" {
		t.Fatalf("替换后应写代际信号：%+v ok=%v", signal, ok)
	}

	entries := audit.snapshot()
	if len(entries) != 1 {
		t.Fatalf("应写一条 update.apply 审计，实际 %+v", entries)
	}
	got := entries[0]
	if got.entry.Action != "update.apply" || got.entry.OldVersion != "0.1.0" ||
		got.entry.NewVersion != "v0.2.0" || got.entry.Result != "ok" {
		t.Fatalf("审计摘要不符：%+v", got)
	}
	if got.source != "web_ui" || got.ip != "127.0.0.1" {
		t.Fatalf("审计来源不符：%+v", got)
	}

	wantSteps := []UpdateStep{UpdateStepCheck, UpdateStepDownload, UpdateStepVerify, UpdateStepStaged, UpdateStepReplace}
	if len(steps) != len(wantSteps) {
		t.Fatalf("进度步骤不符：%v", steps)
	}
	for i, step := range wantSteps {
		if steps[i] != step {
			t.Fatalf("进度步骤第 %d 个是 %v，应为 %v", i, steps[i], step)
		}
	}
}

func TestUpdateServiceRemembersPendingRestartAndRejectsSecondApply(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.2.0")
	withStagedVersion(t, "0.2.0")
	_, self, _ := writeFakeSelf(t)

	replaceCalls := 0
	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState:    func(string) (UpdateClientState, error) { return UpdateClientState{}, nil },
		ReplaceBinary: func(staged, target string) error {
			replaceCalls++
			return updater.ReplaceBinary(staged, target)
		},
	})
	if _, err := service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()}); err != nil {
		t.Fatalf("第一次升级失败：%v", err)
	}

	status, err := service.Check(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !status.PendingRestart || status.PendingRestartVersion != "v0.2.0" ||
		status.CanUpgrade || !strings.Contains(status.Reason, "待重启") {
		t.Fatalf("替换后检查状态应显示待重启并禁止再次替换：%+v", status)
	}

	_, err = service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()})
	var updateErr *UpdateError
	if !errors.As(err, &updateErr) || updateErr.Step != UpdateStepCheck || replaceCalls != 1 {
		t.Fatalf("待重启时再次升级应被拒绝且不替换二进制：错误=%v 替换次数=%d", err, replaceCalls)
	}
}

func TestUpdateServiceApplyRejectsChecksumMismatchAndKeepsBinary(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.corruptChecksum(t, "v0.2.0")
	site.setLatest(t, "v0.2.0")
	_, self, original := writeFakeSelf(t)

	audit := &recordingUpdateAudit{}
	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState:    func(string) (UpdateClientState, error) { return UpdateClientState{}, nil },
		Audit:          audit.record,
	})
	_, err := service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()})
	var updateErr *UpdateError
	if err == nil || !errors.As(err, &updateErr) {
		t.Fatalf("校验和不符时应拒绝，实际 %v", err)
	}
	if updateErr.Step != UpdateStepVerify {
		t.Fatalf("失败步骤应为校验，实际 %v", updateErr.Step)
	}

	kept, readErr := os.ReadFile(self)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(kept, original) {
		t.Fatal("校验失败后旧二进制必须保持不变")
	}
	if _, ok := updater.ReadGenerationSignal(self); ok {
		t.Fatal("校验失败后不应写代际信号")
	}
	entries := audit.snapshot()
	if len(entries) != 1 || entries[0].entry.Result != "error" || entries[0].source != "cli" {
		t.Fatalf("失败也应写 update.apply 审计：%+v", entries)
	}
}

func TestUpdateServiceApplyRejectsClientWithoutRecommendedVersion(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.3.0", testBinaryPayload(t))
	site.setLatest(t, "v0.3.0")
	_, self, original := writeFakeSelf(t)

	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState: func(string) (UpdateClientState, error) {
			return UpdateClientState{IsClient: true}, nil
		},
	})
	_, err := service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()})
	var updateErr *UpdateError
	if err == nil || !errors.As(err, &updateErr) || updateErr.Step != UpdateStepCheck {
		t.Fatalf("读不到推荐版本时应拒绝升级，实际 %v", err)
	}
	kept, readErr := os.ReadFile(self)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(kept, original) {
		t.Fatal("拒绝升级时旧二进制必须保持不变")
	}
}

func TestUpdateServiceApplyAllowsOnlyOneAtATime(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.2.0")
	withStagedVersion(t, "0.2.0")
	_, self, _ := writeFakeSelf(t)

	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState:    func(string) (UpdateClientState, error) { return UpdateClientState{}, nil },
	})
	release := make(chan struct{})
	service.downloadPause = func() {
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()})
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if service.UpgradeInProgress() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待第一个升级开始超时")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()})
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("并发升级应返回升级正在进行，实际 %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("第一个升级应成功：%v", err)
	}
}

func TestUpdateServiceApplyContinuesWhenReplaceCommitsWithWarning(t *testing.T) {
	site.baseURL(t)
	site.addRelease(t, "v0.2.0", testBinaryPayload(t))
	site.setLatest(t, "v0.2.0")
	withStagedVersion(t, "0.2.0")
	_, self, _ := writeFakeSelf(t)

	audit := &recordingUpdateAudit{}
	service := NewUpdateService(UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: func() string { return "0.1.0" },
		ClientState:    func(string) (UpdateClientState, error) { return UpdateClientState{}, nil },
		Audit:          audit.record,
		ReplaceBinary: func(staged, target string) error {
			if err := updater.ReplaceBinary(staged, target); err != nil {
				return err
			}
			return &updater.CommittedWithError{Path: target, Err: errors.New("替换后清理旧备份失败")}
		},
	})
	result, err := service.Apply(context.Background(), UpdateApplyRequest{Source: CLISource()})
	if err != nil {
		t.Fatalf("已提交的替换不应按失败处理：%v", err)
	}
	if result.NewVersion != "v0.2.0" || !strings.Contains(result.Warning, "替换后清理旧备份失败") {
		t.Fatalf("结果应携带警告：%+v", result)
	}
	if _, ok := updater.ReadGenerationSignal(self); !ok {
		t.Fatal("替换已提交时应继续写代际信号")
	}
	entries := audit.snapshot()
	if len(entries) != 1 || entries[0].entry.Result != "ok" || !strings.Contains(entries[0].entry.Detail, "替换后清理旧备份失败") {
		t.Fatalf("已提交替换的审计应为成功并带警告：%+v", entries)
	}
}
