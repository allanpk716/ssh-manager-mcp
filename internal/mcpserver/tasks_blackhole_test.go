package mcpserver

// Plan 52 票03: 黑洞形态后台任务用例 (spec Testing Decisions 第 2 层末条)。
//
// 保活时序纪律 (防 vacuous pass): 后台任务走 ConnectKeepAlive, 其保活判死
// 阈值为 30s 周期 × 3 次无响应 = 90 秒 (sshbroker/client.go keepAliveSpec),
// 远大于本文件用例的 10 秒外部死线——死线内唯一可能的解锁源是
// killWatchdog 第三段 (宽限耗尽后关整条连接)。因此「死线内观察到终态」
// 必须依赖票 01 的三段式看门狗: 若未来移除/绕过第三段 (或让超时/停止退化
// 为只发信号 +关通道), 本文件两用例会在死线处红掉 (fail 红, 不是挂死),
// 不会 vacuous pass。testsshd 本体底层库自动回关通道, 复现不了挂死形态
// ——黑洞代理是快速通道替代: blackout() 后停止双向转发但双侧 TCP 保持
// 开放, 客户端的 SIGKILL 请求与通道关闭消息落入代理被吞, 服务器永远收不
// 到, Session.Wait 永不返回 (真机 OpenSSH 9.6p1 同形态)。
//
// 夹具纪律: 仿本包 tasks_exec_test.go 的 killableProxy 先例在包内自建
// (Go 测试包不跨包共享 test helper, 与 sshbroker 侧夹具的少量重复是可接
// 受的包边界惯例); pump 在 blackout 后改为只读丢弃 (不回压: 客户端写永远
// "成功", 正是黑洞语义); cleanup 关双侧连接防协程泄漏; testsshd 的门控
// Exec handler 阻塞在 release 通道上 (不读 stdin, 关连接解不了), 用
// defer gate.open() 放行防 goroutine 泄漏。
//
// 观察路径: 用例经 m.Output (exec_output 的生产长轮询路径) 轮询至终态。

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

// bgOutputDeadline 是黑洞用例的外部死线: 必须 « 保活判死阈值 90s, 使得
// 死线内进终态的唯一解释是看门狗第三段 (见文件头)。
const bgOutputDeadline = 10 * time.Second

// blackholeProxy 是停转不拆线的黑洞代理: 客户端经它连 testsshd, blackout()
// 前双向转发, blackout() 后继续读两侧但丢弃一切数据 (双侧 TCP 保持开放,
// 无回压)——复刻「服务器收下拆除请求但永不回应」的不配合形态。
type blackholeProxy struct {
	ln         net.Listener
	ready      chan struct{} // 双侧连接已建立, 转发在途
	mu         sync.Mutex
	blackholed bool
	client     net.Conn
	backend    net.Conn
}

func startBlackholeProxy(t *testing.T, backend string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{ln: ln, ready: make(chan struct{})}
	t.Cleanup(func() { // 关双侧连接: 解锁 pump, 拆掉 testsshd 侧会话
		ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.client != nil {
			p.client.Close()
		}
		if p.backend != nil {
			p.backend.Close()
		}
	})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b, err := net.Dial("tcp", backend)
		if err != nil {
			c.Close()
			return
		}
		p.mu.Lock()
		p.client, p.backend = c, b
		p.mu.Unlock()
		close(p.ready)
		go p.pump(c, b) // client → backend
		go p.pump(b, c) // backend → client
	}()
	return p
}

// pump 把 src 读到的数据写给 dst; blackout 后改为丢弃 (黑洞吞数据, 连接不动)。
func (p *blackholeProxy) pump(dst, src net.Conn) {
	buf := make([]byte, 16*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			p.mu.Lock()
			bh := p.blackholed
			p.mu.Unlock()
			if !bh {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
		}
		if rerr != nil {
			return
		}
	}
}

