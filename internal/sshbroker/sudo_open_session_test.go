package sshbroker

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

// TestExecSudoOpenTimeoutClosesConnection 是
// TestExecOpenTimeoutClosesConnection 的提权孪生用例。提权执行先开一条
// 会话探测免密 sudo：探测会话放行；真正的提权命令会话被测试服务器挂住
// 不答，黑洞代理同时停掉双向转发（复现半死连接的形状）。带超时的提权执
// 行必须按期返回超时结果并关闭整条 SSH 连接；连接死亡类错误不得泄漏为
// 报错，随后的提权执行只能拿到连接已关闭的错误。
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

// TestExecSudoOpenCancelKeepsConnectionAndClosesLateSession 是
// TestExecOpenCancelKeepsConnectionAndClosesLateSession 的提权孪生用例，
// 入口走 ExecSudoWriters：免密探测会话放行、提权命令会话挂住后取消调用
// 方上下文，必须按期返回取消错误且不算超时；这条 SSH 连接保持可用（随后
// 的提权执行成功）；释放挂住的开通道请求后，迟到的会话被后台关闭——以
// 协程数回到基线作为「无协程或会话泄漏」的可观察证明。
func TestExecSudoOpenCancelKeepsConnectionAndClosesLateSession(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHold)
	var sessionOpens atomic.Int32 // 只挂住提权命令的会话；免密探测与后续会话直接放行
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
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	goroutineBaseline := runtime.NumGoroutine()
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

	// 连接未被取消牵连：同一条连接上的下一次提权执行必须成功。
	res, err := c.ExecSudo(context.Background(), "ping", []byte("sudopw"), 0, 0)
	if err != nil || res.Stdout != "out:ping\n" {
		t.Fatalf("取消后的同连接提权执行：res=%+v err=%v", res, err)
	}
}
