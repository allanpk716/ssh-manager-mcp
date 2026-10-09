package hotswap_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// Handover(领养启动 + 就绪协议 + 泵 + 回退)的全链测试:继任都是本测试
// 二进制的再执行形态(角色见 main_test.go),宿主由测试进程用管道扮演。

// newHostPipes 建宿主↔桥的标准输入输出管道:桥侧持 hostIn(读,桥的标准
// 输入)/hostOut(写,桥的标准输出),测试侧持 hostInW/hostOutR,扮演宿主。
func newHostPipes(t *testing.T) (hostIn, hostOut, hostInW, hostOutR *os.File) {
	t.Helper()
	hostIn, hostInW = mustPipe(t)
	hostOutR, hostOutW := mustPipe(t)
	t.Cleanup(func() {
		hostInW.Close()
		hostOutR.Close()
		hostIn.Close()
		hostOutW.Close()
	})
	return hostIn, hostOutW, hostInW, hostOutR
}

// TestHandoverFirstGenLoopback:验收①+⑥——首代拉起→就绪→退位前应答钩子
// (写回应答)→泵化,双向字节透传无损;应答回声先于泵化转发的字节到达。
func TestHandoverFirstGenLoopback(t *testing.T) {
	hostIn, hostOut, hostInW, hostOutR := newHostPipes(t)
	reply := []byte("REPLY-FROM-OLD-BRIDGE\n")
	hookDone := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- hotswap.Handover(hotswap.Options{
			Exe:        os.Args[0],
			Env:        []string{testRoleEnv + "=echo"},
			Session:    hotswap.Session{"mcp_protocol": "2025-06-18", "host_caps": "tools"},
			Generation: hotswap.GenerationFirst,
			HostIn:     hostIn,
			HostOut:    hostOut,
			// 会话参数经环境传给继任:echo 角色若拿不到 SSHMGR_HOTSWAP_READY
			// 就写不出就绪文件,下面的等待会超时——即隐式验证了参数管道。
			ReadyTimeout: 5 * time.Second,
			PollInterval: 20 * time.Millisecond,
			BeforeRetire: func() error {
				_, werr := hostOut.Write(reply)
				close(hookDone) // 应答写回完成,先于泵化
				return werr
			},
		})
	}()

	<-hookDone // 钩子已执行,泵化即将开始

	payload := testPayload(256 * 1024)
	go writeChunks(t, hostInW, payload)
	got := readExact(t, hostOutR, len(reply)+len(payload), 30*time.Second)
	assertBytesEqual(t, "pre-retire reply", got[:len(reply)], reply)
	assertBytesEqual(t, "echo through pump", got[len(reply):], payload)

	// 干净收场:宿主侧读端关闭 → 继任读标准输入 EOF 自行退出 → 泵返回 nil
	hostInW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handover must wind down nil after host close, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Handover did not wind down after the host closed its side")
	}
}

