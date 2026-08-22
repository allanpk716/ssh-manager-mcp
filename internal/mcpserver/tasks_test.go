package mcpserver

import (
	"strconv"
	"sync"
	"testing"
	"time"
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
