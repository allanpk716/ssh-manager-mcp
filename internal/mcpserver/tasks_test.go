package mcpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// newTestTM 白盒构造 TaskManager: 指定 max 上限, runCap/retain 各 1h。
// 不启动 sweeper——测试直接驱动 SweepExpired (照 tunnels.go 测试形态,
// 也避免真实 ticker 与可覆写时钟 m.now 的数据竞争)。
func newTestTM(t *testing.T, max int) *TaskManager {
	t.Helper()
	m, err := newTaskManagerForTest(max, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// newTaskManagerForTest 绕过 env seam 直接构造 (不启动 sweeper)。
func newTaskManagerForTest(maxTasks int, runCap, retain time.Duration) (*TaskManager, error) {
	return &TaskManager{
		tasks:    map[string]*bgTask{},
		runCap:   runCap,
		retain:   retain,
		maxTasks: maxTasks,
		now:      time.Now,
		quit:     make(chan struct{}),
	}, nil
}

// finishedSpec 白盒终态 spec: PreFinished=true → Insert 直接以终态落表,
// 不 spawn Run goroutine。i 仅用于使命令可区分。
func finishedSpec(i int) *BgTaskSpec {
	return &BgTaskSpec{
		ProjectID:   "proj",
		ServerID:    "srv",
		Command:     "echo " + strconv.Itoa(i),
		Timeout:     time.Minute,
		PreFinished: true,
	}
}

// runningSpec 常规 spec: Run 留空 → Insert 默认 no-op (T3 白盒不依赖 ssh),
// 状态恒 running (无人转终态)。
func runningSpec() *BgTaskSpec {
	return &BgTaskSpec{
		ProjectID: "proj",
		ServerID:  "srv",
		Command:   "sleep 60",
		Timeout:   time.Minute,
	}
}

func TestAdmissionCapAndEviction(t *testing.T) {
	m := newTestTM(t, 3)
	// 三个终态任务占满; finishedAt 显式错峰 (i=0 最旧)——驱逐受害者确定性,
	// 不赌 uuid 平局的 smallest-id tie-break (helper 插入时三任务同 now)。
	ids := make([]string, 3)
	base := time.Now()
	for i := 0; i < 3; i++ {
		id, err := m.Insert(finishedSpec(i)) // helper: 直接以终态插入(白盒置 status/finishedAt)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		m.mu.Lock()
		m.tasks[id].finishedAt = base.Add(time.Duration(i) * time.Minute)
		m.mu.Unlock()
	}
	// lookup 是本任务产出 seam (T5/6/7 内部消费), 白盒即刻验证
	if _, ok := m.lookup(ids[0]); !ok {
		t.Fatal("lookup must find inserted task")
	}
	// 满员驱逐: Reserve 成功(驱逐最旧终态) → Insert 成功
	if err := m.Reserve(); err != nil {
		t.Fatalf("reserve with evictable: %v", err)
	}
	// 受害者身份: 最旧终态 (ids[0]) 已逐出表, 最新终态 (ids[2]) 幸存。
	if _, ok := m.lookup(ids[0]); ok {
		t.Fatal("oldest terminal task must be the eviction victim")
	}
	if _, ok := m.lookup(ids[2]); !ok {
		t.Fatal("newest terminal task must survive eviction")
	}
	if _, err := m.Insert(runningSpec()); err != nil {
		t.Fatal(err)
	}
	// 现剩 2 终态(最旧被逐) + 1 running; 全 running 才拒绝:
	m2 := newTestTM(t, 1)
	if _, err := m2.Insert(runningSpec()); err != nil {
		t.Fatal(err)
	}
	if err := m2.Reserve(); err == nil {
		t.Fatal("all-running full should refuse")
	}
}

func TestAdmissionConcurrentStartsBounded(t *testing.T) {
	m := newTestTM(t, 8)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ { // 64 并发启动压 8 上限
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Reserve(); err != nil {
				errs <- err
				return
			}
			// 模拟锁外慢 Connect
			if _, err := m.Insert(runningSpec()); err != nil {
				m.ReleaseReservation()
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	if m.Len() > 8 {
		t.Fatalf("admission breached: %d", m.Len())
	}
}

func TestReserveAfterCloseAllRefused(t *testing.T) {
	m := newTestTM(t, 4)
	m.CloseAll()
	if err := m.Reserve(); err == nil {
		t.Fatal("closed manager must refuse Reserve")
	}
}

func TestSweeperDeletesExpiredNotRunning(t *testing.T) {
	m := newTestTM(t, 8)
	id, _ := m.Insert(finishedSpec(0))
	m.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // 时钟越过 retain
	if got := m.SweepExpired(); len(got) != 1 || got[0] != id {
		t.Fatalf("sweep=%v", got)
	}
	rid, _ := m.Insert(runningSpec())
	m.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if got := m.SweepExpired(); len(got) != 0 { // running 永不删
		t.Fatalf("running swept: %v", got)
	}
	_ = rid
}

// TestEnvSeamValidation: env seam 三态全表 (spec §7): (a) 非法值拒绝启动
// (fail-closed); (b) 合法覆写真实生效 (NewTaskManager 返回的 manager 字段
// 白盒校验); (c) 全未设回落包级默认 (32 / 24h / 1h)。每行显式写全三键
// ("" = 未设)——t.Setenv 只在测试结束还原, 跨行残留会串扰, 全量覆写保证
// 每行从同一状态出发。
func TestEnvSeamValidation(t *testing.T) {
	cases := []struct {
		name                     string
		maxTasks, runCap, retain string
		wantErr                  bool
		wantMax                  int
		wantRunCap, wantRetain   time.Duration
	}{
		{name: "run cap abc", runCap: "abc", wantErr: true},
		{name: "run cap 0", runCap: "0", wantErr: true},
		{name: "run cap -5s", runCap: "-5s", wantErr: true},
		{name: "max tasks 0", maxTasks: "0", wantErr: true},
		{name: "retain abc", retain: "abc", wantErr: true},
		{name: "retain 0", retain: "0", wantErr: true},
		{name: "retain -5s", retain: "-5s", wantErr: true},
		{name: "max tasks override", maxTasks: "2", wantMax: 2, wantRunCap: bgRunCapDefault, wantRetain: bgRetainDefault},
		{name: "run cap override", runCap: "48h", wantMax: bgMaxTasksDefault, wantRunCap: 48 * time.Hour, wantRetain: bgRetainDefault},
		{name: "retain override", retain: "30m", wantMax: bgMaxTasksDefault, wantRunCap: bgRunCapDefault, wantRetain: 30 * time.Minute},
		{name: "defaults", wantMax: bgMaxTasksDefault, wantRunCap: bgRunCapDefault, wantRetain: bgRetainDefault},
	}
	for _, tc := range cases {
		t.Setenv("SSHMGR_BG_MAX_TASKS", tc.maxTasks)
		t.Setenv("SSHMGR_BG_RUN_CAP", tc.runCap)
		t.Setenv("SSHMGR_BG_RETAIN", tc.retain)
		m, err := NewTaskManager()
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: env (%q,%q,%q) must be refused", tc.name, tc.maxTasks, tc.runCap, tc.retain)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: env (%q,%q,%q): %v", tc.name, tc.maxTasks, tc.runCap, tc.retain, err)
		}
		m.mu.Lock()
		gotMax, gotCap, gotRet := m.maxTasks, m.runCap, m.retain
		m.mu.Unlock()
		if gotMax != tc.wantMax || gotCap != tc.wantRunCap || gotRet != tc.wantRetain {
			t.Fatalf("%s: fields=(%d,%v,%v), want (%d,%v,%v)", tc.name,
				gotMax, gotCap, gotRet, tc.wantMax, tc.wantRunCap, tc.wantRetain)
		}
	}
}

// ---------- Plan 47 T3: ReserveRelay / AuditAction / 双连接槽 (spec §1.1 运行中
// 冲突拒绝 + §2⑧⑨ + §8 ReserveRelay/admission 段) ----------

// relayRSpec 构造 relay 形 spec (Run 留空 → no-op, 任务恒 running; 取值传递
// 是 ReserveRelay 的入参形态, 与 runningSpec 的指针形态区分)。
func relayRSpec(cmd string) BgTaskSpec {
	return BgTaskSpec{
		ProjectID: "proj",
		ServerID:  "srv",
		Command:   cmd,
		Timeout:   time.Minute,
	}
}

// reservedCount 白盒读 admission 计数 (持锁)。
func reservedCount(m *TaskManager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserved
}

// TestReserveRelayConcurrentSameArtifactSetOneWinner: 同工件集真并发启动——
// 恰一胜者、其余全败, 败者文案点名碰撞键并指引 exec_stop; 表内恰一条目
// (无占位残留、计数零泄漏)。
func TestReserveRelayConcurrentSameArtifactSetOneWinner(t *testing.T) {
	m := newTestTM(t, 8)
	const n = 16
	key := relayKeyOf("srvB", "/data/f")
	keys := []string{key}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := m.ReserveRelay(relayRSpec("relay same"), nil, keys); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	if wins := n - len(errs); wins != 1 {
		t.Fatalf("concurrent same-set ReserveRelay: %d winners, want exactly 1", wins)
	}
	for err := range errs {
		// 错误文案对 NUL 键走 %q 转义渲染——断言对齐转义形态。
		if !strings.Contains(err.Error(), strconv.Quote(key)) {
			t.Fatalf("conflict error must name the colliding key %q: %v", key, err)
		}
		if !strings.Contains(err.Error(), "exec_stop") {
			t.Fatalf("conflict error must advise exec_stop: %v", err)
		}
	}
	if got := m.Len(); got != 1 {
		t.Fatalf("Len=%d after one winner, want 1 (no placeholder residue)", got)
	}
	if r := reservedCount(m); r != 0 {
		t.Fatalf("reserved=%d after concurrent runs, want 0", r)
	}
}

// TestReserveRelayDistinctEndpointsNotMutuallyExclusive: 不同 to_server_id 同
// 路径真并行不互斥 (rev4 kimi#1 锚——分发场景反例); 共享 read 键 read∩read 允许;
// `/tmp/a\b` 与 `/tmp/a/b` 是不同键 (本层零 canonical 化——键空间字面求交,
// canonical 化归 T4 参数层 ①)。
func TestReserveRelayDistinctEndpointsNotMutuallyExclusive(t *testing.T) {
	m := newTestTM(t, 8)
	shared := []string{relayKeyOf("srvA", "/src/big.tar")}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, _, err := m.ReserveRelay(relayRSpec("to-B"), shared, []string{relayKeyOf("srvB", "/data/f")}); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if _, _, err := m.ReserveRelay(relayRSpec("to-C"), shared, []string{relayKeyOf("srvC", "/data/f")}); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("distinct to_server_id + shared source must both run: %v", err)
	}
	if got := m.Len(); got != 2 {
		t.Fatalf("Len=%d, want 2 (both admitted)", got)
	}
	// 字面键空间: 反斜杠路径与斜杠路径互不碰撞。
	if _, _, err := m.ReserveRelay(relayRSpec("bs"), nil, []string{relayKeyOf("srvB", `/tmp/a\b`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ReserveRelay(relayRSpec("slash"), nil, []string{relayKeyOf("srvB", "/tmp/a/b")}); err != nil {
		t.Fatalf(`/tmp/a\b and /tmp/a/b are distinct keys, must not collide: %v`, err)
	}
}

// TestReserveRelaySameServerArtifactCollisionRejected: 同 server 工件碰撞拒——
// 第二任务的 to_path 恰为运行中任务 write 集中的 partial 名 (partial 可以是另一
// 任务的合法目标, rev2 codex#4); 写键命中运行中任务 read 键 (write∩read 另一向)。
func TestReserveRelaySameServerArtifactCollisionRejected(t *testing.T) {
	m := newTestTM(t, 8)
	first := []string{
		relayKeyOf("srvB", "/data/f"),
		relayKeyOf("srvB", "/data/f.sshmgr-partial"),
		relayKeyOf("srvB", "/data/f.sshmgr-manifest.json"),
		relayKeyOf("srvB", "/data/f.sshmgr-manifest.json.tmp"),
	}
	if _, _, err := m.ReserveRelay(relayRSpec("first"), []string{relayKeyOf("srvA", "/src")}, first); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.ReserveRelay(relayRSpec("second"), nil, []string{relayKeyOf("srvB", "/data/f.sshmgr-partial")})
	if err == nil {
		t.Fatal("to_path matching a running task's partial must be rejected")
	}
	if !strings.Contains(err.Error(), "exec_stop") {
		t.Fatalf("conflict text must advise exec_stop: %v", err)
	}
	if _, _, err = m.ReserveRelay(relayRSpec("third"), nil, []string{relayKeyOf("srvA", "/src")}); err == nil {
		t.Fatal("write key matching a running task's read key must be rejected")
	}
}

// TestReserveRelayReadWriteConflictRejected: own read ∩ running writes = ∅——
// 源=运行中任务的目标工件拒 (fresh 会删自己的源、非 fresh 会 truncate 正在读的
// partial, rev3 codex#2)。
func TestReserveRelayReadWriteConflictRejected(t *testing.T) {
	m := newTestTM(t, 8)
	key := relayKeyOf("srvB", "/data/f")
	if _, _, err := m.ReserveRelay(relayRSpec("writer"), nil, []string{key}); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.ReserveRelay(relayRSpec("reader"), []string{key}, []string{relayKeyOf("srvC", "/copy")})
	if err == nil {
		t.Fatal("source = running task's target artifact must be rejected")
	}
	if !strings.Contains(err.Error(), strconv.Quote(key)) { // %q 转义渲染对齐
		t.Fatalf("conflict error must name the colliding key %q: %v", key, err)
	}
}

// TestReserveRelayTerminalTaskDoesNotBlock: 四矩阵只对 running 求交——终态任务
// 键仍在表 (保留期) 但不阻塞同工件重跑/续传 (resume 流的闸门前提)。
func TestReserveRelayTerminalTaskDoesNotBlock(t *testing.T) {
	m := newTestTM(t, 8)
	keys := []string{relayKeyOf("srvB", "/data/f")}
	dead := relayRSpec("dead")
	dead.PreFinished = true // 白盒: 直接以终态落表 (键仍挂在条目上)
	if _, _, err := m.ReserveRelay(dead, nil, keys); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ReserveRelay(relayRSpec("retry"), nil, keys); err != nil {
		t.Fatalf("terminal task's keys must not block re-run: %v", err)
	}
}

// TestReserveRelayAdmissionEvictionAndLimit: 自定义 SSHMGR_BG_MAX_TASKS=2 下
// (a) 满员+有终态可逐 → 驱逐最旧终态后占位成功; (b) 满员全 running →
// ErrBgTaskLimit + 引导文案与 Reserve 逐字一致 (relay 不绕 maxTasks, spec §2⑧)。
func TestReserveRelayAdmissionEvictionAndLimit(t *testing.T) {
	t.Setenv("SSHMGR_BG_MAX_TASKS", "2")
	t.Setenv("SSHMGR_BG_RUN_CAP", "")
	t.Setenv("SSHMGR_BG_RETAIN", "")
	keys := func(i int) []string { return []string{relayKeyOf("srvB", "/data/f"+strconv.Itoa(i))} }

	// (a) 两个终态占满 (finishedAt 错峰——驱逐确定性, 照 TestAdmissionCapAndEviction)。
	m, err := NewTaskManager()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	ids := make([]string, 2)
	for i := 0; i < 2; i++ {
		spec := relayRSpec("done" + strconv.Itoa(i))
		spec.PreFinished = true
		id, _, ierr := m.ReserveRelay(spec, nil, keys(i))
		if ierr != nil {
			t.Fatal(ierr)
		}
		ids[i] = id
		m.mu.Lock()
		m.tasks[id].finishedAt = base.Add(time.Duration(i) * time.Minute)
		m.mu.Unlock()
	}
	if _, _, err = m.ReserveRelay(relayRSpec("fresh"), nil, keys(9)); err != nil {
		t.Fatalf("full with evictable terminal must admit via eviction: %v", err)
	}
	if _, ok := m.lookup(ids[0]); ok {
		t.Fatal("oldest terminal task must be the eviction victim")
	}
	if _, ok := m.lookup(ids[1]); !ok {
		t.Fatal("newer terminal task must survive eviction")
	}

	// (b) 两个 running 占满 → ErrBgTaskLimit 引导文案逐字。
	m2, err := NewTaskManager()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err = m2.ReserveRelay(relayRSpec("run"+strconv.Itoa(i)), nil, keys(i)); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = m2.ReserveRelay(relayRSpec("overflow"), nil, keys(9))
	if !errors.Is(err, ErrBgTaskLimit) {
		t.Fatalf("all-running full must refuse with ErrBgTaskLimit, got %v", err)
	}
	want := "background task limit (2) reached — wait for a running task to finish or call exec_stop"
	if err.Error() != want {
		t.Fatalf("guidance text must stay byte-identical to Reserve:\n got %q\nwant %q", err.Error(), want)
	}
	if r := reservedCount(m2); r != 0 {
		t.Fatalf("refused admission must not leak a reservation: reserved=%d", r)
	}
}

// TestReserveRelayFailurePathsRestoreAdmission: ⑧ 后一切失败路径 reserved 计数
// 归零 (单一记账——计数漂移会让任务表远低于上限即拒一切新任务, rev4 kimi#2)。
// 反复冲突失败 N 次后正常建任务仍成功; 满员拒绝与 manager 已关同样零计数残留。
func TestReserveRelayFailurePathsRestoreAdmission(t *testing.T) {
	m := newTestTM(t, 4)
	held := []string{relayKeyOf("srvB", "/data/f")}
	if _, _, err := m.ReserveRelay(relayRSpec("holder"), nil, held); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := m.ReserveRelay(relayRSpec("clash"), nil, held); err == nil {
			t.Fatal("conflict expected")
		}
		if r := reservedCount(m); r != 0 {
			t.Fatalf("conflict failure left reserved=%d (iteration %d) — admission accounting must roll back", r, i)
		}
		if got := m.Len(); got != 1 {
			t.Fatalf("Len=%d after failed attempt %d, want 1 (no placeholder residue)", got, i)
		}
	}
	if _, err := m.Insert(runningSpec()); err != nil {
		t.Fatalf("normal task creation must still succeed after repeated failures: %v", err)
	}
	// 满员拒绝路径零计数残留。
	m2 := newTestTM(t, 1)
	if _, _, err := m2.ReserveRelay(relayRSpec("fill"), nil, []string{relayKeyOf("srvB", "/x")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m2.ReserveRelay(relayRSpec("over"), nil, []string{relayKeyOf("srvB", "/y")}); !errors.Is(err, ErrBgTaskLimit) {
		t.Fatalf("want ErrBgTaskLimit, got %v", err)
	}
	if r := reservedCount(m2); r != 0 {
		t.Fatalf("limit refusal left reserved=%d", r)
	}
	// manager 已关: 拒绝且计数为零。
	m3 := newTestTM(t, 4)
	m3.CloseAll()
	if _, _, err := m3.ReserveRelay(relayRSpec("closed"), nil, held); !errors.Is(err, ErrBgManagerClosed) {
		t.Fatalf("want ErrBgManagerClosed, got %v", err)
	}
	if r := reservedCount(m3); r != 0 {
		t.Fatalf("closed refusal left reserved=%d", r)
	}
}

// TestAuditActionRelayVsExecDefault: BgTaskSpec.AuditAction 进 auditEnd 闭包——
// relay 传 "relay-bg-end" 落行 (Command=taskID 形态继承); exec 不传 → 缺省字面量
// "exec-bg-end" (零回归)。白盒直接驱动 Insert 绑定的 t.auditEnd (runTask 的调用
// 点系既有路径, 既有 exec 测试 + T5 e2e 再锚)。
func TestAuditActionRelayVsExecDefault(t *testing.T) {
	m := newTestTM(t, 4)
	var rows []store.AuditRow
	collect := func(row store.AuditRow) error { rows = append(rows, row); return nil }

	relayID, err := m.Insert(&BgTaskSpec{
		ProjectID: "proj", ServerID: "srvB", Command: "relay srvA:/src -> srvB:/data/f",
		Timeout: time.Minute, PreFinished: true,
		AuditAction: "relay-bg-end",
		AuditEnd:    collect,
	})
	if err != nil {
		t.Fatal(err)
	}
	execID, err := m.Insert(&BgTaskSpec{
		ProjectID: "proj", ServerID: "srv", Command: "echo hi",
		Timeout: time.Minute, PreFinished: true,
		AuditEnd: collect, // 不传 AuditAction → 缺省
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{relayID, execID} {
		m.mu.Lock()
		endFn := m.tasks[id].auditEnd
		m.mu.Unlock()
		if endFn == nil {
			t.Fatalf("task %s: auditEnd never bound", id)
		}
		if err := endFn(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2: %+v", len(rows), rows)
	}
	if rows[0].Action != "relay-bg-end" {
		t.Fatalf("relay end row action=%q, want relay-bg-end", rows[0].Action)
	}
	if rows[0].Command != relayID {
		t.Fatalf("relay end row Command=%q, want taskID %q (现状形态继承)", rows[0].Command, relayID)
	}
	if rows[1].Action != "exec-bg-end" {
		t.Fatalf("exec end row action=%q, want literal exec-bg-end (零回归)", rows[1].Action)
	}
}

// TestRelayDualConnectionSlots: 双连接槽 (client+auxClient, spec §2 连接生命周期)。
// (a) 引擎终态即关两条 (runTask 终态段, 与 client 槽逐字同款——锁外关、幂等);
// (b) CloseAll 可达即关两条。
func TestRelayDualConnectionSlots(t *testing.T) {
	m := newTestTM(t, 4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	dial := func() *sshbroker.Client {
		t.Helper()
		cli, cerr := sshbroker.ConnectKeepAlive(context.Background(), host, port, "u",
			sshbroker.PasswordAuth("pw"), ssh.FixedHostKey(hk))
		if cerr != nil {
			t.Fatal(cerr)
		}
		return cli
	}

	// (a) 终态即关两条: Run 闭包挂双槽后交 runTask, exec 立即成功 → done。
	dest, src := dial(), dial()
	spec := relayRSpec("relay term")
	spec.AuditStart = func() {}
	spec.AuditEnd = func(store.AuditRow) error { return nil }
	spec.Run = func(ctx context.Context, tb *bgTask) {
		m.mu.Lock()
		tb.client, tb.auxClient = dest, src
		m.mu.Unlock()
		m.runTask(ctx, tb, dest, func(context.Context, io.Writer, io.Writer) (int, bool, error) {
			return 0, false, nil
		}, nil)
	}
	id, eff, err := m.ReserveRelay(spec, nil, []string{relayKeyOf("srvB", "/data/f")})
	if err != nil {
		t.Fatal(err)
	}
	if eff != time.Minute {
		t.Fatalf("effectiveTimeout=%v, want spec.Timeout verbatim (T4 已钳定 relayRunCap)", eff)
	}
	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status=%q, want done", s.status)
	}
	waitClientClosed(t, dest)
	waitClientClosed(t, src)

	// (b) CloseAll 即关两条: Run 闭包挂双槽后在途阻塞, CloseAll 的 cancel 解阻。
	dest2, src2 := dial(), dial()
	spec2 := relayRSpec("relay closeall")
	spec2.Run = func(ctx context.Context, tb *bgTask) {
		m.mu.Lock()
		tb.client, tb.auxClient = dest2, src2
		m.mu.Unlock()
		m.runTask(ctx, tb, dest2, func(ectx context.Context, _, _ io.Writer) (int, bool, error) {
			<-ectx.Done() // 在途传输形态
			return 0, false, ectx.Err()
		}, nil)
	}
	id2, _, err := m.ReserveRelay(spec2, nil, []string{relayKeyOf("srvB", "/data/g")})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		tb := m.tasks[id2]
		set := tb != nil && tb.client != nil && tb.auxClient != nil
		m.mu.Unlock()
		if set {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dual connection slots never set")
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.CloseAll()
	waitClientClosed(t, dest2)
	waitClientClosed(t, src2)
	if _, ok := m.lookup(id2); ok {
		t.Fatal("CloseAll must remove the entry")
	}
}