// TestHandoverReadyTimeoutRollback:验收②——继任不写就绪文件,超时后调用方
// 收到失败与原因,自身宿主侧管道状态不变(可继续读写)。
func TestHandoverReadyTimeoutRollback(t *testing.T) {
	hostIn, hostOut, hostInW, hostOutR := newHostPipes(t)
	stopFile := filepath.Join(t.TempDir(), "stop")
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- hotswap.Handover(hotswap.Options{
			Exe:          os.Args[0],
			Env:          []string{testRoleEnv + "=stuck", testStopFileEnv + "=" + stopFile},
			Generation:   hotswap.GenerationFirst,
			HostIn:       hostIn,
			HostOut:      hostOut,
			ReadyTimeout: 400 * time.Millisecond,
			PollInterval: 20 * time.Millisecond,
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, hotswap.ErrReadyTimeout) {
			t.Fatalf("err = %v, want ErrReadyTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Handover did not fail on ready timeout")
	}
	if e := time.Since(start); e < 350*time.Millisecond {
		t.Fatalf("rollback after %v, before the 400ms deadline", e)
	}
	if e := time.Since(start); e > 5*time.Second {
		t.Fatalf("rollback too slow: %v", e)
	}

	// 父侧自身状态不变:宿主侧管道两端仍可写可读(继续以服务进程应答)
	withDeadline(t, "hostOut still writable after rollback", 5*time.Second, func() error {
		_, err := hostOut.Write([]byte("ALIVE"))
		return err
	})
	assertBytesEqual(t, "hostOut loopback", readExact(t, hostOutR, 5, 5*time.Second), []byte("ALIVE"))
	withDeadline(t, "hostIn direction intact", 5*time.Second, func() error {
		_, err := hostInW.Write([]byte("x"))
		return err
	})
}

// TestHandoverSuccessorExitedRollback:继任未写就绪就退出(退出码 7)→
// 快速失败并回退,不等满超时;宿主侧管道不受影响。
func TestHandoverSuccessorExitedRollback(t *testing.T) {
	hostIn, hostOut, _, hostOutR := newHostPipes(t)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- hotswap.Handover(hotswap.Options{
			Exe:          os.Args[0],
			Env:          []string{testRoleEnv + "=diefast"},
			Generation:   hotswap.GenerationFirst,
			HostIn:       hostIn,
			HostOut:      hostOut,
			ReadyTimeout: 5 * time.Second,
			PollInterval: 20 * time.Millisecond,
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, hotswap.ErrSuccessorExited) {
			t.Fatalf("err = %v, want ErrSuccessorExited", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Handover did not fail on early successor exit")
	}
	if e := time.Since(start); e > 3*time.Second {
		t.Fatalf("early-exit detection too slow: %v", e)
	}
	withDeadline(t, "hostOut still writable after rollback", 5*time.Second, func() error {
		_, err := hostOut.Write([]byte("ALIVE"))
		return err
	})
	assertBytesEqual(t, "hostOut loopback", readExact(t, hostOutR, 5, 5*time.Second), []byte("ALIVE"))
}

// TestHandoverHookFailureRollback:验收⑥回退臂——退位前应答钩子失败 →
// 不泵化、不退出,杀继任,返回钩子错误;宿主侧管道收不到任何被转发的字节。
func TestHandoverHookFailureRollback(t *testing.T) {
	hostIn, hostOut, hostInW, hostOutR := newHostPipes(t)
	sentinel := errors.New("reply write exploded")
	done := make(chan error, 1)
	go func() {
		done <- hotswap.Handover(hotswap.Options{
			Exe:          os.Args[0],
			Env:          []string{testRoleEnv + "=echo"},
			Generation:   hotswap.GenerationFirst,
			HostIn:       hostIn,
			HostOut:      hostOut,
			ReadyTimeout: 5 * time.Second,
			PollInterval: 20 * time.Millisecond,
			BeforeRetire: func() error { return sentinel },
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want the hook's sentinel error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handover must return (not pump) when the pre-retire hook fails")
	}

	// 泵没有启动:继任已被杀,任何宿主侧写入都不会被回声
	withDeadline(t, "write after rollback", 5*time.Second, func() error {
		_, err := hostInW.Write([]byte("PING"))
		return err
	})
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		b := make([]byte, 4)
		n, err := hostOutR.Read(b)
		ch <- readResult{n, err}
	}()
	select {
	case r := <-ch:
		if r.n > 0 {
			t.Fatalf("%d byte(s) relayed after hook-failure rollback; pump must not have started", r.n)
		}
	case <-time.After(300 * time.Millisecond):
	}
	withDeadline(t, "hostOut still writable", 5*time.Second, func() error {
		_, err := hostOut.Write([]byte("ALIVE"))
		return err
	})
}

// TestHandoverSuccessorClosedStdin:验收③全链——继任写完就绪后关闭自己的
// 标准输入;宿主侧流量经泵写往泵侧管道必失败 → 泵退出(Handover 带错返回)。
func TestHandoverSuccessorClosedStdin(t *testing.T) {
	hostIn, hostOut, hostInW, _ := newHostPipes(t)
	stopFile := filepath.Join(t.TempDir(), "stop")
	hookDone := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- hotswap.Handover(hotswap.Options{
			Exe:          os.Args[0],
			Env:          []string{testRoleEnv + "=stdinclose", testStopFileEnv + "=" + stopFile},
			Generation:   hotswap.GenerationFirst,
			HostIn:       hostIn,
			HostOut:      hostOut,
			ReadyTimeout: 5 * time.Second,
			PollInterval: 20 * time.Millisecond,
			BeforeRetire: func() error { close(hookDone); return nil },
		})
	}()
	<-hookDone // 就绪已确认、钩子已过,泵化在即(stdinclose 的标准输入此时已关)

	withDeadline(t, "host write while pump runs", 5*time.Second, func() error {
		_, err := hostInW.Write([]byte("PING"))
		return err
	})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Handover must return an error when the pump-side write fails")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not exit on successor-side write failure")
	}
	// 兜底:让挂住的伪继任自了(正常路径它已被 Handover 收掉)
	os.WriteFile(stopFile, []byte("stop"), 0o600)
}

// TestHandoverHostCloseEOF:验收④全链——宿主侧读端关闭 → 泵关泵侧管道 →
// 继任观察到标准输入 EOF 并自行退出(无进程句柄杀),泵干净返回 nil。
func TestHandoverHostCloseEOF(t *testing.T) {
	hostIn, hostOut, hostInW, hostOutR := newHostPipes(t)
	eofMarker := filepath.Join(t.TempDir(), "eof-marker")
	hookDone := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- hotswap.Handover(hotswap.Options{
			Exe:          os.Args[0],
			Env:          []string{testRoleEnv + "=echo", testEOFMarkerEnv + "=" + eofMarker},
			Generation:   hotswap.GenerationFirst,
			HostIn:       hostIn,
			HostOut:      hostOut,
			ReadyTimeout: 5 * time.Second,
			PollInterval: 20 * time.Millisecond,
			BeforeRetire: func() error { close(hookDone); return nil },
		})
	}()
	<-hookDone

	// 先来一轮回声,证明泵化正在透传
	withDeadline(t, "ping", 5*time.Second, func() error {
		_, err := hostInW.Write([]byte("PING1"))
		return err
	})
	assertBytesEqual(t, "echo before host close", readExact(t, hostOutR, 5, 5*time.Second), []byte("PING1"))

	// 宿主退出:关闭宿主侧写端(泵的宿主侧读端随之 EOF)
	hostInW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("host-close wind-down must return nil, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Handover did not wind down after the host closed")
	}
	waitForFile(t, eofMarker, 5*time.Second) // 继任真的观察到了标准输入 EOF
}

