package softwareinventory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newScheduleFixture 打开一个真实 SQLite 临时库，按给定设备名各挂一个服
// 务器条目；时钟固定在 now。采集函数由各测试注入。
func newScheduleFixture(t *testing.T, now time.Time, deviceNames ...string) (*Scheduler, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir()+"/vault.db", key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.NowFn = func() time.Time { return now }
	for _, name := range deviceNames {
		addCollectDevice(t, st, name, "entry-"+name)
	}
	return &Scheduler{
		Store: st, Now: func() time.Time { return now },
	}, st
}

// fakeCollect 记录采集调用顺序并返回成功报告；可选的错误按设备名返回。
type fakeCollect struct {
	mu      sync.Mutex
	calls   []string
	failFor map[string]error
	block   time.Duration
}

func (f *fakeCollect) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeCollect) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCollect) fn() SchedulerCollectFunc {
	return func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		f.record(request.DeviceName)
		if f.block > 0 {
			time.Sleep(f.block)
		}
		if err := f.failFor[request.DeviceName]; err != nil {
			return nil, err
		}
		return &CollectionReport{DeviceName: request.DeviceName}, nil
	}
}

// collectSuccessAtTime 用真实存储方法把一台设备的上次成功时间落到过去某
// 个时刻，供到期判断测试使用。
func collectSuccessAtTime(t *testing.T, st *store.Store, deviceName string, at time.Time) {
	t.Helper()
	previous := st.NowFn
	st.NowFn = func() time.Time { return at }
	servers, err := st.FullServersForDevice(deviceName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems(deviceName, store.SoftwareObservedWrite{
		ServerID: servers[0].ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = previous
}

// TestSchedulerExecuteRoundDueDevices 覆盖「执行一轮」的到期判断：距上次
// 成功已满一天与从未采集的设备被采集；不到一天的设备跳过；没有服务器条
// 目的设备不参与；别的采集执行者名下的设备不参与。
func TestSchedulerExecuteRoundDueDevices(t *testing.T) {
	now := time.Unix(1770000000, 0)
	scheduler, st := newScheduleFixture(t, now, "工作站", "备用机", "新机器")
	collectSuccessAtTime(t, st, "工作站", now.Add(-25*time.Hour))
	collectSuccessAtTime(t, st, "新机器", now.Add(-time.Hour))
	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()

	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.names(); len(got) != 2 || got[0] != "备用机" || got[1] != "工作站" {
		t.Fatalf("本轮应按设备名采集到期设备，得到 %v", got)
	}
	if report.Due != 2 || report.SkippedNotDue != 1 || report.Succeeded != 2 || report.Failed != 0 {
		t.Fatalf("本轮统计 = %+v", report)
	}
	for _, device := range report.Devices {
		if device.Err != "" {
			t.Fatalf("成功采集不应带错误：%+v", device)
		}
	}

	otherCollector := &Scheduler{
		Store: st, Collector: models.ClientCollector("cache-token-9"), Now: scheduler.Now, Collect: fake.fn(),
	}
	report, err = otherCollector.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 0 || len(report.Devices) != 0 {
		t.Fatalf("别的采集执行者名下没有设备：%+v", report)
	}
}

// TestSchedulerImmediateRequestFirst 验证立即采集请求优先执行：即使设备
// 刚采集过不到一天，待执行请求也让它排在本轮第一位。
func TestSchedulerImmediateRequestFirst(t *testing.T) {
	now := time.Unix(1770000000, 0)
	scheduler, st := newScheduleFixture(t, now, "工作站", "新机器")
	collectSuccessAtTime(t, st, "工作站", now.Add(-25*time.Hour))
	collectSuccessAtTime(t, st, "新机器", now.Add(-time.Hour))
	if err := st.RequestSoftwareImmediateCollection("新机器", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()
	scheduler.Concurrency = 1

	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.names(); len(got) != 2 || got[0] != "新机器" || got[1] != "工作站" {
		t.Fatalf("立即采集请求应排在本轮第一位，得到 %v", got)
	}
	if report.Due != 2 || report.Succeeded != 2 {
		t.Fatalf("本轮统计 = %+v", report)
	}
	if !report.Devices[0].PendingImmediate || report.Devices[1].PendingImmediate {
		t.Fatalf("只有新机器带待执行请求：%+v", report.Devices)
	}
}

// TestSchedulerConcurrencyLimit 验证并发上限：同一时刻最多四个采集在跑，
// 全部设备最终都被处理。
func TestSchedulerConcurrencyLimit(t *testing.T) {
	now := time.Unix(1770000000, 0)
	devices := []string{"设备一", "设备二", "设备三", "设备四", "设备五", "设备六", "设备七", "设备八"}
	scheduler, _ := newScheduleFixture(t, now, devices...)
	var current, peak atomic.Int64
	scheduler.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		running := current.Add(1)
		for {
			observed := peak.Load()
			if running <= observed || peak.CompareAndSwap(observed, running) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		return &CollectionReport{DeviceName: request.DeviceName}, nil
	}

	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Succeeded != len(devices) || report.Failed != 0 {
		t.Fatalf("本轮统计 = %+v", report)
	}
	if peak.Load() > DefaultScheduleConcurrency {
		t.Fatalf("并发峰值 %d 超过上限 %d", peak.Load(), DefaultScheduleConcurrency)
	}
	if peak.Load() < 2 {
		t.Fatalf("并发峰值 %d 说明设备没有并行采集", peak.Load())
	}
}

// TestSchedulerExecuteRoundRecordsFailures 验证单台设备失败不中断本轮：
// 失败按设备记入报告，其余设备继续。
func TestSchedulerExecuteRoundRecordsFailures(t *testing.T) {
	now := time.Unix(1770000000, 0)
	scheduler, _ := newScheduleFixture(t, now, "工作站", "备份机")
	fake := &fakeCollect{failFor: map[string]error{"备份机": errors.New("连接超时")}}
	scheduler.Collect = fake.fn()

	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Succeeded != 1 || report.Failed != 1 {
		t.Fatalf("本轮统计 = %+v", report)
	}
	var backupErr string
	for _, device := range report.Devices {
		if device.DeviceName == "备份机" {
			backupErr = device.Err
		}
	}
	if backupErr == "" || !strings.Contains(backupErr, "连接超时") {
		t.Fatalf("备份机的失败原因 = %q", backupErr)
	}
}

// TestSchedulerExecuteRoundStopsCleanlyOnCanceledContext 验证执行前取消的
// 一轮立即停止：不发起采集，未执行到的设备记为失败而不是成功。
func TestSchedulerExecuteRoundStopsCleanlyOnCanceledContext(t *testing.T) {
	now := time.Unix(1770000000, 0)
	scheduler, _ := newScheduleFixture(t, now, "工作站", "备份机")
	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := scheduler.ExecuteRound(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的上下文应返回 context.Canceled，得到 %v", err)
	}
	if len(fake.names()) != 0 {
		t.Fatalf("取消后不应发起采集，得到 %v", fake.names())
	}
	if report.Succeeded != 0 || report.Failed != 2 {
		t.Fatalf("未执行到的设备应记为失败：统计 = %+v", report)
	}
	for _, device := range report.Devices {
		if device.Err == "" {
			t.Fatalf("未执行到的设备应有停止说明：%+v", device)
		}
	}
}

// TestSchedulerRunStartsImmediatelyAtStartup 验证 Run 启动即执行一轮，不等
// 第一个周期，退出时干净返回。
func TestSchedulerRunStartsImmediatelyAtStartup(t *testing.T) {
	now := time.Unix(1770000000, 0)
	scheduler, _ := newScheduleFixture(t, now, "工作站")
	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()
	scheduler.Interval = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- scheduler.Run(ctx) }()

	deadline := time.After(500 * time.Millisecond)
	for {
		if len(fake.names()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run 启动后应立即执行第一轮，而不是等第一个周期")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if elapsed := time.Since(start); elapsed >= scheduler.Interval {
		t.Fatalf("第一轮在 %v 后才执行，说明在等周期", elapsed)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 退出应返回空错误，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 取消后没有及时退出")
	}
	if got := fake.names(); len(got) != 1 {
		t.Fatalf("启动第一轮之外不应有周期执行，得到 %v", got)
	}
}

// TestSchedulerRunRepeatsPeriodically 验证 Run 按周期反复执行一轮。
func TestSchedulerRunRepeatsPeriodically(t *testing.T) {
	now := time.Unix(1770000000, 0)
	scheduler, _ := newScheduleFixture(t, now, "工作站", "备份机")
	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()
	scheduler.Interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		if len(fake.names()) >= 6 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("周期执行没有发生，采集次数 %d", len(fake.names()))
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 退出应返回空错误，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 取消后没有及时退出")
	}
}
