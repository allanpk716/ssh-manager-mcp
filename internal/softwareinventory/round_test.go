package softwareinventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newRoundFixture 打开一个真实 SQLite 临时库，按给定设备名各挂一个服务
// 器条目并设为受管，时钟固定在 now。采集函数由各测试注入。
func newRoundFixture(t *testing.T, now time.Time, deviceNames ...string) (*RoundManager, *store.Store) {
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
	manager := NewRoundManager(func() (*store.Store, func(), error) {
		return st, func() {}, nil
	})
	manager.Now = func() time.Time { return now }
	return manager, st
}

// waitRoundLocalDone 轮询等待一轮的本机部分执行结束。
func waitRoundLocalDone(t *testing.T, round *CollectionRound) RoundSnapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		snapshot := round.Snapshot()
		if snapshot.LocalDone {
			return snapshot
		}
		select {
		case <-deadline:
			t.Fatal("轮次的本机部分没有在时限内结束")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRoundManagerCollectsEveryVisibleManagedDevice 覆盖「发起一轮全部采
// 集」：全部可见受管设备并行采集成功并带条目数；不受管设备不参与；进行
// 中再次发起返回同一轮。
func TestRoundManagerCollectsEveryVisibleManagedDevice(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, st := newRoundFixture(t, now, "工作站", "备份机")
	mustDevice(t, st, "不受管存储", false)

	var mu sync.Mutex
	collected := map[string]int{}
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	manager.Concurrency = 2
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		mu.Lock()
		collected[request.DeviceName]++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return &CollectionReport{DeviceName: request.DeviceName, ItemCounts: map[models.SoftwareCategory]int{
			CategoryServiceProgram: 3, CategorySystemPackage: 5,
		}}, nil
	}

	round, err := manager.Start("", WebUISource("198.51.100.9"))
	if err != nil {
		t.Fatal(err)
	}
	// 两台设备并行采集中：此时间内再次发起应返回同一轮，不新建。
	<-started
	<-started
	again, err := manager.Start("", WebUISource("198.51.100.9"))
	if err != nil {
		t.Fatal(err)
	}
	if again != round {
		t.Fatal("有轮次进行中时再次发起应返回当前轮")
	}
	running := round.Snapshot()
	if running.LocalDone {
		t.Fatal("两台设备都卡在采集里，本机部分不应已完成")
	}
	byName := map[string]RoundDeviceStatus{}
	for _, device := range running.Devices {
		byName[device.DeviceName] = device
	}
	if len(running.Devices) != 2 {
		t.Fatalf("本轮只应包含两台受管设备：%+v", running.Devices)
	}
	for _, name := range []string{"工作站", "备份机"} {
		if state := byName[name].State; state != RoundDeviceCollecting && state != RoundDeviceQueued {
			t.Fatalf("设备 %s 应在采集或排队，得到 %q", name, state)
		}
	}
	close(release)

	snapshot := waitRoundLocalDone(t, round)
	if snapshot.Succeeded != 2 || snapshot.Failed != 0 || snapshot.WaitingClient != 0 {
		t.Fatalf("整轮统计 = %+v", snapshot)
	}
	for _, device := range snapshot.Devices {
		if device.State != RoundDeviceSucceeded || device.ItemCount != 8 {
			t.Fatalf("设备 %s 应成功并带条目数 8：%+v", device.DeviceName, device)
		}
	}
	mu.Lock()
	count := len(collected)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("不受管设备不应被采集，实际采集 %v", collected)
	}
	if snapshot.NeedsRefresh() {
		t.Fatal("本机完成且无等待设备后不应再刷新")
	}
}

// TestRoundManagerStartsNewRoundAfterPreviousLocalPartDone 验证「最近一轮保
// 留到下一轮开始」。
func TestRoundManagerStartsNewRoundAfterPreviousLocalPartDone(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, _ := newRoundFixture(t, now, "工作站")
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		return &CollectionReport{DeviceName: request.DeviceName}, nil
	}
	first, err := manager.Start("", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	waitRoundLocalDone(t, first)
	if manager.Current() != first {
		t.Fatal("最近一轮应保留")
	}
	second, err := manager.Start("", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("旧轮本机部分已结束，再发起应新建一轮")
	}
	if manager.Current() != second {
		t.Fatal("Current 应指向新轮")
	}
}

