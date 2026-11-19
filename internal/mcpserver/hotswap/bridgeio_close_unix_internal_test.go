//go:build unix

package hotswap

// bridgeio_close_unix_internal_test.go — Close 等待窥视探测路径的确定性
// 次序测试,沿用 peek_unix_internal_test.go 的包内注入先例(poll 调用可
// 替换)。全平台的外部行为重复测试在 bridgeio_test.go,靠竞争检测命中
// 轮询窗口;Unix 的 poll 与关闭同一文件在竞争检测器下不成对,故在这里
// 用注入把次序钉死。

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBridgeIOCloseWaitsForInFlightPeekProbe:输入转发循环已发起在途窥视探测
// (poll 系统调用被注入扣住)时调用 Close——探测返回前 Close 不得返回;
// 探测放行后 Close 必须有界收尾。若 Close 不等待,调用方在 Close 返回后
// 立即关闭真实输入读端,就与探测对同一文件的访问并发。
func TestBridgeIOCloseWaitsForInFlightPeekProbe(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		t.Fatal(err)
	}
	b, err := NewBridgeIO(inR, outW, true)
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	original := pollPipeReadableFunc
	setPollPipeReadable(func(fds []unix.PollFd) (int, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return 0, nil
	})
	defer releaseProbe()
	t.Cleanup(func() {
		releaseProbe()
		setPollPipeReadable(original)
	})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("peek probe did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		b.Close()
		close(closeDone)
	}()

	select {
	case <-b.feeder.frozen:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not request the input forwarding loop to stop")
	}

	// 关键断言:探测尚未返回(release 未放行),Close 不得返回。
	select {
	case <-closeDone:
		t.Fatal("Close returned while the peek probe was still running")
	case <-time.After(50 * time.Millisecond):
	}

	releaseProbe()

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the peek probe finished")
	}

	// 竞争现场的收尾交错:Close 返回后,调用方才关闭真实输入读端。
	if err := inR.Close(); err != nil {
		t.Fatal(err)
	}
	inW.Close()
	outR.Close()
	outW.Close()
}

// TestBridgeIOCloseSkipsWaitWhenRealReadBlocked:平台窥视不可用、输入
// 转发循环退回真实输入的阻塞读后,该读无法由桥侧取消(不允许关闭真实
// 输入);Close 不得等它返回,否则收尾悬挂。阻塞中的读随进程退出回收。
func TestBridgeIOCloseSkipsWaitWhenRealReadBlocked(t *testing.T) {
	original := pollPipeReadableFunc
	setPollPipeReadable(func(fds []unix.PollFd) (int, error) {
		return 0, errors.New("injected: platform peek unavailable")
	})
	t.Cleanup(func() { setPollPipeReadable(original) })

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		t.Fatal(err)
	}
	b, err := NewBridgeIO(inR, outW, true)
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		t.Fatal(err)
	}

	// 注入立即返回,转发循环有充裕时间落进真实输入的阻塞读。
	time.Sleep(20 * time.Millisecond)

	closeDone := make(chan struct{})
	go func() {
		b.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung waiting for a blocking read of the real input")
	}

	// 收尾交错保持合法用法:Close 返回后关闭输入读端,残留的转发循环
	// 随读错误自行退出。
	inR.Close()
	inW.Close()
	outR.Close()
	outW.Close()
}
