package mcpserver

// Plan 32 T5: 代际广播消费 + Output(=WaitFor) 等待回路 (spec §2.3 结构钉死)。
// 六用例对应 brief Step 1: 虚假唤醒续等到 deadline / 广播唤醒全部等待者 /
// 注入唤醒不重置预算 (绝对 deadline) / 零等待者短路 (gen 不推进) / 超前游标
// 立即回拉返回 / 锁序压测 (TM.mu→buffer.mu 唯一合法嵌套, ABBA 死锁表现为
// hang 由 10s 兜底捕获)。
// 另补两用例 (本任务两处接线的直接锚): CloseAll 广播唤醒在途等待者 (spec §3
// 触发点③——摘表循环逐任务 notify 接线的验证) 与 view 诚实降级 (gap 分支
// truncated+lost——BgView 按 spec §4 补齐的 Truncated/Lost* 字段)。

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitParked 轮询至至少 n 个等待者确已泊车 (waiters 计数 ≥ n)——使后续注入
// 广播/CloseAll 必命中已入 select 的等待者, 消除「先通知后泊车」竞态。
func waitParked(t *testing.T, tk *bgTask, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for tk.waiters.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("waiters never reached %d (now %d)", n, tk.waiters.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWaitSpuriousWakeHolds: 运行中任务无数据, 注入若干次虚假广播 (无新字节)
// ——等待回路必须续等, 恰在绝对 deadline (~300ms) 返回, 且快照为空。
func TestWaitSpuriousWakeHolds(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	go func() { // 200ms 内每 20ms 一次虚假广播 (仅 notify, 零字节)
		for i := 0; i < 10; i++ {
			time.Sleep(20 * time.Millisecond)
			m.notifyWrite(tk)
		}
	}()
	start := time.Now()
	v, ok, oerr := m.Output(id, 0, 0, 300*time.Millisecond, context.Background())
	elapsed := time.Since(start)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if !ok {
		t.Fatal("task must be found")
	}
	if elapsed < 280*time.Millisecond {
		t.Fatalf("spurious wakes returned early: %v (< 280ms)", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("wait far past deadline: %v", elapsed)
	}
	if v.Status != bgStatusRunning || v.Truncated {
		t.Fatalf("view = status %q truncated=%v, want running/false", v.Status, v.Truncated)
	}
	if len(v.Stdout) != 0 || len(v.Stderr) != 0 || v.NextStdout != 0 || v.NextStderr != 0 ||
		v.StdoutTotal != 0 || v.StderrTotal != 0 {
		t.Fatalf("snapshot must be empty: %+v", v)
	}
	if n := tk.waiters.Load(); n != 0 {
		t.Fatalf("waiters leaked: %d after return", n)
	}
}

// TestWaitBroadcastWakesAllWaiters: 3 个并发等待者同任务同 offset; 新字节落笔
// + 广播 → 三者全醒, 各自拿到同增量。
func TestWaitBroadcastWakesAllWaiters(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	const N = 3
	type waitRes struct {
		v  BgView
		ok bool
	}
	results := make(chan waitRes, N)
	for i := 0; i < N; i++ {
		go func() {
			v, ok, werr := m.Output(id, 0, 0, 5*time.Second, context.Background())
			if werr != nil {
				t.Errorf("Output waiter: %v", werr)
			}
			results <- waitRes{v, ok}
		}()
	}
	waitParked(t, tk, N)
	tk.stdout.Write([]byte("broadcast-wake\n")) // 新字节 (真实路径: 落笔后才广播)
	m.notifyWrite(tk)
	for i := 0; i < N; i++ {
		select {
		case r := <-results:
			if !r.ok {
				t.Fatalf("waiter %d: ok=false", i)
			}
			if r.v.Status != bgStatusRunning {
				t.Fatalf("waiter %d: status=%q, want running", i, r.v.Status)
			}
			if string(r.v.Stdout) != "broadcast-wake\n" {
				t.Fatalf("waiter %d: stdout=%q, want increment", i, r.v.Stdout)
			}
			if r.v.NextStdout != int64(len("broadcast-wake\n")) || r.v.StdoutTotal != r.v.NextStdout {
				t.Fatalf("waiter %d: next=%d total=%d, want both 15", i, r.v.NextStdout, r.v.StdoutTotal)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d not woken by broadcast within 2s", i)
		}
	}
	if n := tk.waiters.Load(); n != 0 {
		t.Fatalf("waiters leaked: %d after all returned", n)
	}
}

// TestWaitBudgetNotExtended: wait=300ms, 期间注入 20 次虚假广播——总阻塞必须
// ≤ 300ms+ε (绝对 deadline 不随唤醒重置)。
func TestWaitBudgetNotExtended(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	go func() {
		for i := 0; i < 20; i++ { // 10ms×20: 200ms 注入窗, 全程无新字节
			time.Sleep(10 * time.Millisecond)
			m.notifyWrite(tk)
		}
	}()
	start := time.Now()
	_, ok, oerr := m.Output(id, 0, 0, 300*time.Millisecond, context.Background())
	elapsed := time.Since(start)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if !ok {
		t.Fatal("task must be found")
	}
	if elapsed < 280*time.Millisecond {
		t.Fatalf("returned early: %v (< 280ms)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("budget extended by injected wakes: %v (> 300ms+ε)", elapsed)
	}
}

// TestNotifyShortCircuitZeroWaiters: 无等待者时 notifyWrite 短路——gen 不推进、
// waitCh 不换新 (白盒持锁断言)。
func TestNotifyShortCircuitZeroWaiters(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	m.mu.Lock()
	genBefore, chBefore := tk.gen, tk.waitCh
	m.mu.Unlock()
	m.notifyWrite(tk) // waiters==0 → 短路
	m.mu.Lock()
	genAfter, chAfter := tk.gen, tk.waitCh
	m.mu.Unlock()
	if genAfter != genBefore {
		t.Fatalf("gen advanced with zero waiters: %d → %d", genBefore, genAfter)
	}
	if chAfter != chBefore {
		t.Fatal("waitCh replaced with zero waiters (must short-circuit)")
	}
}

// TestWaitAheadOffsetReturnsImmediately: stdoutOffset=999 超前 (流仅 10 字节) +
// wait=5s → 立即返回: 空 chunk、next 回拉到 total (自恢复, 不空等满预算)。
func TestWaitAheadOffsetReturnsImmediately(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	tk.stdout.Write([]byte("0123456789"))
	start := time.Now()
	v, ok, oerr := m.Output(id, 999, 0, 5*time.Second, context.Background())
	elapsed := time.Since(start)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if !ok {
		t.Fatal("task must be found")
	}
	if elapsed > time.Second {
		t.Fatalf("ahead offset must return immediately, took %v", elapsed)
	}
	if len(v.Stdout) != 0 {
		t.Fatalf("ahead offset chunk=%q, want empty", v.Stdout)
	}
	if v.NextStdout != 10 || v.StdoutTotal != 10 {
		t.Fatalf("cursor pull-back: next=%d total=%d, want 10/10", v.NextStdout, v.StdoutTotal)
	}
	if v.Status != bgStatusRunning {
		t.Fatalf("status=%q, want running", v.Status)
	}
	if n := tk.waiters.Load(); n != 0 {
		t.Fatalf("waiters leaked: %d", n)
	}
}

// TestLockOrderStressNoDeadlock: 并发「写侧 buffer.mu→释→TM.mu(notify)」与
// 「等待侧 TM.mu 内读 Total (向下嵌套 buffer.mu) + 锁外 Snapshot」压测。
// ABBA 死锁表现为 hang——10s 兜底由测试侧 watchdog 捕获 (非整包超时)。
func TestLockOrderStressNoDeadlock(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // 写侧: 落笔后广播 (buffer.mu 已释才进 TM.mu——锁序铁则)
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tk.stdout.Write([]byte("stress-out\n"))
			m.notifyWrite(tk)
			tk.stderr.Write([]byte("e\n"))
			m.notifyWrite(tk)
			time.Sleep(time.Millisecond)
		}
	}()
	waiter := func(rounds int, wait time.Duration, cancelAfter time.Duration) { // 等待侧: 三条 select 臂全覆盖
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ctx := context.Background()
			so := tk.stdout.Total() // 追平游标 → 条件常假, 真泊车等待 (唤醒/取消/到期三臂)
			if cancelAfter > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.Background(), cancelAfter)
				_, _, _ = m.Output(id, so, 0, wait, ctx)
				cancel()
				continue
			}
			if _, _, werr := m.Output(id, so, 0, wait, ctx); werr != nil {
				t.Errorf("Output stress round: %v", werr)
				return
			}
		}
	}
	wg.Add(2)
	go waiter(30, 50*time.Millisecond, 0)                  // 长预算: 唤醒续等/到期返回
	go waiter(20, 50*time.Millisecond, 5*time.Millisecond) // ctx 取消臂: 立即快照返回

	time.Sleep(300 * time.Millisecond) // 压测窗 (有界)
	close(stop)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress goroutines did not finish within 10s — deadlock suspected (ABBA lock order)")
	}
	if n := tk.waiters.Load(); n != 0 {
		t.Fatalf("waiters leaked after stress: %d", n)
	}
}

