package hotswap

// bridgeio.go — 桥进程标准输入输出的字节层接管(reload_self 集成的地基)。
//
// 为什么要接管:退位为泵要求「宿主侧字节面」在退位瞬间无丢失、无重复地
// 交给泵,而 Go MCP SDK 的 StdioTransport 会直接读写进程的 0/1 号句柄,
// 且其 jsonrpc2 层在「读侧 EOF 后」会丢弃尚未写出的应答(读侧一断即进入
// shuttingDown,新写一律跳过)。因此字节面必须由本层独占:
//
//   - feeder(仅首代)独占真实标准输入的读:SDK 只读 feeder 喂的管道;
//     退位时 feeder 把目的地原子切到泵侧管道,切换期间读到的字节先扣住,
//     保证切换前后不丢不重。
//   - copier 独占真实标准输出的写:SDK 的应答先进管道,由 copier 顺序
//     搬到真实标准输出;退位后 copier 的数据源切到泵侧管道。顺序性保证
//     「旧桥应答先落盘、继任输出后落盘」(规格 F5 的落盘次序)。
//   - interceptConn 在 SDK 与字节面之间数账:进账=已送达 SDK 的带 ID
//     请求(call)数,出账=已写进 SDK 侧管道的应答数。两者相等即「SDK
//     已见过的每个请求其应答都已写出」——这是退位前的静默判据(证明见
//     Quiescent 的注释)。

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BridgeIO 持有桥进程标准输入输出的接管面。一个桥进程一份,由 run.go
// 两形态入口在起服务前构造;首代与被领养的后续代都经 feeder 接管读侧
// (ownStdin=true)——后续代退位需要「停读」语义(见 Park),直读真实
// 标准输入的老形态仅供不打算退位的调用方。
type BridgeIO struct {
	realIn  *os.File // 真实标准输入(feeder 独占读)
	realOut *os.File // 真实标准输出(由 copier 独占写)

	// SDK 侧管道:SDK 的传输只碰这两端,不碰真实句柄。
	sdkInR, sdkInW   *os.File // feeder 写 → SDK 读(ownStdin 时存在)
	sdkOutR, sdkOutW *os.File // SDK 写 → copier 读

	feeder *feeder // ownStdin=false 时为 nil(SDK 直读真实标准输入,不可退位)
	copier *copier

	callsIn, responsesOut atomic.Int64 // interceptConn 的进/出账

	copierPhase1Done chan struct{} // copier 排干 SDK 侧管道(或写失败)时关闭,一次
	copierQuit       chan struct{} // Close 时关闭,让 copier 不再等退位
	closeOnce        sync.Once
}

// NewBridgeIO 构造接管面并启动搬运 goroutine。ownStdin=true 时 feeder 接管
// 真实标准输入(SDK 只读 feeder 喂的管道)——退位编排(首代 Hold/Retire、
// 后续代 Park/EndSdkRead)都以此为前提,桥入口对两代一律传 true;false
// 仅供确定不会退位的调用方(SDK 直读真实标准输入,届时 Hold/Park 一律
// 返回 false)。
func NewBridgeIO(realIn, realOut *os.File, ownStdin bool) (*BridgeIO, error) {
	sdkOutR, sdkOutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	b := &BridgeIO{
		realIn:           realIn,
		realOut:          realOut,
		sdkOutR:          sdkOutR,
		sdkOutW:          sdkOutW,
		copierPhase1Done: make(chan struct{}),
		copierQuit:       make(chan struct{}),
	}
	if ownStdin {
		sdkInR, sdkInW, err := os.Pipe()
		if err != nil {
			sdkOutR.Close()
			sdkOutW.Close()
			return nil, err
		}
		b.sdkInR, b.sdkInW = sdkInR, sdkInW
		b.feeder = newFeeder(realIn, sdkInW)
	}
	b.copier = newCopier(sdkOutR, realOut, b.copierPhase1Done, b.copierQuit)
	return b, nil
}

// Transport 返回 SDK 服务用的传输:读写都走 SDK 侧管道(读侧在被领养桥
// 上直读真实标准输入),并经 interceptConn 记账。
func (b *BridgeIO) Transport() mcp.Transport { return &bridgeTransport{b: b} }

type bridgeTransport struct{ b *BridgeIO }

func (t *bridgeTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	var r io.ReadCloser = t.b.realIn
	if t.b.sdkInR != nil {
		r = t.b.sdkInR
	}
	delegate, err := (&mcp.IOTransport{Reader: r, Writer: t.b.sdkOutW}).Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interceptConn{Connection: delegate, b: t.b}, nil
}

