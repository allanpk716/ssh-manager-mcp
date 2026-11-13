package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	got, err := standaloneUIServerSource{}.Servers("")
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

// TestStandaloneUISecretStoreReadsVault 是单机角色的机密项读取来源：每次读
// 取重新打开本机 vault，并只返回可见元数据。
func TestStandaloneUISecretStoreReadsVault(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := st.AddServer(&models.Server{Name: "web", Host: "192.0.2.10", Port: 22, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("WEB_TOKEN", "网站", []byte("standalone-secret-value"), "WEB_TOKEN_FILE", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"WEB_TOKEN"}, []string{"web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := standaloneUISecretStore().ListSecrets("", "网站", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "WEB_TOKEN" || rows[0].Note != "网站" ||
		rows[0].EnvironmentName != "WEB_TOKEN_FILE" || len(rows[0].AllowedServers) != 1 ||
		rows[0].AllowedServers[0] != "web" || rows[0].ID == "" || rows[0].ID == serverID {
		t.Fatalf("单机机密项读取结果不正确：%+v", rows)
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

	got, err := clientUIServerSource{}.Servers("")
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
	}, SecretDeliveryRecords: []store.SnapshotSecretDeliveryRecord{{
		ID: "record", ServerID: "n", Path: "/etc/app/config.yml", PathKey: "/etc/app/config.yml",
	}}}, "laptop")

	got, err := clientUIServerSource{}.Servers("laptop")
	if err != nil {
		t.Fatalf("读取指定实例服务器列表失败：%v", err)
	}
	if len(got) != 1 || got[0].Name != "笔记本实例服务器" {
		t.Fatalf("指定实例应读到自己的快照，实际：%+v", got)
	}
	list, err := clientUIServerSource{}.ServerList("laptop", false)
	if err != nil {
		t.Fatalf("读取指定实例服务器页列表失败：%v", err)
	}
	if len(list) != 1 || list[0].ID != "n" {
		t.Fatalf("服务器页应读取指定实例快照，实际：%+v", list)
	}
	detail, err := clientUIServerSource{}.ServerDetail("laptop", "n")
	if err != nil {
		t.Fatalf("读取指定实例服务器详情失败：%v", err)
	}
	if detail == nil || detail.ID != "n" {
		t.Fatalf("服务器详情应读取指定实例快照，实际：%+v", detail)
	}
	if detail.ShowDeliveryRecords || len(detail.DeliveryRecords) != 0 || len(detail.SiblingDeliveryRecords) != 0 {
		t.Fatalf("客户端 Web UI 不得装配下发记录：%+v", detail)
	}
}

// TestClientUIServerSourceAssemblesCroppedReverseGroups 验证客户端服务器详
// 情只从本地快照装配机密项、共享存储与只读主机密钥锚，并且同设备提示不
// 推断快照外的服务器条目。
func TestClientUIServerSourceAssemblesCroppedReverseGroups(t *testing.T) {
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "web", Name: "网站", ShellType: "posix", DeviceID: "device-workstation"},
		{ID: "worker", Name: "工作进程", ShellType: "posix", DeviceID: "device-workstation"},
	}, Devices: []store.SnapshotDevice{{ID: "device-workstation", Name: "workstation"}},
		Secrets: []store.SnapshotSecret{
			{ID: "secret-web", Name: "WEB_TOKEN", Note: "网站令牌", EnvironmentName: "WEB_TOKEN_FILE"},
			{ID: "secret-worker", Name: "WORKER_TOKEN", Note: "工作进程令牌"},
			{ID: "secret-share", Name: "DATASET_MOUNT_PASSWORD", Note: "挂载密码"},
			{ID: "secret-worker-share", Name: "WORKER_MOUNT_PASSWORD", Note: "工作目录挂载密码"},
			{ID: "secret-missing-share", Name: "MISSING_SIBLING_MOUNT_PASSWORD", Note: "快照外同一设备其他服务器条目的挂载密码"},
		}, SecretAllows: []store.SnapshotSecretAllow{
			{SecretID: "secret-web", ServerID: "web"},
			{SecretID: "secret-worker", ServerID: "worker"},
			{SecretID: "secret-share", ServerID: "web"},
			{SecretID: "secret-worker-share", ServerID: "worker"},
			{SecretID: "secret-missing-share", ServerID: "missing-worker"},
		}, SharedStorages: []store.SnapshotSharedStorage{
			{ID: "share-datasets", Name: "datasets", DeviceName: "workstation", MountUser: "mount-user", PasswordSecretID: "secret-share", Drive: "E"},
			{ID: "share-worker-data", Name: "worker-data", DeviceName: "workstation", MountUser: "worker-mount-user", PasswordSecretID: "secret-worker-share", Drive: "F"},
			{ID: "share-missing-worker-data", Name: "missing-worker-data", DeviceName: "workstation", MountUser: "missing-mount-user", PasswordSecretID: "secret-missing-share", Drive: "G"},
		}, Profiles: []store.SnapshotProfile{{ID: "profile-1", Name: "客户端配置组"}},
		Grants: []store.SnapshotGrant{
			{ProfileID: "profile-1", ServerID: "web"},
			{ProfileID: "profile-1", ServerID: "worker"},
		}, EntryHostKeys: []store.SnapshotEntryHostKey{{
			ServerID: "web", KeyBlob: []byte("SHA256:" + strings.Repeat("b", 43)),
			CreatedAt: 1760000900, PinFormat: store.PinFormatFingerprint,
			PinSource: store.PinSourceTofu, PinDevice: "office-laptop",
			HandshakeAddr: "192.0.2.10:22",
		}}}, "")

	detail, err := clientUIServerSource{}.ServerDetail("", "网站")
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil {
		t.Fatal("应找到快照内服务器")
	}
	if len(detail.Secrets) != 1 || detail.Secrets[0].ID != "secret-web" ||
		detail.Secrets[0].Name != "WEB_TOKEN" || detail.Secrets[0].Note != "网站令牌" {
		t.Fatalf("客户端详情机密项裁剪不正确：%+v", detail.Secrets)
	}
	if len(detail.SharedStorages) != 1 || detail.SharedStorages[0].ID != "share-datasets" ||
		detail.SharedStorages[0].Name != "datasets" || detail.SharedStorages[0].ProviderDeviceName != "workstation" ||
		detail.SharedStorages[0].MountUser != "mount-user" ||
		detail.SharedStorages[0].PasswordSecretName != "DATASET_MOUNT_PASSWORD" ||
		detail.SharedStorages[0].MountPoint != "/mnt/sshmgr/datasets" {
		t.Fatalf("客户端详情共享存储裁剪不正确：%+v", detail.SharedStorages)
	}
	if len(detail.SiblingSharedStorages) != 1 ||
		detail.SiblingSharedStorages[0].Name != "worker-data" ||
		detail.SiblingSharedStorages[0].SourceServerName != "工作进程" ||
		detail.SiblingSharedStorages[0].MountPoint != "/mnt/sshmgr/worker-data" {
		t.Fatalf("客户端详情同设备共享存储提示不正确：%+v", detail.SiblingSharedStorages)
	}
	if !detail.ShowHostKeyAnchor || detail.HostKeyAnchor == nil ||
		detail.HostKeyAnchor.Fingerprint != "SHA256:"+strings.Repeat("b", 43) ||
		detail.HostKeyAnchor.SourceLabel != "首次信任" ||
		detail.HostKeyAnchor.Device != "office-laptop" ||
		detail.HostKeyAnchor.HandshakeAddr != "192.0.2.10:22" ||
		!detail.HostKeyAnchor.PinnedAt.Equal(time.Unix(1760000900, 0)) {
		t.Fatalf("客户端详情主机密钥锚应只读来自本地快照：%+v", detail.HostKeyAnchor)
	}
	if len(detail.Profiles) != 0 || detail.ShowDeliveryRecords || len(detail.DeliveryRecords) != 0 ||
		len(detail.SiblingDeliveryRecords) != 0 || len(detail.SecretChoices) != 0 ||
		len(detail.SharedStorageChoices) != 0 {
		t.Fatalf("客户端详情不得装配 profile、下发记录或编辑选项：%+v", detail)
	}
}

