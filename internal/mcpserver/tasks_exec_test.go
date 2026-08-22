package mcpserver

// Plan 32 T4: 后台执行引擎生命周期 e2e (真 testsshd)。六用例对应 spec §7
// 「单测：testsshd 生命周期 e2e」: 增量/排空、stop、timeout、failed(RST fixture)、
// 审计 end 四态、终态即关 client、CloseAll 抑制补写。
// 等待手段是测试侧轮询 (生产等待回路是 T5 的代际广播, 不在本任务)。
// 终审修复波补: Sudo:true 生命周期 (Start 的 ExecSudoWriters 分支)。

import (
	"context"
	"io"
	"net"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// ---------- 白盒观察面 (引擎全程持 m.mu 写——测试持锁读, -race 干净) ----------

// bgTaskSnapshot 是任务的持锁观察快照: 状态机字段值拷贝 + client/buffer 引用。
type bgTaskSnapshot struct {
	status   string
	exitCode int
	errText  string
	client   *sshbroker.Client
	stdout   *sshbroker.RollingBuffer
	stderr   *sshbroker.RollingBuffer
}

func snapshotTask(m *TaskManager, id string) (bgTaskSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return bgTaskSnapshot{}, false
	}
	return bgTaskSnapshot{
		status: t.status, exitCode: t.exitCode, errText: t.errText,
		client: t.client, stdout: t.stdout, stderr: t.stderr,
	}, true
}

func mustSnap(t *testing.T, m *TaskManager, id string) bgTaskSnapshot {
	t.Helper()
	s, ok := snapshotTask(m, id)
	if !ok {
		t.Fatalf("task %s vanished from registry", id)
	}
	return s
}