// TestRoundManagerHonorsConcurrencyLimit 验证轮次复用每日定时采集的并发上限。
func TestRoundManagerHonorsConcurrencyLimit(t *testing.T) {
	now := time.Unix(1770000000, 0)
	devices := []string{"设备一", "设备二", "设备三", "设备四", "设备五", "设备六", "设备七", "设备八"}
	manager, _ := newRoundFixture(t, now, devices...)
	var current, peak atomic.Int64
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
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

	round, err := manager.Start("", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitRoundLocalDone(t, round)
	if snapshot.Succeeded != len(devices) || snapshot.Failed != 0 {
		t.Fatalf("整轮统计 = %+v", snapshot)
	}
	if peak.Load() > DefaultScheduleConcurrency {
		t.Fatalf("并发峰值 %d 超过上限 %d", peak.Load(), DefaultScheduleConcurrency)
	}
	if peak.Load() < 2 {
		t.Fatalf("并发峰值 %d 说明设备没有并行采集", peak.Load())
	}
}

// TestRoundManagerDeviceFailureDoesNotAffectOthers 验证单台失败不影响其余设备。
func TestRoundManagerDeviceFailureDoesNotAffectOthers(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, _ := newRoundFixture(t, now, "工作站", "备份机")
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		if request.DeviceName == "备份机" {
			return nil, errors.New("连接超时，端口不通")
		}
		return &CollectionReport{DeviceName: request.DeviceName, ItemCounts: map[models.SoftwareCategory]int{
			CategoryServiceProgram: 2,
		}}, nil
	}

	round, err := manager.Start("", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitRoundLocalDone(t, round)
	if snapshot.Succeeded != 1 || snapshot.Failed != 1 {
		t.Fatalf("整轮统计 = %+v", snapshot)
	}
	for _, device := range snapshot.Devices {
		switch device.DeviceName {
		case "备份机":
			if device.State != RoundDeviceFailed || !strings.Contains(device.Err, "连接超时") {
				t.Fatalf("备份机应带失败原因：%+v", device)
			}
		case "工作站":
			if device.State != RoundDeviceSucceeded || device.ItemCount != 2 {
				t.Fatalf("工作站应成功：%+v", device)
			}
		}
	}
}

// TestRoundManagerClientDeviceWaitsThenSucceeds 覆盖客户端实例执行者：等待
// 状态带实例名与最近拉取时间；本轮开始后有新结果落库即转成功。
func TestRoundManagerClientDeviceWaitsThenSucceeds(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, st := newRoundFixture(t, now, "工作站", "客户端采集设备")
	pullAt := now.Add(-2 * time.Hour)
	profileID, err := st.AddProfile("采集执行客户端")
	if err != nil {
		t.Fatal(err)
	}
	servers, err := st.FullServersForDevice("客户端采集设备")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{servers[0].ID}); err != nil {
		t.Fatal(err)
	}
	tokenID, _, err := st.AddCacheToken("mini6", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TouchCacheTokenPull(tokenID, pullAt); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("客户端采集设备", "mini6", "", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		return &CollectionReport{DeviceName: request.DeviceName, ItemCounts: map[models.SoftwareCategory]int{
			CategoryServiceProgram: 4,
		}}, nil
	}

	round, err := manager.Start("", WebUISource("198.51.100.9"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitRoundLocalDone(t, round)
	if snapshot.Succeeded != 1 || snapshot.WaitingClient != 1 {
		t.Fatalf("本机部分结束后应剩一台等待客户端：%+v", snapshot)
	}
	var waiting RoundDeviceStatus
	for _, device := range snapshot.Devices {
		if device.DeviceName == "客户端采集设备" {
			waiting = device
		}
	}
	if waiting.State != RoundDeviceWaitingClient {
		t.Fatalf("客户端执行者设备应等待：%+v", waiting)
	}
	if waiting.CollectorName != "mini6" || !waiting.CollectorLastPullAt.Equal(pullAt) {
		t.Fatalf("等待设备应带实例名与最近拉取时间：%+v", waiting)
	}
	if !snapshot.NeedsRefresh() {
		t.Fatal("还有等待设备的轮次应继续刷新")
	}
	if status, err := st.SoftwareCollectionStatusForDevice("客户端采集设备"); err != nil || !status.PendingImmediate {
		t.Fatalf("等待设备应登记立即采集请求：%+v %v", status, err)
	}

	// 客户端实例在轮次开始之后上交新结果（last_success_at 晚于轮次开始）。
	arrivedAt := now.Add(time.Minute)
	st.NowFn = func() time.Time { return arrivedAt }
	if err := st.ReplaceSoftwareObservedItems("客户端采集设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
			{Category: CategoryServiceProgram, Name: "redis", Version: "7.2"},
			{Category: CategorySystemPackage, Name: "openssl", Version: "3.0.2"},
		},
		ServerID: servers[0].ID, CommandSetVersion: POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	after := round.Snapshot()
	if after.WaitingClient != 0 || after.Succeeded != 2 {
		t.Fatalf("新结果落库后等待设备应转成功：%+v", after)
	}
	for _, device := range after.Devices {
		if device.DeviceName == "客户端采集设备" {
			if device.State != RoundDeviceSucceeded || device.ItemCount != 3 {
				t.Fatalf("等待转成功应带新结果条目数：%+v", device)
			}
		}
	}
	if after.NeedsRefresh() {
		t.Fatal("全部设备出结果后不应再刷新")
	}
}

// TestRoundManagerClientDeviceFailureReported 验证客户端实例上交失败时等待
// 设备转失败并带原因。
func TestRoundManagerClientDeviceFailureReported(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, st := newRoundFixture(t, now, "客户端采集设备")
	profileID, err := st.AddProfile("采集执行客户端")
	if err != nil {
		t.Fatal(err)
	}
	servers, err := st.FullServersForDevice("客户端采集设备")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{servers[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("mini6", profileID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("客户端采集设备", "mini6", "", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		return &CollectionReport{DeviceName: request.DeviceName}, nil
	}

	round, err := manager.Start("", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	waitRoundLocalDone(t, round)
	failedAt := now.Add(2 * time.Minute)
	st.NowFn = func() time.Time { return failedAt }
	if err := st.RecordSoftwareCollectionFailure(
		"客户端采集设备", "客户端实例连接设备失败：端口不通", servers[0].ID, false, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	snapshot := round.Snapshot()
	if snapshot.WaitingClient != 0 || snapshot.Failed != 1 {
		t.Fatalf("客户端失败也应转为失败：%+v", snapshot)
	}
	for _, device := range snapshot.Devices {
		if !strings.Contains(device.Err, "端口不通") {
			t.Fatalf("等待转失败应带原因：%+v", device)
		}
	}
}

// TestRoundManagerRunsWithoutRequestContext 验证轮次独立于网页请求执行。
func TestRoundManagerRunsWithoutRequestContext(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, _ := newRoundFixture(t, now, "工作站")
	block := make(chan struct{})
	proceeded := make(chan struct{})
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		close(proceeded)
		<-block
		return &CollectionReport{DeviceName: request.DeviceName}, nil
	}
	if _, err := manager.Start("", CLISource()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proceeded:
	case <-time.After(time.Second):
		t.Fatal("轮次应在发起后立即后台执行")
	}
	close(block)
	if round := manager.Current(); round == nil {
		t.Fatal("发起后应能查到当前轮")
	} else {
		waitRoundLocalDone(t, round)
	}
}

// TestRoundManagerStopsWhenManagerContextEnds 验证轮次随根上下文停止。
func TestRoundManagerStopsWhenManagerContextEnds(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, _ := newRoundFixture(t, now, "工作站", "备份机")
	rootCtx, cancel := context.WithCancel(context.Background())
	manager.Context = rootCtx
	manager.Concurrency = 1
	inside := make(chan struct{})
	manager.Collect = func(ctx context.Context, request CollectRequest) (*CollectionReport, error) {
		if request.DeviceName == "工作站" {
			close(inside)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("不应执行到第二台：%s", request.DeviceName)
	}
	round, err := manager.Start("", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	<-inside
	cancel()
	snapshot := waitRoundLocalDone(t, round)
	if snapshot.LocalDone != true {
		t.Fatal("根上下文结束后轮次应结束")
	}
	for _, device := range snapshot.Devices {
		if device.State == RoundDeviceCollecting {
			t.Fatalf("轮次结束后不应有设备仍在采集中：%+v", snapshot.Devices)
		}
	}
}

// TestRoundManagerSharesDeviceLocksWithScheduler 验证轮次与每日定时采集共
// 享每设备互斥，不并发重复采集同一台设备。
func TestRoundManagerSharesDeviceLocksWithScheduler(t *testing.T) {
	now := time.Unix(1770000000, 0)
	manager, st := newRoundFixture(t, now, "工作站")
	collectSuccessAtTime(t, st, "工作站", now.Add(-25*time.Hour))
	locks := NewDeviceCollectionLocks()
	manager.DeviceLocks = locks

	var overlaps atomic.Int64
	var roundCollecting atomic.Bool
	inRound := make(chan struct{})
	roundDone := make(chan struct{})
	manager.Collect = func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
		overlaps.Add(1)
		roundCollecting.Store(true)
		close(inRound)
		<-roundDone
		roundCollecting.Store(false)
		return &CollectionReport{DeviceName: request.DeviceName}, nil
	}
	scheduler := &Scheduler{
		Store: st, Now: manager.Now, DeviceLocks: locks,
		Collect: func(_ context.Context, request CollectRequest) (*CollectionReport, error) {
			if roundCollecting.Load() {
				t.Error("调度器不应在轮次采集同一台设备时并发采集")
			}
			return &CollectionReport{DeviceName: request.DeviceName}, nil
		},
	}

	if _, err := manager.Start("", CLISource()); err != nil {
		t.Fatal(err)
	}
	<-inRound
	// 轮次持有「工作站」期间调度轮执行：到期设备应等待锁而不是并发采集。
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		if _, err := scheduler.ExecuteRound(context.Background()); err != nil {
			t.Errorf("调度轮执行失败：%v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(roundDone)
	<-schedulerDone
	waitRoundLocalDone(t, manager.Current())
	if got := overlaps.Load(); got != 1 {
		t.Fatalf("工作站应只被采集一次，实际 %d 次", got)
	}
}