// interceptConn 在连接层记账:每个带 ID 的进站请求计一笔进账,每个成功
// 写出的应答计一笔出账。通知(无 ID)与出站请求不计——它们不参与
// 「请求必有应答」的配平。
type interceptConn struct {
	mcp.Connection
	b *BridgeIO
}

func (c *interceptConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	m, err := c.Connection.Read(ctx)
	if err == nil {
		if req, ok := m.(*jsonrpc.Request); ok && req.IsCall() {
			c.b.callsIn.Add(1)
		}
	}
	return m, err
}

func (c *interceptConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	err := c.Connection.Write(ctx, msg)
	if err == nil {
		if _, ok := msg.(*jsonrpc.Response); ok {
			c.b.responsesOut.Add(1)
		}
	}
	return err
}

// Quiescent 是退位静默判据:SDK 已见的每个带 ID 请求,其应答都已写进
// SDK 侧管道(由 copier 顺序搬到真实标准输出)。正确性:任一已送达而
// 未应答的请求都会使进账严格大于出账;因此在「发起换手的那个工具处理器
// 已返回(其应答在 jsonrpc2 的出账路径上,写完才减 pending)」之后的任一
// 相等瞬间,该次调用的应答必已写出。
func (b *BridgeIO) Quiescent() bool {
	return b.callsIn.Load() == b.responsesOut.Load()
}

// Hold 让 feeder 停止向 SDK 侧转发(此后读到的字节扣在缓冲里)。返回
// false 表示 feeder 不存在或宿主侧读端已 EOF(此时换手已无意义)。Hold
// 之后到 Retire/ReleaseHold 之前到达的请求不会进 SDK——它们将由继任
// 经泵服务,宿主无感知。
func (b *BridgeIO) Hold() bool {
	return b.feeder != nil && b.feeder.hold()
}

// ReleaseHold 撤销 Hold,回到向 SDK 转发(退位放弃路径)。
func (b *BridgeIO) ReleaseHold() {
	if b.feeder != nil {
		b.feeder.releaseHold()
	}
}

// Retire 把字节面交割给泵:feeder 的目的地原子切到 hostInW(扣住的字节
// 随之冲进泵侧),SDK 读侧管道写端关闭(SDK 读到 EOF,会话按序收场),
// SDK 写端也一并关闭(栅栏:静默判据已过,旧 SDK 此后不得再写一字节;
// 违者写失败、连接自断),copier 的数据源切换到 hostOutR。
// hostInW/hostOutR 是退位侧(泵面)管道的写端/读端,由调用方创建、与
// 本调用一并交割。
func (b *BridgeIO) Retire(hostInW, hostOutR *os.File) {
	if b.feeder != nil {
		b.feeder.retireTo(hostInW)
	}
	b.sdkOutW.Close()
	b.copier.switchTo(hostOutR)
}

// Park 是后续代(被领养桥)退位的第一步:登记停读请求。feeder 将在
// 下一个消息边界(转发流的换行处)停止消费真实标准输入——此后到达的
// 字节留在管道里,归直接继承本进程句柄的继任;停读前已进入转发流程的
// 半行会补完并照常送达 SDK(由静默判据兜底应答)。返回 false 表示 feeder
// 不存在或宿主侧已断。生效时刻经 WaitFrozen 可等。
func (b *BridgeIO) Park() bool {
	return b.feeder != nil && b.feeder.park()
}

// WaitFrozen 等 feeder 冻结(hold/park 已在消息边界生效,或 feeder 已
// 死亡):此后转发循环不再投递任何字节,关闭 SDK 读侧(EndSdkRead /
// Retire)不再与写路径竞争。超时返回 false(宿主停在半行等病态输入,
// 调用方按兜底路径继续)。
func (b *BridgeIO) WaitFrozen(d time.Duration) bool {
	if b.feeder == nil {
		return true
	}
	select {
	case <-b.feeder.frozen:
		return true
	case <-time.After(d):
		return false
	}
}

// EndSdkRead 关闭 SDK 读侧管道的写端:SDK 读到 EOF,会话按序收场。前置
// 条件是静默判据已过且 feeder 已冻结——SDK 此后再收不到新请求,也不会
// 有未写出的应答被 jsonrpc2 的读侧 EOF 路径丢弃。后续代退位专用:真实
// 标准输入不动(句柄已由继任继承,最外层泵无感知)。
func (b *BridgeIO) EndSdkRead() {
	if b.feeder != nil {
		b.feeder.endSdkRead()
	}
}

