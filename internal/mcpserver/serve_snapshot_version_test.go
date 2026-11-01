package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// getSnapshotVersion 请求 /snapshot/version 并返回状态码与解码后的响应体。
func getSnapshotVersion(t *testing.T, srvURL, token, method string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srvURL+"/snapshot/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("响应不是 JSON 对象:%v(%s)", err, body)
		}
	}
	return res.StatusCode, decoded
}

// TestSnapshotVersion_BoundDeviceCodeReturnsRevisionOnly: 已绑定 profile 的
// 设备码查询修订号;响应只有一个字段,不记录最后拉取时间与客户端版本。
func TestSnapshotVersion_BoundDeviceCodeReturnsRevisionOnly(t *testing.T) {
	srv, cacheToken, _, st := newSnapshotRunner(t)
	want, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	status, body := getSnapshotVersion(t, srv.URL, cacheToken, http.MethodGet)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d,应为 200", status)
	}
	if len(body) != 1 {
		t.Fatalf("响应字段 = %v,应只有 snapshot_revision", body)
	}
	got, ok := body["snapshot_revision"].(float64)
	if !ok || int64(got) != want {
		t.Fatalf("snapshot_revision = %v,应为 %d", body["snapshot_revision"], want)
	}

	// 查询不得改变 cache-tokens ls 显示的最后拉取时间与客户端版本。
	tokens, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range tokens {
		if !ct.LastPullAt.IsZero() || ct.ClientVersion != "" {
			t.Fatalf("设备码 %s 被查询污染:last_pull=%v client_version=%q", ct.Name, ct.LastPullAt, ct.ClientVersion)
		}
	}

	// 配置写入后,下一次查询返回更大的修订号;依旧不写设备码状态。
	if _, err := st.AddServer(&models.Server{Name: "new", Host: "192.0.2.20", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	status, body = getSnapshotVersion(t, srv.URL, cacheToken, http.MethodGet)
	if status != http.StatusOK {
		t.Fatalf("第二次查询状态码 = %d,应为 200", status)
	}
	got, _ = body["snapshot_revision"].(float64)
	if int64(got) <= want {
		t.Fatalf("写入后的 snapshot_revision = %d,应大于 %d", int64(got), want)
	}
	tokens, err = st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range tokens {
		if !ct.LastPullAt.IsZero() || ct.ClientVersion != "" {
			t.Fatalf("设备码 %s 被第二次查询污染:last_pull=%v client_version=%q", ct.Name, ct.LastPullAt, ct.ClientVersion)
		}
	}
}

// TestSnapshotVersion_UnboundDeviceCode403: 未绑定 profile 的设备码与
// /snapshot 一样返回 403,而不是会触发客户端隔离的 401。
func TestSnapshotVersion_UnboundDeviceCode403(t *testing.T) {
	srv, cacheToken, _, st := newSnapshotRunner(t)
	if err := st.ExecForTest(`UPDATE cache_tokens SET profile_id=NULL WHERE name='laptop'`); err != nil {
		t.Fatal(err)
	}
	status, _ := getSnapshotVersion(t, srv.URL, cacheToken, http.MethodGet)
	if status != http.StatusForbidden {
		t.Fatalf("未绑定设备码状态码 = %d,应为 403", status)
	}
}

// TestSnapshotVersion_OtherTokensAndMethods: 项目令牌不能查询修订号,非
// GET 请求返回 405。
func TestSnapshotVersion_OtherTokensAndMethods(t *testing.T) {
	srv, _, projToken, _ := newSnapshotRunner(t)
	if status, _ := getSnapshotVersion(t, srv.URL, projToken, http.MethodGet); status != http.StatusUnauthorized {
		t.Fatalf("项目令牌状态码 = %d,应为 401", status)
	}
	srv2, cacheToken, _, _ := newSnapshotRunner(t)
	if status, _ := getSnapshotVersion(t, srv2.URL, cacheToken, http.MethodPost); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d,应为 405", status)
	}
}