// blackout 停转: 此刻起不再向任何一侧转发, 但双侧连接保持开放 (客户端的
// 通道关闭/SIGKILL 与服务器的响应互相不可达, 双方都不见 EOF/写错误)。
func (p *blackholeProxy) blackout(t *testing.T) {
	t.Helper()
	select {
	case <-p.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy never established forwarding")
	}
	p.mu.Lock()
	p.blackholed = true
	p.mu.Unlock()
}

// waitTerminalViaOutput 经 m.Output (exec_output 生产长轮询路径) 轮询至任务
// 离开 running, 上限 d (外部死线)。恒以 offset 0 观测: 黑洞任务无输出字节,
// 每轮就是一次纯状态等待 (终态广播会提前唤醒长轮询)。
func waitTerminalViaOutput(t *testing.T, m *TaskManager, id string, d time.Duration) BgView {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		v, ok, err := m.Output(id, 0, 0, time.Second, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("task %s unknown to Output (vanished before terminal)", id)
		}
		if v.Status != bgStatusRunning {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s still running after %v — 黑洞形态下死线内唯一解锁源是看门狗第三段, 未生效即回归", id, d)
		}
	}
}

// TestBackgroundTimeoutBlackhole: 黑洞代理 + 门控阻塞命令 + TimeoutSec=1 →
// 服务器收不到任何拆除请求, 任务仍必须在死线内进终态 timeout (而非永远
// running)——解锁只能来自看门狗第三段关整条连接。
func TestBackgroundTimeoutBlackhole(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms") // 缩短宽限: 用例总耗时可控 (~1.5s)
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open() // handler 阻塞在 release 上 (不读 stdin), defer 放行防泄漏

	p := startBlackholeProxy(t, addr)
	id, eff := startBg(t, m, st, p.ln.Addr().String(), hk, "gated", 1)
	if eff != time.Second {
		t.Fatalf("effective timeout = %v, want 1s (直通, 未触 runCap 钳定)", eff)
	}
	gate.waitEntered(t, "gated") // 会话确在运行态 → 此刻黑洞必命中在途拆除路径
	p.blackout(t)

	v := waitTerminalViaOutput(t, m, id, bgOutputDeadline)
	if v.Status != bgStatusTimeout {
		t.Fatalf("status=%q, want timeout (黑洞下仍必须有界进终态)", v.Status)
	}
	if v.ErrText != "" {
		t.Fatalf("timeout task errText=%q, want empty (连接死亡错误须折叠, 不泄漏)", v.ErrText)
	}
	waitClientClosed(t, mustSnap(t, m, id).client)
}

// TestBackgroundStopBlackhole: 黑洞代理 + 门控阻塞命令 (长超时) → Stop 触发
// cancel → 服务器收不到 SIGKILL/通道关闭 → 任务仍必须在死线内进终态 stopped。
func TestBackgroundStopBlackhole(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms")
	st := newStore(t)
	m := newTestTM(t, 4)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	p := startBlackholeProxy(t, addr)
	id, _ := startBg(t, m, st, p.ln.Addr().String(), hk, "gated", 60)
	gate.waitEntered(t, "gated")
	waitClientSet(t, m, id) // 引擎确已起跑 (与既有 StopPath 同纪律)
	p.blackout(t)

	got, ok := m.Stop(id)
	if !ok || got != bgStatusRunning {
		t.Fatalf("Stop = (%q, %v), want (%q, true) — 触发时刻 status 应为 running", got, ok, bgStatusRunning)
	}
	v := waitTerminalViaOutput(t, m, id, bgOutputDeadline)
	if v.Status != bgStatusStopped {
		t.Fatalf("status=%q, want stopped (黑洞下 exec_stop 仍必须有界进终态)", v.Status)
	}
	if v.ErrText != "" {
		t.Fatalf("stopped task errText=%q, want empty (Canceled 不落 errText)", v.ErrText)
	}
	waitClientClosed(t, mustSnap(t, m, id).client)
}
