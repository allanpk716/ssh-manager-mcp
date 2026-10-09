package mcpserver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 忙判据测试 (桥热升级 spec 实施决策第 2 条, D5/F3): 活跃清单四分项的计数与
// 聚合 —— 活跃隧道 / 运行中后台任务 (终态保留期不算) / 在飞未答请求 (豁免
// 指定调用)。进行中传输已挂任务槽, 由运行中任务口径覆盖, 不单列。

// TestBusyTunnelCountFollowsRegistry: 有活跃隧道 → 忙且报告含隧道计数; 全关
// → 该项归零回到不忙。白盒播种注册表条目 (计数只读 map 长度; 条目的 tunnel/
// client 槽为 nil——真拆除路径的语义已由 tunnels 相关既有测试覆盖, 这里不触
// Close/SweepIdle, nil 条目过不得 Tunnel.Close 的 listener 关闭)。
func TestBusyTunnelCountFollowsRegistry(t *testing.T) {
	mgr := NewTunnelManager()
	busy := NewBusyTracker(mgr.ActiveTunnels, func() int { return 0 })

	if r := busy.Report(); r.Busy || r.ActiveTunnels != 0 {
		t.Fatalf("empty registry must be idle, got %+v", r)
	}
	if mgr.ActiveTunnels() != 0 {
		t.Fatalf("ActiveTunnels on empty manager = %d, want 0", mgr.ActiveTunnels())
	}

	mgr.mu.Lock()
	mgr.tunnels["t1"] = &managedTunnel{lastActivity: time.Now()}
	mgr.tunnels["t2"] = &managedTunnel{lastActivity: time.Now()}
	mgr.mu.Unlock()

	if mgr.ActiveTunnels() != 2 {
		t.Fatalf("ActiveTunnels = %d, want 2", mgr.ActiveTunnels())
	}
	if r := busy.Report(); !r.Busy || r.ActiveTunnels != 2 {
		t.Fatalf("two live tunnels must be busy with count 2, got %+v", r)
	}

	// 模拟全关: 逐条摘除 (与 Close/CloseAll 摘表动作同形)。
	mgr.mu.Lock()
	delete(mgr.tunnels, "t1")
	delete(mgr.tunnels, "t2")
	mgr.mu.Unlock()

	if mgr.ActiveTunnels() != 0 {
		t.Fatalf("ActiveTunnels after close = %d, want 0", mgr.ActiveTunnels())
	}
	if r := busy.Report(); r.Busy || r.ActiveTunnels != 0 {
		t.Fatalf("all tunnels closed must return to idle, got %+v", r)
	}
}

// TestBusyRunningTaskCountIgnoresTerminalRetention: 运行中后台任务 → 忙; 终态
// 保留期内的已结束任务不算忙; 任务离开运行中 → 归零。
func TestBusyRunningTaskCountIgnoresTerminalRetention(t *testing.T) {
	m, err := newTaskManagerForTest(8, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	busy := NewBusyTracker(func() int { return 0 }, m.RunningTasks)

	// 终态保留期条目 (PreFinished 白盒直插终态, 保留期内不被驱逐): 不算忙。
	if _, err := m.Insert(finishedSpec(1)); err != nil {
		t.Fatal(err)
	}
	if m.RunningTasks() != 0 {
		t.Fatalf("RunningTasks with only a terminal-retained task = %d, want 0", m.RunningTasks())
	}
	if r := busy.Report(); r.Busy || r.RunningTasks != 0 {
		t.Fatalf("terminal-retained task must not count busy, got %+v", r)
	}

	// 运行中任务 → 忙。Run 阻塞在 release 前, 模拟长任务。
	release := make(chan struct{})
	started := make(chan struct{})
	spec := &BgTaskSpec{
		ProjectID: "proj",
		ServerID:  "srv",
		Command:   "sleep 60",
		Timeout:   time.Hour,
		Run: func(context.Context, *bgTask) {
			close(started)
			<-release
		},
	}
	id, err := m.Insert(spec)
	if err != nil {
		t.Fatal(err)
	}
	<-started

	if m.RunningTasks() != 1 {
		t.Fatalf("RunningTasks = %d, want 1", m.RunningTasks())
	}
	if r := busy.Report(); !r.Busy || r.RunningTasks != 1 {
		t.Fatalf("one running task must be busy with count 1, got %+v", r)
	}

	// 任务离开运行中 → 归零。白盒转终态 (T3 白盒路径无执行引擎转终态,
	// 状态语义照 tasks_test.go 直接改字段; release 先于转态, goroutine 已返回)。
	close(release)
	m.mu.Lock()
	m.tasks[id].status = bgStatusDone
	m.tasks[id].finishedAt = m.now()
	m.mu.Unlock()

	if m.RunningTasks() != 0 {
		t.Fatalf("RunningTasks after finish = %d, want 0", m.RunningTasks())
	}
	if r := busy.Report(); r.Busy || r.RunningTasks != 0 {
		t.Fatalf("finished task must return to idle, got %+v", r)
	}
}

// TestBusyInflightWrapperCountsDuringHandler: 包装器包裹的处理器执行期间在飞
// 计数 >0, 返回 (含错误路径) 后归零。
func TestBusyInflightWrapperCountsDuringHandler(t *testing.T) {
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })

	type unit struct{}
	seen := make(chan int, 1) // 处理器执行期观察到的计数
	release := make(chan struct{})
	h := func(context.Context, *mcp.CallToolRequest, unit) (*mcp.CallToolResult, unit, error) {
		seen <- busy.InflightRequests()
		<-release
		return nil, unit{}, nil
	}
	wrapped := wrapInflight(busy, h, false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := wrapped(context.Background(), &mcp.CallToolRequest{}, unit{}); err != nil {
			t.Errorf("handler: %v", err)
		}
	}()

	if c := <-seen; c != 1 {
		t.Fatalf("inflight during handler = %d, want 1", c)
	}
	if c := busy.InflightRequests(); c != 1 {
		t.Fatalf("inflight observed from outside during handler = %d, want 1", c)
	}
	close(release)
	<-done
	if c := busy.InflightRequests(); c != 0 {
		t.Fatalf("inflight after handler returns = %d, want 0", c)
	}

	// 错误路径同样归零 (defer 增减配平)。
	errH := func(context.Context, *mcp.CallToolRequest, unit) (*mcp.CallToolResult, unit, error) {
		return nil, unit{}, context.DeadlineExceeded
	}
	_, _, _ = wrapInflight(busy, errH, false)(context.Background(), &mcp.CallToolRequest{}, unit{})
	if c := busy.InflightRequests(); c != 0 {
		t.Fatalf("inflight after erroring handler = %d, want 0", c)
	}
}

