package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
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
	// client 槽 (T4): 引擎入场即挂 (持锁), 终态即关; CloseAll 可达即关。
	// 终态保留期记录不持有活连接 (spec §1: session 返回即关 Client)。
	client *sshbroker.Client
	// auditEnd 由 Insert 从 spec.AuditEnd 绑定 (终态行构造在闭包内); 引擎
	// 终态置位后锁外调用。
	auditEnd func(now time.Time) error
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
	// AuditEnd 回写 exec-bg-end 终态审计行 (T4: Start 传 st.WriteAudit 闭包;
	// nil 时零终态审计)。Insert 绑定为 t.auditEnd——行由闭包调用方从 task 字段
	// 组装 (Command=taskID, Status, ExitCode, DurationMS), 引擎终态后锁外落笔。
	AuditEnd func(row store.AuditRow) error
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

// NewTaskManager 解析 env seam 并构造 TaskManager, 不启动 sweeper (照
// NewTunnelManager 先例——生产接线点 NewServerFromSource 调 StartSweeper)。
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
	return &TaskManager{
		tasks:    map[string]*bgTask{},
		runCap:   runCap,
		retain:   retain,
		maxTasks: maxTasks,
		now:      time.Now,
		quit:     make(chan struct{}),
	}, nil
}

// StartSweeper 至多启动一次清扫 goroutine; CloseAll 后为 no-op。生产入口
// NewServerFromSource 调用 (紧邻 tunnels.StartSweeper, 照 tunnels.go 先例);
// 白盒测试不启——直接驱动 SweepExpired, 免真实 ticker 与可覆写时钟 m.now
// 的数据竞争。
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

// ErrBgTaskLimit 是 Reserve 拒绝 (满员且全 running) 的底层哨兵——Start 原样
// 上抛, Task 6 的 ForProfile 以 errors.Is 识别该分支落 start(超限) 审计行
// (spec §5 start 全分支)。包裹后的完整文本与 spec §3 引导文案逐字一致。
var ErrBgTaskLimit = errors.New("background task limit")

// ErrBgManagerClosed 是 Reserve/Insert 在与 CloseAll 竞态下的拒绝哨兵
// (T6: ForProfile 以 errors.Is 识别, 把该分支与本层词汇表的 error 行对齐——
// 否则它无法与 Start 已自审计的 connect 分支区分, 会漏行或双写)。
// 文本即原先两处的字面错误, 零行为变化。
var ErrBgManagerClosed = errors.New("task manager closed")

