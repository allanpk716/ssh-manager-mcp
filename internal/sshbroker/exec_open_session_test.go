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

// TestExecOpenTimeoutClosesConnection 钉住「开通道请求无应答 + 超时」路径：
// 测试服务器把第一条会话通道打开请求挂住不答，黑洞代理同时把双向转发停
// 掉（复现半死连接的形状）。带超时的普通执行必须按期返回超时结果，且关
// 闭整条 SSH 连接；连接死亡类错误不得泄漏为报错，随后同连接上的执行只
// 能拿到连接已关闭的错误。
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
// 接保持可用（随后的执行成功）；释放被挂住的开通道请求后，迟到的会话被
// 后台关闭——以协程数回到基线作为「无协程或会话泄漏」的可观察证明。
func TestExecOpenCancelKeepsConnectionAndClosesLateSession(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHold)
	var holdOnce sync.Once // 只挂住第一条会话通道；释放后后续通道直接放行
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "out:" + cmd + "\n", "", 0
		},
		SessionOpenHold: func() { holdOnce.Do(func() { <-release }) },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	goroutineBaseline := runtime.NumGoroutine()
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

	// 释放挂住的开通道请求：迟到开出的会话必须被后台关闭（客户端侧协程
	// 退出、服务器侧会话处理协程收到通道关闭后退出），整体协程数回到执
	// 行前的基线。
	releaseHold()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutineBaseline {
		if time.Now().After(deadline) {
			t.Fatalf("释放开通道请求后协程数未回到基线 %d（当前 %d）：迟到会话未被关闭", goroutineBaseline, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 连接未被取消牵连：同一条连接上的下一条命令必须成功。
	res, err := c.Exec(context.Background(), "ping", 0, 0)
	if err != nil || res.Stdout != "out:ping\n" {
		t.Fatalf("取消后的同连接执行：res=%+v err=%v", res, err)
	}
}

// TestExecOpenConsumesSharedTimeoutBudget 钉住「超时预算覆盖开会话 + 执行
// 全程」：开会话耗时 700ms、超时 1s、命令本身远超剩余预算。修复后的总耗
// 时约等于 1s；若开会话后重新计时，总耗时会变成 700ms + 1s，超过上限断
// 言而失败。入口走 ExecWriters，覆盖与 Exec 同一内核的另一条公开入口。
func TestExecOpenConsumesSharedTimeoutBudget(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	timer := time.AfterFunc(700*time.Millisecond, releaseHold)
	t.Cleanup(func() { timer.Stop(); releaseHold() })
	var holdOnce sync.Once // 只挂住第一条会话通道；释放后命令阶段正常放行
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			time.Sleep(3 * time.Second) // 远超开会话后剩余的预算
			return "done\n", "", 0
		},
		SessionOpenHold: func() { holdOnce.Do(func() { <-release }) },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	start := time.Now()
	_, timedOut, err := c.ExecWriters(context.Background(), "slow", time.Second, io.Discard, io.Discard)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("超时必须是结果而不是错误：%v", err)
	}
	if !timedOut {
		t.Fatal("期望 timedOut=true")
	}
	if elapsed < 850*time.Millisecond {
		t.Fatalf("总耗时 %v，不应在预算到期前返回", elapsed)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("总耗时 %v，超时预算应覆盖开会话全程而不是开会话后重新计时", elapsed)
	}
}