// waitTerminal 轮询至任务离开 running (引擎终态置位), 上限 d。
func waitTerminal(t *testing.T, m *TaskManager, id string, d time.Duration) bgTaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if s, ok := snapshotTask(m, id); ok && s.status != bgStatusRunning {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not leave running within %v", id, d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitClientSet 轮询至引擎已挂 client 槽 (任务确已起跑, exec 在途)。
func waitClientSet(t *testing.T, m *TaskManager, id string) *sshbroker.Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, ok := snapshotTask(m, id); ok && s.client != nil {
			return s.client
		}
		if time.Now().After(deadline) {
			t.Fatal("client slot never set within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitClientClosed 轮询至对捕获 client 的后续操作报错 (即关已生效)。引擎关闭
// 时点在终态置位之后 (锁外), 终态可见时关闭可能仍在途——轮询兜底消除竞态。
func waitClientClosed(t *testing.T, cli *sshbroker.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := cli.Exec(context.Background(), "true", time.Second, 0); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("client still usable after terminal state (must be closed at terminal)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- 启动/门控/杀连 fixtures ----------

// startBg 经 m.Start 全链 (Reserve→ConnectKeepAlive→Insert→引擎) 起一个真任务。
func startBg(t *testing.T, m *TaskManager, st *store.Store, addr string, hk ssh.PublicKey, command string, timeoutSec int) (string, time.Duration) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	id, eff, serr := m.Start(context.Background(), st, BgStartSpec{
		ProjectID: "proj", ServerID: "srv", Command: command,
		TimeoutSec: timeoutSec,
		Server:     &models.Server{Host: host, Port: port, User: "u"},
		Auth:       sshbroker.PasswordAuth("pw"),
		HostKeyCb:  ssh.FixedHostKey(hk),
	})
	if serr != nil {
		t.Fatalf("start %q: %v", command, serr)
	}
	return id, eff
}

// gatedExec 构造可门控的 testsshd Exec handler: 命中 gatedCmds 之一时, 该命令
// 首次进入即记 entered 标记 (会话确已进入运行态——确定性锚, 此刻 Stop/RST/
// 读流必命中在途会话), 随后阻塞至 release 放行, 放行后返回 gatedOut。其余
// 命令立即返回 "out:<cmd>"。open() 以 sync.Once 放行全部滞留 handler
// (defer 兜底, 重复调用安全)。
type gatedExec struct {
	mu       sync.Mutex
	entered  map[string]bool
	release  chan struct{}
	openOnce sync.Once
}

const gatedOut = "seg-late\n"

func newGatedExec() *gatedExec {
	return &gatedExec{entered: map[string]bool{}, release: make(chan struct{})}
}

func (g *gatedExec) handler(gatedCmds ...string) func(string, io.Reader) (string, string, int) {
	gated := map[string]bool{}
	for _, c := range gatedCmds {
		gated[c] = true
	}
	return func(cmd string, _ io.Reader) (string, string, int) {
		if gated[cmd] {
			g.mu.Lock()
			g.entered[cmd] = true
			g.mu.Unlock()
			<-g.release
			return gatedOut, "", 0
		}
		return "out:" + cmd + "\n", "", 0
	}
}

func (g *gatedExec) open() { g.openOnce.Do(func() { close(g.release) }) }

// waitEntered 等指定门控命令的会话确已进入运行态。
func (g *gatedExec) waitEntered(t *testing.T, cmd string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		ok := g.entered[cmd]
		g.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gated session %q never entered the exec handler", cmd)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// killableProxy 是可对客户端侧发 TCP RST 的杀连接代理 (产品化自 .xcheck 实验
// 20260821-223410 exp_test.go 的 startProxy: 客户端经代理连 testsshd, rst()
// 对朝向客户端的一侧 SetLinger(0)+Close——内核发 RST 而非 FIN, 生产「连接被
// 重置」形态; spec §4 failed 态 no-leak 断言网的触发器)。
type killableProxy struct {
	ln     net.Listener
	ready  chan struct{} // 客户端侧连接已 accept
	mu     sync.Mutex
	client net.Conn
}

func startKillableProxy(t *testing.T, backend string) *killableProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &killableProxy{ln: ln, ready: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.client = c
		p.mu.Unlock()
		close(p.ready)
		b, err := net.Dial("tcp", backend)
		if err != nil {
			c.Close()
			return
		}
		go io.Copy(b, c)
		go io.Copy(c, b)
	}()
	return p
}

// rst 朝向客户端硬拆连接 (SetLinger(0)+Close → RST)。
func (p *killableProxy) rst(t *testing.T) {
	t.Helper()
	select {
	case <-p.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy never accepted the client connection")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tc, ok := p.client.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = p.client.Close()
}

// addrShapeRe 探测文本中的 IPv4/host:port/括号 IPv6 地址形态 (no-leak 断言网)。
var addrShapeRe = regexp.MustCompile(`(\d{1,3}\.){3}\d{1,3}(:\d+)?|\[[0-9a-fA-F:.]+:[0-9a-fA-F:.]*\]`)

// ---------- 六个生命周期用例 ----------

// TestBackgroundLifecycleIncremental: 增量语义近似 + 排空锚。testsshd 的 Exec
// handler 一次性返回输出, 无法逐行——用「快任务终态取尾 + 门控慢任务运行中
// 读零字节、放行后增量到位」两段近似 (brief Step 2 口径):
//   - 快任务: 终态后 offset 0 → next==total 取全量; 复读 next → 零新增 (spec §7 排空锚);
//   - 门控任务: 运行中 (未放行) 读流恒零字节; 放行终态后全量到位、复读稳定。
func TestBackgroundLifecycleIncremental(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	// 段一: 快任务——终态取尾, offset 推到 next==total, 复读零新增。
	fastID, _ := startBg(t, m, st, addr, hk, "fast", 60)
	s := waitTerminal(t, m, fastID, 5*time.Second)
	if s.status != bgStatusDone || s.exitCode != 0 {
		t.Fatalf("fast: status=%q exit=%d, want done/0", s.status, s.exitCode)
	}
	chunk, next, startOff := s.stdout.Snapshot(0)
	if string(chunk) != "out:fast\n" || next != s.stdout.Total() || startOff != 0 {
		t.Fatalf("fast snapshot: chunk=%q next=%d start=%d total=%d",
			chunk, next, startOff, s.stdout.Total())
	}
	chunk2, next2, _ := s.stdout.Snapshot(next)
	if len(chunk2) != 0 || next2 != next {
		t.Fatalf("drain anchor violated: re-read chunk=%q next=%d, want empty/%d", chunk2, next2, next)
	}

	// 段二: 门控任务——运行中读流 (零字节), 放行后增量到位。
	gatedID, _ := startBg(t, m, st, addr, hk, "gated", 60)
	gate.waitEntered(t, "gated")
	if snap := mustSnap(t, m, gatedID); snap.status != bgStatusRunning {
		t.Fatalf("gated: status=%q while blocked, want running", snap.status)
	}
	if snap := mustSnap(t, m, gatedID); snap.stdout.Total() != 0 {
		t.Fatalf("gated: stdout total=%d while blocked, want 0", snap.stdout.Total())
	}
	gate.open() // 放行 → 会话退出 → 引擎落终态
	s2 := waitTerminal(t, m, gatedID, 5*time.Second)
	if s2.status != bgStatusDone {
		t.Fatalf("gated: status=%q after release, want done", s2.status)
	}
	chunk3, next3, _ := s2.stdout.Snapshot(0)
	if string(chunk3) != gatedOut || next3 != s2.stdout.Total() {
		t.Fatalf("gated snapshot: chunk=%q next=%d total=%d", chunk3, next3, s2.stdout.Total())
	}
	// 偏移稳定性: 缓冲未滚动, 同 offset 复读返回同数据。
	chunk4, _, _ := s2.stdout.Snapshot(0)
	if string(chunk4) != gatedOut {
		t.Fatalf("offset stability: re-read=%q want %q", chunk4, gatedOut)
	}
}

// TestBackgroundStopPath: sleep 形任务 (门控) → Stop 立即返回触发时刻的
// "running" → 终态 stopped; client 已关 (捕获槽内引用, 后续 op 报错)。
func TestBackgroundStopPath(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	id, _ := startBg(t, m, st, addr, hk, "gated", 60)
	gate.waitEntered(t, "gated")
	waitClientSet(t, m, id) // 引擎确已起跑

	got, ok := m.Stop(id)
	if !ok || got != bgStatusRunning {
		t.Fatalf("Stop = (%q, %v), want (%q, true) — 触发时刻 status 应为 running", got, ok, bgStatusRunning)
	}
	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusStopped {
		t.Fatalf("status=%q after stop, want stopped", s.status)
	}
	if s.errText != "" {
		t.Fatalf("stopped task errText=%q, want empty", s.errText)
	}
	waitClientClosed(t, s.client)
}

// TestBackgroundTimeoutPath: TimeoutSec=1 跑长任务 → 引擎 ctx deadline →
// status=timeout; 生效超时钳定值随 Start 响式回显。
func TestBackgroundTimeoutPath(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	id, eff := startBg(t, m, st, addr, hk, "gated", 1)
	if eff != time.Second {
		t.Fatalf("effective timeout = %v, want 1s (直通, 未触 runCap 钳定)", eff)
	}
	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusTimeout {
		t.Fatalf("status=%q, want timeout", s.status)
	}
	if s.errText != "" {
		t.Fatalf("timeout task errText=%q, want empty", s.errText)
	}
	waitClientClosed(t, s.client)
}

// TestBackgroundFailedRSTPath: RST 代理 fixture 真触发 failed 态; errText 恒等
// ExitMissingError 文本 (实测形态, 零地址) 且过清洗后无地址残留 (no-leak 网)。
func TestBackgroundFailedRSTPath(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	p := startKillableProxy(t, addr)
	id, _ := startBg(t, m, st, p.ln.Addr().String(), hk, "gated", 60)
	gate.waitEntered(t, "gated") // 会话确在运行态 → RST 必命中在途会话
	p.rst(t)

	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusFailed {
		t.Fatalf("status=%q after RST, want failed", s.status)
	}
	const exitMissing = "wait: remote command exited without exit status or exit signal"
	if s.errText != exitMissing {
		t.Fatalf("errText=%q, want 恒等 ExitMissingError 文本 %q", s.errText, exitMissing)
	}
	if addrShapeRe.MatchString(s.errText) {
		t.Fatalf("errText leaks address shapes: %q", s.errText)
	}
	waitClientClosed(t, s.client)
}

// TestBackgroundAuditEndRows: done/stopped/timeout/failed 四态各落一行
// exec-bg-end (Command=taskID, Status/Dur 断言); start(ok) 行先于 end 行
// (AuditRows 按 id 倒序——end 行在切片中位于 start 行之前 = 落笔次序在后)。
func TestBackgroundAuditEndRows(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 8)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     gate.handler("a-stop", "a-to", "a-fail"),
	})
	defer cleanup()
	defer gate.open()

	// done: 立即退出 (非门控命令走默认分支)。
	idDone, _ := startBg(t, m, st, addr, hk, "a-done", 60)
	waitTerminal(t, m, idDone, 5*time.Second)

	// stopped: 门控运行中 Stop。
	idStop, _ := startBg(t, m, st, addr, hk, "a-stop", 60)
	gate.waitEntered(t, "a-stop")
	if _, ok := m.Stop(idStop); !ok {
		t.Fatal("stop failed")
	}

	// timeout: 门控 + 1s 生效超时。
	idTO, _ := startBg(t, m, st, addr, hk, "a-to", 1)
	gate.waitEntered(t, "a-to")

	// failed: RST 代理 (独立 proxy, 经代理地址连同一 testsshd)。
	p := startKillableProxy(t, addr)
	idFail, _ := startBg(t, m, st, p.ln.Addr().String(), hk, "a-fail", 60)
	gate.waitEntered(t, "a-fail")
	p.rst(t)

	for _, id := range []string{idStop, idTO, idFail} {
		waitTerminal(t, m, id, 5*time.Second)
	}

	rows, err := st.AuditRows(50)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		cmd      string // start 行 Command (命令原文)
		endStat  string // end 行 Status
		minDurMS int64  // end 行 DurationMS 下界
	}{
		idDone: {"a-done", "ok", 0},
		idStop: {"a-stop", "stopped", 0},
		idTO:   {"a-to", "timeout", 900}, // 1s 生效超时 → 运行时长 ≥ ~1s
		idFail: {"a-fail", "failed", 0},
	}
	startIdx := map[string]int{}
	endIdx := map[string]int{}
	endRow := map[string]store.AuditRow{}
	for i, r := range rows {
		if r.Action == "exec-bg-start" {
			for id, w := range want {
				if r.Command == w.cmd {
					startIdx[id] = i
				}
			}
		} else if r.Action == "exec-bg-end" {
			if _, ok := want[r.Command]; ok {
				endIdx[r.Command] = i
				endRow[r.Command] = r
			}
		}
	}
	for id, w := range want {
		si, hasStart := startIdx[id]
		ei, hasEnd := endIdx[id]
		if !hasStart || !hasEnd {
			t.Fatalf("task %s: start=%v end=%v (rows=%d), want both", id, hasStart, hasEnd, len(rows))
		}
		if ei >= si {
			t.Fatalf("task %s: end row (idx %d) must sort before start row (idx %d) — start 行先于 end 行落笔", id, ei, si)
		}
		if row := endRow[id]; row.Status != w.endStat {
			t.Fatalf("task %s: end status=%q, want %q", id, row.Status, w.endStat)
		} else if row.DurationMS < w.minDurMS {
			t.Fatalf("task %s: end duration=%dms, want ≥ %dms", id, row.DurationMS, w.minDurMS)
		}
	}
	// 共 8 行: 每任务 start+end 各一, 无多余。
	if len(rows) != 8 {
		t.Fatalf("audit rows = %d, want 8 (4×(start+end)): %+v", len(rows), rows)
	}
}

// TestBackgroundClientClosedAtTerminal: 终态后记录仍在 (Output 可读语义——
// stdout Snapshot 可取) 而 client 已关 (后续 op 报错)。
func TestBackgroundClientClosedAtTerminal(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "out:" + cmd + "\n", "", 0 },
	})
	defer cleanup()

	id, _ := startBg(t, m, st, addr, hk, "ephemeral", 60)
	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status=%q, want done", s.status)
	}
	waitClientClosed(t, s.client)
	// 记录仍在: 表项未删, 输出仍可读 (终态保留期语义)。
	if _, ok := snapshotTask(m, id); !ok {
		t.Fatal("task record must survive terminal state (retention window)")
	}
	if chunk, _, _ := s.stdout.Snapshot(0); string(chunk) != "out:ephemeral\n" {
		t.Fatalf("post-terminal stdout=%q", chunk)
	}
}

