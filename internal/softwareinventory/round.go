package softwareinventory

import (
	"context"
	"errors"
	"sync"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// errRoundStopped 表示设备在轮次停止前没有执行到采集，与每日定时采集的
// 停止口径一致。
var errRoundStopped = errors.New("本轮在执行前已停止")

// RoundDeviceState 是一台设备在一轮里的阶段。
type RoundDeviceState string

// 设备在一轮里的阶段代号。
const (
	// RoundDeviceQueued 排队等待执行。
	RoundDeviceQueued RoundDeviceState = "queued"
	// RoundDeviceCollecting 正在本机采集。
	RoundDeviceCollecting RoundDeviceState = "collecting"
	// RoundDeviceSucceeded 本轮采集成功（附条目数；等待客户端实例转成功
	// 也算）。
	RoundDeviceSucceeded RoundDeviceState = "succeeded"
	// RoundDeviceWaitingClient 已登记立即采集请求，等待客户端实例执行。
	RoundDeviceWaitingClient RoundDeviceState = "waiting_client"
	// RoundDeviceFailed 本轮采集失败（附原因）。
	RoundDeviceFailed RoundDeviceState = "failed"
)

// RoundDeviceStatus 是一台设备在一轮里的当前结论。
type RoundDeviceStatus struct {
	DeviceName string
	State      RoundDeviceState
	// ItemCount 是成功时的本次条目数（各类条目之和；等待转成功也带）。
	ItemCount int
	// CollectorName 是等待执行的客户端实例名。
	CollectorName string
	// CollectorLastPullAt 是该实例最近拉取时间；零值表示从未同步。
	CollectorLastPullAt time.Time
	// Err 是失败原因。
	Err string
}

// RoundSnapshot 是一轮在某一时刻的只读快照。
type RoundSnapshot struct {
	// Device 是单台轮次的设备名；空表示全部设备轮次。
	Device    string
	StartedAt time.Time
	// LocalDone 表示本机执行的设备是否全部结束；等待客户端实例结果的设
	// 备不阻塞它。
	LocalDone     bool
	LocalEndedAt  time.Time
	Succeeded     int
	Failed        int
	WaitingClient int
	Devices       []RoundDeviceStatus
}

// Running 表示本机执行的设备是否仍在采集中。
func (s RoundSnapshot) Running() bool { return !s.LocalDone }

// NeedsRefresh 表示进度页是否还需要继续刷新：本机部分未结束，或仍有等
// 待客户端实例结果的设备。
func (s RoundSnapshot) NeedsRefresh() bool { return !s.LocalDone || s.WaitingClient > 0 }

// roundDevicePlan 是发起时缓存的一台设备的采集分派信息，轮次执行期间不
// 再回读。
type roundDevicePlan struct {
	collector         models.Collector
	collectorName     string
	preferredServerID string
}

// CollectionRound 是一次「立即采集轮次」：在权威端后台执行，不随发起它
// 的网页请求结束而中断。状态保存在进程内存里，只保留最近一轮。
type CollectionRound struct {
	manager   *RoundManager
	device    string
	source    Source
	startedAt time.Time
	plan      []roundDevicePlan

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	devices      []RoundDeviceStatus
	localDone    bool
	localEndedAt time.Time
	// localDoneCh 在本机部分结束时关闭，供等待方阻塞等待。
	localDoneCh chan struct{}
}

// Snapshot 返回这一轮的只读快照。仍有等待客户端实例结果的设备时，先开
// 存储刷新它们：本轮开始之后有新成功或新失败落库即转成功/失败，否则更新
// 实例最近拉取时间；刷新失败只跳过本次刷新并记日志，不中断快照。等待不
// 设超时。汇总计数在快照里重算。
func (r *CollectionRound) Snapshot() RoundSnapshot {
	r.mu.Lock()
	devices := append([]RoundDeviceStatus(nil), r.devices...)
	localDone, localEndedAt := r.localDone, r.localEndedAt
	r.mu.Unlock()
	waiting := false
	for _, device := range devices {
		if device.State == RoundDeviceWaitingClient {
			waiting = true
			break
		}
	}
	if waiting {
		r.refreshWaitingDevices(devices)
		r.mu.Lock()
		devices = append([]RoundDeviceStatus(nil), r.devices...)
		r.mu.Unlock()
	}
	snapshot := RoundSnapshot{
		Device: r.device, StartedAt: r.startedAt,
		LocalDone: localDone, LocalEndedAt: localEndedAt,
		Devices: devices,
	}
	for _, device := range devices {
		switch device.State {
		case RoundDeviceSucceeded:
			snapshot.Succeeded++
		case RoundDeviceFailed:
			snapshot.Failed++
		case RoundDeviceWaitingClient:
			snapshot.WaitingClient++
		}
	}
	return snapshot
}

// waitingTransition 是一次快照刷新决定的一台等待设备的去向。
type waitingTransition struct {
	index    int
	toFailed bool
	reason   string
	toCount  bool
	pullAt   time.Time
	hasPull  bool
}

// refreshWaitingDevices 读取存储刷新等待设备：devices 是当前设备状态的一
// 份拷贝，先用它决定去向，再在锁外读成功设备的本次条目数，最后一次性写
// 回轮次状态。
func (r *CollectionRound) refreshWaitingDevices(devices []RoundDeviceStatus) {
	st, done, err := r.manager.OpenStore()
	if err != nil {
		r.manager.logf("刷新等待客户端实例的设备失败：%v", err)
		return
	}
	statuses, err := st.SoftwareCollectionStatuses("")
	if err != nil {
		done()
		r.manager.logf("刷新等待客户端实例的设备失败：%v", err)
		return
	}
	tokens, tokenErr := st.ListCacheTokens()
	if tokenErr != nil {
		// 实例最近拉取时间拿不到只影响展示，不算刷新失败。
		r.manager.logf("读取设备码列表失败，本次刷新不带实例最近拉取时间：%v", tokenErr)
		tokens = nil
	}
	lastPullByToken := make(map[string]time.Time, len(tokens))
	for _, token := range tokens {
		lastPullByToken[token.ID] = token.LastPullAt
	}
	statusByName := make(map[string]models.SoftwareCollectionStatus, len(statuses))
	for _, status := range statuses {
		statusByName[status.DeviceName] = status
	}
	var transitions []waitingTransition
	needCounts := make([]string, 0, 1)
	for index, device := range devices {
		if device.State != RoundDeviceWaitingClient {
			continue
		}
		status, ok := statusByName[device.DeviceName]
		if !ok {
			continue
		}
		transition := waitingTransition{index: index}
		switch {
		case status.LastSuccessAt.After(r.startedAt):
			transition.toCount = true
			needCounts = append(needCounts, device.DeviceName)
		case status.LastFailureAt.After(r.startedAt):
			transition.toFailed = true
			transition.reason = status.LastFailureReason
		default:
			transition.hasPull = true
			transition.pullAt = lastPullByToken[status.Collector.CacheTokenID]
		}
		transitions = append(transitions, transition)
	}
	itemCounts := r.itemCountsFor(st, needCounts)
	done()
	r.mu.Lock()
	for _, transition := range transitions {
		device := &r.devices[transition.index]
		switch {
		case transition.toCount:
			// 条目数读取失败的设备保持等待，下一次刷新重试。
			if count, ok := itemCounts[device.DeviceName]; ok {
				device.State = RoundDeviceSucceeded
				device.ItemCount = count
			}
		case transition.toFailed:
			device.State = RoundDeviceFailed
			device.Err = transition.reason
		case transition.hasPull:
			device.CollectorLastPullAt = transition.pullAt
		}
	}
	r.mu.Unlock()
}

// itemCountsFor 读取几台设备当前实测条目的数量；单台读取失败记日志后跳过。
func (r *CollectionRound) itemCountsFor(st *store.Store, deviceNames []string) map[string]int {
	if len(deviceNames) == 0 {
		return nil
	}
	counts := make(map[string]int, len(deviceNames))
	for _, name := range deviceNames {
		items, err := st.SoftwareObservedItemsForDevice(name)
		if err != nil {
			r.manager.logf("读取设备 %s 的新采集结果失败：%v", name, err)
			continue
		}
		counts[name] = len(items)
	}
	return counts
}

// run 在后台执行本轮：worker 池照每日定时采集的样板，按并发上限执行，
// 单台失败不影响其他设备。全部本机设备结束后标记本机部分完成。
func (r *CollectionRound) run() {
	workerCount := r.manager.concurrency()
	if workerCount > len(r.devices) {
		workerCount = len(r.devices)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				r.collectDevice(index)
			}
		}()
	}
	for index := range r.devices {
		if r.ctx.Err() != nil {
			break
		}
		select {
		case jobs <- index:
		case <-r.ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	// 没有被分派到的设备（轮次被停时停止发任务）在这里补上结论，避免停
	// 在排队状态。
	r.mu.Lock()
	r.localEndedAt = r.manager.now()
	for i := range r.devices {
		if r.devices[i].State == RoundDeviceQueued {
			r.devices[i].State = RoundDeviceFailed
			r.devices[i].Err = errRoundStopped.Error()
		}
	}
	r.localDone = true
	r.mu.Unlock()
	close(r.localDoneCh)
}

// collectDevice 执行一轮里的一台设备：先拿该设备的采集互斥，采集执行者
// 是客户端实例时登记立即采集请求并进入等待；否则本机采集。
func (r *CollectionRound) collectDevice(index int) {
	r.mu.Lock()
	name := r.devices[index].DeviceName
	plan := r.plan[index]
	r.mu.Unlock()

	locks := r.manager.deviceLocks()
	if !locks.Acquire(r.ctx, name) {
		r.finishDevice(index, RoundDeviceFailed, func(device *RoundDeviceStatus) {
			device.Err = errRoundStopped.Error()
		})
		return
	}
	defer locks.Release(name)

	if plan.collector.IsClient() {
		st, done, err := r.manager.OpenStore()
		if err != nil {
			r.finishDevice(index, RoundDeviceFailed, func(device *RoundDeviceStatus) {
				device.Err = err.Error()
			})
			return
		}
		err = st.RequestSoftwareImmediateCollection(name, r.source.origin)
		done()
		if err != nil {
			r.finishDevice(index, RoundDeviceFailed, func(device *RoundDeviceStatus) {
				device.Err = err.Error()
			})
			return
		}
		r.finishDevice(index, RoundDeviceWaitingClient, func(device *RoundDeviceStatus) {
			device.CollectorName = plan.collectorName
		})
		return
	}

	r.mu.Lock()
	r.devices[index].State = RoundDeviceCollecting
	r.mu.Unlock()
	report, err := r.manager.collect()(r.ctx, CollectRequest{
		DeviceName: name, ServerEntry: plan.preferredServerID, Source: r.source,
	})
	if err != nil {
		r.finishDevice(index, RoundDeviceFailed, func(device *RoundDeviceStatus) {
			device.Err = err.Error()
		})
		return
	}
	count := 0
	for _, each := range report.ItemCounts {
		count += each
	}
	r.finishDevice(index, RoundDeviceSucceeded, func(device *RoundDeviceStatus) {
		device.ItemCount = count
	})
}

// finishDevice 在锁内写入一台设备的结论。
func (r *CollectionRound) finishDevice(
	index int, state RoundDeviceState, fill func(device *RoundDeviceStatus),
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices[index].State = state
	fill(&r.devices[index])
}

func (r *CollectionRound) localDoneValue() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.localDone
}