// WaitCopierPhase1 等待 copier 排干 SDK 侧管道(SDK 应答全部落到真实
// 标准输出)。超时返回 false(顺序性仍然成立——copier 顺序搬运,只是
// 尚未确认;调用方可选择记日志后继续)。
func (b *BridgeIO) WaitCopierPhase1(d time.Duration) bool {
	select {
	case <-b.copierPhase1Done:
		return true
	case <-time.After(d):
		return false
	}
}

// Close 收尾:关闭 SDK 侧管道、唤醒 copier。只在进程将退时调用;真实
// 句柄不关(随进程回收)。
func (b *BridgeIO) Close() {
	b.closeOnce.Do(func() {
		close(b.copierQuit)
		if b.feeder != nil {
			b.feeder.stop()
		}
		if b.sdkInR != nil {
			b.sdkInR.Close()
		}
		b.sdkInW.Close()
		b.sdkOutR.Close()
		b.sdkOutW.Close()
	})
}

// ---------------------------------------------------------------------------
// feeder:真实标准输入的独占读者。
//
// 读循环是窥视式的:管道无数据时不发起阻塞读(短暂轮询)。「挂起的读
// 迟到返回」的字节已离开管道,既回不去也送不到继任——Park(后续代停读)
// 需要一个「无在飞读」的确定点生效,阻塞读给不出这个点。平台不支持
// 窥视时退回阻塞读,Park 的收尾由调用方兜底(见 ReloadService.retireLater)。
//
// Hold(首代冻结)与 Park(后续代停读)都只在消息边界生效:线上帧是换行
// 分隔 JSON(串内换行必转义),转发流最后字节是 '\n' 即帧边界。跨请求
// 瞬间的大帧由此不被撕成两半(规格「在消息边界停止解析」)。

type feederState int

const (
	feederRun  feederState = iota // 转发进 SDK 侧管道
	feederHold                    // 扣住新字节(Hold 与 Retire/Release 之间)
	feederPark                    // 后续代退位:停止消费真实标准输入(循环已退出)
	feederPump                    // 退位后:转发进泵侧管道
)

// feederIdlePoll 是窥视无数据时的轮询间隔;peekProbeTimeout 是单次窥视
// 的卡死探测预算(正常窥视在微秒级返回,超时即认定该句柄形态不可窥视)。
const (
	feederIdlePoll   = 2 * time.Millisecond
	peekProbeTimeout = 250 * time.Millisecond
)

type feeder struct {
	in *os.File

	mu         sync.Mutex
	state      feederState
	dest       *os.File      // 当前转发目的地(run: SDK 写端;pump: 泵侧写端)
	held       []byte        // hold 期间扣住的字节
	dead       bool          // 真实标准输入已 EOF/出错
	wantHold   bool          // Hold 已登记,待消息边界生效
	wantPark   bool          // Park 已登记,待消息边界生效
	noPeek     bool          // 窥视卡死后置真:本 feeder 永久退回阻塞读
	atBoundary bool          // 已转发流的最后字节是 '\n'(或尚无转发)——帧边界
	frozen     chan struct{} // hold/park 生效或 feeder 死亡时关闭,一次
	frozenOnce sync.Once
}

// peekProbe 窥视一次,带卡死探测:stuck=true 表示窥视调用在预算内没有
// 返回(调用方应永久放弃窥视)。
func (f *feeder) peekProbe() (readable, ok, stuck bool) {
	f.mu.Lock()
	noPeek := f.noPeek
	f.mu.Unlock()
	if noPeek {
		return false, false, false
	}
	type res struct {
		readable, ok bool
	}
	ch := make(chan res, 1)
	go func() { r, k := peekReadable(f.in); ch <- res{r, k} }()
	select {
	case r := <-ch:
		return r.readable, r.ok, false
	case <-time.After(peekProbeTimeout):
		return false, false, true
	}
}

func newFeeder(in, dest *os.File) *feeder {
	f := &feeder{in: in, dest: dest, atBoundary: true, frozen: make(chan struct{})}
	go f.loop()
	return f
}

