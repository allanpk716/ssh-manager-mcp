package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
)

// recordingMountSyncer 是测试注入的挂载前快照保鲜替身。hook 模拟同步器的
// 两个外部效果：返回权威端修订号，以及让统一换快照入口拿到新快照。
type recordingMountSyncer struct {
	mu    sync.Mutex
	calls int
	hook  func() (int64, error)
}

// TestMountShareClientUnreachableAuthorityKeepsMounting：客户端连不上权威
// 端时不挡住挂载，继续用本地快照执行；挂载失败时才附「快照可能过期」的
// 排查提示。
func TestMountShareClientUnreachableAuthorityKeepsMounting(t *testing.T) {
	healthy := newMountShareClientFixture(t)
	healthy.syncer.setHook(func() (int64, error) {
		return 0, errors.New("authority unreachable")
	})

	output, texts, isError := callClientMountShare(t, healthy.session, map[string]any{
		"share": "datasets", "server_id": healthy.targetID,
	})
	if isError {
		t.Fatalf("权威端不可达不应挡住挂载：%s", strings.Join(texts, "\n"))
	}
	if output["version"] != float64(1) {
		t.Fatalf("应继续使用本地快照（版本 1）挂载，实际输出：%#v", output)
	}
	_, _, credential, _ := healthy.fake.recorded()
	if credential != "username=mount-user\npassword="+mountShareTestPassword+"\n" {
		t.Fatalf("本地快照挂载的凭据文件 = %q", credential)
	}
	if strings.Contains(strings.Join(texts, "\n"), "快照可能过期") {
		t.Fatalf("挂载成功时不应附加过期提示：%s", strings.Join(texts, "\n"))
	}

	failing := newMountShareClientFixture(t, func(setup *mountShareFixtureSetup) {
		setup.missingMountCIFS = true
	})
	failing.syncer.setHook(func() (int64, error) {
		return 0, errors.New("authority unreachable")
	})
	_, texts, isError = callClientMountShare(t, failing.session, map[string]any{
		"share": "datasets", "server_id": failing.targetID,
	})
	if !isError {
		t.Fatalf("缺少 mount.cifs 时挂载应失败：%v", texts)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "缺少 mount.cifs") {
		t.Fatalf("应保留原有失败原因：%s", joined)
	}
	if !strings.Contains(joined, "快照可能过期") {
		t.Fatalf("权威端不可达且挂载失败时应附「快照可能过期」：%s", joined)
	}
}

func (s *recordingMountSyncer) SyncBeforeMount(context.Context) (int64, error) {
	s.mu.Lock()
	s.calls++
	hook := s.hook
	s.mu.Unlock()
	if hook == nil {
		return 0, nil
	}
	return hook()
}

