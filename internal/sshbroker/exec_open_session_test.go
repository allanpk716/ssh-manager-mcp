package sshbroker

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

// stableGoroutineBaseline 先等协程数稳定再取样：连续若干次读数相同才把
// 它当基线，避免上一个测试残留的协程仍在退出途中时取到偏高或偏低的值。
func stableGoroutineBaseline(t *testing.T) int {
	t.Helper()
	const stableSamples = 4
	baseline := runtime.NumGoroutine()
	same := 1
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == baseline {
			same++
			if same >= stableSamples {
				return baseline
			}
			continue
		}
		baseline = current
		same = 1
	}
	return baseline
}

// waitGoroutinesReturnToBaseline 轮询等待协程数回到基线。它只作为辅助证
// 明；迟到会话被关闭的主要断言是测试服务器侧观察到会话通道结束。
func waitGoroutinesReturnToBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("协程数未回到基线 %d（当前 %d）：迟到会话疑似未被关闭", baseline, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForSessionCloseEvents 等待测试服务器报告指定数量的会话通道结束事
// 件。连接随后仍能执行命令、且该会话从未收到执行请求时，事件只能来自客
// 户端主动关闭通道，因此它是「迟到会话被回收」的直接证据。
func waitForSessionCloseEvents(t *testing.T, events <-chan struct{}, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for received := 0; received < want; {
		select {
		case <-events:
			received++
		case <-deadline:
			t.Fatalf("等待服务器侧会话通道结束事件超时：收到 %d 个，期望 %d 个", received, want)
		}
	}
}

// TestExecOpenTimeoutClosesConnection 钉住「开通道请求无应答 + 超时」路径：
// 测试服务器把第一条会话通道打开请求挂住不答，黑洞代理同时把双向转发停
// 掉（模拟开通道请求永远得不到应答的连接）。带超时的普通执行必须按期返
// 回超时结果，且关闭整条 SSH 连接；连接死亡类错误不得泄漏为报错，随后同
// 连接上的执行只能拿到连接已关闭的错误。
func TestExecOpenTimeoutClosesConnection(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms") // 连接构造时读取，必须早于 connectVia
	openReached := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 释放服务器侧挂住的通道处理，测试自身不漏协程
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() {
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

	oc := boundedExec(t, "Exec(open hang)", func() (ExecResult, error) {
		return c.Exec(context.Background(), "hello", 300*time.Millisecond, 0)
	})

	if oc.err != nil {
		t.Fatalf("超时必须是结果而不是错误（关闭整条连接产生的连接死亡错误不得泄漏）：%v", oc.err)
	}
	if !oc.res.TimedOut {
		t.Fatalf("期望 TimedOut=true，得到 %+v", oc.res)
	}
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("Exec 在 %v 后才返回，期望远早于 5 秒", oc.elapsed)
	}

	// 超时路径必须关闭整条连接：随后的开会话应立即报错，而不是继续挂住。
	after := boundedExec(t, "Exec(after timeout)", func() (ExecResult, error) {
		return c.Exec(context.Background(), "hello", 0, 0)
	})
	if after.err == nil {
		t.Fatalf("超时后连接应已关闭，随后执行应报错，得到 res=%+v err=%v", after.res, after.err)
	}
	if after.res.TimedOut {
		t.Fatalf("连接已关闭的错误不是超时结果：%v", after.err)
	}
}

// TestExecOpenCancelKeepsConnectionAndClosesLateSession 钉住「开通道请求挂
// 住 + 调用方取消」路径：取消必须按期返回取消错误且不算超时；这条 SSH 连
// 接保持可用（随后的执行成功）；释放被挂住的开通道请求后，服务器侧观察
// 到会话通道被客户端关闭（迟到会话被后台回收的直接证明），协程数回到基
// 线只作为辅助证据。
func TestExecOpenCancelKeepsConnectionAndClosesLateSession(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHold)
	var holdOnce sync.Once // 只挂住第一条会话通道；释放后后续通道直接放行
	sessionClosed := make(chan struct{}, 4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() { holdOnce.Do(func() { <-release }) },
		SessionClosed:   func() { sessionClosed <- struct{}{} },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	goroutineBaseline := stableGoroutineBaseline(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	oc := boundedExec(t, "Exec(open hang, cancel)", func() (ExecResult, error) {
		return c.Exec(ctx, "hello", 0, 0)
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

	// 释放挂住的开通道请求：迟到开出的会话必须被客户端后台关闭。这是
	// 本测试中唯一一条会话，且它从未收到执行请求，服务器侧的处理结束只
	// 能因为客户端关闭了通道——这是主要断言。
	releaseHold()
	waitForSessionCloseEvents(t, sessionClosed, 1)

	// 协程数回到基线作为辅助证明：客户端侧协程退出、服务器侧会话处理
	// 协程收到通道关闭后退出。
	waitGoroutinesReturnToBaseline(t, goroutineBaseline)

	// 连接未被取消牵连：同一条连接上的下一条命令必须成功。
	res, err := c.Exec(context.Background(), "ping", 0, 0)
	if err != nil || res.Stdout != "out:ping\n" {
		t.Fatalf("取消后的同连接执行：res=%+v err=%v", res, err)
	}
}

// TestExecOpenConsumesSharedTimeoutBudget 钉住「超时预算覆盖开会话 + 执行
// 全程」：开会话耗时 1.5s、超时 2s、命令本身远超剩余预算。修复后的总耗
// 时约等于 2s；若开会话后错误地重新计时，总耗时会变成 1.5s + 2s = 3.5s。
// 断言窗口取 1.7s–2.8s：下沿距正确值 300ms、上沿距正确值 800ms、距错误
// 值 700ms，两种行为相差 1.5s，持续集成慢机器上也有足够裕量。入口走
// ExecWriters，覆盖与 Exec 同一内核的另一条公开入口。
func TestExecOpenConsumesSharedTimeoutBudget(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	timer := time.AfterFunc(1500*time.Millisecond, releaseHold)
	t.Cleanup(func() { timer.Stop(); releaseHold() })
	var holdOnce sync.Once // 只挂住第一条会话通道；释放后命令阶段正常放行
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			time.Sleep(5 * time.Second) // 远超开会话后剩余的预算
			return "done\n", "", 0
		},
		SessionOpenHold: func() { holdOnce.Do(func() { <-release }) },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	start := time.Now()
	_, timedOut, err := c.ExecWriters(context.Background(), "slow", 2*time.Second, io.Discard, io.Discard)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("超时必须是结果而不是错误：%v", err)
	}
	if !timedOut {
		t.Fatal("期望 timedOut=true")
	}
	if elapsed < 1700*time.Millisecond {
		t.Fatalf("总耗时 %v，不应在预算到期前返回", elapsed)
	}
	if elapsed >= 2800*time.Millisecond {
		t.Fatalf("总耗时 %v，超时预算应覆盖开会话全程而不是开会话后重新计时", elapsed)
	}
}