func (f *feeder) loop() {
	buf := make([]byte, 32*1024)
	for {
		readable, known, stuck := f.peekProbe()
		if stuck {
			// 该句柄形态上窥视调用不返回(实测:本进程自建的 os.Pipe 读端
			// 被另一进程并发读时,PeekNamedPipe 会卡死;生产形态——继承的
			// 标准输入句柄——不受影响)。永久退回阻塞读:链路绝不悬挂,
			// park 的收尾改由退位编排的冻结超时兜底。
			f.mu.Lock()
			f.noPeek = true
			f.mu.Unlock()
		}
		if known && !readable {
			if f.idleCycle() {
				return // Park 已生效:此后的字节留在管道里归继任
			}
			time.Sleep(feederIdlePoll)
			continue
		}
		n, rerr := f.in.Read(buf) // 不持锁读:状态切换只动锁内状态
		f.mu.Lock()
		if n > 0 {
			switch f.state {
			case feederRun, feederPump:
				// 扣住的字节永远先于新字节写出(懒前置):任何并发冲刷
				//(retireTo 的异步冲刷)与本循环的交错都不会乱序。
				data := buf[:n]
				if len(f.held) > 0 {
					f.held = append(f.held, data...)
					data = f.held
				}
				if _, werr := f.dest.Write(data); werr != nil {
					// 目的地已断:SDK 会话收场(SDK 端关闭)或泵已终态。
					// 字节面已无意义,feeder 退役。
					f.dieLocked()
					f.mu.Unlock()
					return
				}
				f.held = nil
				if f.state == feederRun {
					// hold/park 只在帧边界生效(见文件头注释)。
					f.atBoundary = data[len(data)-1] == '\n'
					if f.wantPark && f.atBoundary {
						f.state = feederPark
						f.markFrozenLocked()
						f.mu.Unlock()
						return // 停读:字节此后留在管道里归继任
					}
					if f.wantHold && f.atBoundary {
						f.state = feederHold
						f.markFrozenLocked()
					}
				}
			case feederHold, feederPark:
				f.held = append(f.held, buf[:n]...)
			}
		}
		if rerr != nil {
			// 宿主侧读端关闭:关掉当前目的地,让下游(SDK 或泵)读到 EOF
			// 按各自语义收场(规格 F4)。hold 期间扣住的字节随宿主离去
			// 一并丢弃(退位本就不该发生在宿主已断之后——Hold 会先拒绝)。
			f.dieLocked()
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
	}
}

// idleCycle 处理一次「窥视无数据」(已确定无在飞读):park/hold 请求在
// 帧边界即时生效。返回 true 表示循环应当退出(park 已生效或 feeder 已死)。
// 只在 run 态翻状态——hold/park/pump 一旦定型(如 retireTo 已切泵侧)不再
// 回退,否则停读后的空闲周期会把泵态错翻回扣住态,字节被扣进缓冲再无人
// 冲刷。
func (f *feeder) idleCycle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		f.markFrozenLocked()
		return true
	}
	if f.state != feederRun {
		return false
	}
	if f.wantPark && f.atBoundary {
		f.state = feederPark
		f.markFrozenLocked()
		return true
	}
	if f.wantHold && f.atBoundary {
		f.state = feederHold
		f.markFrozenLocked()
	}
	return false
}

// hold 登记首代冻结请求(在下一个帧边界生效,生效前在途的帧照常送达
// SDK 并被应答)。dead(宿主已 EOF 或 feeder 退役)时返回 false。
func (f *feeder) hold() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead || (f.state != feederRun && f.state != feederHold) {
		return false
	}
	f.wantHold = true
	if f.atBoundary {
		f.state = feederHold
		f.markFrozenLocked()
	}
	return true
}

// releaseHold 回到 run,扣住的字节冲回 SDK 侧管道(退位放弃路径)。
// 未及生效的 hold 请求(等待半行补完中)一并撤销。
func (f *feeder) releaseHold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return
	}
	f.wantHold = false
	if f.state != feederHold {
		return
	}
	f.state = feederRun
	f.flushLocked()
}

// park 登记后续代停读请求(在下一个帧边界生效)。dead 时返回 false。
func (f *feeder) park() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return false
	}
	f.wantPark = true
	return true
}

// endSdkRead 关闭 SDK 读侧管道的写端(SDK 读到 EOF,会话按序收场)。
// 只应在 feeder 冻结(见 WaitFrozen)之后调用——那是「不会再有投递」的
// 确定点,关闭与转发循环的写路径无竞争。
func (f *feeder) endSdkRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dest != nil {
		f.dest.Close()
	}
}