// RoundManager 管理权威端进程内的「立即采集轮次」：发起一轮（全部可见
// 受管设备或指定单台）、查询当前或最近一轮。轮次在后台执行，使用独立于
// 网页请求的上下文；并发上限复用每日定时采集的默认值。
type RoundManager struct {
	// OpenStore 每次调用打开存储并返回关闭函数；轮次不长期持有句柄。
	OpenStore func() (*store.Store, func(), error)
	// Collect 是单台本机采集函数；空时默认实现为每次 OpenStore 后用绑
	// 定该存储的单台采集服务执行（采集期间持有、采完关）。单机 Web UI
	// 与权威端 serve 都不长期持有 vault 句柄，这样的短开短关与每请求开
	// 关 vault 的请求侧并存（store 的 openMu 把进程内打开串行化）。
	Collect SchedulerCollectFunc
	// DeviceLocks 与每日定时采集共享的每设备采集互斥；空值时内部自建
	// 一个。
	DeviceLocks *DeviceCollectionLocks
	// Concurrency 是一轮的并发上限；小于 1 时用每日定时采集的默认值。
	Concurrency int
	// Now 提供轮次时间；空值使用真实时钟。
	Now func() time.Time
	// Logf 为空时不输出日志。
	Logf func(format string, args ...any)
	// Context 是轮次的根上下文；空值 context.Background()。serve 传入
	// 自身上下文，进程退出时轮次一并停止。
	Context context.Context

	mu      sync.Mutex
	current *CollectionRound
	// locks 是 DeviceLocks 为空时自建的互斥表。
	locks *DeviceCollectionLocks
}

