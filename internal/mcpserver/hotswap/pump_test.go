package hotswap_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// 泵(纯字节搬运)的进程内单元测试:伪继任用 goroutine + os.Pipe 模拟,
// 逐条验收票 03 的泵语义。全链(真实继任进程)形态见 adopt_test.go。
//
// 接线约定:os.Pipe 恒按(读端, 写端)接 mustPipe 返回;变量名以用途命名——
// hostIn*/hostOut* 是宿主侧管道的两端,succIn*/succOut* 是泵侧(继任
// 标准输入输出)管道的两端;R 后缀=读端,W 后缀=写端。

// TestPumpRelaysBothDirectionsLossless:管道对回环——双向字节透传无损;
// 宿主侧读端关闭 → 泵关泵侧写端 → 继任读标准输入 EOF 自行退出 →
// 继任标准输出关闭 → 泵干净收场(返回 nil)。
func TestPumpRelaysBothDirectionsLossless(t *testing.T) {
	hostInR, hostInW := mustPipe(t)   // 泵读 hostInR;测试写 hostInW
	hostOutR, hostOutW := mustPipe(t) // 泵写 hostOutW;测试读 hostOutR
	succInR, succInW := mustPipe(t)   // 泵写 succInW;伪继任读 succInR(其标准输入)
	succOutR, succOutW := mustPipe(t) // 伪继任写 succOutW(其标准输出);泵读 succOutR
	t.Cleanup(func() {
		hostInW.Close()
		hostOutR.Close()
		succInR.Close()
		succOutW.Close()
	})

	// 伪继任:全双工回声;标准输入 EOF 时关闭两端(模拟进程退出的句柄关闭)。
	succDone := make(chan struct{})
	go func() {
		defer close(succDone)
		defer succInR.Close()
		defer succOutW.Close()
		io.Copy(succOutW, succInR)
	}()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- hotswap.Pump(hostInR, hostOutW, succInW, succOutR) }()

	payload := testPayload(128 * 1024)
	go writeChunks(t, hostInW, payload)
	got := readExact(t, hostOutR, len(payload), 30*time.Second)
	assertBytesEqual(t, "bidirectional relay", got, payload)

	// 宿主侧读端关闭(宿主不再发字节)
	hostInW.Close()
	select {
	case err := <-pumpDone:
		if err != nil {
			t.Fatalf("clean wind-down must return nil, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not wind down after host closed its write side")
	}
	select {
	case <-succDone:
	case <-time.After(5 * time.Second):
		t.Fatal("fake successor never observed standard-input EOF")
	}
}

// TestPumpExitsOnSuccessorSideWriteFailure:伪造对端关闭——继任侧读端
// (继任的标准输入)先关,泵往泵侧管道写必失败 → 泵退出并报错。
func TestPumpExitsOnSuccessorSideWriteFailure(t *testing.T) {
	hostInR, hostInW := mustPipe(t)
	hostOutR, hostOutW := mustPipe(t)
	succInR, succInW := mustPipe(t)
	succOutR, succOutW := mustPipe(t)
	t.Cleanup(func() {
		hostInW.Close()
		hostOutR.Close()
		succOutW.Close() // 数据面 EOF,让泵里可能仍阻塞的泵侧读协程退出
	})

	succInR.Close() // 伪造:继任关闭了自己的标准输入(泵侧管道读端没了)

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- hotswap.Pump(hostInR, hostOutW, succInW, succOutR) }()

	// 注入宿主→继任流量:写往已无读端的泵侧管道必须报错
	withDeadline(t, "host write after successor closed stdin", 5*time.Second, func() error {
		_, err := hostInW.Write([]byte("PING"))
		return err
	})
	select {
	case err := <-pumpDone:
		if err == nil || !strings.Contains(err.Error(), "write to successor") {
			t.Fatalf("err = %v, want a successor-side write failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not exit on successor-side write failure")
	}
}

// TestPumpExitsOnHostWriteFailure:宿主侧写失败(宿主读端已关)→ 泵退出并报错。
func TestPumpExitsOnHostWriteFailure(t *testing.T) {
	hostInR, hostInW := mustPipe(t)
	hostOutR, hostOutW := mustPipe(t)
	succInR, succInW := mustPipe(t)
	succOutR, succOutW := mustPipe(t)
	t.Cleanup(func() {
		hostInW.Close()
		succInR.Close()
		succOutW.Close()
	})

	hostOutR.Close() // 伪造:宿主侧读端已关 → 泵写宿主必失败

	// 伪继任:发一个字节,然后等泵收场(泵会关泵侧写端,这里随之退出)
	go func() {
		defer succOutW.Close()
		defer succInR.Close()
		if _, err := succOutW.Write([]byte("X")); err != nil {
			return
		}
		one := make([]byte, 1)
		io.ReadFull(succInR, one) // 等待泵侧写端关闭;错误即退出
	}()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- hotswap.Pump(hostInR, hostOutW, succInW, succOutR) }()

	select {
	case err := <-pumpDone:
		if err == nil || !strings.Contains(err.Error(), "write to host") {
			t.Fatalf("err = %v, want a host-side write failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not exit on host-side write failure")
	}
}

// TestPumpClosesSuccessorStdinOnHostClose:宿主侧读端关闭 → 泵关泵侧管道
// (继任可观察到标准输入 EOF),全程不用进程句柄杀继任。
func TestPumpClosesSuccessorStdinOnHostClose(t *testing.T) {
	hostInR, hostInW := mustPipe(t)
	succInR, succInW := mustPipe(t)
	succOutR, succOutW := mustPipe(t)
	t.Cleanup(func() {
		hostInW.Close()
		succInR.Close()
		succOutW.Close()
	})

	observedEOF := make(chan struct{})
	go func() { // 伪继任:只观察标准输入 EOF
		defer close(observedEOF)
		defer succInR.Close()
		defer succOutW.Close()
		io.Copy(io.Discard, succInR)
	}()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- hotswap.Pump(hostInR, io.Discard, succInW, succOutR) }()

	hostInW.Close() // 宿主侧读端关闭(宿主不再发字节)
	select {
	case <-observedEOF:
	case <-time.After(10 * time.Second):
		t.Fatal("successor never observed standard-input EOF: pump did not close the pump-side pipe")
	}
	select {
	case err := <-pumpDone:
		if err != nil {
			t.Fatalf("host-close wind-down must return nil, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not wind down")
	}
}
