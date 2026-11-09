package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubSecretWriter 按测试需要返回固定错误，用来区分「可直接展示的校验错
// 误」与「不能透出的内部错误」。
type stubSecretWriter struct {
	addErr error
}

func (s stubSecretWriter) AddSecret(string, string, string, []byte, SecretWriteSource) error {
	return s.addErr
}

func (s stubSecretWriter) SetSecretValue(string, []byte, SecretWriteSource) error { return nil }

func (s stubSecretWriter) EditSecretMetadata(string, string, string, SecretWriteSource) error {
	return nil
}

func (s stubSecretWriter) DeleteSecret(string, SecretWriteSource) error { return nil }

func (s stubSecretWriter) PreviewAuthorization([]SecretAuthorizationCell) (SecretAuthorizationPreview, error) {
	return SecretAuthorizationPreview{}, nil
}

func (s stubSecretWriter) ApplyAuthorization([]SecretAuthorizationCell, string, SecretWriteSource) (SecretApplyResult, *SecretAuthorizationConflict, error) {
	return SecretApplyResult{}, nil, nil
}

func newSecretsHandler(t *testing.T, writer SecretWriter) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role:          RoleStandalone,
		Servers:       &stubServerSource{},
		Access:        &stubAccessStore{},
		Secrets:       stubSecretSource{},
		SecretsWriter: writer,
		VersionHint:   func() string { return "" },
		Port:          18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return handler, cookies[0]
}

func postSecretForm(t *testing.T, handler http.Handler, cookie *http.Cookie, target string, form url.Values) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

// TestSecretNewInternalErrorUsesGenericText 覆盖审查第 7 项：适配层的内部错
// 误不能原样透给页面，只有可展示的校验错误保留原文。
func TestSecretNewInternalErrorUsesGenericText(t *testing.T) {
	handler, cookie := newSecretsHandler(t, stubSecretWriter{
		addErr: errors.New("内部错误：数据库文件 /vault/store.db 被锁"),
	})
	page := doGetWithCookie(t, handler, cookie, "/ui/secrets/")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, page))

	internal := postSecretForm(t, handler, cookie, "/ui/secrets/new", url.Values{
		"csrf_token": {csrf}, "name": {"API_TOKEN"}, "value": {"secret-value"},
	})
	internalBody := readResponseBody(t, internal)
	if internal.StatusCode != http.StatusInternalServerError {
		t.Fatalf("内部错误应返回 500，实际 %d：\n%s", internal.StatusCode, internalBody)
	}
	if strings.Contains(internalBody, "/vault/store.db") || strings.Contains(internalBody, "被锁") {
		t.Fatalf("内部错误细节不得透给页面：\n%s", internalBody)
	}
	if !strings.Contains(internalBody, "新增机密项失败，请稍后重试。") {
		t.Fatalf("内部错误应显示通用文案：\n%s", internalBody)
	}
}

// TestSecretAuthorizationConfirmRejectsGetMethod 覆盖审查第 8 项：确认路由
// 用 GET 访问应返回 405，与其他写路由一致。
func TestSecretAuthorizationConfirmRejectsGetMethod(t *testing.T) {
	handler, cookie := newSecretsHandler(t, stubSecretWriter{})
	resp := doGetWithCookie(t, handler, cookie, "/ui/secrets/authorization/confirm")
	respBody := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 访问确认路由应返回 405，实际 %d：\n%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, "确认授权只接受 POST 请求。") {
		t.Fatalf("405 页面应说明只接受 POST 请求：\n%s", respBody)
	}
}
