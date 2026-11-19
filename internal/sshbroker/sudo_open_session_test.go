package sshbroker

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

// TestExecSudoOpenTimeoutClosesConnection 是与
// TestExecOpenTimeoutClosesConnection 成对的提权用例。提权执行先开一条
// 会话探测免密 sudo：探测会话放行；真正的提权命令会话被测试服务器挂住
// 不答，黑洞代理同时停掉双向转发（模拟开通道请求永远得不到应答的连接）。
// 带超时的提权执行必须按期返回超时结果并关闭整条 SSH 连接；连接死亡类错
// 误不得泄漏为报错，随后的提权执行只能拿到连接已关闭的错误。
func TestExecSudoOpenTimeoutClosesConnection(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms") // 连接构造时读取，必须早于 connectVia
	openReached := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 释放服务器侧挂住的通道处理，测试自身不漏协程
	var sessionOpens atomic.Int32
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() {
			// 第一条会话是免密 sudo 探测；第二条才是提权命令本身。
			if sessionOpens.Add(1) == 2 {
				close(openReached)
				<-release
			}
		},
	})
	defer cleanup()
	p := startBlackholeProxy(t, addr)
	c := connectVia(t, p.Addr(), hk)

	go func() {
		<-openReached
		p.blackout()
	}()

	oc := boundedExec(t, "ExecSudo(open hang)", func() (ExecResult, error) {
		return c.ExecSudo(context.Background(), "hello", []byte("sudopw"), 300*time.Millisecond, 0)
	})

	if oc.err != nil {
		t.Fatalf("超时必须是结果而不是错误（关闭整条连接产生的连接死亡错误不得泄漏）：%v", oc.err)
	}
	if !oc.res.TimedOut {
		t.Fatalf("期望 TimedOut=true，得到 %+v", oc.res)
	}
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("ExecSudo 在 %v 后才返回，期望远早于 5 秒", oc.elapsed)
	}

	// 超时路径必须关闭整条连接：随后的提权执行（其免密探测也要先开会
	// 话）应立即报错，而不是继续挂住。
	after := boundedExec(t, "ExecSudo(after timeout)", func() (ExecResult, error) {
		return c.ExecSudo(context.Background(), "ping", []byte("sudopw"), 0, 0)
	})
	if after.err == nil {
		t.Fatalf("超时后连接应已关闭，随后提权执行应报错，得到 res=%+v err=%v", after.res, after.err)
	}
	if after.res.TimedOut {
		t.Fatalf("连接已关闭的错误不是超时结果：%v", after.err)
	}
}

// TestExecSudoOpenCancelKeepsConnectionAndClosesLateSession 是与
// TestExecOpenCancelKeepsConnectionAndClosesLateSession 成对的提权用例，
// 入口走 ExecSudoWriters：免密探测会话放行、提权命令会话挂住后取消调用
// 方上下文，必须按期返回取消错误且不算超时；这条 SSH 连接保持可用（随后
// 的提权执行成功）；释放挂住的开通道请求后，服务器侧观察到提权命令的会
// 话通道被客户端关闭（迟到会话被后台回收的直接证明），协程数回到基线只
// 作为辅助证据。
func TestExecSudoOpenCancelKeepsConnectionAndClosesLateSession(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHold)
	var sessionOpens atomic.Int32 // 只挂住提权命令的会话；免密探测与后续会话直接放行
	sessionClosed := make(chan struct{}, 4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() {
			// 第一条会话是免密 sudo 探测；第二条才是提权命令本身。
			if sessionOpens.Add(1) == 2 {
				<-release
			}
		},
		SessionClosed: func() { sessionClosed <- struct{}{} },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	goroutineBaseline := stableGoroutineBaseline(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	oc := boundedExec(t, "ExecSudoWriters(open hang, cancel)", func() (ExecResult, error) {
		exitCode, timedOut, err, _ := c.ExecSudoWriters(ctx, "hello", "sudopw", 0, io.Discard, io.Discard)
		return ExecResult{ExitCode: exitCode, TimedOut: timedOut}, err
	})

	if !errors.Is(oc.err, context.Canceled) {
		t.Fatalf("err = %v，期望 context.Canceled", oc.err)
	}
	if oc.res.TimedOut {
		t.Fatalf("取消不是超时，期望 TimedOut=false，得到 %+v", oc.res)
	}
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("取消后 %v 才返回，期望远早于 5 秒", oc.elapsed)
	}

	// 释放挂住的开通道请求：迟到的提权命令会话必须被客户端后台关闭。
	// 服务器侧一共应观察到两条会话通道结束——免密探测会话正常结束在
	// 前，被挂住的提权命令会话在释放后被客户端关闭在后；连接随后还能
	// 执行命令，第二条会话又从未收到执行请求，所以它的处理结束只能来
	// 自客户端关闭通道。这是主要断言。
	releaseHold()
	waitForSessionCloseEvents(t, sessionClosed, 2)

	// 协程数回到基线作为辅助证明：客户端侧协程退出、服务器侧会话处理
	// 协程收到通道关闭后退出。
	waitGoroutinesReturnToBaseline(t, goroutineBaseline)

	// 连接未被取消牵连：同一条连接上的下一次提权执行必须成功。
	res, err := c.ExecSudo(context.Background(), "ping", []byte("sudopw"), 0, 0)
	if err != nil || res.Stdout != "out:ping\n" {
		t.Fatalf("取消后的同连接提权执行：res=%+v err=%v", res, err)
	}
}