// TestWaitCloseAllWakesWaiters: 在途等待者泊车后 CloseAll——摘表循环的逐任务
// notify 广播必命中, 等待者立即以 unknown (ok=false) 返回 (spec §3 触发点③)。
func TestWaitCloseAllWakesWaiters(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	type waitRes struct {
		ok bool
	}
	rc := make(chan waitRes, 1)
	go func() {
		_, ok, werr := m.Output(id, 0, 0, 5*time.Second, context.Background())
		if werr != nil {
			t.Errorf("Output during CloseAll: %v", werr)
		}
		rc <- waitRes{ok}
	}()
	waitParked(t, tk, 1)
	m.CloseAll() // 持锁置 closed→摘表→cancel→notify (广播)→锁外 wg.Wait
	select {
	case r := <-rc:
		if r.ok {
			t.Fatal("waiter after CloseAll: ok=true, want false (表项已失 → unknown 三因)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken by CloseAll broadcast within 2s")
	}
	if n := tk.waiters.Load(); n != 0 {
		t.Fatalf("waiters after CloseAll wake: %d, want 0", n)
	}
}

// TestViewHonestDegradationGap: 游标落后保留窗首 (gap) → truncated + lost =
// start-since + 整窗可用 (诚实降级, spec §4; BgView 补齐字段的直接锚)。
func TestViewHonestDegradationGap(t *testing.T) {
	m := newTestTM(t, 4)
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	// 一次写超整窗 (1 MiB+10): 只留尾 1 MiB, total=1 MiB+10, start=10。
	tk.stdout.Write(make([]byte, int(MaxOutputBytes)+10))
	v := m.view(tk, 1, 0) // so=1 < start=10 → gap 分支
	if !v.Truncated {
		t.Fatal("gap offset must set Truncated")
	}
	if v.LostStdout != 9 {
		t.Fatalf("LostStdout=%d, want 9 (start-since = 10-1)", v.LostStdout)
	}
	if int64(len(v.Stdout)) != MaxOutputBytes {
		t.Fatalf("gap chunk len=%d, want whole retained window %d", len(v.Stdout), MaxOutputBytes)
	}
	if v.NextStdout != MaxOutputBytes+10 || v.StartStdout != 10 || v.StdoutTotal != MaxOutputBytes+10 {
		t.Fatalf("gap cursors: next=%d start=%d total=%d, want %d/10/%d",
			v.NextStdout, v.StartStdout, v.StdoutTotal, MaxOutputBytes+10, MaxOutputBytes+10)
	}
	if v.LostStderr != 0 { // stderr 通道未写未落后: 无第二因降级
		t.Fatalf("stderr untouched: LostStderr=%d, want 0", v.LostStderr)
	}
	if v.Status != bgStatusRunning {
		t.Fatalf("status=%q, want running", v.Status)
	}
}