// retireTo 切到泵侧:扣住的字节冲进 hostInW,原 SDK 读侧写端关闭(SDK
// 读到 EOF,会话按序收场——静默判据已过,SDK 不会再有未写出的应答被
// jsonrpc2 的 shuttingDown 路径丢弃)。冲刷是异步的:扣住的字节可能超过
// 泵侧管道缓冲,同步冲会在泵启动前把本调用挂死(管道满→等泵读→泵在
// PumpThen、而 PumpThen 在本调用之后),异步冲刷与后续新字节经懒前置
// 机制保持次序(见 loop 写路径),且泵一旦启动即可排干。
func (f *feeder) retireTo(hostInW *os.File) {
	f.mu.Lock()
	if f.dead {
		// 宿主已 EOF:feeder 在退出时已关掉当时的 SDK 写端;泵侧写端
		// 也关掉,让泵读侧 EOF、继任自灭(规格 F4)。
		f.mu.Unlock()
		hostInW.Close()
		return
	}
	old := f.dest
	f.state = feederPump
	f.dest = hostInW
	old.Close()
	f.mu.Unlock()
	go f.flushPumpSide()
}

// flushPumpSide 异步冲刷扣住的字节(持锁;与 loop 的写路径互斥,懒前置
// 保证两种获得锁的次序都正确)。
func (f *feeder) flushPumpSide() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead || f.state != feederPump || len(f.held) == 0 {
		return
	}
	if _, werr := f.dest.Write(f.held); werr != nil {
		f.dieLocked()
		return
	}
	f.held = nil
}

// stop 请求 feeder 停止(Close 路径):置 dead 并关目的地。feeder 可能
// 仍阻塞在真实标准输入的读上——随进程退出回收,不影响收尾。
func (f *feeder) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return
	}
	f.dieLocked()
}

// flushLocked 把扣住的字节冲进当前目的地(持锁调用)。
func (f *feeder) flushLocked() {
	if len(f.held) == 0 {
		return
	}
	_, _ = f.dest.Write(f.held)
	f.held = nil
}

// dieLocked 标记 feeder 死亡,关当前目的地让下游读到 EOF,并放行冻结
// 等待者(持锁调用)。
func (f *feeder) dieLocked() {
	if f.dead {
		return
	}
	f.dead = true
	f.markFrozenLocked()
	_ = f.dest.Close()
}

func (f *feeder) markFrozenLocked() {
	f.frozenOnce.Do(func() { close(f.frozen) })
}

// ---------------------------------------------------------------------------
// copier:真实标准输出的独占写者。phase 1 搬 SDK 应答;退位后 phase 2
// 搬泵侧(继任)输出。顺序性由单 goroutine 顺序搬运保证。

type copier struct {
	sdkOutR  *os.File
	realOut  *os.File
	phase1   chan struct{} // 排干(或写失败)信号,由 BridgeIO 持有并关闭
	quit     <-chan struct{}
	switchCh chan *os.File // 退位切换:泵侧数据源
}

func newCopier(sdkOutR, realOut *os.File, phase1 chan struct{}, quit chan struct{}) *copier {
	c := &copier{
		sdkOutR:  sdkOutR,
		realOut:  realOut,
		phase1:   phase1,
		quit:     quit,
		switchCh: make(chan *os.File, 1),
	}
	go c.loop()
	return c
}

func (c *copier) loop() {
	// phase 1:SDK 应答 → 真实标准输出。
	if !c.pump1() {
		close(c.phase1) // 写失败也视为 phase 1 结束(字节面已坏,如实暴露)
		return
	}
	close(c.phase1)
	// 等退位切换;进程未退位就收场(Close)则直接退出。已交割的切换
	// (switchCh 有货)优先于 quit:Close 与 Retire 并发时先完成转发。
	select {
	case src := <-c.switchCh:
		c.pump2(src)
	default:
		select {
		case src := <-c.switchCh:
			c.pump2(src)
		case <-c.quit:
		}
	}
}

// pump1 搬到 EOF;返回 false 表示写真实标准输出失败(宿主侧已断)。
func (c *copier) pump1() bool {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := c.sdkOutR.Read(buf)
		if n > 0 {
			if _, werr := c.realOut.Write(buf[:n]); werr != nil {
				return false
			}
		}
		if rerr != nil {
			return true
		}
	}
}

func (c *copier) pump2(src *os.File) {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := c.realOut.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// switchTo 交割泵侧数据源(Retire 调用)。
func (c *copier) switchTo(src *os.File) {
	c.switchCh <- src
}