func (s *recordingMountSyncer) setHook(hook func() (int64, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *recordingMountSyncer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type mountShareClientFixture struct {
	mountShareSeed
	session   *mcp.ClientSession
	syncer    *recordingMountSyncer
	offerSync *atomic.Bool
	auditPath string
}

func newMountShareClientFixture(t *testing.T, options ...func(*mountShareFixtureSetup)) mountShareClientFixture {
	t.Helper()
	setup := mountShareFixtureSetup{sudo: true}
	for _, option := range options {
		option(&setup)
	}
	fake := &fakeLinuxMountServer{missingMountCIFS: setup.missingMountCIFS, workingVersion: setup.workingVersion}
	seed := seedMountShareVault(t, setup, fake)
	_, projectToken, err := seed.store.AddProject("共享挂载项目", seed.profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := seed.store.ExportSnapshotForProfile(seed.profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if setup.oldAuthoritySnapshot {
		snapshot.RecommendedVersion = ""
	}
	offerSync := &atomic.Bool{}
	reload := func() (*store.Snapshot, bool, error) {
		if !offerSync.Load() {
			return nil, false, nil
		}
		fresh, err := seed.store.ExportSnapshotForProfile(seed.profileID, true, "")
		if err != nil {
			return nil, false, err
		}
		return fresh, true, nil
	}
	syncer := &recordingMountSyncer{}
	auditPath := filepath.Join(t.TempDir(), "mount-share-audit.jsonl")
	broker, tunnels, tasks, _, cleanup, err := NewCacheBrokerWithRefresher(
		projectToken, snapshot, auditPath, reload, nil, "laptop", nil, syncer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	return mountShareClientFixture{
		mountShareSeed: seed, session: connectListSharesClient(t, broker),
		syncer: syncer, offerSync: offerSync, auditPath: auditPath,
	}
}

// TestMountShareClientOldAuthoritySnapshotRefuses：客户端快照没有推荐版本
// 字段（旧权威端下发）时，mount_share 与 list_shares 一样直接报错，不做挂
// 载，也不先做快照保鲜。
func TestMountShareClientOldAuthoritySnapshotRefuses(t *testing.T) {
	fixture := newMountShareClientFixture(t, func(setup *mountShareFixtureSetup) {
		setup.oldAuthoritySnapshot = true
	})
	fixture.syncer.setHook(func() (int64, error) {
		t.Error("旧权威端快照应在快照保鲜之前拒绝，不应查询权威端")
		return 0, nil
	})

	output, texts, isError := callClientMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError {
		t.Fatalf("旧权威端快照应报错，实际返回：%#v", output)
	}
	want := "权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端"
	if strings.Join(texts, "\n") != want {
		t.Fatalf("报错文本 = %q，期待 %q", strings.Join(texts, "\n"), want)
	}
	if got := fixture.syncer.count(); got != 0 {
		t.Fatalf("旧权威端快照不应触发快照保鲜，调用次数 = %d", got)
	}
	if commands, _, _, _ := fixture.fake.recorded(); len(commands) != 0 {
		t.Fatalf("旧权威端快照不应连上目标服务器：%v", commands)
	}
}

// TestMountShareSkipsSnapshotSyncOnWritableStore：单机与权威端的存储可写，
// 挂载前没有快照保鲜这一步。
func TestMountShareSkipsSnapshotSyncOnWritableStore(t *testing.T) {
	fixture := newMountShareFixture(t)
	syncer := &recordingMountSyncer{}
	syncer.setHook(func() (int64, error) {
		t.Error("可写存储不应执行挂载前快照保鲜")
		return 0, nil
	})

	output, err := MountShareForProfile(
		context.Background(), fixture.store, "project-test", fixture.profileID,
		"datasets", fixture.targetID, nil, syncer, func() *store.Store { return fixture.store },
	)
	if err != nil {
		t.Fatalf("单机挂载失败：%v", err)
	}
	if output.Version != 1 {
		t.Fatalf("单机挂载输出 = %#v", output)
	}
	if got := syncer.count(); got != 0 {
		t.Fatalf("单机与权威端不应执行挂载前快照保鲜，调用次数 = %d", got)
	}
}

// callClientMountShare 解析客户端面上带会话提示的 mount_share 结果：提示
// 是独立的文本段，最后一段才是工具输出本身。
func callClientMountShare(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (map[string]any, []string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "mount_share", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, textContent.Text)
		}
	}
	output := map[string]any{}
	if !result.IsError {
		if len(texts) == 0 {
			t.Fatalf("mount_share 结果没有文本：%+v", result)
		}
		if err := json.Unmarshal([]byte(texts[len(texts)-1]), &output); err != nil {
			t.Fatalf("解析 mount_share 输出失败：%v\n%s", err, strings.Join(texts, "\n"))
		}
	}
	return output, texts, result.IsError
}

// TestMountShareClientSyncsStaleSnapshotBeforeMount：客户端本地快照落后于
// 权威端时，挂载前先同步，再用同步后的登记与挂载密码执行挂载。
func TestMountShareClientSyncsStaleSnapshotBeforeMount(t *testing.T) {
	fixture := newMountShareClientFixture(t)
	if err := fixture.store.SetSecretValue(
		"DATASETS_MOUNT_PASSWORD", []byte("rotated-mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	fixture.syncer.setHook(func() (int64, error) {
		revision, err := fixture.store.SnapshotRevision()
		if err != nil {
			return 0, err
		}
		fixture.offerSync.Store(true)
		return revision, nil
	})

	output, texts, isError := callClientMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("快照落后时应先同步再挂载：%s", strings.Join(texts, "\n"))
	}
	if output["version"] != float64(2) {
		t.Fatalf("挂载应使用同步后的共享版本 2，实际输出：%#v", output)
	}
	if got := fixture.syncer.count(); got != 1 {
		t.Fatalf("挂载前快照保鲜调用次数 = %d，应为 1", got)
	}
	_, _, credential, _ := fixture.fake.recorded()
	wantCredential := "username=mount-user\npassword=rotated-mount-password\n"
	if credential != wantCredential {
		t.Fatalf("目标服务器凭据文件 = %q，期待 %q", credential, wantCredential)
	}
	if strings.Contains(credential, mountShareTestPassword) {
		t.Fatalf("挂载不应继续使用同步前的旧密码：%q", credential)
	}
	auditText, err := os.ReadFile(fixture.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(auditText)), "\n") {
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("解析客户端审计行失败：%v（%s）", err, line)
		}
		if row["action"] != "mount-share" || row["status"] != "ok" {
			t.Fatalf("客户端 mount-share 审计行 = %#v", row)
		}
		var summary map[string]any
		if err := json.Unmarshal([]byte(row["command"].(string)), &summary); err != nil {
			t.Fatal(err)
		}
		if summary["version"] != float64(2) {
			t.Fatalf("客户端审计应记录同步后的版本 2：%#v", summary)
		}
		if strings.Contains(line, "rotated-mount-password") || strings.Contains(line, mountShareTestPassword) ||
			strings.Contains(line, "192.0.2.10") {
			t.Fatalf("客户端审计泄漏密码或地址：%s", line)
		}
	}
}