// TestClientUISecretSourceReadsSnapshot 是客户端角色的机密项读取来源：读取
// 当前实例的本地快照，并只显示快照范围内的服务器。
func TestClientUISecretSourceReadsSnapshot(t *testing.T) {
	snap := &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "s", Name: "快照服务器", User: "u", Host: "192.0.2.10", Port: 22, CredentialID: "c", Role: "r"},
	}, Secrets: []store.SnapshotSecret{{
		ID: "secret", Name: "CLIENT_TOKEN", Note: "客户端", EnvironmentName: "CLIENT_TOKEN_FILE",
		Value: []byte("client-secret-value"), CreatedAt: 100, ValueUpdatedAt: 200,
	}}, SecretAllows: []store.SnapshotSecretAllow{{SecretID: "secret", ServerID: "s"}}}
	seedClientCacheSnapshot(t, snap, "")

	rows, err := clientUISecretSource{}.ListSecrets("", "客户端", "快照服务器")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "secret" || rows[0].Name != "CLIENT_TOKEN" ||
		rows[0].Note != "客户端" || rows[0].EnvironmentName != "CLIENT_TOKEN_FILE" ||
		len(rows[0].AllowedServers) != 1 || rows[0].AllowedServers[0] != "快照服务器" {
		t.Fatalf("客户端机密项读取结果不正确：%+v", rows)
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

func TestClientWebUIShowsNewerClientHintForOldAuthority(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "s", Name: "服务器", User: "u", Host: "192.0.2.10", Port: 22, CredentialID: "c", Role: "r"},
	}}, "")
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	handler, startup, err := webui.New(webui.Config{
		Role:        webui.RoleClient,
		Servers:     clientUIServerSource{},
		VersionHint: func() string { return uiVersionHintSource("") },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造客户端 Web UI Handler 失败：%v", err)
	}
	start := httptest.NewRequest(http.MethodGet, "/ui/start?token="+startup.Token(), nil)
	start.Host = "127.0.0.1:18080"
	startRecorder := httptest.NewRecorder()
	handler.ServeHTTP(startRecorder, start)
	if startRecorder.Code != http.StatusSeeOther || len(startRecorder.Result().Cookies()) != 1 {
		t.Fatalf("启动链接应换取会话，状态码 = %d cookie = %v", startRecorder.Code, startRecorder.Result().Cookies())
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	pageRequest.Host = "127.0.0.1:18080"
	pageRequest.AddCookie(startRecorder.Result().Cookies()[0])
	pageRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pageRecorder, pageRequest)
	page, _ := io.ReadAll(pageRecorder.Result().Body)
	want := "客户端比权威端新，建议先升级权威端"
	if pageRecorder.Code != http.StatusOK || !strings.Contains(string(page), want) {
		t.Fatalf("客户端 Web UI 应显示 %q，状态码 = %d 页面：\n%s", want, pageRecorder.Code, page)
	}
}
