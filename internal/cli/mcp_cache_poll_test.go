package cli

// 快照修订号后台轮询的主要端到端测试入口:在「客户端缓存加锚定转发」
// 测试环境上验证后台轮询。本文件覆盖:
//   - 共享缓存被别的桥更新后,本桥经由现有统一换快照入口立即热加载;
//   - 后台轮询循环(短间隔注入):修订号不变只查询不下载、变化才拉取;
//   - 两个桥指向同一实例目录:一个拉取,另一个不下载直接热加载;
//   - 权威端停掉后工具调用照常工作;旧权威端返回 404 后停止轮询。

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// waitForCount 在截止时间前等待 fn() 至少达到 want,失败时打印 settle 信息。
// 后台轮询是异步的:断言只看外部可见结果(请求计数、缓存文件、工具结果),
// 不检查循环内部状态。
func waitForCount(t *testing.T, what string, want int, fn func() int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := fn(); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 达到 %d 超时,当前 %d", what, want, fn())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitStable 等待 fn() 在整个窗口内保持不变,用于断言「不再下载」这类
// 负向性质:轮询循环持续运行而计数不动。
func waitStable(t *testing.T, what string, fn func() int) int {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	got := fn()
	for {
		time.Sleep(25 * time.Millisecond)
		now := fn()
		if now != got {
			if time.Now().After(deadline) {
				t.Fatalf("%s 在观察窗口内仍在变化: %d -> %d", what, got, now)
			}
			got = now
			continue
		}
		if time.Now().After(deadline) {
			return got
		}
	}
}

// waitForTrue 在截止时间前等待条件成立(后台轮询里「拉取完成」与「统一入口
// 换上新快照完成」是两步,断言只等最终可见状态,不看中间时序)。
func waitForTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// setUpPollableCache 把端到端测试环境接到一个临时实例目录:写入拉取凭据、完成
// 初次全量拉取,并把缓存解密密钥指到进程内的内存密钥提供器(与
// mcp_cache_revision_test.go 相同的隔离方式)。
func setUpPollableCache(t *testing.T, fx *cacheFwdFixture) string {
	t.Helper()
	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	mem := &store.MemKeyProvider{}
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	prevDek := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prevDek })
	if err := clientops.WriteCacheCred(&fx.cred); err != nil {
		t.Fatal(err)
	}
	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{}); err != nil {
		t.Fatalf("初次全量拉取: %v", err)
	}
	return cacheDir
}