// TestExecSudoProbeOpenTimeoutClosesConnection 钉住「免密探测会话挂住 +
// 调用方超时」路径。提权执行的第一条会话是探测 sudo 能否免密执行的命令，
// 测试服务器把这条会话通道的打开请求挂住不答，黑洞代理同时停掉双向转发
// （模拟开通道请求永远得不到应答的连接）。带超时的提权执行必须在探测阶
// 段就按期返回超时结果并关闭整条 SSH 连接，不得把连接已关闭错误泄漏为报
// 错，也不得继续在已关闭的连接上为实际命令开会话。
func TestExecSudoProbeOpenTimeoutClosesConnection(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms") // 连接构造时读取，必须早于 connectVia
	openReached := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 释放服务器侧挂住的通道处理，测试自身不漏协程
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() {
			// 第一条会话就是免密 sudo 探测；把它挂住即复现真实半死连接里最
			// 先卡住的会话。
			close(openReached)
			<-release
		},
	})
	defer cleanup()
	p := startBlackholeProxy(t, addr)
	c := connectVia(t, p.Addr(), hk)

	go func() {
		<-openReached
		p.blackout()
	}()

	oc := boundedExec(t, "ExecSudo(probe open hang)", func() (ExecResult, error) {
		return c.ExecSudo(context.Background(), "hello", []byte("sudopw"), time.Second, 0)
	})

	if oc.err != nil {
		t.Fatalf("探测阶段超时必须是结果而不是错误（连接已关闭的错误不得泄漏）：%v", oc.err)
	}
	if !oc.res.TimedOut {
		t.Fatalf("期望 TimedOut=true，得到 %+v", oc.res)
	}
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("ExecSudo 在 %v 后才返回，期望远早于 5 秒", oc.elapsed)
	}

	// 超时路径必须关闭整条连接：随后的提权执行应立即报错，而不是继续挂住。
	after := boundedExec(t, "ExecSudo(after probe timeout)", func() (ExecResult, error) {
		return c.ExecSudo(context.Background(), "ping", []byte("sudopw"), 0, 0)
	})
	if after.err == nil {
		t.Fatalf("超时后连接应已关闭，随后提权执行应报错，得到 res=%+v err=%v", after.res, after.err)
	}
	if after.res.TimedOut {
		t.Fatalf("连接已关闭的错误不是超时结果：%v", after.err)
	}
}

// TestExecSudoProbeOpenCancelKeepsConnectionAndClosesLateSession 钉住「免密
// 探测会话挂住 + 调用方取消」路径：取消必须在探测阶段就按期返回取消错误
// 且不算超时；这条 SSH 连接保持可用（随后的提权执行成功）；释放挂住的开
// 通道请求后，迟到的探测会话被后台关闭——以协程数回到基线作为「无协程或
// 会话泄漏」的可观察证明。
func TestExecSudoProbeOpenCancelKeepsConnectionAndClosesLateSession(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHold)
	var sessionOpens atomic.Int32 // 只挂住第一条会话（免密探测）；后续会话直接放行
	sessionClosed := make(chan struct{}, 8)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() {
			if sessionOpens.Add(1) == 1 {
				<-release
			}
		},
		SessionClosed: func() { sessionClosed <- struct{}{} },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	goroutineBaseline := stableGoroutineBaseline(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	oc := boundedExec(t, "ExecSudo(probe open hang, cancel)", func() (ExecResult, error) {
		return c.ExecSudo(ctx, "hello", []byte("sudopw"), 0, 0)
	})

	if !errors.Is(oc.err, context.Canceled) {
		t.Fatalf("err = %v，期望 context.Canceled", oc.err)
	}
	if oc.res.TimedOut {
		t.Fatalf("取消不是超时，期望 TimedOut=false，得到 %+v", oc.res)
	}
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("取消后 %v 才返回，期望远早于 5 秒", oc.elapsed)
	}

	// 释放挂住的开通道请求：迟到开出的探测会话必须被后台关闭。服务器侧
	// 的会话通道处理结束是直接证据——这条连接随后还能执行命令（见下），
	// 且这条会话从未收到执行请求，处理结束只能因为客户端关闭了通道。
	releaseHold()
	waitForSessionCloseEvents(t, sessionClosed, 1)

	// 取消发生在免密探测阶段：提权执行必须就此停止，不得再为实际命令另
	// 开一条会话。等待一个安静窗口后仍只有一次会话打开请求才算通过。
	quiet := time.NewTimer(500 * time.Millisecond)
	defer quiet.Stop()
	for quietStopped := false; !quietStopped; {
		if got := sessionOpens.Load(); got != 1 {
			t.Fatalf("取消后服务器侧会话打开次数 = %d，期望保持 1（不得再为实际命令开会话）", got)
		}
		select {
		case <-quiet.C:
			quietStopped = true
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 协程数回到基线作为辅助证明：客户端侧协程退出、服务器侧会话处理协
	// 程收到通道关闭后退出。
	waitGoroutinesReturnToBaseline(t, goroutineBaseline)

	// 连接未被取消牵连：同一条连接上的下一次提权执行必须成功。
	res, err := c.ExecSudo(context.Background(), "ping", []byte("sudopw"), 0, 0)
	if err != nil || res.Stdout != "out:ping\n" {
		t.Fatalf("取消后的同连接提权执行：res=%+v err=%v", res, err)
	}
}
