package hotswap

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// retireLaterTestIO 是退位编排的字节面测试替身:它把冻结超时与测试主流程
// 同步起来,并记录是否在确认冻结前关闭了 MCP 软件开发工具包读侧。
type retireLaterTestIO struct {
	parkResult       bool
	quiescentResult  bool
	copierDrain      bool
	endSdkReadCalled bool

	firstFreezeSeen  chan struct{}
	allowFreezeDone  chan struct{}
	firstFreezeReady bool
}

func (io *retireLaterTestIO) Hold() bool { return false }
func (io *retireLaterTestIO) ReleaseHold() {
}
func (io *retireLaterTestIO) Retire(_, _ *os.File) {
}
func (io *retireLaterTestIO) Park() bool { return io.parkResult }
func (io *retireLaterTestIO) WaitFrozen(time.Duration) bool {
	if !io.firstFreezeReady {
		io.firstFreezeReady = true
		close(io.firstFreezeSeen)
		<-io.allowFreezeDone
		return false
	}
	return true
}
func (io *retireLaterTestIO) EndSdkRead() { io.endSdkReadCalled = true }
func (io *retireLaterTestIO) WaitCopierPhase1(time.Duration) bool {
	return io.copierDrain
}
func (io *retireLaterTestIO) Quiescent() bool { return io.quiescentResult }

// freezeTimeoutIO 包装真实的字节面接管:第一次冻结等待立即模拟超时,之后
// 交还给真实实现。这样能确定地停在「停读尚未确认」的窗口里。
type freezeTimeoutIO struct {
	*BridgeIO
	timeoutSeen      chan struct{}
	firstFreezeDone  bool
	endSdkReadCalled atomic.Bool
}

func (io *freezeTimeoutIO) WaitFrozen(d time.Duration) bool {
	if !io.firstFreezeDone {
		io.firstFreezeDone = true
		close(io.timeoutSeen)
		return false
	}
	return io.BridgeIO.WaitFrozen(d)
}

func (io *freezeTimeoutIO) EndSdkRead() {
	io.endSdkReadCalled.Store(true)
	io.BridgeIO.EndSdkRead()
}

// TestReloadServiceRetireLaterWaitsForConfirmedFreeze 验证后续代退位的新
// 顺序:冻结等待超时后不关 MCP 软件开发工具包读侧;停读确认后才收场。
func TestReloadServiceRetireLaterWaitsForConfirmedFreeze(t *testing.T) {
	io := &retireLaterTestIO{
		parkResult:      true,
		quiescentResult: true,
		copierDrain:     true,
		firstFreezeSeen: make(chan struct{}),
		allowFreezeDone: make(chan struct{}),
	}
	rs := NewReloadService(ReloadConfig{
		IO:                io,
		QuiesceTimeout:    20 * time.Millisecond,
		SessionEndTimeout: 20 * time.Millisecond,
	})

	retired := make(chan struct{})
	go func() {
		rs.retireLater(&SuccessorHandle{})
		close(retired)
	}()

	select {
	case <-io.firstFreezeSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("retire (later generation) did not reach the freeze wait")
	}
	if io.endSdkReadCalled {
		t.Fatal("SDK read side was closed before stopping reads was confirmed")
	}

	close(io.allowFreezeDone)
	select {
	case <-retired:
	case <-time.After(2 * time.Second):
		t.Fatal("retire (later generation) did not finish after freeze confirmation")
	}
	if !io.endSdkReadCalled {
		t.Fatal("SDK read side was not closed after freeze confirmation")
	}
}

// TestReloadServiceRetireLaterKeepsServingDuringFreezeWait 用真实字节面验证:
// 冻结等待超时后,旧代仍会补完并应答停读前已进入转发流程的半行请求;直到
// 停读确认后才关闭 MCP 软件开发工具包读侧。
func TestReloadServiceRetireLaterKeepsServingDuringFreezeWait(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outR.Close()
	defer outW.Close()
	bio, err := NewBridgeIO(inR, outW, true)
	if err != nil {
		t.Fatal(err)
	}
	defer bio.Close()
	conn, err := bio.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	half := `{"jsonrpc":"2.0","id":91,"method":"big"`
	if _, err := inW.WriteString(half); err != nil {
		t.Fatal(err)
	}
	// 等 MCP 软件开发工具包读走这半行:字节两层配平说明它已进入解析器,
	// 但还没有换行,停读不能在消息边界生效。
	deadline := time.Now().Add(2 * time.Second)
	for {
		delivered := bio.sdkInDelivered.Load()
		consumed := bio.sdkInConsumed.Load()
		if delivered > 0 && delivered == consumed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("half frame was not consumed: delivered=%d consumed=%d", delivered, consumed)
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // 让 feeder 完成帧边界状态的锁内更新

	io := &freezeTimeoutIO{BridgeIO: bio, timeoutSeen: make(chan struct{})}
	rs := NewReloadService(ReloadConfig{
		IO:                 io,
		QuiesceTimeout:     2 * time.Second,
		SessionEndTimeout:  2 * time.Second,
		CopierDrainTimeout: 10 * time.Millisecond,
	})
	retired := make(chan struct{})
	go func() {
		rs.retireLater(&SuccessorHandle{})
		close(retired)
	}()

	select {
	case <-io.timeoutSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("retire (later generation) did not reach a simulated freeze timeout")
	}
	if io.endSdkReadCalled.Load() {
		t.Fatal("SDK read side was closed before the old generation finished serving")
	}

	if _, err := inW.WriteString(`,"params":{"x":1}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan jsonrpc.Message, 1)
	go func() {
		message, readErr := conn.Read(context.Background())
		if readErr != nil {
			t.Errorf("old generation did not serve the completed frame: %v", readErr)
			return
		}
		readDone <- message
	}()
	var request *jsonrpc.Request
	select {
	case message := <-readDone:
		request, _ = message.(*jsonrpc.Request)
	case <-time.After(2 * time.Second):
		t.Fatal("completed frame did not reach the MCP software development kit side")
	}
	if request == nil || request.Method != "big" {
		t.Fatalf("unexpected completed frame: %#v", request)
	}
	response := &jsonrpc.Response{ID: request.ID, Result: json.RawMessage(`{"ok":true}`)}
	if err := conn.Write(context.Background(), response); err != nil {
		t.Fatalf("old generation could not answer the completed frame: %v", err)
	}

	select {
	case <-retired:
	case <-time.After(5 * time.Second):
		t.Fatal("retire (later generation) did not finish after freeze confirmation")
	}
	if !io.endSdkReadCalled.Load() {
		t.Fatal("SDK read side was not closed after freeze confirmation")
	}
}