// newBrokerWithRefresher 按 `mcp --cache` 的装配方式构建桥,并额外交出统一
// 换快照入口的触发器(后台轮询循环用它安装新快照)。
func (fx *cacheFwdFixture) newBrokerWithRefresher(t *testing.T, snap *store.Snapshot, reload func() (*store.Snapshot, bool, error)) (*mcp.ClientSession, mcpserver.CacheRefresher) {
	t.Helper()
	srv, tunnels, tasks, refresher, cleanup, err := mcpserver.NewCacheBrokerWithRefresher(
		fx.projToken, snap, filepath.Join(t.TempDir(), "audit.log"), reload,
		clientops.ForwardingHostKeys(mustForwarder(t, fx.cred)), "laptop", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	return attachMCPClient(t, srv), refresher
}

// startTestPoller 用注入的短间隔启动快照修订号后台轮询循环(生产默认 5 分钟,
// 测试不真等),日志丢弃以保持测试输出干净。
func startTestPoller(t *testing.T, interval time.Duration, refresher mcpserver.CacheRefresher) {
	t.Helper()
	stop := clientops.StartSnapshotPoller(clientops.SnapshotPollerConfig{
		Interval:  interval,
		Refresher: refresher,
		LogOutput: io.Discard,
	})
	t.Cleanup(stop)
}

// TestMCPCache_InvalidSnapshotPollIntervalFallsBack — 桥启动时发现轮询间隔
// 非法,只写一行英文告警并使用默认 5 分钟;桥的后续装配继续执行。
func TestMCPCache_InvalidSnapshotPollIntervalFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prepare      func(t *testing.T, dir string)
		wantFeedback string
	}{
		{
			name: "环境变量",
			prepare: func(t *testing.T, _ string) {
				t.Setenv("SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL", "30s")
			},
			wantFeedback: `sshmgr: invalid SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL "30s"`,
		},
		{
			name: "实例配置文件",
			prepare: func(t *testing.T, dir string) {
				blob := []byte(`{"snapshot_poll_interval":"30s"}`)
				if err := os.WriteFile(filepath.Join(dir, "cache.config.json"), blob, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantFeedback: `sshmgr: invalid snapshot_poll_interval in cache.config.json "30s"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCacheFwdFixture(t)
			dir := setUpPollableCache(t, fx)
			t.Setenv("SSHMGR_TOKEN", fx.projToken)
			t.Setenv("SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL", "")
			tc.prepare(t, dir)

			// 让审计文件路径无法打开:如果间隔解析已经放行,命令会走到桥装配
			// 阶段,再因这个路径返回确定错误,而不是因为轮询间隔拒绝启动。
			auditPath := filepath.Join(dir, "cache-audit.log")
			if err := os.MkdirAll(auditPath, 0o700); err != nil {
				t.Fatal(err)
			}

			cmd := newMCPCmd()
			cmd.SetArgs([]string{"--cache"})
			var stderr bytes.Buffer
			cmd.SetOut(&stderr)
			cmd.SetErr(&stderr)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("审计路径是目录时,命令最终应返回打开失败")
			}
			if strings.Contains(err.Error(), "snapshot poll interval") {
				t.Fatalf("非法轮询间隔不应让桥拒绝启动,实际错误: %v", err)
			}
			warning := stderr.String()
			if !strings.HasPrefix(warning, tc.wantFeedback) {
				t.Fatalf("标准错误流应先出现一行来源与取值都明确的告警,实际:\n%s", warning)
			}
			if !strings.Contains(warning, "using the default 5m snapshot poll interval") {
				t.Fatalf("告警应说明已改用默认 5 分钟,实际:\n%s", warning)
			}
		})
	}
}

// TestSnapshotPollE2E_SharedCacheHotReloadThroughUnifiedEntry — 同一台机器上
// 另一个桥把共享缓存更新到新修订号后,本桥的后台轮询经由现有统一换快照入口
// 立即安装新快照:不需要工具调用触发,换上后下一次工具调用就看到新服务器。
func TestSnapshotPollE2E_SharedCacheHotReloadThroughUnifiedEntry(t *testing.T) {
	fx := newCacheFwdFixture(t)
	setUpPollableCache(t, fx)

	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rel := clientops.NewCacheReloader(30 * time.Minute)
	_, refresher := fx.newBrokerWithRefresher(t, snap, rel.Check)
	firstRevision, err := fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if got := refresher.CurrentRevision(); got != firstRevision {
		t.Fatalf("启动后内存快照修订号 = %d,应为 %d", got, firstRevision)
	}

	// 模拟同一实例目录上的另一个桥:权威端改配置后,它完成全量拉取。
	if _, err := fx.serve.AddServer(&models.Server{Name: "poll-box", Host: "192.0.2.60", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{}); err != nil {
		t.Fatalf("另一个桥的全量拉取: %v", err)
	}
	if got := refresher.CurrentRevision(); got != firstRevision {
		t.Fatalf("尚未热加载时内存修订号 = %d,应保持 %d", got, firstRevision)
	}

	// 经统一换快照入口安装新快照:不再发起全量请求,内存修订号立刻前进。
	refresher.ReloadNow()
	secondRevision, err := fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if got := refresher.CurrentRevision(); got != secondRevision {
		t.Fatalf("热加载后内存修订号 = %d,应为 %d", got, secondRevision)
	}
	if got := fx.snapshotCount(); got != 2 {
		t.Fatalf("热加载本身不得发起全量快照请求,请求次数 = %d,应为 2", got)
	}
}

// TestSnapshotPollE2E_BackgroundPollPullsOnlyOnChange — 短间隔注入下定时轮询
// 生效:修订号不变时只查询修订号、不发全量快照请求;权威端改配置后恰好拉取
// 一次,并经统一入口换上新快照。
func TestSnapshotPollE2E_BackgroundPollPullsOnlyOnChange(t *testing.T) {
	fx := newCacheFwdFixture(t)
	setUpPollableCache(t, fx)

	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rel := clientops.NewCacheReloader(30 * time.Minute)
	sess, refresher := fx.newBrokerWithRefresher(t, snap, rel.Check)
	startTestPoller(t, 20*time.Millisecond, refresher)

	// 修订号不变:至少查过 3 轮修订号,全量快照请求仍是初次那一次。
	waitForCount(t, "修订号查询", 3, fx.versionCount)
	if got := waitStable(t, "全量快照请求", fx.snapshotCount); got != 1 {
		t.Fatalf("修订号未变化时的全量快照请求 = %d,应为 1", got)
	}
	beforeRevision := refresher.CurrentRevision()

	// 权威端写入配置:轮询发现修订号变化,全量拉取一次并热加载。
	boxID, err := fx.serve.AddServer(&models.Server{Name: "poll-box", Host: "192.0.2.60", Port: 22, User: "u", AuthMethod: models.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.GrantServers(fx.profID, []string{boxID}); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, "变化后的全量快照请求", 2, fx.snapshotCount)
	if got := waitStable(t, "变化后的全量快照请求", fx.snapshotCount); got != 2 {
		t.Fatalf("修订号变化后应恰好多一次全量拉取,当前 = %d,应为 2", got)
	}
	wantRevision, err := fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	waitForTrue(t, "轮询热加载后的内存修订号", func() bool { return refresher.CurrentRevision() == wantRevision })
	loaded, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SnapshotRevision != wantRevision {
		t.Fatalf("缓存文件修订号 = %d,应为 %d", loaded.SnapshotRevision, wantRevision)
	}
	texts := callToolTexts(t, sess, "list_servers", map[string]any{})
	wantHint := fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：新增服务器 poll-box",
		beforeRevision, wantRevision,
	)
	if len(texts) != 2 || texts[0] != wantHint || !strings.Contains(texts[1], "poll-box") {
		t.Fatalf("后台轮询换上新快照后,工具结果应先携带变更提示,实际为 %#v,应为 %q", texts, wantHint)
	}
	if again := callToolTexts(t, sess, "list_servers", map[string]any{}); len(again) != 1 || strings.Contains(again[0], "权威端配置已更新") {
		t.Fatalf("同一会话的变更提示只应出现一次,实际为 %#v", again)
	}
}

// TestSnapshotPollE2E_TwoBridgesSharedCache — 两个桥先在同一实例目录上启动,
// 权威端改配置后桥 A 完成全量拉取;等共享缓存到达新修订号后再启动桥 B 的
// 轮询,它只查修订号、不再下载,直接从共享缓存热加载;两个会话各自看到一
// 次变更提示。轮询实现允许两个桥同时发现旧缓存并各自原子拉取,因此这里不
// 把桥 B 的第一轮放在桥 A 下载期间。
func TestSnapshotPollE2E_TwoBridgesSharedCache(t *testing.T) {
	fx := newCacheFwdFixture(t)
	setUpPollableCache(t, fx)

	snapshot, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	relA := clientops.NewCacheReloader(30 * time.Minute)
	sessA, refresherA := fx.newBrokerWithRefresher(t, snapshot, relA.Check)
	relB := clientops.NewCacheReloader(30 * time.Minute)
	sessB, refresherB := fx.newBrokerWithRefresher(t, snapshot, relB.Check)
	for _, sess := range []*mcp.ClientSession{sessA, sessB} {
		if texts := callToolTexts(t, sess, "list_servers", map[string]any{}); len(texts) != 1 {
			t.Fatalf("配置变化前不应有变更提示,实际为 %#v", texts)
		}
	}
	startTestPoller(t, 20*time.Millisecond, refresherA)
	beforeVersions := fx.versionCount()
	beforeRevision := refresherA.CurrentRevision()

	// 权威端写入配置,桥 A 的轮询先完成全量拉取。
	boxID, err := fx.serve.AddServer(&models.Server{Name: "shared-box", Host: "192.0.2.61", Port: 22, User: "u", AuthMethod: models.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.GrantServers(fx.profID, []string{boxID}); err != nil {
		t.Fatal(err)
	}
	wantRevision, err := fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	waitForCount(t, "桥 A 的全量快照请求", 2, fx.snapshotCount)
	waitForTrue(t, "桥 A 热加载后的修订号", func() bool { return refresherA.CurrentRevision() == wantRevision })
	waitForCount(t, "桥 A 的启动修订号查询", beforeVersions+1, fx.versionCount)
	startTestPoller(t, 20*time.Millisecond, refresherB)
	waitForTrue(t, "桥 B 从共享缓存热加载后的修订号", func() bool { return refresherB.CurrentRevision() == wantRevision })
	if got := refresherB.CurrentRevision(); got != wantRevision {
		t.Fatalf("桥 B 热加载后的修订号 = %d,应为 %d(不得再下载)", got, wantRevision)
	}
	if got := waitStable(t, "全量快照请求", fx.snapshotCount); got != 2 {
		t.Fatalf("两个桥共存时全量快照请求 = %d,应为 2(只有一个桥下载)", got)
	}
	wantHint := fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：新增服务器 shared-box",
		beforeRevision, wantRevision,
	)
	for name, sess := range map[string]*mcp.ClientSession{"A": sessA, "B": sessB} {
		texts := callToolTexts(t, sess, "list_servers", map[string]any{})
		if len(texts) != 2 || texts[0] != wantHint || !strings.Contains(texts[1], "shared-box") {
			t.Fatalf("桥 %s 应先看到一次变更提示再看到新快照,实际为 %#v,应为 %q", name, texts, wantHint)
		}
		if again := callToolTexts(t, sess, "list_servers", map[string]any{}); len(again) != 1 || strings.Contains(again[0], "权威端配置已更新") {
			t.Fatalf("桥 %s 的同一会话不应重复看到变更提示,实际为 %#v", name, again)
		}
	}
}

// TestSnapshotPollE2E_AuthorityDownToolsStillWork — 权威端停掉后:轮询失败只记
// 日志,下一轮再试,桥继续用现有快照服务工具调用。
func TestSnapshotPollE2E_AuthorityDownToolsStillWork(t *testing.T) {
	fx := newCacheFwdFixture(t)
	setUpPollableCache(t, fx)

	// 权威端在线时先用一个临时桥执行一次:未知主机密钥经锚定转发落在权威端
	// (这次写入会让修订号 +1),随后把缓存拉平到该修订号。之后的断言就不再
	// 与「锚定写入触发再次拉取」的真实行为相互竞争。
	pinSnap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	pinRel := clientops.NewCacheReloader(30 * time.Minute)
	pinSess, _ := fx.newBrokerWithRefresher(t, pinSnap, pinRel.Check)
	if text, isErr := callExec(t, pinSess, fx.targetID, "ping"); isErr || !strings.Contains(text, "PONG:ping") {
		t.Fatalf("锚定阶段的工具调用应成功: %q (isError=%v)", text, isErr)
	}
	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{}); err != nil {
		t.Fatalf("锚定后的全量拉取: %v", err)
	}

	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rel := clientops.NewCacheReloader(30 * time.Minute)
	sess, refresher := fx.newBrokerWithRefresher(t, snap, rel.Check)
	startTestPoller(t, 15*time.Millisecond, refresher)
	waitForCount(t, "停机前的修订号查询", 1, fx.versionCount)
	revisionBeforeOutage, err := fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	// 权威端停掉:轮询会持续失败(连接被拒,请求到不了计数器),桥不受影响。
	fx.serveHTTP.Close()
	time.Sleep(100 * time.Millisecond)
	if got := waitStable(t, "全量快照请求", fx.snapshotCount); got != 2 {
		t.Fatalf("权威端不可达时不得发起全量快照请求,当前 = %d,应为 2(初次拉取与锚定后拉平各一次)", got)
	}
	if got := refresher.CurrentRevision(); got != revisionBeforeOutage {
		t.Fatalf("权威端停掉后内存修订号 = %d,应保持 %d", got, revisionBeforeOutage)
	}
	text, isErr := callExec(t, sess, fx.targetID, "ping")
	if isErr || !strings.Contains(text, "PONG:ping") {
		t.Fatalf("权威端停掉后工具调用应照常工作: %q (isError=%v)", text, isErr)
	}
}

// TestSnapshotPollE2E_OldAuthorityStopsPolling — 旧权威端的修订号接口返回 404:
// 这个桥停止轮询,不再反复查询。
func TestSnapshotPollE2E_OldAuthorityStopsPolling(t *testing.T) {
	fx := newCacheFwdFixture(t)
	setUpPollableCache(t, fx)
	fx.versionNotFound.Store(true)

	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rel := clientops.NewCacheReloader(30 * time.Minute)
	_, refresher := fx.newBrokerWithRefresher(t, snap, rel.Check)
	startTestPoller(t, 15*time.Millisecond, refresher)

	// 首轮查询得到 404 后停止:观察窗口内修订号查询次数不再增长。
	waitForCount(t, "旧权威端的修订号查询", 1, fx.versionCount)
	got := waitStable(t, "旧权威端的修订号查询", fx.versionCount)
	if got != 1 {
		t.Fatalf("404 后应停止轮询,修订号查询次数 = %d,应为 1", got)
	}
	if got := waitStable(t, "全量快照请求", fx.snapshotCount); got != 1 {
		t.Fatalf("停止轮询后不应有轮询引发的全量拉取,当前 = %d,应为 1", got)
	}
}
