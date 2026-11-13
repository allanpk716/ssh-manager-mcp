package webui

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backupConfigStubOpsStore struct {
	*stubOpsStore
	backupConfig    OpsBackupConfig
	backupConfigErr error
	savedConfig     OpsBackupConfig
	saveSource      OpsWriteSource
	saveCalls       int
	saveErr         error
}

func (s *backupConfigStubOpsStore) ReadBackupConfig() (OpsBackupConfig, error) {
	return s.backupConfig, s.backupConfigErr
}

func (s *backupConfigStubOpsStore) SaveBackupConfig(config OpsBackupConfig, source OpsWriteSource) error {
	s.saveCalls++
	s.savedConfig = config
	s.saveSource = source
	return s.saveErr
}

func newBackupConfigStub(status OpsBackupStatus) *backupConfigStubOpsStore {
	return &backupConfigStubOpsStore{
		stubOpsStore: &stubOpsStore{backupStatus: status},
		backupConfig: OpsBackupConfig{Dir: status.ConfigDir, Keep: status.ConfigKeep, PassphraseFile: status.ConfigPassphraseFile},
	}
}

func TestOpsBackupPageRendersConfigForm(t *testing.T) {
	status := opsBackupStatusFixture()
	ops := newBackupConfigStub(status)
	handler, cookie := newOpsBackupHandler(t, ops)

	resp := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
	body := readResponseBody(t, resp)
	for _, want := range []string{
		`action="/ui/ops/backup/config"`, `name="dir"`, `name="keep"`,
		`name="passphrase_file"`, `value="/backups"`, `value="7"`, `value="/vault/backup.pass"`,
		"保存备份配置",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("备份页配置表单缺少 %q：\n%s", want, body)
		}
	}
	if ops.saveCalls != 0 {
		t.Fatal("打开备份页不应保存配置")
	}

	guideOps := newBackupConfigStub(OpsBackupStatus{
		ConfigPath: "/vault/backup.json", ConfigMissing: true, ConfigError: "file not found",
	})
	guideHandler, guideCookie := newOpsBackupHandler(t, guideOps)
	guide := doGetWithCookie(t, guideHandler, guideCookie, "/ui/ops/backup")
	guideBody := readResponseBody(t, guide)
	if guide.StatusCode != http.StatusOK || !strings.Contains(guideBody, `href="#backup-config"`) {
		t.Fatalf("配置缺失的引导应直达配置表单，状态码=%d 页面：\n%s", guide.StatusCode, guideBody)
	}
}

func TestOpsBackupConfigSaveWithoutReverifyForStandalone(t *testing.T) {
	ops := newBackupConfigStub(opsBackupStatusFixture())
	handler, cookie := newOpsBackupHandler(t, ops)
	newDir, newPassFile := testBackupConfigSavePaths(t)

	resp := postStandaloneForm(t, handler, cookie, "/ui/ops/backup/config", url.Values{
		"dir": {newDir}, "keep": {"9"}, "passphrase_file": {newPassFile},
	})
	body := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/ops/backup" ||
		ops.saveCalls != 1 || ops.savedConfig.Dir != newDir || ops.savedConfig.Keep != 9 ||
		ops.savedConfig.PassphraseFile != newPassFile ||
		ops.saveSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("单机保存备份配置不应要求重新验证，状态码=%d 跳转=%q 调用=%+v 页面：\n%s",
			resp.StatusCode, resp.Header.Get("Location"), ops, body)
	}
	page := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
	pageBody := readResponseBody(t, page)
	if !strings.Contains(pageBody, "备份配置已保存") {
		t.Fatalf("保存成功后应显示提示：\n%s", pageBody)
	}
}

func TestOpsBackupConfigInvalidValuesRerenderFieldErrors(t *testing.T) {
	ops := newBackupConfigStub(opsBackupStatusFixture())
	handler, cookie := newOpsBackupHandler(t, ops)
	invalidDir, invalidPassFile := testBackupConfigInvalidPaths(t)

	resp := postStandaloneForm(t, handler, cookie, "/ui/ops/backup/config", url.Values{
		"dir": {invalidDir}, "keep": {"abc"}, "passphrase_file": {invalidPassFile},
	})
	body := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || ops.saveCalls != 0 {
		t.Fatalf("非法配置应重新渲染且不调用保存，状态码=%d 调用=%d 页面：\n%s",
			resp.StatusCode, ops.saveCalls, body)
	}
	for _, want := range []string{
		`id="backup-config-keep-error"`, "保留份数必须是 0 或正整数。",
		`id="backup-config-passphrase-file-error"`, "口令文件不能位于备份目录里。",
		`value="` + invalidDir + `"`, `value="abc"`, `value="` + invalidPassFile + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("非法配置重新渲染缺少 %q：\n%s", want, body)
		}
	}
}