// NewRoundManager 构造一个轮次管理器；采集函数、并发上限等字段按需注入。
func NewRoundManager(open func() (*store.Store, func(), error)) *RoundManager {
	return &RoundManager{OpenStore: open}
}

// Start 发起一轮。deviceName 空 = 全部可见受管设备；非空 = 单台，设备不
// 存在或不可见返回 ErrDeviceNotVisible。单轮互斥：当前轮的本机部分未结
// 束时再发起直接返回当前轮，不新建（返回同一指针）。
func (m *RoundManager) Start(deviceName string, source Source) (*CollectionRound, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && !m.current.localDoneValue() {
		return m.current, nil
	}
	st, done, err := m.OpenStore()
	if err != nil {
		return nil, err
	}
	if deviceName != "" {
		if _, err := st.SoftwareDeviceOverviewForDevice(deviceName, AllDevicesVisible()); err != nil {
			done()
			if err == store.ErrSoftwareDeviceNotVisible {
				return nil, ErrDeviceNotVisible
			}
			return nil, err
		}
	}
	statuses, err := st.SoftwareCollectionStatuses(deviceName)
	done()
	if err != nil {
		return nil, err
	}
	plans := make([]roundDevicePlan, 0, len(statuses))
	devices := make([]RoundDeviceStatus, 0, len(statuses))
	for _, status := range statuses {
		// 不受管设备没有服务器条目，不参与全部设备的立即采集轮次。
		if deviceName == "" && !status.Managed {
			continue
		}
		plans = append(plans, roundDevicePlan{
			collector: status.Collector, collectorName: status.CollectorName,
			preferredServerID: status.PreferredServerID,
		})
		devices = append(devices, RoundDeviceStatus{
			DeviceName: status.DeviceName, State: RoundDeviceQueued,
		})
	}
	root := m.Context
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithCancel(root)
	round := &CollectionRound{
		manager: m, device: deviceName, source: source, startedAt: m.now(),
		plan: plans, ctx: ctx, cancel: cancel,
		devices: devices, localDoneCh: make(chan struct{}),
	}
	// 旧轮的本机部分已结束，这里替换掉它；旧轮对象若仍被进度页持有，
	// 继续读它的快照不再影响管理器。
	m.current = round
	go round.run()
	return round, nil
}

// Current 返回当前或最近一轮；无则 nil。
func (m *RoundManager) Current() *CollectionRound {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

func (m *RoundManager) deviceLocks() *DeviceCollectionLocks {
	if m.DeviceLocks != nil {
		return m.DeviceLocks
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = NewDeviceCollectionLocks()
	}
	return m.locks
}

func (m *RoundManager) concurrency() int {
	if m.Concurrency < 1 {
		return DefaultScheduleConcurrency
	}
	return m.Concurrency
}

func (m *RoundManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *RoundManager) collect() SchedulerCollectFunc {
	if m.Collect != nil {
		return m.Collect
	}
	return func(ctx context.Context, request CollectRequest) (*CollectionReport, error) {
		st, done, err := m.OpenStore()
		if err != nil {
			return nil, err
		}
		defer done()
		return (&Service{Store: st}).Collect(ctx, request)
	}
}

func (m *RoundManager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}
