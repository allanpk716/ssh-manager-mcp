# Plan 32 实施计划：后台任务三件套 exec_background / exec_output / exec_stop

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 backlog #13 三件套（长活后台命令 + 增量拉取 + 停止），前台 exec_command 钳制改响，目标发布 v0.10.0（纯增量）。

**Architecture:** `mcpserver.TaskManager` 镜像 TunnelManager（per-Server 实例、单互斥锁、sweeper）；`sshbroker` 抽 `runSession` writer-seam（前台零行为变化）+ 新固定容量环形 `rollingBuffer`；任务 goroutine 挂解耦 ctx 跑 runSession，exec_output 以字节游标拉增量、代际广播长轮询。权威设计 = `docs/superpowers/specs/2026-08-21-plan-32-background-tasks-design.md.rev3.md`（下称 **spec**）——本文任务引用其节号，冲突以 spec 为准。

**Tech Stack:** Go 1.25（go-version-file 钉）、golang.org/x/crypto/ssh（含 `ssh.KeepAliveConfig`）、官方 MCP Go SDK、in-process testsshd。

## Global Constraints

- 铁律不变：agent 永远摸不到凭据；新工具全部 profile 门控、审计、并自动并入 `BrokerTools` 单源切片。
- **锁序（spec §2.3，全文唯一允许的嵌套方向）**：`TaskManager.mu → rollingBuffer.mu`；任何路径在持有 rollingBuffer.mu 期间**绝不**获取 TaskManager.mu（Write 落笔释锁后才 notify）。
- 常量与环境 seam（spec §3）：`SSHMGR_BG_RUN_CAP`（默认 24h）/`SSHMGR_BG_RETAIN`（默认 1h）/`SSHMGR_BG_MAX_TASKS`（默认 32）——构造期 env 覆盖，**非法值或非正数 → 构造报错拒绝启动**（fail-closed）。包级 var（可被同包测试缩时）。
- 数值入参 schema 一律拒负（`timeout_seconds`/`wait_seconds`/`stdout_offset`/`stderr_offset`），`encoding` 为 enum `["text","base64"]` 拒非法值；`0`/缺省保留默认语义。
- 输出编码两态（spec §4）：text=U+FFFD 有损（与前台同语义）、base64=字节精确；offset 恒为字节口径。
- 非 ASCII 编辑**逐字节验证**（Plan 25 教训）；仓库 `.gitattributes` 强制 LF，勿引入 CRLF。
- 每个 Task 收尾：`gofmt -l .` 为空 + `go build ./... && go test ./...` 全绿（gated 套件自跳过）+ commit。
- 计划里的 verbatim 代码是**可证伪下限**：实现者发现平台/库行为与计划冲突时，以实证为准并在任务报告记录（Plan 26 教训），但接口签名不得擅改。

---

### Task 1: sshbroker.rollingBuffer——固定容量环形 + 三分支 Snapshot + 深拷贝

**Files:**
- Create: `internal/sshbroker/rolling.go`
- Test: `internal/sshbroker/rolling_test.go`

**Interfaces:**
- Produces: `type RollingBuffer struct{...}`；`func NewRollingBuffer(cap int64) *RollingBuffer`；`func (b *RollingBuffer) Write(p []byte) (int, error)`（io.Writer）；`func (b *RollingBuffer) Snapshot(since int64) (chunk []byte, next, start int64)`；`func (b *RollingBuffer) Total() int64`。Task 4 的 notifyWriter 与 Task 7 的编码装配消费它们。

- [ ] **Step 1: 写失败测试**（`rolling_test.go`，覆盖 spec §7 rollingBuffer 行全部用例）

```go
package sshbroker

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestRollingBufferCursorBranches(t *testing.T) {
	b := NewRollingBuffer(8)
	b.Write([]byte("0123456789ABCD")) // total=14, 保留尾 8 字节 "789ABCD" 之外——ring= "7ABCD..."? 断言按三分支:
	// 正常分支: since=10 → chunk="ABCD", next=14
	c, next, start := b.Snapshot(10)
	if string(c) != "ABCD" || next != 14 || start != 6 {
		t.Fatalf("normal: chunk=%q next=%d start=%d", c, next, start)
	}
	// 超前分支: since=99 → 空 chunk + next 回拉 total
	c, next, _ = b.Snapshot(99)
	if len(c) != 0 || next != 14 {
		t.Fatalf("ahead: chunk=%q next=%d", c, next)
	}
	// gap 分支: since=0 (< start=6) → 整窗 + next=total
	c, next, start = b.Snapshot(0)
	if string(c) != "789ABCD" || next != 14 || start != 6 {
		t.Fatalf("gap: chunk=%q next=%d start=%d", c, next, start)
	}
}

func TestRollingBufferCapZeroRetainsNothing(t *testing.T) {
	b := NewRollingBuffer(0)
	b.Write([]byte("xyz"))
	if _, next, start := b.Snapshot(0); next != 3 || start != 3 {
		t.Fatalf("cap=0: next=%d start=%d (want 3/3)", next, start)
	}
}

func TestRollingBufferSnapshotIsDeepCopy(t *testing.T) {
	b := NewRollingBuffer(4)
	b.Write([]byte("AAAA"))
	c, _, _ := b.Snapshot(0)
	b.Write([]byte("BBBB")) // 丢头滚动——若 Snapshot 返回内部视图, c 已被腐蚀
	if string(c) != "AAAA" {
		t.Fatalf("corroded: %q", c)
	}
	if s, _, _ := b.Snapshot(2); string(s) != "BB" {
		t.Fatalf("after roll: %q", s)
	}
}

func TestRollingBufferAllocationBounded(t *testing.T) {
	b := NewRollingBuffer(1024)
	for i := 0; i < 1000; i++ {
		b.Write(bytes.Repeat([]byte("x"), 64)) // 反复写超量
	}
	if cap(b.buf) != 1024 { // 分配恒等于 cap（固定容量承诺, 白盒）
		t.Fatalf("cap grew: %d", cap(b.buf))
	}
}

func TestRollingBufferConcurrentReadWrite(t *testing.T) {
	b := NewRollingBuffer(256)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); for i := 0; i < 2000; i++ { b.Write([]byte(strings.Repeat("a", 7))) } }()
	go func() { defer wg.Done(); for i := 0; i < 2000; i++ { b.Snapshot(int64(i * 3)) } }()
	wg.Wait()
	if b.Total() != 2000*7 {
		t.Fatalf("total=%d", b.Total())
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/sshbroker/ -run TestRollingBuffer -v` → Expected: FAIL（未定义 NewRollingBuffer）