// Reserve 预约一个任务槽位 (admission, 持锁)。closed → error;
// len(tasks)+reserved >= maxTasks → 驱逐最旧终态 (非 running 中 finishedAt 最小,
// 平局按 id 字典序取小保确定性; 仅 delete from map, 零审计行); 无终态可逐 →
// error (引导文案 spec §3 原文); 否则 reserved++。预约由 Insert 转正式; Insert
// 失败时调用方必须 ReleaseReservation 归还。
func (m *TaskManager) Reserve() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrBgManagerClosed
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
			return fmt.Errorf("%w (%d) reached — wait for a running task to finish or call exec_stop", ErrBgTaskLimit, m.maxTasks)
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
		return "", ErrBgManagerClosed
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
		waitCh:    make(chan struct{}), // T5: 代际广播初代通道 (notify 的 close+换新对象)
	}
	if spec.PreFinished {
		t.status = bgStatusDone
		t.finishedAt = now
	}
	// 每通道 1 MiB 固定容量环形缓冲 (spec §2.2: 滚动容量 = MaxOutputBytes,
	// 64 MiB/project 为真实分配上界)。T5 的观察路径对 PreFinished 任务读到
	// 恒空缓冲。
	t.stdout = sshbroker.NewRollingBuffer(MaxOutputBytes)
	t.stderr = sshbroker.NewRollingBuffer(MaxOutputBytes)
	if spec.AuditEnd != nil {
		endFn := spec.AuditEnd
		t.auditEnd = func(finish time.Time) error {
			status := t.status
			if status == bgStatusDone {
				status = "ok" // spec §5: 自然退出的终态行审计词是 ok (码值看 ExitCode)
			}
			return endFn(store.AuditRow{
				TS: finish, ProjectID: t.projectID, ServerID: t.serverID,
				Action: "exec-bg-end", Command: t.id, Sudo: t.sudo,
				Status: status, ExitCode: t.exitCode,
				DurationMS: finish.Sub(t.startedAt).Milliseconds(),
			})
		}
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
		if t.client != nil {
			_ = t.client.Close() // T4: client 槽接入——引擎可能尚未挂槽/尚未自关, 幂等双保险
		}
		m.notify(t) // T5 触发点③: 摘表广播——唤醒在途 Output 等待者 (零等待者短路)
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

// ---------- Task 4: 执行引擎 (代际广播原语 + runTask + Start 编排) ----------

// notify 唤醒全部持有旧代通道引用的等待者（代际广播, spec §2.3）。
// 调用方必须已持有 m.mu; 零等待者短路（高频输出在无人轮询时零锁开销）。
func (m *TaskManager) notify(t *bgTask) {
	if t.waiters.Load() == 0 {
		return
	}
	close(t.waitCh)
	t.waitCh = make(chan struct{})
	t.gen++
}

// notifyWrite 是写侧入口: RollingBuffer.Write 返回时其内部锁已释放,
// 此处才取 m.mu——锁序铁则（spec §2.3: 绝不持 buffer.mu 取 TM.mu）。
func (m *TaskManager) notifyWrite(t *bgTask) {
	m.mu.Lock()
	m.notify(t)
	m.mu.Unlock()
}

// notifyWriter: 落笔后先释 buffer.mu 再广播（Write 内部锁已释放, after 才取 m.mu）
type notifyWriter struct {
	buf   *sshbroker.RollingBuffer
	after func()
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.after()
	return n, err
}

// ---------- Task 5: 代际广播消费 + Output 等待回路 (spec §2.3) ----------

// BgView 是 Output 返回的观察快照, Task 7 的 exec_output ForProfile 消费。
// 字段与 spec §4 出参一一对应: 游标 (Next*) / 全流字节计数 (Total*) / 保留窗
// 首偏移 (Start*)。Truncated/Lost* 是诚实降级契约 (spec §4: offset 落后保留窗
// 首 → truncated + lost 丢弃量)——brief 原型的结构遗漏, 已按 spec 补齐并记录。
type BgView struct {
	Status   string
	ExitCode int
	ErrText  string
	Stdout   []byte
	Stderr   []byte
	// NextStdout/NextStderr: 下轮应传的游标 (三分支恒指向快照时流尾或回拉点)。
	NextStdout int64
	NextStderr int64
	// StdoutTotal/StderrTotal: 通道全流字节计数 (快照后取, 单调 → 恒 ≥ Next*)。
	StdoutTotal int64
	StderrTotal int64
	// StartStdout/StartStderr: 保留窗首字节的流内偏移 (gap 判据的锚)。
	StartStdout int64
	StartStderr int64
	// Truncated: 任一通道发生 gap (offset < 保留窗首) 即置位; Lost* = 各通道
	// 被滚动丢弃的字节数 (start-since)。
	Truncated  bool
	LostStdout int64
	LostStderr int64
}

// Output 是 exec_output 的长轮询等待回路 (=WaitFor, spec §2.3 结构钉死): 等到
// 「任一通道相对所传 offset 有新字节 / 超前游标 / 任务离开 running / 表项已失
// 或 manager 关闭 (→ unknown) / 绝对 deadline / ctx 取消」才返回。deadline 在
// 入口一次定死 (进入时 now + wait), 不随唤醒重置; timer 全程复用 (每轮
// Reset 剩余)。代捕获与条件检查在同一把 TM.mu 内 (无丢唤醒窗口); 条件只读
// totals/status (TM.mu→buffer.mu 唯一合法嵌套方向, 廉价), Snapshot 深拷贝由
// view 在 TM.mu 外做。wait 由调用方钳定 (T7 clampWaitSeconds), 此处原样生效;
// 负值等价于零 (deadline 已过 → 立即快照返回)。表项已失/manager 关闭 →
// (零值, false, nil)——三因文案由调用方 (T7) 拼装, 本层不猜原因。
func (m *TaskManager) Output(id string, so, eo int64, wait time.Duration, ctx context.Context) (BgView, bool, error) {
	deadline := m.now().Add(wait)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		m.mu.Lock()
		t, ok := m.tasks[id]
		if !ok || m.closed {
			m.mu.Unlock()
			return BgView{}, false, nil // 表项已失/manager 关闭 → unknown 三因 (调用方拼文案)
		}
		ch := t.waitCh // 代捕获+等待者计数 (与条件检查同一把锁——无丢唤醒窗口)
		st, ot := t.stdout.Total(), t.stderr.Total()
		cond := so < st || eo < ot || // 任一通道有新字节 (相对所传 offset)
			(so > 0 && so >= st) || (eo > 0 && eo >= ot) || // 超前游标立即返回 (0 = 等首字节, 不算超前)
			t.status != bgStatusRunning
		// 条件只读 totals/status (廉价)——绝不在此做 Snapshot 深拷贝 (阻塞全局锁)
		if cond {
			m.mu.Unlock()
			return m.view(t, so, eo), true, nil // 深拷贝在锁外 (view 内 Snapshot 各自持 buffer.mu, 方向合法)
		}
		t.waiters.Add(1)
		m.mu.Unlock()
		remain := deadline.Sub(m.now())
		if remain <= 0 || ctx.Err() != nil {
			t.waiters.Add(-1) // 早退路径必须配平 in-lock 注册 (Add+1)
			t2, _ := m.lookup(id)
			if t2 == nil {
				return BgView{}, false, nil
			}
			return m.view(t2, so, eo), true, nil
		}
		if timer == nil {
			timer = time.NewTimer(remain) // timer 全程复用 (每轮 Reset 剩余, 不攒 timer)
		} else {
			timer.Reset(remain)
		}
		select {
		case <-ch:
			t.waiters.Add(-1)
			continue // 唤醒后重查——虚假/陈旧唤醒不满足则续等
		case <-timer.C:
			t.waiters.Add(-1)
			t3, _ := m.lookup(id)
			if t3 == nil {
				return BgView{}, false, nil
			}
			return m.view(t3, so, eo), true, nil
		case <-ctx.Done():
			t.waiters.Add(-1)
			t4, _ := m.lookup(id)
			if t4 == nil {
				return BgView{}, false, nil
			}
			return m.view(t4, so, eo), true, nil
		}
	}
}