func TestAuthorityOpsBackupConfigRequiresReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	ops := newBackupConfigStub(opsBackupStatusFixture())
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Ops: ops,
		Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	newDir, newPassFile := testBackupConfigSavePaths(t)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/backup", cookie)
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")
	form := url.Values{
		"csrf_token": {csrfToken}, "dir": {newDir}, "keep": {"9"},
		"passphrase_file": {newPassFile},
	}

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/backup/config", form, cookie)
	rejectedBody := readResponseBody(t, rejected)
	wantRedirect := `/ui/reverify?next=` + url.QueryEscape("/ui/ops/backup")
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantRedirect ||
		ops.saveCalls != 0 {
		t.Fatalf("权威端保存配置应先重新验证，状态码=%d 跳转=%q 调用=%d 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), ops.saveCalls, rejectedBody)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantRedirect, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {"/ui/ops/backup"},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != "/ui/ops/backup" {
		t.Fatalf("重新验证后应回到备份页，状态码=%d 跳转=%q 页面：\n%s",
			reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	accepted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/backup/config", form, cookie)
	acceptedBody := readResponseBody(t, accepted)
	if accepted.StatusCode != http.StatusSeeOther || ops.saveCalls != 1 ||
		ops.saveSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("重新验证后应能保存配置，状态码=%d 调用=%+v 页面：\n%s",
			accepted.StatusCode, ops, acceptedBody)
	}
}

// testBackupConfigSavePaths 返回当前操作系统认可的绝对路径。Unix 的斜杠
// 根路径不能在 Windows 上代表绝对路径，因此测试用临时目录生成两端通用
// 的输入。
func testBackupConfigSavePaths(t *testing.T) (dir, passphraseFile string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "new-backups")
	passphraseFile = filepath.Join(t.TempDir(), "new-backup.pass")
	return dir, passphraseFile
}

// testBackupConfigInvalidPaths 返回当前操作系统上绝对、但口令文件位于备
// 份目录里的非法组合。
func testBackupConfigInvalidPaths(t *testing.T) (dir, passphraseFile string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "backups")
	passphraseFile = filepath.Join(dir, "backup.pass")
	return dir, passphraseFile
}

func TestOpsBackupConfigWriteRequiresOriginAndCSRFToken(t *testing.T) {
	ops := newBackupConfigStub(opsBackupStatusFixture())
	handler, cookie := newOpsBackupHandler(t, ops)
	form := url.Values{"dir": {"/new-backups"}, "keep": {"9"}, "passphrase_file": {"/vault/new-backup.pass"}}
	missingToken := url.Values{}
	for key, values := range form {
		missingToken[key] = append([]string(nil), values...)
	}

	missing := postAccessWrite(t, handler, cookie, "/ui/ops/backup/config", missingToken, "http://127.0.0.1:18080", "")
	missingBody := readResponseBody(t, missing)
	if missing.StatusCode != http.StatusForbidden || ops.saveCalls != 0 {
		t.Fatalf("缺少防跨站请求伪造令牌应拒绝，状态码=%d 调用=%d 页面：\n%s",
			missing.StatusCode, ops.saveCalls, missingBody)
	}

	page := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")
	wrongOrigin := postAccessWrite(t, handler, cookie, "/ui/ops/backup/config", form, "https://evil.example", csrfToken)
	wrongOriginBody := readResponseBody(t, wrongOrigin)
	if wrongOrigin.StatusCode != http.StatusForbidden || ops.saveCalls != 0 {
		t.Fatalf("来源不对应拒绝，状态码=%d 调用=%d 页面：\n%s",
			wrongOrigin.StatusCode, ops.saveCalls, wrongOriginBody)
	}
}