- [ ] **Step 3: 实现**（`rolling.go`）

```go
package sshbroker

import "sync"

// RollingBuffer is an io.Writer that retains the LAST cap bytes of the stream
// (the rolling tail — the mirror of cappedBuffer's first-N prefix retention)
// while counting every byte in total. Unlike an append-and-drop-front slice,
// the backing array is allocated once at exactly cap bytes and never grows:
// the 64 MiB/project memory ceiling in the spec is a real allocation bound.
//
// Snapshot deep-copies under the buffer lock — it never returns a view of the
// internal array (an escaping view would be corrupted by later rolling writes,
// a corruption the race detector cannot see because it happens under the lock).
type RollingBuffer struct {
	mu    sync.Mutex
	buf   []byte // len ≤ cap(buf); capacity fixed at construction
	total int64
}

func NewRollingBuffer(cap int64) *RollingBuffer {
	if cap <= 0 {
		return &RollingBuffer{} // retains nothing, counts only (cap=0 boundary)
	}
	return &RollingBuffer{buf: make([]byte, 0, cap)}
}

func (b *RollingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	c := int64(cap(b.buf))
	if c == 0 {
		return n, nil
	}
	if int64(n) > c { // 一次写超整窗: 只留尾 cap 字节
		p = p[int64(n)-c:]
	}
	if int64(len(b.buf)+len(p)) > c { // 丢头滚动(copy 挪动, 容量不变)
		drop := int64(len(b.buf)+len(p)) - c
		copy(b.buf, b.buf[drop:])
		b.buf = b.buf[:len(b.buf)-int(drop)]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

// Snapshot returns the bytes after stream offset `since`, per the spec's three
// pinned branches. start = the stream offset of the first retained byte.
//   since <  start (gap):    whole retained window, caller reports lost=start-since; next = total
//   since >= total (ahead):  empty chunk, cursor pulled back; next = total
//   otherwise (normal):      the window tail; next = since + len(chunk)
func (b *RollingBuffer) Snapshot(since int64) (chunk []byte, next, start int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start = b.total - int64(len(b.buf))
	if start < 0 {
		start = 0
	}
	switch {
	case since >= b.total:
		return nil, b.total, start
	case since < start:
		out := make([]byte, len(b.buf))
		copy(out, b.buf)
		return out, b.total, start
	default:
		i := since - start
		out := make([]byte, int64(len(b.buf))-i)
		copy(out, b.buf[i:])
		return out, since + int64(len(out)), start
	}
}

func (b *RollingBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}
```

- [ ] **Step 4: 跑测试确认全绿（-race）**

Run: `go test ./internal/sshbroker/ -run TestRollingBuffer -v -race` → Expected: 5/5 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sshbroker/rolling.go internal/sshbroker/rolling_test.go
git commit -m "feat(sshbroker): rollingBuffer 固定容量环形——尾N保留+三分支游标+锁内深拷贝(Plan 32 T1)"
```

---

### Task 2: runSession / runSudoSession writer-seam 抽取（前台零行为）

**Files:**
- Modify: `internal/sshbroker/exec.go`（Exec 现体拆内核）
- Modify: `internal/sshbroker/sudo.go`（ExecSudo 同型拆）
- Test: `internal/sshbroker/seam_test.go`（新增; 既有 exec/sudo 测试**零改动**即回归网）

**Interfaces:**
- Produces: `func (c *Client) runSession(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error)`；`func (c *Client) runSudoSession(ctx context.Context, cmd, pass string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error)`——Task 4 的任务 goroutine 消费。

- [ ] **Step 1: 读两文件**：Read `exec.go` 与 `sudo.go` 全文（拆移不改行为的下限是逐行对照）。

- [ ] **Step 2: 写失败测试**（`seam_test.go`——证明内核收外部 writers）

```go
package sshbroker

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