// view 组装 BgView: 标量 (status/exitCode/errText) 在 TM.mu 内拷贝——任务字段
// 只在持锁下变异, 不持锁读即数据竞争; 随后两通道 Snapshot 深拷贝在 TM.mu 外
// (各自持 buffer.mu, 串行无嵌套, 与 TM.mu→buffer.mu 唯一合法嵌套方向自洽)。
// gap 分支 (offset < 保留窗首) 的诚实降级由 Snapshot 三分支语义 + 此处置位:
// truncated + lost = start-since, chunk 为整个保留窗 (spec §4)。
func (m *TaskManager) view(t *bgTask, so, eo int64) BgView {
	m.mu.Lock()
	v := BgView{Status: t.status, ExitCode: t.exitCode, ErrText: t.errText}
	m.mu.Unlock()

	oChunk, oNext, oStart := t.stdout.Snapshot(so)
	eChunk, eNext, eStart := t.stderr.Snapshot(eo)
	if so < oStart { // gap: 游标落后保留窗首 → 诚实降级
		v.Truncated = true
		v.LostStdout = oStart - so
	}
	if eo < eStart {
		v.Truncated = true
		v.LostStderr = eStart - eo
	}
	v.Stdout, v.NextStdout, v.StartStdout = oChunk, oNext, oStart
	v.Stderr, v.NextStderr, v.StartStderr = eChunk, eNext, eStart
	v.StdoutTotal = t.stdout.Total() // Snapshot 后取 total (单调): 恒 ≥ Next*, 游标不倒退
	v.StderrTotal = t.stderr.Total()
	return v
}