// TestBackgroundCloseAllSuppressesEndRows: CloseAll 杀 running → 零 exec-bg-end
// 补写行 (closed 抑制, spec §3); 仅剩 start(ok) 一行, 表项已摘。
func TestBackgroundCloseAllSuppressesEndRows(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	id, _ := startBg(t, m, st, addr, hk, "gated", 60)
	gate.waitEntered(t, "gated")
	waitClientSet(t, m, id)

	m.CloseAll() // 持锁置 closed → 摘表 → cancel+关 client → wg.Wait 引擎退出

	if _, ok := snapshotTask(m, id); ok {
		t.Fatal("task must be removed from registry by CloseAll")
	}
	rows, err := st.AuditRows(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (start only): %+v", len(rows), rows)
	}
	if rows[0].Action != "exec-bg-start" || rows[0].Status != "ok" {
		t.Fatalf("sole row = %+v, want exec-bg-start/ok", rows[0])
	}
}

// TestBackgroundSudoLifecycle (终审修复波): Sudo:true 经 m.Start 走
// ExecSudoWriters 分支 (tasks.go Start 的 spec.Sudo 选择)。testsshd 的
// SudoPassword 模拟 sudo -S——服务端吃掉密码行后, Exec handler 收到内层命令
// (见 internal/sshbroker/sudo_test.go 同一惯用法)。断言: 终态 done、退出码
// 传递、handler 输出经 Output 轮询 (生产观察路径, 区别于白盒 Snapshot) 可见。
func TestBackgroundSudoLifecycle(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sp",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "whoami" {
				return "root\n", "", 0
			}
			return "", "unknown: " + cmd + "\n", 1
		},
	})
	defer cleanup()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	id, _, serr := m.Start(context.Background(), st, BgStartSpec{
		ProjectID: "proj", ServerID: "srv", Command: "whoami",
		Sudo: true, SudoPass: "sp", TimeoutSec: 60,
		Server:    &models.Server{Host: host, Port: port, User: "u"},
		Auth:      sshbroker.PasswordAuth("pw"),
		HostKeyCb: ssh.FixedHostKey(hk),
	})
	if serr != nil {
		t.Fatalf("start sudo task: %v", serr)
	}

	// 生产观察路径: Output 轮询至终态 (每轮 1s 预算), 断言输出与退出码。
	var v BgView
	deadline := time.Now().Add(5 * time.Second)
	for {
		var ok bool
		v, ok, err = m.Output(id, 0, 0, time.Second, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("task must be found by Output")
		}
		if v.Status != bgStatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never left running in views: %+v", v)
		}
	}
	if v.Status != bgStatusDone || v.ExitCode != 0 {
		t.Fatalf("view = status %q exit %d, want done/0", v.Status, v.ExitCode)
	}
	if string(v.Stdout) != "root\n" {
		t.Fatalf("stdout=%q via Output polling, want inner command output", v.Stdout)
	}
	waitClientClosed(t, mustSnap(t, m, id).client)
}