// TestHandoverLaterGeneration:验收⑤——本测试进程扮演最外层泵;二代桥
// (gen2bridge 角色)经 Handover(GenerationLater) 把同一泵侧管道交给三代
// (echo 角色)后退出;泵对端点换人无感知,透传如常,退位前钩子输出先至。
func TestHandoverLaterGeneration(t *testing.T) {
	hostIn, hostOut, hostInW, hostOutR := newHostPipes(t)
	toSuccR, toSuccW := mustPipe(t)     // 泵写 toSuccW → 继任(二代/三代)标准输入
	fromSuccR, fromSuccW := mustPipe(t) // 继任标准输出 → 泵读 fromSuccR
	t.Cleanup(func() {
		toSuccR.Close()
		toSuccW.Close()
		fromSuccR.Close()
		fromSuccW.Close()
	})

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- hotswap.Pump(hostIn, hostOut, toSuccW, fromSuccR) }()

	dir := t.TempDir()
	gen2Ready := filepath.Join(dir, "gen2-ready.json")
	gen3Ready := filepath.Join(dir, "gen3-ready.json")
	eofMarker := filepath.Join(dir, "gen3-eof")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		testRoleEnv+"=gen2bridge",
		hotswap.EnvReadyPath+"="+gen2Ready,
		testNextReadyEnv+"="+gen3Ready,
		testEOFMarkerEnv+"="+eofMarker,
	)
	cmd.Stdin = toSuccR
	cmd.Stdout = fromSuccW
	cmd.Stderr = os.Stderr
	hideTestChildWindow(cmd)
	gen2Exit := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// 子进程已持有自己那份句柄;父侧副本必须关,否则二代退出后管道 EOF 语义失效
	toSuccR.Close()
	fromSuccW.Close()
	go func() { gen2Exit <- cmd.Wait() }()

	// 二代就绪(它本身按同一领养协议被拉起)
	withDeadline(t, "gen2 ready", 10*time.Second, func() error {
		_, err := hotswap.WaitReady(gen2Ready, 5*time.Second, 20*time.Millisecond)
		return err
	})
	// 三代就绪(二代以 GenerationLater 拉起并直接继承同一泵侧管道)
	withDeadline(t, "gen3 ready", 10*time.Second, func() error {
		_, err := hotswap.WaitReady(gen3Ready, 5*time.Second, 20*time.Millisecond)
		return err
	})
	// 二代应答写回后退出
	select {
	case err := <-gen2Exit:
		if err != nil {
			t.Fatalf("gen2 bridge exited with %v, want clean exit after handover", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gen2 bridge did not exit after handing over to gen3")
	}

	// 泵无感知:三代接管同一泵侧管道,退位钩子的标记行先于三代回声到达宿主
	const hookLine = "GEN2HOOK\n"
	payload := testPayload(64 * 1024)
	go writeChunks(t, hostInW, payload)
	got := readExact(t, hostOutR, len(hookLine)+len(payload), 30*time.Second)
	assertBytesEqual(t, "gen2 hook line", got[:len(hookLine)], []byte(hookLine))
	assertBytesEqual(t, "gen3 echo through the same pump-side pipe", got[len(hookLine):], payload)

	// 收场:宿主关闭 → 三代读标准输入 EOF 自行退出 → 泵返回 nil
	hostInW.Close()
	select {
	case err := <-pumpDone:
		if err != nil {
			t.Fatalf("pump must wind down nil, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not wind down after the host closed")
	}
	waitForFile(t, eofMarker, 5*time.Second)
}