func TestRunSessionWritersReceiveDirect(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(string, io.Reader) (string, string, int) { return "OUT", "ERR", 3 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	var mu sync.Mutex
	var out, errb strings.Builder
	code, timedOut, err := c.runSession(context.Background(), "x", 0, writerFunc(func(p []byte) { mu.Lock(); out.Write(p); mu.Unlock() }), writerFunc(func(p []byte) { mu.Lock(); errb.Write(p); mu.Unlock() }))
	if err != nil || timedOut || code != 3 || out.String() != "OUT" || errb.String() != "ERR" {
		t.Fatalf("code=%d timedOut=%v err=%v out=%q err=%q", code, timedOut, err, out.String(), errb.String())
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }
```

- [ ] **Step 3: 跑测试确认失败**：`go test ./internal/sshbroker/ -run TestRunSessionWriters -v` → FAIL（runSession 未定义）

- [ ] **Step 4: 抽取**——`Exec` 现体（exec.go:33-86）逐行搬入 `runSession`：NewSession、watchdog goroutine（`Signal(SIGKILL)+sess.Close()` on ctx.Done、`done` channel）、`sess.Run(cmd)`、ctx.Err() 三分类（DeadlineExceeded→timedOut=true+nil err / Canceled→ctx.Err() / ExitError→exitCode+nil / 其余 err）。差别只有：stdout/stderr 从参数 io.Writer 来（替代 cappedBuffer 构造），返回三元组。`Exec` 变薄壳：

```go
func (c *Client) Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	exitCode, timedOut, err := c.runSession(ctx, cmd, timeout, stdout, stderr)
	res := ExecResult{
		Stdout: stdout.buf.String(), Stderr: stderr.buf.String(),
		StdoutBytes: stdout.total, StderrBytes: stderr.total,
		Truncated: stdout.truncated || stderr.truncated,
		ExitCode: exitCode, TimedOut: timedOut,
	}
	if err == nil || timedOut || isExitOnly(err) { // ExitError 已折进 exitCode, 薄壳不再重复分类
		return res, nil
	}
	return res, err
}
```

`isExitOnly` 若拆移后不再需要（ExitError 在内核已折算）则不引入——以「既有 exec_test.go 全部用例不改一字仍绿」为验收。`runSudoSession` 同型：保留 sudo.go 的密码喂入舞步（`sudo -S -p '' --` 拼接、密码行写 stdin、flush、close），仅 writers/返回值同构化；`ExecSudo` 变薄壳组装 ExecResult。

- [ ] **Step 5: 回归网验证**

Run: `go test ./internal/sshbroker/ -v` → Expected: 既有全部 PASS（含 exec_test/sudo_test/client_test 零改动）+ 新 seam 测试 PASS

- [ ] **Step 6: Commit**

```bash
git add internal/sshbroker/exec.go internal/sshbroker/sudo.go internal/sshbroker/seam_test.go
git commit -m "refactor(sshbroker): runSession/runSudoSession writer-seam 抽取——前台 Exec/ExecSudo 薄壳化, 既有测试零改动(Plan 32 T2)"
```

---

### Task 3: TaskManager 注册表 + 槽位预约 admission + 满员驱逐 + sweeper + env seam

**Files:**
- Create: `internal/mcpserver/tasks.go`
- Test: `internal/mcpserver/tasks_test.go`

**Interfaces:**
- Produces: `func NewTaskManager() (*TaskManager, error)`（env seam 解析，非法/非正 → error）；`func (m *TaskManager) Reserve() error`；`func (m *TaskManager) ReleaseReservation()`；`func (m *TaskManager) Insert(t *BgTaskSpec) (taskID string, err error)`（预约转正式；BgTaskSpec 在本任务定义为最小骨架：ProjectID/ServerID/Command/Sudo/TimeoutSec + 可注入的 run 字段，Task 4 才填真执行）；`func (m *TaskManager) SweepExpired() []string`；`func (m *TaskManager) CloseAll()`；`func (m *TaskManager) Len() int`（含预约）；`func (m *TaskManager) lookup(id string) (*bgTask, bool)`（Task 5/6/7 内部消费）。测试注入用 `m.now = func() time.Time`（包级可覆写时钟）与 `m.run`（任务体 func(ctx, *bgTask)，T3 默认 no-op 让白盒测试不依赖 ssh）。

- [ ] **Step 1: 写失败测试**（覆盖 spec §7 TaskManager 行：上限/驱逐/预约/admission 并发/closed 拒绝/sweeper/env seam；无执行引擎——run 注入 no-op）

```go
package mcpserver

import (
	"sync"
	"testing"
	"time"
)

func newTestTM(t *testing.T, max int) *TaskManager {
	t.Helper()
	m, err := newTaskManagerForTest(max, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAdmissionCapAndEviction(t *testing.T) {
	m := newTestTM(t, 3)
	// 三个终态任务占满
	for i := 0; i < 3; i++ {
		id, err := m.Insert(finishedSpec(i)) // helper: 直接以终态插入(白盒置 status/finishedAt)
		if err != nil {
			t.Fatal(err)
		}
		_ = id
	}
	// 满员驱逐: Reserve 成功(驱逐最旧终态) → Insert 成功
	if err := m.Reserve(); err != nil {
		t.Fatalf("reserve with evictable: %v", err)
	}
	if _, err := m.Insert(runningSpec()); err != nil {
		t.Fatal(err)
	}
	// 现剩 2 终态(最旧被逐) + 1 running; 全 running 才拒绝:
	m2 := newTestTM(t, 1)
	if _, err := m2.Insert(runningSpec()); err != nil { t.Fatal(err) }
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
			if err := m.Reserve(); err != nil { errs <- err; return }
			// 模拟锁外慢 Connect
			if _, err := m.Insert(runningSpec()); err != nil { m.ReleaseReservation(); errs <- err }
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

func TestEnvSeamValidation(t *testing.T) {
	for _, v := range []string{"0", "-5s", "abc"} {
		t.Setenv("SSHMGR_BG_RUN_CAP", v)
		if _, err := NewTaskManager(); err == nil {
			t.Fatalf("run cap %q must be refused", v)
		}
	}
	t.Setenv("SSHMGR_BG_MAX_TASKS", "0")
	if _, err := NewTaskManager(); err == nil {
		t.Fatal("max tasks 0 must be refused")
	}
}
```

（helpers `finishedSpec`/`runningSpec`/`newTaskManagerForTest` 在测试文件内实现：直接构造带 status/finishedAt 的插入路径——`Insert` 接受 spec 里的 `preFinished bool` 仅供白盒。）

- [ ] **Step 2: 跑测试确认失败**：`go test ./internal/mcpserver/ -run 'TestAdmission|TestReserve|TestSweeper|TestEnvSeam' -v` → FAIL

- [ ] **Step 3: 实现 tasks.go 骨架**

```go
package mcpserver

import (
	"context"
	"errors"
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
	bgRunCapDefault    = 24 * time.Hour
	bgRetainDefault    = time.Hour
	bgMaxTasksDefault  = 32
)

const bgSweepInterval = time.Minute

type bgTask struct {
	id        string
	projectID string
	serverID  string
	command   string
	sudo      bool
	status    string // running|done|stopped|timeout|failed
	exitCode  int
	errText   string
	stdout, stderr *sshbroker.RollingBuffer
	cancel    context.CancelFunc
	stopReq   bool
	startedAt time.Time
	finishedAt time.Time
	deadline  time.Time
	// 代际广播（Task 5 用; 本任务先占位字段）
	gen    uint64
	waitCh chan struct{}
	waiters atomic.Int32
}

type BgTaskSpec struct {
	ProjectID, ServerID, Command string
	Sudo bool
	Timeout time.Duration // 已钳定
	Run   func(ctx context.Context, t *bgTask) // T3 测试注 no-op; T4 接真引擎
	PreFinished bool // 白盒: 直接以终态插入
}

type TaskManager struct {
	mu       sync.Mutex
	tasks    map[string]*bgTask
	reserved int
	closed   bool
	runCap, retain time.Duration
	maxTasks int
	now      func() time.Time // 测试可覆写
	quit     chan struct{}
	startOnce, stopOnce sync.Once
	wg       sync.WaitGroup
}
```

`NewTaskManager`：读三 env（`SSHMGR_BG_RUN_CAP`/`SSHMGR_BG_RETAIN` 用 `time.ParseDuration`、`SSHMGR_BG_MAX_TASKS` 用 `strconv.Atoi`；**解析错或值 ≤0 → 返回 error**；缺省用包级默认），构造并 `StartSweeper`（照 tunnels.go:74-96 的 startOnce/quit/wg 形态，tick 调 SweepExpired）。`Reserve`（持锁）：closed → error「task manager closed」；`len(tasks)+reserved >= maxTasks` → 找最旧终态（`finishedAt` 最小的非-running）驱逐（delete from map，**零审计行**），无终态可逐 → error「background task limit (N) reached — wait for a running task to finish or call exec_stop」（引导文案 spec §3 原文）；否则 `reserved++`。`ReleaseReservation`（持锁）`reserved--`。`Insert`（持锁）：closed → error；生成 uuid taskID、置 running/startedAt/deadline=now+spec.Timeout、`reserved--`、map 写入；`start(ok)` 审计行由调用方（Task 6 的 ForProfile）在 Insert 前写还是这里写？——**在 Insert 持锁段内、`spec.Run` goroutine 启动前**（spec §3/§5 顺序钉死），故 Insert 需要审计回写：spec 加字段 `AuditStart func()`（ForProfile 传入闭包 `st.WriteAudit(start行)`），Insert 在持锁段调用（闭包内只做 DB 写，无锁交互）。随后 `go spec.Run(ctx, t)`（ctx=Background+WithTimeout(Timeout)，cancel 存 t）。`SweepExpired`（持锁）：终态且 `now()>finishedAt+retain` → delete；running 且 `now()>deadline+1min`（宽限）→ `t.cancel()` 再 cancel 一次（防御）；**绝不删 running**。`CloseAll`：照 spec §3 顺序——持锁置 closed、摘全部表项（先记待处理列表）、逐任务 `cancel`+`client 关闭回调`（T3 阶段 task.client 为 nil 跳过；T4 接入）、**逐任务 notify 一次**（T5 实装 notify；本任务留 TODO 注释「T5 接线」——**唯一允许的 TODO，T5 必须消除**）；锁外 `stopOnce`+`wg.Wait`（每个 Run goroutine 持一张 wg 票）。`Len`：`len(tasks)+reserved`。`lookup`：持锁取。

- [ ] **Step 4: 跑测试确认全绿**：`go test ./internal/mcpserver/ -run 'TestAdmission|TestReserve|TestSweeper|TestEnvSeam' -v -race` → PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/tasks.go internal/mcpserver/tasks_test.go
git commit -m "feat(mcpserver): TaskManager 注册表——槽位预约 admission+满员驱逐最旧终态+sweeper+env seam 非正拒绝(Plan 32 T3)"
```

---

### Task 4: 后台执行引擎（goroutine + runSession + notifyWriter + 终态映射 + 审计 end + client 即关 + keepalive + closed 抑制）

**Files:**
- Modify: `internal/mcpserver/tasks.go`（BgTaskSpec.Run 的真实现 `runTask`；keepalive 连接变体）
- Modify: `internal/sshbroker/connect.go`（`ConnectKeepAlive` 变体）
- Test: `internal/mcpserver/tasks_exec_test.go`（testsshd 生命周期 e2e）

**Interfaces:**
- Consumes: T1 `RollingBuffer`、T2 `runSession/runSudoSession`
- Produces: `type BgStartSpec struct { ProjectID, ServerID, Command string; Sudo bool; SudoPass string /*瞬时传递,用完即弃,不入 task 记录*/; TimeoutSec int; Server *models.Server; Auth sshbroker.AuthMethod; HostKeyCb ssh.HostKeyCallback }`；`func (m *TaskManager) Start(ctx context.Context, st *store.Store, spec BgStartSpec) (taskID string, effectiveTimeout time.Duration, err error)`——Task 6 的 ForProfile 只调这一个入口（含 Reserve/Connect/Insert 编排）；`sshbroker.ConnectKeepAlive(ctx, host, port, user, auth, hkCb) (*Client, error)`；`func (m *TaskManager) notify(t *bgTask)`（调用方持 m.mu；零等待者短路+代际 close+换新——**本任务实装**，T5 的等待回路与测试消费它）。

- [ ] **Step 1: ConnectKeepAlive 变体**——`connect.go` 现有 Connect 抽出 `connectWith(ctx, ..., ka *ssh.KeepAliveConfig)`；`Connect` 传 nil（零行为变化）；`ConnectKeepAlive` 传 `&ssh.KeepAliveConfig{Interval: 30 * time.Second, CountMax: 3}`（连不上 3 次无响应判死→连接错误→任务 failed）。先在 `connect_knob_test.go` 风格加单测：KeepAlive 变体能连 testsshd 并 Exec 成功（不测判死本身——真连接冒烟归 conformance T10）。

- [ ] **Step 2: 写失败测试**（`tasks_exec_test.go`；testsshd 起真任务）

```go
func TestBackgroundLifecycleIncremental(t *testing.T) {
	// testsshd: 逐行输出由 Exec handler 一次性返回——增量语义用两段任务近似:
	// 任务1 快速产出→运行中 exec_output 拿到前段; 终态后取尾 offset 到 next==total 无新增(spec §7 排空锚)
	...
	out := m.Output(id, 0, 0, OutputOpts{Wait: 50 * time.Millisecond}) // 运行中快照
	...
}

func TestBackgroundStopPath(t *testing.T)  // sleep 任务 → Stop → status 变 stopped; client 已关(捕获 ref 后续 op 报错)
func TestBackgroundTimeoutPath(t *testing.T) // Timeout=200ms 跑 sleep 2s → status=timeout
func TestBackgroundFailedRSTPath(t *testing.T) // RST 代理 fixture(从 .xcheck 实验装置产品化: killableProxy 移入 testsshd 包或本包 helper) → status=failed; errText 恒等 ExitMissingError 文本(零地址)+过 redactAddr
func TestBackgroundAuditEndRows(t *testing.T)  // done/stopped/timeout/failed 四态各落一行 exec-bg-end(Command=taskID, Status/Dur 断言); start(ok) 行先于 end(时间戳/顺序断言)
func TestBackgroundClientClosedAtTerminal(t *testing.T) // 终态后记录仍在(Output 可读)而 client 已关
func TestBackgroundCloseAllSuppressesEndRows(t *testing.T) // CloseAll 杀 running → 零 exec-bg-end 补写行
```

（完整断言体量较大，按上述骨架每个用例写全——执行器拿到本任务时 testsshd.Options.Exec 的形态见 `exec_test.go:27-35`。）

- [ ] **Step 3: 实现**（tasks.go 追加——代际广播原语 + runTask 引擎）

```go
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

func (m *TaskManager) runTask(ctx context.Context, t *bgTask, cli *sshbroker.Client, exec func(ctx context.Context, stdout, stderr io.Writer) (int, bool, error)) {
	defer m.wg.Done()
	code, timedOut, rerr := exec(ctx, &notifyWriter{buf: t.stdout, after: m.notifyWrite(t)}, &notifyWriter{buf: t.stderr, after: m.notifyWrite(t)})
	m.mu.Lock()
	switch {
	case m.closed: // 终态补写抑制（spec §3 CloseAll）
	case timedOut:
		t.status, t.finishedAt = "timeout", m.now()
	case rerr != nil:
		t.status, t.errText, t.finishedAt = "failed", redactRuntimeErr(rerr), m.now()
	default:
		t.status, t.exitCode, t.finishedAt = "done", code, m.now()
	}
	writeEnd := t.status != "running" && !m.closed // 终态行由本 goroutine 落笔（同点同锁）
	t.cancel() // 释放 WithTimeout 资源
	m.mu.Unlock()
	_ = cli.Close() // 终态即关 Client（锁外关, 幂等）
	if writeEnd {
		_ = t.auditEnd(m.now()) // exec-bg-end 行: Command=taskID, Status, ExitCode, DurationMS
	}
}
```

`redactRuntimeErr`：对 err 文本跑与 `redactAddr` 同源清洗（`internal/sshbroker` 导出 `RedactAddr(err error) error`——若 redactAddr 未导出，本任务顺手导出薄包装，不改其行为）。`Start`（编排入口，Task 6 消费）：profile 门外的全部启动链（Reserve → ConnectKeepAlive（连接错误：ReleaseReservation+锁外已无 client）→ Insert(持锁写 start(ok) 审计) → runTask goroutine），连接失败分支的 start 审计行（connect_error 等）由 Start 在 Release 前落笔。

- [ ] **Step 4: 跑测试**：`go test ./internal/mcpserver/ -run 'TestBackground' -v -race` → 全 PASS；`go test ./...`（前台回归网仍绿）

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/tasks.go internal/mcpserver/tasks_exec_test.go internal/sshbroker/connect.go internal/sshbroker/connect_knob_test.go
git commit -m "feat(mcpserver): 后台执行引擎——runSession+notifyWriter+四终态映射+client 即关+keepalive+closed 补写抑制(Plan 32 T4)"
```

---

### Task 5: 代际广播 + WaitFor 等待回路（虚假唤醒/广播/预算/短路/锁序）

**Files:**
- Modify: `internal/mcpserver/tasks.go`（notify/notifyWrite/Output(=WaitFor) 实装，消除 T3 的 TODO）
- Test: `internal/mcpserver/tasks_wait_test.go`

**Interfaces:**
- Consumes: T4 `notify`/`notifyWrite`（已实装）
- Produces: `type BgView struct { Status string; ExitCode int; ErrText string; Stdout, Stderr []byte; NextStdout, NextStderr, StdoutTotal, StderrTotal, StartStdout, StartStderr int64 }`；`func (m *TaskManager) Output(id string, stdoutOff, stderrOff int64, wait time.Duration, ctx context.Context) (BgView, bool /*taskFound*/, error)`——Task 7 的 exec_output ForProfile 消费。unknown 由 bool+error 表达（错误文本=三因文案，Task 7 拼装）。本任务还补两处 notify 接线：runTask 终态写入持锁段调 `m.notify(t)`；CloseAll 摘表循环调 `m.notify(t)`（消除 T3 遗留 TODO）。

- [ ] **Step 1: 写失败测试**

```go
func TestWaitSpuriousWakeHolds(t *testing.T) {
	// 运行中任务, 无数据; 白盒直接 m.notifyWrite(t) 注入虚假广播若干次
	// 断言 Output(wait=300ms) 恰在 ~300ms 返回(虚假唤醒不提前), 且返回快照空
}
func TestWaitBroadcastWakesAllWaiters(t *testing.T) {
	// 3 个并发 Output 等同一任务; 写入新字节 → 三者全醒(各自拿到同增量)
}
func TestWaitBudgetNotExtended(t *testing.T) {
	// wait=300ms; 循环 20 次每 10ms 注入虚假 notify → 总阻塞 ≤ 300ms+ε(绝对 deadline 不重置)
}
func TestNotifyShortCircuitZeroWaiters(t *testing.T) {
	// 无等待者时 notifyWrite 不推进 gen(白盒断言 t.gen 不变)
}
func TestWaitAheadOffsetReturnsImmediately(t *testing.T) {
	// stdoutOff=999 超前 + wait=5s → 立即返回 next=total 回拉
}
func TestLockOrderStressNoDeadlock(t *testing.T) {
	// 并发: 一个 goroutine 持续 Output(等待回路, TM.mu↔buffer.mu 嵌套), 一个持续写 buffer+notify
	// 测试超时 10s 兜底: ABBA 死锁表现为 hang 而非失败
}
```

- [ ] **Step 2: 跑测试确认失败**（Output/notify 未定义）

- [ ] **Step 3: 实现**（spec §2.3 伪代码逐行落 Go；此处为完整形态。`notify`/`notifyWrite` 已在 T4 实装，本任务只加 Output 与两处接线）

```go
func (m *TaskManager) Output(id string, so, eo int64, wait time.Duration, ctx context.Context) (BgView, bool, error) {
	deadline := m.now().Add(wait)
	var timer *time.Timer
	defer func() { if timer != nil { timer.Stop() } }()
	for {
		m.mu.Lock()
		t, ok := m.tasks[id]
		if !ok || m.closed {
			m.mu.Unlock()
			return BgView{}, false, nil // 表项已失/manager 关闭 → unknown 三因（调用方拼文案）
		}
		ch := t.waitCh // 代捕获（与条件检查同一把锁——无丢唤醒）
		st, ot := t.stdout.Total(), t.stderr.Total()
		cond := so < st || eo < ot || // 任一通道有新字节（相对所传 offset）
			(so > 0 && so >= st) || (eo > 0 && eo >= ot) || // 超前游标立即返回（so=0/total=0 = 等首字节, 不算超前）
			t.status != "running"
		// 条件只读 totals/status（廉价）——绝不在此做 Snapshot 深拷贝
		if cond {
			m.mu.Unlock()
			return m.view(t, so, eo), true, nil // 深拷贝在锁外（view 内 Snapshot 各自持 buffer.mu, 方向合法）
		}
		m.mu.Unlock()
		remain := deadline.Sub(m.now())
		if remain <= 0 || ctx.Err() != nil {
			t2, _ := m.lookup(id)
			if t2 == nil {
				return BgView{}, false, nil
			}
			return m.view(t2, so, eo), true, nil
		}
		t.waiters.Add(1)
		if timer == nil {
			timer = time.NewTimer(remain) // timer 全程复用（Reset 剩余, 不攒 timer）
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
```

（`m.view` 组装 BgView：两通道 `Snapshot(so/eo)` 各自产出 chunk/next/start；`t.waiters.Done()` 用 `Add(-1)` 语义按实际实现校正命名。实现者按编译器反馈微调命名，**回路结构与锁序不得偏离**。）同步消除 T3 遗留 TODO：CloseAll 摘表循环里调 `m.notify(t)`；终态写入处（runTask 持锁段）补 `m.notify(t)`。

- [ ] **Step 4: 跑测试**：`go test ./internal/mcpserver/ -run 'TestWait|TestNotify|TestLockOrder' -v -race` → PASS（锁序压测 10s 超时不 hang）

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/tasks.go internal/mcpserver/tasks_wait_test.go
git commit -m "feat(mcpserver): 代际广播+等待回路——零等待者短路+绝对 deadline+虚假唤醒续等+锁序压测(Plan 32 T5)"
```

---

### Task 6: exec_background 工具（ForProfile 全错误分支 + no-leak 网 + NewServer* 接线）

**Files:**
- Create: `internal/mcpserver/bgtools.go`（三个 ForProfile 落此；本任务只做 start）
- Modify: `internal/mcpserver/server.go`（BrokerTools 追加三名 + 注册 + 描述；NewServer/NewServerFromSource 返回值加 `*TaskManager`）
- Modify: `internal/mcpserver/run.go`、`internal/mcpserver/serve.go`（三模式接线：scopedServer 持 tasks；RunStdio/RunStdioCache defer CloseAll；ServeRunner.Close 关 tasks）
- Test: `internal/mcpserver/bgtools_test.go`

**Interfaces:**
- Consumes: T4 `TaskManager.Start`
- Produces: `func ExecBackgroundForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, command string, sudo bool, timeoutSec int, mgr *TaskManager) (out BgStartOutput, err error)`；`type BgStartOutput struct { TaskID string; EffectiveTimeoutSeconds int; Status string }`（types.go 注册 jsonschema；T7 的 BgReadOutput/BgStopOutput 同族命名）。`BrokerTools = [..., "exec_background", "exec_output", "exec_stop"]`（index 6/7/8——本任务起单源切片就位，T7 注册余二）。

- [ ] **Step 1: 写失败测试**——profile 拒绝（denied）/ no_credential / connect_error（`127.0.0.1:1` 真触发）/ no_sudo / 超限（max=1 白盒）/ 成功路径（testsshd，断言 task_id 非空 + effective 回显 + audit `exec-bg-start` 行全分支状态）。**no-leak 断言网**：照 Plan 31 `assertBranch` 分支签名锁形态，对每个错误分支断言 `err.Error()` 不含 `srv.Host` 且不匹配 IP 正则。

- [ ] **Step 2: 确认失败** → **Step 3: 实现**：`ExecBackgroundForProfile` 骨架逐分支镜像 `ExecCommandForProfile`（core.go:85-191 的门链 + 状态映射 + defer 审计 start 行，Action=`exec-bg-start`、全分支词汇表含`超限`），差别：timeout 用 `clampBackgroundTimeout`（0→runCap、>runCap→runCap，回显 effective）+ 成功路径走 `mgr.Start`（内含 Reserve/ConnectKeepAlive/Insert/start(ok)）。Connect 在 Reserve 之后（spec §1 admission 顺序）。server.go：BrokerTools 追加三名；NewServerFromSource 构造 `tasks, err := NewTaskManager()`（err 透传——签名从 3 值变 4 值）并注册 exec_background 工具（描述文案照 spec §8「Agent 说明书」风格写足：server_id 寻址/sudo 语义/24h 与 effective/32 上限与驱逐/重启即失/`cd /dir && VAR=x cmd` 惯用法/长活走后台短活走前台）；run.go/serve.go 全调用点更新（编译器驱动逐一改）+ defer tasks.CloseAll / ServeRunner.Close 循环补 `s.tasks.CloseAll()`。

- [ ] **Step 4: 全绿**：`go build ./... && go test ./... -race`（既有 e2e/serve/run 测试因新签名可能需**机械适配**——允许改调用点形参，不许改断言语义）

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/bgtools.go internal/mcpserver/bgtools_test.go internal/mcpserver/server.go internal/mcpserver/run.go internal/mcpserver/serve.go internal/mcpserver/types.go
git commit -m "feat(mcpserver): exec_background 工具+三模式 TaskManager 接线+BrokerTools 单源扩三(Plan 32 T6)"
```

---

### Task 7: exec_output / exec_stop 工具 + 前台 effective_timeout_seconds

**Files:**
- Modify: `internal/mcpserver/bgtools.go`、`internal/mcpserver/types.go`、`internal/mcpserver/server.go`（注册余二工具）
- Modify: `internal/mcpserver/core.go`（ExecOutput 加字段 + ExecCommandForProfile 回显）
- Test: `internal/mcpserver/bgtools_test.go`（追加）

**Interfaces:**
- Consumes: T5 `Output`；`TaskManager.Stop(id) (string, bool)`（本任务实现：持锁置 stopReq+cancel，返回触发时刻 status；已终态幂等回终态零审计行）
- Produces: `func ExecOutputForProfile(ctx context.Context, st *store.Store, projectID, taskID string, waitSec int, stdoutOff, stderrOff int64, encoding string, mgr *TaskManager) (out BgReadOutput, err error)`；`func ExecStopForProfile(ctx, st, projectID, taskID, mgr) (out BgStopOutput, err error)`。jsonschema：`ExecCommandInput` 不变；新 Input/Output 类型字段含 `encoding enum`（MCP SDK 的 jsonschema tag 写 `enum:"text,base64"` 形态——按 SDK 实际语法）与四数值参数 `minimum:"0"`；`ExecOutput` 增 `EffectiveTimeoutSeconds int json:"effective_timeout_seconds"` 恒存在。

- [ ] **Step 1: 写失败测试**：encoding 两态（text: 非法 UTF-8 fixture→U+FFFD 断言；base64: 3 字节中文跨两窗切断后拼接解码==原文——spec §7 编码回归锚）；诚实降级（gap→truncated+lost+next=total）；超前立即返回；unknown 三因文案断言（含 expired/evicted/restarted 字样）；负值与非法 encoding schema 拒绝（in-memory client 调用断言 IsError）；exec_stop 立即返回 running（运行中）/幂等（终态）/unknown；stop 与自然退出竞态（并发触发，终态唯一+审计恰一行）；exec_output 零审计行；前台 `effective_timeout_seconds`（0→120、400→300、90→90，恒存在）。
- [ ] **Step 2: 确认失败** → **Step 3: 实现**：`OutputForProfile` 薄壳——audit 零行、`mgr.Output(...)`、按 encoding 装配（text=`string(chunk)`；base64=`base64.StdEncoding.EncodeToString(chunk)`）、unknown 时拼三因文案（spec §4 原文）；`Stop` 实现如 Interfaces；`clampWaitSeconds`（0→0、>60→60）单测；前台 core.go:154 钳制后 `out.EffectiveTimeoutSeconds = int(timeout.Seconds())`。
- [ ] **Step 4: 全绿** → **Step 5: Commit**

```bash
git add internal/mcpserver/
git commit -m "feat(mcpserver): exec_output/exec_stop 工具——encoding 两态+诚实降级+三因文案+前台钳制改响(Plan 32 T7)"
```

---

### Task 8: in-memory MCP e2e 三件套全流 + revoke 钉住 + BrokerTools 连带核对

**Files:**
- Modify: `internal/mcpserver/e2e_test.go`（追加）
- Test: `internal/mcpserver/revoke_semantics_test.go`（追加 `TestRevokedProjectKeepsBackgroundTaskRunning`）

**Interfaces:** 无新接口——验证型任务。

- [ ] **Step 1: e2e 全流测试**：照 `TestE2EIronRule` 形态 in-memory MCP client：initialize → tools/list 断言 **9 工具**（6+3，BrokerTools 单源核对）→ exec_background（testsshd 逐行任务）→ exec_output wait 轮询收增量（offset 推进）→ exec_stop（sleep 任务）→ 终态取尾 next==total。零容忍联动：`grep -rn "BrokerTools\[" --include=*.go` 逐点核对硬编码下标（server.go 注释 0-5 之外新增 6/7/8 注释）。
- [ ] **Step 2: revoke 钉住**：Read `TestRevokedProjectKeepsOpenTunnelForwarding`（revoke_semantics_test.go）→ 镜像写后台版：起任务 → revoke project token → 断言任务仍在跑（Output 可读、状态 running）、不被 CloseAll 连带；**若未来 revoke 实现加了 CloseAll 连带，本测试红**（测试名+注释写明反向钉住意图）。
- [ ] **Step 3: 全绿** → **Step 4: Commit**

```bash
git add internal/mcpserver/e2e_test.go internal/mcpserver/revoke_semantics_test.go
git commit -m "test(mcpserver): 三件套 in-memory e2e 全流+revoke 不杀任务反向钉住+BrokerTools 9 工具核对(Plan 32 T8)"
```

---

### Task 9: 文档八处联动（spec §8 全表）

**Files:** `docs/agent-tools.md`、`README.md`、`docs/agent-access.md`、`docs/threat-model.md`、`docs/concepts.md`、`docs/compat-matrix.md`、`docs/ssh-conformance/differences-ledger.md`（server.go 描述已随 T6/T7）

**Interfaces:** 无——纯文档；**每个文件的改动内容以 spec §8 表格该行为准逐字执行**，非 ASCII 编辑逐字节验证（gofmt 不管 md，用 `git diff` 目检）。

- [ ] **Step 1**: agent-tools.md 三件套节（语义/tail -f 惯用法 wait≤30s/`cd && VAR=x`/重启即失/三因失效/kill 语义诚实段/编码两态+GBK 建议/offset 与降级/前台 effective）+ 错误对照表补行。
- [ ] **Step 2**: README 工具清单三行 + v0.10 callout；agent-access.md「断连语义（四层）」各层补后台任务行（spec §8 原文）；threat-model §3.5 一句（任务表=进程内状态同隧道类/sudo 密码不落记录/failed 过清洗）；concepts.md 工具清单一句。
- [ ] **Step 3**: compat-matrix.md v0.10.0 行（纯增量：3 新工具+ExecOutput 新字段，无破坏——已验证组合表留「发版后双端实测回写」占位注释）；differences-ledger.md 三件套偏差登记（无 ssh 二进制对应物不做 differential/kill=会话关闭 SIGHUP 语义）。
- [ ] **Step 4**: `grep -rn "exec_command" docs/ README.md | head -30` 目检遗漏面（工具清单/quickstart 若提及工具数或清单需同步）。
- [ ] **Step 5: Commit**

```bash
git add docs/ README.md
git commit -m "docs: Plan 32 三件套八处联动——agent-tools/断连语义/threat-model/compat-matrix v0.10.0/差异台账(Plan 32 T9)"
```

---

### Task 10: eval T9 后台任务（gated）+ conformance 后台生命周期（gated）

**Files:**
- Modify: `internal/eval/`（新任务 T9 + scorer）
- Modify: `internal/conformance/`（后台生命周期用例）

**Interfaces:** 双 gated（`SSHMGR_AGENT_EVAL=1` / `SSHMGR_CONFORMANCE=1`），默认 `go test ./...` 自跳过、零 LLM/零 docker。

- [ ] **Step 1**: eval T9：照 `runTaskM` 形态加任务——docker 内脚本 `for i in 1 2 3 4 5; do echo "line $i"; sleep 1; done` 后台化 → agent 轮询 exec_output（wait_seconds=10）收齐 5 行 → exec_stop 一个 `sleep 300` 任务；scorer 确定性断言（5 行齐 + next offset 推进 + stop 后终态 stopped）。`internal/eval/README.md` 任务表补行；`seedBroker` 零改动（按 id 寻址）。scoreT6/scoreT8 零容忍面经 BrokerTools 切片自动覆盖三新工具——加一条断言三新工具名在 `BrokerTools` 内（防切片漂移）。
- [ ] **Step 2**: conformance：`internal/conformance/` 加 `TestBackgroundLifecycleRealSSH`（真 OpenSSH 容器：start→增量→stop→终态；前台 interop/differential **零改动**跑绿=runSession 拆移的 SSH 层证据；differences-ledger 已在 T9 登记）。keepalive 真连接冒烟（可选步：容器内 sleep 任务挂 90s 观察 keepalive 不拆）。
- [ ] **Step 3**: 门禁内本地跑：`SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v`（docker 在位则绿）；eval 至少 `go vet ./internal/eval/` + dry 编译（真实跑 CI/owner gate 再做——按 Plan 31 惯例「CI eval Docker 必须先跑」记入 owner 待办）。
- [ ] **Step 4: Commit**

```bash
git add internal/eval/ internal/conformance/
git commit -m "test(eval+conformance): T9 后台任务 agent 用例+真 OpenSSH 生命周期+零容忍面自动扩展(Plan 32 T10)"
```

---

## 验收对照（spec §7 全表 → 任务映射）

| spec §7 层 | 任务 |
|---|---|
| rollingBuffer 全用例 | T1 |
| runSession 前台回归网 | T2 |
| TaskManager 上限/驱逐/admission/预约归还/closed 拒绝/sweeper/终态即关/CloseAll/竞态/审计顺序 | T3+T4 |
| 唤醒原语五件（虚假唤醒/广播/预算/短路/锁序） | T5 |
| testsshd 生命周期 e2e（含 RST fixture、排空锚） | T4 |
| 编码两态 + 超前立即返回 + 负值/enum schema + 三因文案 | T7 |
| ForProfile no-leak 网 + start 全分支审计 | T6 |
| in-memory e2e + BrokerTools 连带 | T8 |
| revoke 反向钉住 | T8 |
| 前台钳制响应 | T7 |
| env seam | T3 |
| 文档八处 | T9 |
| eval T9 / conformance | T10 |

**Owner gate（发版前，不在本计划内）**：v0.10.0 tag 前双端部署实测 + compat-matrix 回写（惯例）；eval CI 先跑（Plan 31 遗留同源）。
