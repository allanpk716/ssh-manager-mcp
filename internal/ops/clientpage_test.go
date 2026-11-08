package ops

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func TestListClientInstancesMarksMissingMaterials(t *testing.T) {
	userDir := t.TempDir()
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)

	defaultDir := filepath.Join(userDir, "ssh-manager")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"cache.auth.json":   `{"url":"https://authority","token":"code","pin":"sha256:00"}`,
		"cache.bin":         "snapshot",
		"cache.meta.json":   `{"pulled_at":1}`,
		"cache.config.json": "{}",
	} {
		if err := os.WriteFile(filepath.Join(defaultDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dekDir, "cache-dek.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	laptopDir := filepath.Join(defaultDir, "instances", "laptop")
	if err := os.MkdirAll(laptopDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(laptopDir, "cache.auth.json"), []byte(`{"url":"https://authority","token":"code","pin":"sha256:00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dekDir, "cache-dek-laptop.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	instances, err := ListClientInstances(time.Unix(100, 0))
	if err != nil {
		t.Fatalf("列出客户端实例失败：%v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("实例数量 = %d，应为 2（默认实例与 laptop）：%+v", len(instances), instances)
	}
	if instances[0].Name != "" || !instances[0].Complete || len(instances[0].MissingMaterials) != 0 {
		t.Fatalf("默认实例应材料齐全：%+v", instances[0])
	}
	laptop := instances[1]
	if laptop.Name != "laptop" || laptop.Complete {
		t.Fatalf("laptop 实例状态不符：%+v", laptop)
	}
	if len(laptop.MissingMaterials) != 2 ||
		laptop.MissingMaterials[0] != "快照" || laptop.MissingMaterials[1] != "快照元数据" {
		t.Fatalf("laptop 应标明缺少快照与快照元数据：%+v", laptop)
	}
}

func TestReadClientStatusShowsConnectionAndSnapshot(t *testing.T) {
	cacheDir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "cache-dek.key")
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	t.Setenv("SSHMGR_CACHE_DEK", keyPath)

	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &store.Snapshot{
		Version: 1, SnapshotRevision: 42, RecommendedVersion: "1.2.0",
		Servers:  []store.SnapshotServer{{ID: "one", Name: "一台服务器"}},
		Profiles: []store.SnapshotProfile{{ID: "profile-id", Name: "团队"}},
	}
	plain, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := vaultio.EncryptWithKey(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"cache.auth.json": []byte(`{"url":"https://authority:7878","token":"device-code","pin":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`),
		"cache.bin":       encrypted,
		"cache.meta.json": []byte(`{"url":"https://authority:7878","pulled_at":120,"server_anchored":true,"scoped":true}`),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cacheDir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := ReadClientStatus("", time.Unix(200, 0))
	if err != nil {
		t.Fatalf("读取客户端状态失败：%v", err)
	}
	if status.AuthorityURL != "https://authority:7878" ||
		status.CertificateFingerprint != "sha256:0000000000000000000000000000000000000000000000000000000000000000" ||
		status.Profile != "团队" || status.ServerCount != 1 ||
		!status.PulledAt.Equal(time.Unix(120, 0)) || status.SnapshotRevision != 42 ||
		status.RecommendedVersion != "1.2.0" {
		t.Fatalf("客户端状态不符：%+v", status)
	}
}

func TestSwitchClientInstanceRejectsIncompleteAndWritesAudit(t *testing.T) {
	userDir := t.TempDir()
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	cacheRoot := filepath.Join(userDir, "ssh-manager")

	seedClientPageInstance(t, cacheRoot, dekDir, "")
	seedClientPageInstance(t, cacheRoot, dekDir, "laptop")
	brokenDir := filepath.Join(cacheRoot, "instances", "broken")
	if err := os.MkdirAll(brokenDir, 0o700); err != nil {
		t.Fatal(err)
	}

	incompleteErr := SwitchClientInstance("", "broken", WebUISource("127.0.0.1"))
	if incompleteErr == nil || !strings.Contains(incompleteErr.Error(), "快照") {
		t.Fatalf("材料不齐全应拒绝切换，实际错误：%v", incompleteErr)
	}
	if _, statErr := os.Stat(filepath.Join(brokenDir, "cache-audit.log")); !os.IsNotExist(statErr) {
		t.Fatal("拒绝切换时不应写实例审计日志")
	}

	if err := SwitchClientInstance("startup", "laptop", WebUISource("127.0.0.1")); err != nil {
		t.Fatalf("切换到材料齐全实例失败：%v", err)
	}
	data, err := os.ReadFile(filepath.Join(cacheRoot, "instances", "laptop", "cache-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("实例审计日志应是 JSON 每行一条：%s", data)
	}
	if row["action"] != "instance.switch" || row["source"] != store.AuditSourceWebUI ||
		row["source_ip"] != "127.0.0.1" || row["status"] != "ok" {
		t.Fatalf("实例切换审计行不符：%s", data)
	}
	if command, _ := row["command"].(string); !strings.Contains(command, `"from_instance":"startup"`) ||
		!strings.Contains(command, `"to_instance":"laptop"`) {
		t.Fatalf("实例切换审计摘要应记录原实例与目标实例：%s", data)
	}
}

func TestSyncClientUsesPinnedHTTPSAndWritesAudit(t *testing.T) {
	cacheDir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "cache-dek.key")
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	t.Setenv("SSHMGR_CACHE_DEK", keyPath)
	seedReadableClientSnapshot(t, cacheDir, keyPath)

	var request ClientPullRequest
	status, err := SyncClient("", WebUISource("127.0.0.1"), func(got ClientPullRequest) error {
		request = got
		return nil
	})
	if err != nil {
		t.Fatalf("立即同步失败：%v", err)
	}
	if request.Instance != "" || request.URL != "https://authority:7878" ||
		request.Token != "device-code" || request.Timeout != 10*time.Second {
		t.Fatalf("拉取请求不符：%+v", request)
	}
	if status.SnapshotRevision != 42 || status.ServerCount != 1 {
		t.Fatalf("同步后应重新读取快照状态：%+v", status)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "cache-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("实例审计日志应是 JSON 每行一条：%s", data)
	}
	if row["action"] != "cache.pull" || row["source"] != store.AuditSourceWebUI ||
		row["source_ip"] != "127.0.0.1" || row["status"] != "ok" {
		t.Fatalf("立即同步审计行不符：%s", data)
	}
	if command, _ := row["command"].(string); !strings.Contains(command, `"snapshot_revision":42`) {
		t.Fatalf("立即同步审计摘要应记录新快照修订号：%s", data)
	}
}

func TestSyncClientClassifiesFailuresAndWritesAudit(t *testing.T) {
	cacheDir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "cache-dek.key")
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	t.Setenv("SSHMGR_CACHE_DEK", keyPath)
	seedReadableClientSnapshot(t, cacheDir, keyPath)

	cases := []struct {
		name string
		err  error
		kind ClientSyncErrorKind
	}{
		{
			name: "无法连接",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			kind: ClientSyncErrorUnreachable,
		},
		{
			name: "设备码被撤销",
			err:  errors.New("cache was QUARANTINED by server rejection"),
			kind: ClientSyncErrorDeviceCodeRevoked,
		},
		{
			name: "证书公钥指纹不符",
			err:  errors.New("server fingerprint mismatch (expected sha256:aa, got sha256:bb)"),
			kind: ClientSyncErrorCertificateFingerprint,
		},
	}
	for _, testCase := range cases {
		_, err := SyncClient("", WebUISource("127.0.0.1"), func(ClientPullRequest) error {
			return testCase.err
		})
		var syncErr *ClientSyncError
		if !errors.As(err, &syncErr) {
			t.Fatalf("%s 应返回立即同步错误，实际：%v", testCase.name, err)
		}
		if syncErr.Kind != testCase.kind {
			t.Fatalf("%s 错误类别 = %s，应为 %s", testCase.name, syncErr.Kind, testCase.kind)
		}
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "cache-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("失败审计行数量 = %d，应为 %d：%s", len(lines), len(cases), data)
	}
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("实例审计日志应是 JSON 每行一条：%s", data)
		}
		if row["action"] != "cache.pull" || row["status"] != "error" || row["source"] != store.AuditSourceWebUI {
			t.Fatalf("失败同步审计行不符：%s", line)
		}
	}
}

// seedClientPageInstance 写出材料齐全的实例目录，不要求快照内容可解密。
func seedClientPageInstance(t *testing.T, cacheRoot, dekDir, instance string) {
	t.Helper()
	directory := filepath.Join(cacheRoot, instance)
	if instance != "" {
		directory = filepath.Join(cacheRoot, "instances", instance)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := `{"url":"https://authority","token":"device-code","pin":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`
	for name, content := range map[string]string{
		"cache.auth.json": credential,
		"cache.bin":       "snapshot",
		"cache.meta.json": `{"pulled_at":100}`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keyName := "cache-dek.key"
	if instance != "" {
		keyName = "cache-dek-" + instance + ".key"
	}
	if err := os.WriteFile(filepath.Join(dekDir, keyName), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedReadableClientSnapshot 写出可解密的默认实例快照，供状态与立即同步测试使用。
func seedReadableClientSnapshot(t *testing.T, cacheDir, keyPath string) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &store.Snapshot{
		Version: 1, SnapshotRevision: 42, RecommendedVersion: "1.2.0",
		Servers: []store.SnapshotServer{{ID: "one", Name: "一台服务器"}},
	}
	plain, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := vaultio.EncryptWithKey(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"cache.auth.json": []byte(`{"url":"https://authority:7878","token":"device-code","pin":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`),
		"cache.bin":       encrypted,
		"cache.meta.json": []byte(`{"url":"https://authority:7878","pulled_at":120,"scoped":true}`),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cacheDir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
}
