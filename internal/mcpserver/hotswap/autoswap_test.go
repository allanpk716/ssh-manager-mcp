package hotswap_test

// autoswap_test.go — 自动换手循环(桥热升级 spec 实施决策第 1/2/5 条)的
// 库内闭环测试:定期比对盘上代际与出生代际,新且不忙时经与 reload_self 工具
// 同一个 Arm 入口自动完成换手;忙时静默跳过本轮(不换手、无输出),空闲后的
// 下一轮完成;无新代际不动;无力拉继任的桥(Adopted)不轮询;默认轮询间隔
// 不是测试用短周期。观察点与 reload_test.go 的 TestReloadServiceArmAndDance
// 相同:退位交割在 SDK 侧连接上表现为 EOF,泵化后宿主字节直通继任(echo 角色)。

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/updater"
)

// autoSwapDance 是自动换手循环的库内测试台:真 MCP 会话(内存传输握手,Arm
// 需要从中快照参数)、字节面接管管道、SDK 侧连接(退位交割=EOF 的观测点)
// 与按 tweak 定制的 ReloadService。echo 继任角色保证 Arm 一旦发生就能成功
// ——因此「窗口内无 EOF」即「本轮没有发起换手」的强证明。
type autoSwapDance struct {
	rs      *hotswap.ReloadService
	inW     *os.File // 宿主侧写端:退位后喂泵
	outR    *os.File // 宿主侧读端:收回声
	sdkConn mcp.Connection
	cancel  context.CancelFunc
}

// newAutoSwapDance 装配测试台并写好一个较出生代际新的盘上信号。tweak 在
// NewReloadService 之前定制配置(注入 Busy/间隔/出生代际等)。
func newAutoSwapDance(t *testing.T, tweak func(cfg *hotswap.ReloadConfig)) *autoSwapDance {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// 出生代际:先清掉可能残留的信号(包内其他测试会写同一文件),快照出生
	// 代际=0,再落一个新信号(必然更新)。
	sigPath := updater.SignalPath(exe)
	os.Remove(sigPath)
	t.Cleanup(func() { os.Remove(sigPath) })
	born := updater.BirthGeneration(exe) // 0(刚清掉)
	sig, err := updater.WriteGenerationSignal(exe, "v-autoswap-disk")
	if err != nil {
		t.Fatal(err)
	}
	if sig.Gen <= born {
		t.Fatalf("fixture: new signal gen %d must exceed birth %d", sig.Gen, born)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), t1, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cliSess.Close() })

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	bio, err := hotswap.NewBridgeIO(inR, outW, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bio.Close() })

	cfg := hotswap.ReloadConfig{
		IO:                 bio,
		Srv:                srv,
		Exe:                exe,
		Env:                []string{testRoleEnv + "=echo"},
		BirthGeneration:    born,
		QuiesceTimeout:     5 * time.Second,
		ReadyTimeout:       5 * time.Second,
		PollInterval:       20 * time.Millisecond,
		SessionEndTimeout:  300 * time.Millisecond,
		CopierDrainTimeout: 2 * time.Second,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	rs := hotswap.NewReloadService(cfg)

	sdkConn, err := bio.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &autoSwapDance{rs: rs, inW: inW, outR: outR, sdkConn: sdkConn}
}

// start 启动循环(记录 cancel 供收尾)。
func (d *autoSwapDance) start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.rs.StartAutoSwap(ctx)
}

// awaitHandoff 等待退位交割(SDK 侧 EOF)——即「换手已发生并推进到交割」。
func (d *autoSwapDance) awaitHandoff(t *testing.T, what string, limit time.Duration) {
	t.Helper()
	withDeadline(t, what+" (SDK-side EOF)", limit, func() error {
		_, err := d.sdkConn.Read(context.Background())
		if err == nil {
			return errEOFExpected
		}
		return nil
	})
}

// assertNoHandoffWithin 断言窗口内没有任何换手推进到交割(echo 继任必就绪,
// 故一次被发起的换手会很快表现为 EOF)。探测读可取消(jsonrpc2 连接单读者,
// 探测 goroutine 不得滞留——TestReloadServiceQuiesceTimeoutRollback 同款)。
func (d *autoSwapDance) assertNoHandoffWithin(t *testing.T, window time.Duration) {
	t.Helper()
	probeCtx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, err := d.sdkConn.Read(probeCtx); got <- err }()
	select {
	case err := <-got:
		t.Fatalf("SDK side observed a handover within the window (read returned %v) where none was expected", err)
	case <-time.After(window):
	}
	cancel()
	<-got // 收走已取消的探测读
}