// Stop 请求停止运行中任务 (spec §3): 持锁置 stopReq + cancel, 立即返回触发
// 时刻的 status（运行中任务即 running——status 枚举无 stopping, 终态经观察
// 路径获取, exec_stop 不阻塞）。对已终态任务幂等: 回其终态、不动 stopReq/
// cancel、零审计行。未知 id → (ok=false)。引擎侧的终态判定见 runTask 的
// stopReq 分支 (置位先于 cancel, 同在 m.mu 内——与自然退出的竞态无窗口)。
func (m *TaskManager) Stop(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return "", false
	}
	if t.status == bgStatusRunning {
		t.stopReq = true
		if t.cancel != nil {
			t.cancel()
		}
	}
	return t.status, true
}

// redactRuntimeErr 清洗运行期错误的渲染文本 (spec §4 failed 态防御性清洗)。
// 实测 (实验 2026-08-21-223410): 三种连接死亡形态的 session 层错误均为
// ExitMissingError 文本、零地址——清洗是防御性的 (库升级/未测路径可能引入
// 带地址文本), no-leak 断言网用 RST fixture 钉住。
func redactRuntimeErr(err error) string {
	if err == nil {
		return ""
	}
	return sshbroker.RedactAddr(err).Error()
}

// runTask 是后台执行引擎真身 (BgTaskSpec.Run 的生产实现, 由 Start 装配)。
// wg 票由 Insert 的 Add-before-go 持有 (每 goroutine 恰一张, 本函数不再
// Done——brief 原文的 defer m.wg.Done() 与 T3 Insert 组合会双重 Done, 已按
// "恰一张" 口径修正)。终态映射: stopReq→stopped (置位先于 cancel, 同锁无
// 窗口)、timedOut→timeout、err→failed (文本过清洗)、否则 done+exitCode。
// closed 抑制 (spec §3): manager 已关则跳过终态写入与 end 审计行 (表项已被
// CloseAll 摘除), 但 client 仍由本 goroutine 自关 (幂等)。终态即关 Client:
// 锁外关、幂等; 保留期只保 task 记录 + rollingBuffer (spec §1)。
func (m *TaskManager) runTask(ctx context.Context, t *bgTask, cli *sshbroker.Client, exec func(ctx context.Context, stdout, stderr io.Writer) (int, bool, error)) {
	// 引擎入场即挂 client 槽 (持锁): CloseAll 可达即关, 观察路径可捕获引用。
	m.mu.Lock()
	t.client = cli
	m.mu.Unlock()

	after := func() { m.notifyWrite(t) } // 锁序: buffer.mu 已释, 才进 notifyWrite 取 m.mu
	code, timedOut, rerr := exec(ctx,
		&notifyWriter{buf: t.stdout, after: after},
		&notifyWriter{buf: t.stderr, after: after})

	m.mu.Lock()
	switch {
	case m.closed: // 终态补写抑制 (spec §3 CloseAll): 状态保持 running, 表项已被摘
	case t.stopReq:
		t.status, t.finishedAt = bgStatusStopped, m.now()
	case timedOut:
		t.status, t.finishedAt = bgStatusTimeout, m.now()
	case rerr != nil:
		t.status, t.errText, t.finishedAt = bgStatusFailed, redactRuntimeErr(rerr), m.now()
	default:
		t.status, t.exitCode, t.finishedAt = bgStatusDone, code, m.now()
	}
	writeEnd := t.status != bgStatusRunning && !m.closed // 终态行由本 goroutine 落笔 (同点同锁)
	nowFn := m.now                                       // 锁内捕获时钟, 锁外调用免字段竞争
	m.notify(t)                                          // 终态广播 (spec §2.3 触发点②; 零等待者短路)
	t.cancel()                                           // 释放 WithTimeout 资源
	m.mu.Unlock()
	_ = cli.Close() // 终态即关 Client (锁外关, 幂等)
	if writeEnd && t.auditEnd != nil {
		_ = t.auditEnd(nowFn()) // exec-bg-end 行: Command=taskID, Status, ExitCode, DurationMS
	}
}

