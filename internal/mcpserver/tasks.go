package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"ssh-manager-mcp/internal/sshbroker"
)

// 常量 seam（spec §3）: 包级 var + env 覆盖, 生产默认钉死, 非法/非正拒绝启动。
var (
	bgRunCapDefault   = 24 * time.Hour
	bgRetainDefault   = time.Hour
	bgMaxTasksDefault = 32
)

const bgSweepInterval = time.Minute

// bgGracePeriod 是 sweeper 对 running 任务超期后的额外宽限: now() > deadline+1min
// 才防御性再 cancel 一次 (引擎超时路径本应已自行 cancel, 见 SweepExpired)。
const bgGracePeriod = time.Minute

// bgTask.status 取值: running 为唯一非终态; done/stopped/timeout/failed 均为
// 终态 (满员驱逐与 sweeper 只作用于终态)。
const (
	bgStatusRunning = "running"
	bgStatusDone    = "done"
	bgStatusStopped = "stopped"
	bgStatusTimeout = "timeout"
	bgStatusFailed  = "failed"
)

// bgTask 是后台任务注册表的单个条目。stdout/stderr 的 RollingBuffer 由 Task 4
// 的执行引擎填充; gen/waitCh/waiters 为 Task 5 的代际广播占位 (本任务先落字段)。
type bgTask struct {
	id             string
	projectID      string
	serverID       string
	command        string
	sudo           bool
	status         string // running|done|stopped|timeout|failed
	exitCode       int
	errText        string
	stdout, stderr *sshbroker.RollingBuffer
	cancel         context.CancelFunc
	stopReq        bool
	startedAt      time.Time
	finishedAt     time.Time
	deadline       time.Time
	// 代际广播（Task 5 用; 本任务先占位字段）
	gen     uint64
	waitCh  chan struct{}
	waiters atomic.Int32
}

// BgTaskSpec 是后台任务的一次启动描述。Task 6 的 ForProfile 负责钳定 Timeout
// 后填入并 Reserve→Insert; Run 为 nil 时 Insert 默认 no-op (T3 白盒不依赖 ssh,
// T4 接真执行引擎)。
type BgTaskSpec struct {
	ProjectID, ServerID, Command string
	Sudo                         bool
	Timeout                      time.Duration                        // 已钳定
	Run                          func(ctx context.Context, t *bgTask) // T3 测试注 no-op; T4 接真引擎
	PreFinished                  bool                                 // 白盒: 直接以终态插入
	// AuditStart 回写 start(ok) 审计行 (ForProfile 传 st.WriteAudit 闭包, 闭包内
	// 只做 DB 写、无锁交互)。Insert 在持锁段内、goroutine 启动前调用——start 行
	// 必须先于任何可能的 end 行 (spec §3/§5 顺序钉死)。
	AuditStart func()
}

// TaskManager 是后台任务注册表: 槽位预约 admission (Reserve)、满员驱逐最旧终态、
// 过期清扫 sweeper、CloseAll 收尾。所有方法持 mu, 并发安全。生命周期形态照
// TunnelManager (StartSweeper/sweepLoop 的 startOnce/quit/wg, CloseAll 收口)。
type TaskManager struct {
	mu                  sync.Mutex
	tasks               map[string]*bgTask
	reserved            int
	closed              bool
	runCap, retain      time.Duration
	maxTasks            int
	now                 func() time.Time // 测试可覆写
	quit                chan struct{}
	startOnce, stopOnce sync.Once
	wg                  sync.WaitGroup
}

// NewTaskManager 解析 env seam 并构造 TaskManager, 随后启动 sweeper (生产入口)。
// SSHMGR_BG_MAX_TASKS (strconv.Atoi) / SSHMGR_BG_RUN_CAP / SSHMGR_BG_RETAIN
// (time.ParseDuration): 缺省用包级默认; 解析错或值 ≤0 → error (fail-closed——
// 非法配置拒绝启动, 不静默回落默认)。maxTasks 先于 runCap 解析, 使并发非法时
// 报告第一因。白盒测试用 newTaskManagerForTest 绕过本构造器。
func NewTaskManager() (*TaskManager, error) {
	maxTasks := bgMaxTasksDefault
	if v := os.Getenv("SSHMGR_BG_MAX_TASKS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SSHMGR_BG_MAX_TASKS: invalid value %q (want positive integer)", v)
		}
		maxTasks = n
	}
	runCap := bgRunCapDefault
	if v := os.Getenv("SSHMGR_BG_RUN_CAP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SSHMGR_BG_RUN_CAP: invalid value %q (want positive duration)", v)
		}
		runCap = d
	}
	retain := bgRetainDefault
	if v := os.Getenv("SSHMGR_BG_RETAIN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SSHMGR_BG_RETAIN: invalid value %q (want positive duration)", v)
		}
		retain = d
	}
	m := &TaskManager{
		tasks:    map[string]*bgTask{},
		runCap:   runCap,
		retain:   retain,
		maxTasks: maxTasks,
		now:      time.Now,
		quit:     make(chan struct{}),
	}
	m.StartSweeper()
	return m, nil
}

// StartSweeper 至多启动一次清扫 goroutine; CloseAll 后为 no-op。NewTaskManager
// (生产) 调用; 白盒测试不启——直接驱动 SweepExpired, 免真实 ticker 与可覆写
// 时钟 m.now 的数据竞争。
func (m *TaskManager) StartSweeper() {
	m.startOnce.Do(func() {
		m.wg.Add(1)
		go m.sweepLoop()
	})
}

// sweepLoop 每 bgSweepInterval 调一次 SweepExpired, quit 关闭 (CloseAll) 时
// 退出。持一张 wg 票, 使 CloseAll 的 wg.Wait 能等它干净退出 (照 tunnels.go)。
func (m *TaskManager) sweepLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(bgSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.SweepExpired()
		case <-m.quit:
			return
		}
	}
}

