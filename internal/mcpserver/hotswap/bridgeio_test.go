package hotswap_test

// bridgeio_test.go — 字节面接管的外部行为测试:记账配平、Hold/Retire 的
// 不丢不重、copier 两相转发、宿主 EOF 收场。

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// newTestIO 建一对(真实输入读端, 真实输出写端)管道并构 BridgeIO,
// 返回写入端/读端供测试扮演宿主。
func newTestIO(t *testing.T) (b *hotswap.BridgeIO, inW, outR *os.File) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b, err = hotswap.NewBridgeIO(inR, outW, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b, inW, outR
}

// readLineWithTimeout 从 r 读一行(超时判败)。
func readLineWithTimeout(t *testing.T, what string, r *os.File, d time.Duration) string {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		rd := bufio.NewReader(r)
		line, err := rd.ReadString('\n')
		ch <- res{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%s: %v", what, r.err)
		}
		return r.line
	case <-time.After(d):
		t.Fatalf("%s: timed out after %v", what, d)
		return ""
	}
}

func TestBridgeIOCountsCallsAndResponses(t *testing.T) {
	b, inW, outR := newTestIO(t)
	conn, err := b.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 宿主发一个带 ID 请求 + 一个通知:只有前者计进账。
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req1, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notif, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := req1.(*jsonrpc.Request).Method; m != "tools/call" {
		t.Fatalf("first message method = %q", m)
	}
	if m := notif.(*jsonrpc.Request).Method; m != "notifications/initialized" {
		t.Fatalf("second message method = %q", m)
	}
	if b.Quiescent() {
		t.Fatal("one unanswered call: must not be quiescent")
	}

	// SDK 写出该请求的应答:计出账,字节经 copier 落到宿主侧,配平。
	resp := &jsonrpc.Response{ID: req1.(*jsonrpc.Request).ID, Result: json.RawMessage(`{"ok":true}`)}
	if err := conn.Write(ctx, resp); err != nil {
		t.Fatal(err)
	}
	if !b.Quiescent() {
		t.Fatal("call answered: must be quiescent")
	}
	line := readLineWithTimeout(t, "copied response", outR, 2*time.Second)
	var wire struct {
		ID     int64 `json:"id"`
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &wire); err != nil {
		t.Fatalf("copied line is not the response: %v (%s)", err, line)
	}
	if wire.ID != 41 || !wire.Result.OK {
		t.Fatalf("unexpected copied response: %s", line)
	}
}

func TestBridgeIOHoldRetireNoLoss(t *testing.T) {
	b, inW, outR := newTestIO(t)
	conn, err := b.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 请求 A:Hod 前到达,进 SDK。
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}

	// Hold 之后到达的请求 B:不进 SDK(扣住)。
	if !b.Hold() {
		t.Fatal("Hold must succeed on a live host side")
	}
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { _, err := conn.Read(ctx); blocked <- err }()
	select {
	case err := <-blocked:
		t.Fatalf("held request leaked into the SDK side: read returned %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Retire:扣住的 B 冲进泵侧,SDK 读侧收 EOF,copier 切泵侧。
	hostInR, hostInW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	hostOutR, hostOutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b.Retire(hostInW, hostOutR)
	line := readLineWithTimeout(t, "held request on pump side", hostInR, 2*time.Second)
	if line != `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n" {
		t.Fatalf("held request bytes wrong on pump side: %q", line)
	}
	if err := <-blocked; err == nil {
		t.Fatal("SDK-side read must see EOF after retire")
	}

	// Retire 后宿主字节直通泵侧:写进真实输入,应从泵侧读端看到。
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	line = readLineWithTimeout(t, "post-retire request on pump side", hostInR, 2*time.Second)
	if line != `{"jsonrpc":"2.0","id":3,"method":"ping"}`+"\n" {
		t.Fatalf("post-retire bytes wrong: %q", line)
	}

	// copier phase 2:泵侧应答经真实输出转给宿主。
	if _, err := hostOutW.WriteString(`{"jsonrpc":"2.0","id":3,"result":{}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	line = readLineWithTimeout(t, "pump-side response copied to host", outR, 2*time.Second)
	if line != `{"jsonrpc":"2.0","id":3,"result":{}}`+"\n" {
		t.Fatalf("phase-2 bytes wrong: %q", line)
	}

	hostInW.Close()
	hostOutW.Close()
}

func TestBridgeIOHostEOF(t *testing.T) {
	b, inW, _ := newTestIO(t)
	inW.Close() // 宿主断开
	// 等 feeder 退役:Hold 变 false。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !b.Hold() {
			return // 收场符合预期
		}
		b.ReleaseHold()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Hold kept succeeding after host-side EOF (feeder never retired)")
}

// TestBridgeIOReleaseHold 把 Hold 撤销后扣住的字节回到 SDK 侧(退位放弃路径)。
func TestBridgeIOReleaseHold(t *testing.T) {
	b, inW, _ := newTestIO(t)
	conn, err := b.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !b.Hold() {
		t.Fatal("Hold failed")
	}
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":9,"method":"ping"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 让 feeder 扣住它
	b.ReleaseHold()
	ctx := context.Background()
	m, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("released request must reach the SDK side: %v", err)
	}
	if req, ok := m.(*jsonrpc.Request); !ok || req.Method != "ping" {
		t.Fatalf("unexpected released message: %#v", m)
	}
}

// TestBridgeIOHoldRetireNoLoss 把 Hold 期间扣住的超过泵侧管道缓冲的大块
// 字节交割给泵侧:回归测试——同步冲刷会在泵启动前挂死(管道满),异步
// 冲刷 + 懒前置必须既不挂也不乱序。
func TestBridgeIOHoldRetireLargeHeld(t *testing.T) {
	b, inW, _ := newTestIO(t)
	if !b.Hold() {
		t.Fatal("Hold failed")
	}
	payload := testPayload(256 * 1024) // 远超 64KiB 管道缓冲
	// 全部字节在 hold 态写入:确定性全数扣住(写方收工 = feeder 已把真实
	// 输入管道排空,字节全在扣取缓冲里)。
	written := make(chan struct{})
	go func() {
		writeChunks(t, inW, payload)
		close(written)
	}()
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("payload writer did not finish (feeder not consuming while holding?)")
	}

	hostInR, hostInW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	hostOutR, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Retire 不得挂住(挂住即回归:泵侧管道满、泵在读端但还没人读)。
	done := make(chan struct{})
	go func() {
		b.Retire(hostInW, hostOutR)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Retire blocked on flushing a held block larger than the pipe buffer")
	}
	// 扮演泵:排干泵侧读端,应收到全部扣住字节且次序正确。
	got := readExact(t, hostInR, len(payload), 5*time.Second)
	assertBytesEqual(t, "held payload on pump side", got, payload)
	hostInW.Close()
	hostOutR.Close()
}

// 确认 copier phase1 的排干信号在 SDK 写端关闭后置位。
func TestBridgeIOCopierPhase1Done(t *testing.T) {
	b, _, _ := newTestIO(t)
	if b.WaitCopierPhase1(100 * time.Millisecond) {
		t.Fatal("phase 1 must not be done while the SDK write end is open")
	}
	b.Close() // Close 关闭 SDK 侧写端 → copier 读到 EOF
	if !b.WaitCopierPhase1(2 * time.Second) {
		t.Fatal("phase 1 must complete after the SDK write end closes")
	}
}