// BgStartSpec 是 Start 的入参 (Task 6 的 ForProfile 构造): profile 门/凭据/
// 主机键链已过, 这里只剩启动链。SudoPass 瞬时传递——用完即弃, 不入 task
// 记录 (spec §1: task 记录不持有任何秘密)。
type BgStartSpec struct {
	ProjectID, ServerID, Command string
	Sudo                         bool
	SudoPass                     string /*瞬时传递,用完即弃,不入 task 记录*/
	TimeoutSec                   int
	Server                       *models.Server
	Auth                         ssh.AuthMethod
	HostKeyCb                    ssh.HostKeyCallback
}

// clampBgTimeout 钳定后台生效超时 (spec §3): 0/缺省 → cap (生产即 24h
// runCap); > cap → cap; 中值直通。负值本应 schema 层拒 (spec §4)——此处
// 防御性按缺省处理。
func clampBgTimeout(sec int, cap time.Duration) time.Duration {
	d := time.Duration(sec) * time.Second
	if d <= 0 {
		d = cap
	}
	if d > cap {
		d = cap
	}
	return d
}

// Start 编排 profile 门外的全部启动链 (Task 6 只调本入口, spec §1 钉死顺序):
// Reserve（closed/超限原样上抛——该分支的 start 审计行归 ForProfile 词汇表,
// 哨兵 ErrBgTaskLimit 供 errors.Is 识别）→ ConnectKeepAlive（锁外慢连接;
// 连接失败: start 行 (connect_error/cancelled/hostkey_mismatch) 先落笔再
// ReleaseReservation, 此路径无 client 可关）→ Insert（持锁写 start(ok) 行、
// goroutine 启动前——end 行必晚于 start 行）→ runTask goroutine（Insert 的
// wg 票）。Insert 失败 (manager 已关): 锁外关 client + 归还预约, 零泄漏。
// 成功返回 taskID 与钳定后的生效超时。
func (m *TaskManager) Start(ctx context.Context, st *store.Store, spec BgStartSpec) (taskID string, effectiveTimeout time.Duration, err error) {
	t0 := time.Now()
	effective := clampBgTimeout(spec.TimeoutSec, m.runCap)
	auditStart := func(status string) {
		_ = st.WriteAudit(store.AuditRow{
			TS: t0, ProjectID: spec.ProjectID, ServerID: spec.ServerID,
			Action: "exec-bg-start", Command: spec.Command, Sudo: spec.Sudo,
			Status: status, DurationMS: time.Since(t0).Milliseconds(),
		})
	}
	if err := m.Reserve(); err != nil {
		return "", 0, err
	}
	cli, cerr := sshbroker.ConnectKeepAlive(ctx, spec.Server.Host, spec.Server.Port, spec.Server.User, spec.Auth, spec.HostKeyCb)
	if cerr != nil {
		status := "connect_error"
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		}
		auditStart(status)
		m.ReleaseReservation()
		return "", 0, cerr
	}
	exec := func(ctx context.Context, stdout, stderr io.Writer) (int, bool, error) {
		if spec.Sudo {
			return cli.ExecSudoWriters(ctx, spec.Command, spec.SudoPass, 0, stdout, stderr)
		}
		return cli.ExecWriters(ctx, spec.Command, 0, stdout, stderr)
	}
	tid, ierr := m.Insert(&BgTaskSpec{
		ProjectID:  spec.ProjectID,
		ServerID:   spec.ServerID,
		Command:    spec.Command,
		Sudo:       spec.Sudo,
		Timeout:    effective,
		AuditStart: func() { auditStart("ok") }, // Insert 持锁段内、goroutine 前调用
		AuditEnd:   func(row store.AuditRow) error { return st.WriteAudit(row) },
		Run:        func(ctx context.Context, t *bgTask) { m.runTask(ctx, t, cli, exec) },
	})
	if ierr != nil {
		_ = cli.Close() // Insert 拒绝 (manager 已关): 锁外关, 零泄漏
		m.ReleaseReservation()
		return "", 0, ierr
	}
	return tid, effective, nil
}