// assertPumpEcho 验证泵化后宿主字节直通继任(echo 角色)。
func (d *autoSwapDance) assertPumpEcho(t *testing.T, payload string) {
	t.Helper()
	if _, err := d.inW.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if got := readLineWithTimeout(t, "echo through pump", d.outR, 5*time.Second); got != payload {
		t.Fatalf("pump round-trip = %q want %q", got, payload)
	}
}

// finish 收场:宿主断开 → 等编排终态 → 停循环。
func (d *autoSwapDance) finish(t *testing.T) {
	t.Helper()
	d.inW.Close()
	withDeadline(t, "WaitTerminal", 10*time.Second, func() error {
		d.rs.WaitTerminal()
		return nil
	})
	if d.cancel != nil {
		d.cancel()
	}
}

// TestAutoSwapLoopSwapsWhenIdle:空闲桥 + 盘上较出生代际新的信号 → 一个
// 轮询周期内自动完成换手(无任何工具调用),泵化后字节直通继任,宿主断开
// 后全链收场。
func TestAutoSwapLoopSwapsWhenIdle(t *testing.T) {
	d := newAutoSwapDance(t, func(cfg *hotswap.ReloadConfig) {
		cfg.AutoSwapInterval = 80 * time.Millisecond
	})
	d.start()
	d.awaitHandoff(t, "auto handover", 5*time.Second)
	d.assertPumpEcho(t, "ping-auto-idle\n")
	d.finish(t)
}

// TestAutoSwapLoopBusySkipsUntilIdle:忙 → 各轮静默跳过(窗口内无交割=未
// 发起换手);转闲后下一轮完成换手并泵化。
func TestAutoSwapLoopBusySkipsUntilIdle(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	d := newAutoSwapDance(t, func(cfg *hotswap.ReloadConfig) {
		cfg.AutoSwapInterval = 80 * time.Millisecond
		cfg.Busy = func() bool { return busy.Load() }
	})
	d.start()
	// 5 个周期以上仍无交割:忙时逐轮静默跳过。
	d.assertNoHandoffWithin(t, 500*time.Millisecond)
	busy.Store(false)
	d.awaitHandoff(t, "auto handover after idle", 5*time.Second)
	d.assertPumpEcho(t, "ping-auto-busy-then-idle\n")
	d.finish(t)
}

// TestAutoSwapLoopNoNewGeneration:盘上代际不较出生代际新(相等=同一代)→
// 不轮询换手。
func TestAutoSwapLoopNoNewGeneration(t *testing.T) {
	d := newAutoSwapDance(t, func(cfg *hotswap.ReloadConfig) {
		// 出生代际=当前盘上代际:无新代际。
		cfg.BirthGeneration = updater.BirthGeneration(cfg.Exe)
		cfg.AutoSwapInterval = 60 * time.Millisecond
		cfg.Busy = func() bool { return false }
	})
	d.start()
	d.assertNoHandoffWithin(t, 400*time.Millisecond)
	if d.cancel != nil {
		d.cancel()
	}
}

// TestAutoSwapLoopAdoptedBridgeDoesNotPoll:无力拉继任的桥(Adopted,如取
// 不到自身可执行路径)不启动轮询——盘上有新代际也不动。
func TestAutoSwapLoopAdoptedBridgeDoesNotPoll(t *testing.T) {
	d := newAutoSwapDance(t, func(cfg *hotswap.ReloadConfig) {
		cfg.Adopted = true
		cfg.AutoSwapInterval = 60 * time.Millisecond
	})
	d.start()
	d.assertNoHandoffWithin(t, 400*time.Millisecond)
	if d.cancel != nil {
		d.cancel()
	}
}

// TestAutoSwapLoopDefaultIntervalIsSlow:未注入间隔时用生产默认(约 30 秒,
// DefaultAutoSwapInterval)——窗口内(远小于默认值,远大于测试短周期)不得
// 发生换手。echo 继任就绪只需约 50ms,若默认被误配成短周期,交割早已发生。
func TestAutoSwapLoopDefaultIntervalIsSlow(t *testing.T) {
	d := newAutoSwapDance(t, func(cfg *hotswap.ReloadConfig) {
		cfg.AutoSwapInterval = 0 // 零值:NewReloadService 填默认
	})
	d.start()
	d.assertNoHandoffWithin(t, 1200*time.Millisecond)
	if d.cancel != nil {
		d.cancel()
	}
}
