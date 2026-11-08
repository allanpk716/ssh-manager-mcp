package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// TestStandaloneUIServerSourceReadsVault 是单机角色的服务器列表来源：从本机
// vault 读出展示行，并按共享规则标记「需要注意」。
func TestStandaloneUIServerSourceReadsVault(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	rows := []*models.Server{
		{Name: "完整服务器", Host: "192.0.2.10", User: "root", Port: 22, AuthMethod: models.AuthPassword, CredentialID: cid, Role: "数据库主库"},
		{Name: "未挂凭据", Host: "192.0.2.11", User: "root", Port: 22022, AuthMethod: models.AuthPassword, Role: "r"},
		{Name: "未设角色", Host: "192.0.2.12", User: "admin", Port: 22, AuthMethod: models.AuthPassword, CredentialID: cid},
		{Name: "待补口令", Host: "192.0.2.13", User: "admin", Port: 22, AuthMethod: models.AuthPassword, CredentialID: cid, Role: "r", Tags: []string{models.TagNeedsPassphrase}},
	}
	for _, srv := range rows {
		if _, err := st.AddServer(srv); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := standaloneUIServerSource{}.Servers()
	if err != nil {
		t.Fatalf("读取单机服务器列表失败：%v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("展示行数量 = %d，应为 %d", len(got), len(rows))
	}
	byName := make(map[string]struct {
		user, host string
		port       int
		warn       bool
	}, len(got))
	for _, row := range got {
		byName[row.Name] = struct {
			user, host string
			port       int
			warn       bool
		}{row.User, row.Host, row.Port, row.NeedsAttention}
	}
	wantWarn := map[string]bool{"完整服务器": false, "未挂凭据": true, "未设角色": true, "待补口令": true}
	for name, want := range wantWarn {
		row, ok := byName[name]
		if !ok {
			t.Fatalf("缺少服务器 %s 的展示行：%+v", name, got)
		}
		if row.warn != want {
			t.Fatalf("服务器 %s 的需要注意标记 = %v，应为 %v", name, row.warn, want)
		}
	}
	if complete := byName["完整服务器"]; complete.user != "root" || complete.host != "192.0.2.10" || complete.port != 22 {
		t.Fatalf("展示行连接信息不完整：%+v", complete)
	}
}

// TestStandaloneUIOpsStoreQueriesAudit 是单机角色的运维来源：每次查询重新
// 打开本机 vault，不长期持有存储句柄。
func TestStandaloneUIOpsStoreQueriesAudit(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteAudit(store.AuditRow{
		Action: "admin.login", Status: "ok", Source: store.AuditSourceCLI,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := standaloneUIOpsStore().QueryAudit(webui.OpsAuditQuery{
		Actions: []string{"admin.login"}, Source: store.AuditSourceCLI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "admin.login" || rows[0].ID == 0 {
		t.Fatalf("单机 Web UI 应能经运维窄接口读取审计：%+v", rows)
	}
}

// seedClientCacheSnapshot 把指定快照拉成本地加密缓存（指定实例），供 Web UI
// 的客户端来源与版本提示来源测试使用。
func seedClientCacheSnapshot(t *testing.T, snap *store.Snapshot, instance string) {
	t.Helper()
	userDir := t.TempDir()
	vaultDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("APPDATA", userDir)
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if instance == "" {
		t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	} else {
		t.Setenv("SSHMGR_CACHE_DIR", "")
		t.Setenv("SSHMGR_CACHE_DEK", "")
	}

	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	serve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if instance != "" {
			w.Header().Set("X-Sshmgr-Device-Name", instance)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(serve.Close)

	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	mem := &store.MemKeyProvider{}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	prev := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prev })

	pin := mcpserver.SPKIFingerprint(serve.Certificate())
	if _, err := clientops.DoPull(serve.URL, "device-code", pin, clientops.PullOpts{Instance: instance}); err != nil {
		t.Fatalf("准备缓存拉取失败：%v", err)
	}
	serve.Close()
}

// TestClientUIServerSourceReadsSnapshot 是客户端角色的服务器列表来源：从当前
// 实例的本地快照读出展示行，并按同一规则标记「需要注意」。
func TestClientUIServerSourceReadsSnapshot(t *testing.T) {
	snap := &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "ok", Name: "完整服务器", User: "root", Host: "192.0.2.10", Port: 22, CredentialID: "c", Role: "r"},
		{ID: "nocred", Name: "未挂凭据", User: "root", Host: "192.0.2.11", Port: 22022, Role: "r"},
		{ID: "norole", Name: "未设角色", User: "admin", Host: "192.0.2.12", Port: 22, CredentialID: "c"},
		{ID: "passphrase", Name: "待补口令", User: "admin", Host: "192.0.2.13", Port: 22, CredentialID: "c", Role: "r", TagsRaw: `["needs-passphrase"]`},
	}}
	seedClientCacheSnapshot(t, snap, "")

	got, err := clientUIServerSource{}.Servers()
	if err != nil {
		t.Fatalf("读取客户端服务器列表失败：%v", err)
	}
	if len(got) != len(snap.Servers) {
		t.Fatalf("展示行数量 = %d，应为 %d", len(got), len(snap.Servers))
	}
	byName := map[string]webuiServerRowInfo{}
	for _, row := range got {
		byName[row.Name] = webuiServerRowInfo{row.User, row.Host, row.Port, row.NeedsAttention}
	}
	wantWarn := map[string]bool{"完整服务器": false, "未挂凭据": true, "未设角色": true, "待补口令": true}
	for name, want := range wantWarn {
		row, ok := byName[name]
		if !ok {
			t.Fatalf("缺少服务器 %s 的展示行：%+v", name, got)
		}
		if row.warn != want {
			t.Fatalf("服务器 %s 的需要注意标记 = %v，应为 %v", name, row.warn, want)
		}
	}
	if complete := byName["完整服务器"]; complete.user != "root" || complete.host != "192.0.2.10" || complete.port != 22 {
		t.Fatalf("展示行连接信息不完整：%+v", complete)
	}
}

type webuiServerRowInfo struct {
	user, host string
	port       int
	warn       bool
}

// TestClientUIServerSourceHonorsInstance 验证 --instance 语义：客户端来源按
// 指定实例的快照取数，不混入默认实例。
func TestClientUIServerSourceHonorsInstance(t *testing.T) {
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "d", Name: "默认实例服务器", User: "u", Host: "192.0.2.10", Port: 22, CredentialID: "c", Role: "r"},
	}}, "")
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "n", Name: "笔记本实例服务器", User: "u", Host: "192.0.2.20", Port: 22, CredentialID: "c", Role: "r"},
	}}, "laptop")

	got, err := clientUIServerSource{instance: "laptop"}.Servers()
	if err != nil {
		t.Fatalf("读取指定实例服务器列表失败：%v", err)
	}
	if len(got) != 1 || got[0].Name != "笔记本实例服务器" {
		t.Fatalf("指定实例应读到自己的快照，实际：%+v", got)
	}
}

// TestUIVersionHintSourceReusesClientMismatchHint 验证版本提示来源复用客户端
// 版本不一致判定：落后时给出提示，一致或非客户端时不显示。
func TestUIVersionHintSourceReusesClientMismatchHint(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })

	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, RecommendedVersion: "1.1.0", Servers: []store.SnapshotServer{
		{ID: "s", Name: "服务器", User: "u", Host: "192.0.2.10", Port: 22, CredentialID: "c", Role: "r"},
	}}, "")
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	hint := uiVersionHintSource("")
	if hint != "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级" {
		t.Fatalf("版本提示 = %q，应为落后提示", hint)
	}

	buildinfo.Version = "1.1.0"
	if hint := uiVersionHintSource(""); hint != "" {
		t.Fatalf("版本一致时不应提示，实际 %q", hint)
	}
}