// Reserve 预约一个任务槽位 (admission, 持锁)。closed → error;
// len(tasks)+reserved >= maxTasks → 驱逐最旧终态 (非 running 中 finishedAt 最小,
// 平局按 id 字典序取小保确定性; 仅 delete from map, 零审计行); 无终态可逐 →
// error (引导文案 spec §3 原文); 否则 reserved++。预约由 Insert 转正式; Insert
// 失败时调用方必须 ReleaseReservation 归还。
func (m *TaskManager) Reserve() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("task manager closed")
	}
	if len(m.tasks)+m.reserved >= m.maxTasks {
		var victim *bgTask
		for _, t := range m.tasks {
			if t.status == bgStatusRunning {
				continue
			}
			if victim == nil || t.finishedAt.Before(victim.finishedAt) ||
				(t.finishedAt.Equal(victim.finishedAt) && t.id < victim.id) {
				victim = t
			}
		}
		if victim == nil {
			return fmt.Errorf("background task limit (%d) reached — wait for a running task to finish or call exec_stop", m.maxTasks)
		}
		delete(m.tasks, victim.id)
	}
	m.reserved++
	return nil
}

// ReleaseReservation 归还未兑现的预约 (Insert 失败路径)。与 Insert 一致钳非负——
// 白盒直插不经 Reserve, 计数不允许被推负。
func (m *TaskManager) ReleaseReservation() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reserved > 0 {
		m.reserved--
	}
}

// Insert 将预约转正式注册 (持锁)。closed → error; 生成 uuid taskID; 置
// running/startedAt/deadline=now+spec.Timeout; reserved-- (钳非负); map 写入;
// spec.AuditStart 在持锁段内、goroutine 启动前调用 (顺序钉死, 见 BgTaskSpec);
// 随后 go spec.Run(ctx, t) (ctx=Background+WithTimeout(Timeout), cancel 存 t),
// 每个 Run goroutine 持一张 wg 票。PreFinished 白盒路径直接以终态落表
// (status=done, finishedAt=now), 不 spawn Run、不置 cancel。spec.Run 为 nil 时
// 默认 no-op (T3; T4 接真引擎)。
func (m *TaskManager) Insert(spec *BgTaskSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("task manager closed")
	}
	if m.reserved > 0 {
		m.reserved--
	}
	now := m.now()
	t := &bgTask{
		id:        uuid.NewString(),
		projectID: spec.ProjectID,
		serverID:  spec.ServerID,
		command:   spec.Command,
		sudo:      spec.Sudo,
		status:    bgStatusRunning,
		startedAt: now,
		deadline:  now.Add(spec.Timeout),
	}
	if spec.PreFinished {
		t.status = bgStatusDone
		t.finishedAt = now
	}
	m.tasks[t.id] = t
	if spec.AuditStart != nil {
		spec.AuditStart()
	}
	if !spec.PreFinished {
		ctx, cancel := context.WithTimeout(context.Background(), spec.Timeout)
		t.cancel = cancel
		run := spec.Run
		if run == nil {
			run = func(context.Context, *bgTask) {} // T3 默认 no-op; T4 接真引擎
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			run(ctx, t)
		}()
	}
	return t.id, nil
}

// SweepExpired 清扫过期任务 (持锁), 返回被删除的 id 列表: 终态且
// now() > finishedAt+retain → delete (零审计行); running 且
// now() > deadline+bgGracePeriod → t.cancel() 防御性再 cancel 一次
// (引擎超时路径应已自行 cancel); 绝不删 running。
func (m *TaskManager) SweepExpired() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var swept []string
	for id, t := range m.tasks {
		if t.status == bgStatusRunning {
			if t.cancel != nil && now.After(t.deadline.Add(bgGracePeriod)) {
				t.cancel()
			}
			continue
		}
		if now.After(t.finishedAt.Add(m.retain)) {
			delete(m.tasks, id)
			swept = append(swept, id)
		}
	}
	return swept
}

// CloseAll 全量收尾 (照 spec §3 顺序): 持锁置 closed、摘全部表项 (先记待处理
// 列表)、逐任务 cancel + client 关闭回调、逐任务 notify 一次; 锁外 stopOnce
// 关 quit、wg.Wait (每个 Run goroutine 持一张 wg 票, Run 必须观察 ctx 返回,
// 故 Wait 有界)。幂等: 重复调用、与 SweepExpired/Reserve 拒绝路径组合均安全。
func (m *TaskManager) CloseAll() {
	m.mu.Lock()
	m.closed = true
	entries := make([]*bgTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		entries = append(entries, t)
	}
	for _, t := range entries {
		if t.cancel != nil {
			t.cancel()
		}
		// T4: task 的 client 关闭回调在此接入 (T3 阶段 bgTask 尚无 client 字段, 跳过)。
		// TODO(T5): 逐任务 notify 一次 (waitCh 代际广播) — 本文件唯一允许的 TODO, Task 5 接线时消除。
	}
	m.tasks = map[string]*bgTask{}
	m.mu.Unlock()
	// Signal the sweeper loop to exit (no-op if never started or already stopped)
	// and wait for it + every Run goroutine to return (mirrors tunnels.go CloseAll).
	m.stopOnce.Do(func() { close(m.quit) })
	m.wg.Wait()
}

// Len 返回注册表占用: len(tasks)+reserved (含未兑现预约)。
func (m *TaskManager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tasks) + m.reserved
}

// lookup 按 id 取任务 (持锁)。T5/6/7 内部消费 (exec_stop / result 等工具路径)。
func (m *TaskManager) lookup(id string) (*bgTask, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok
}