// TestBusyInflightExemptCallNotCounted: 被豁免的调用不计数 —— 发起换手的
// reload_self 自身不计入, 否则它做忙检查时永远看见自己忙碌 (spec D5)。
func TestBusyInflightExemptCallNotCounted(t *testing.T) {
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })

	type unit struct{}
	seen := make(chan int, 1)
	release := make(chan struct{})
	h := func(context.Context, *mcp.CallToolRequest, unit) (*mcp.CallToolResult, unit, error) {
		seen <- busy.InflightRequests()
		<-release
		return nil, unit{}, nil
	}
	exempt := wrapInflight(busy, h, true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		exempt(context.Background(), &mcp.CallToolRequest{}, unit{})
	}()

	if c := <-seen; c != 0 {
		t.Fatalf("exempt call must not count, handler saw %d, want 0", c)
	}
	close(release)
	<-done
	if c := busy.InflightRequests(); c != 0 {
		t.Fatalf("inflight after exempt handler = %d, want 0", c)
	}
}

// TestBusyInflightConcurrentAccounting: 并发在飞计数的账目平衡 —— n 个包装
// 调用并行执行期间计数恒在 [1, n], 全部返回后归零 (配合 -race 跑)。
func TestBusyInflightConcurrentAccounting(t *testing.T) {
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })

	type unit struct{}
	h := func(context.Context, *mcp.CallToolRequest, unit) (*mcp.CallToolResult, unit, error) {
		time.Sleep(time.Millisecond)
		return nil, unit{}, nil
	}
	wrapped := wrapInflight(busy, h, false)

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := wrapped(context.Background(), &mcp.CallToolRequest{}, unit{}); err != nil {
				t.Errorf("handler: %v", err)
			}
			if c := busy.InflightRequests(); c < 0 || c > n {
				t.Errorf("inflight out of range during flight: %d", c)
			}
		}()
	}
	wg.Wait()
	if c := busy.InflightRequests(); c != 0 {
		t.Fatalf("inflight after all concurrent calls = %d, want 0", c)
	}
}

// TestBusyReportAggregatesAllSources: 忙报告是三分项的或聚合 —— 任一 >0 即忙,
// 全零即闲; 三分项数值逐一对版。
func TestBusyReportAggregatesAllSources(t *testing.T) {
	var tunnelsN, tasksN atomic.Int64
	busy := NewBusyTracker(
		func() int { return int(tunnelsN.Load()) },
		func() int { return int(tasksN.Load()) },
	)

	if r := busy.Report(); r.Busy {
		t.Fatalf("all zero must be idle, got %+v", r)
	}

	// 仅隧道活跃。
	tunnelsN.Store(3)
	r := busy.Report()
	if !r.Busy || r.ActiveTunnels != 3 || r.RunningTasks != 0 || r.InflightRequests != 0 {
		t.Fatalf("tunnels-only report mismatch: %+v", r)
	}

	// 仅任务运行。
	tunnelsN.Store(0)
	tasksN.Store(2)
	r = busy.Report()
	if !r.Busy || r.ActiveTunnels != 0 || r.RunningTasks != 2 || r.InflightRequests != 0 {
		t.Fatalf("tasks-only report mismatch: %+v", r)
	}

	// 仅在飞请求 (白盒注数 —— 聚合分项的注入用包装器路径已由上面的 in-flight
	// 用例覆盖, 这里只为聚合式注值)。
	tasksN.Store(0)
	busy.inflight.Store(5)
	r = busy.Report()
	if !r.Busy || r.ActiveTunnels != 0 || r.RunningTasks != 0 || r.InflightRequests != 5 {
		t.Fatalf("inflight-only report mismatch: %+v", r)
	}

	// 全零 → 不忙。
	busy.inflight.Store(0)
	if r := busy.Report(); r.Busy {
		t.Fatalf("all zero again must be idle, got %+v", r)
	}
}
