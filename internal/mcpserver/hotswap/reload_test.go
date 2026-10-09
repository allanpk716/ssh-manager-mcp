package hotswap_test

// reload_test.go — ReloadService 与两段式拉起的外部行为测试。换手全链的
// 跨进程端到端(mcpserver 两形态入口)在 internal/mcpserver 的
// reload_e2e_test.go;这里测库内可闭环的部分。

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/updater"
)

func TestSessionFromInitializeParamsRoundTrip(t *testing.T) {
	params := &mcp.InitializeParams{
		ProtocolVersion: "2025-06-18",
		Capabilities:    &mcp.ClientCapabilities{},
		ClientInfo:      &mcp.Implementation{Name: "fake-host", Version: "9"},
	}
	params.Capabilities.Roots.ListChanged = true

	sess, err := hotswap.SessionFromInitializeParams(params)
	if err != nil {
		t.Fatal(err)
	}
	st, err := hotswap.AdoptedSessionState(sess)
	if err != nil {
		t.Fatal(err)
	}
	got := st.InitializeParams
	if got.ProtocolVersion != params.ProtocolVersion {
		t.Fatalf("protocol version = %q want %q", got.ProtocolVersion, params.ProtocolVersion)
	}
	if got.ClientInfo == nil || got.ClientInfo.Name != "fake-host" || got.ClientInfo.Version != "9" {
		t.Fatalf("client info lost: %+v", got.ClientInfo)
	}
	if got.Capabilities == nil || !got.Capabilities.Roots.ListChanged {
		t.Fatalf("capabilities lost: %+v", got.Capabilities)
	}
	if st.InitializedParams == nil {
		t.Fatal("adopted state must mark the session initialized")
	}

	// 空会话(缺协议版本)必须拒绝,而不是起一个假握手状态。
	if _, err := hotswap.AdoptedSessionState(hotswap.Session{}); err == nil {
		t.Fatal("adoption without a protocol version must fail")
	}
	if _, err := hotswap.SessionFromInitializeParams(nil); err == nil {
		t.Fatal("nil params must fail")
	}
}

// TestStartSuccessorRollback:两段式拉起一个健康继任(echo 角色)后放弃
// 退位——Rollback 杀继任且不挂;错误路径(就绪超时)带身份。
func TestStartSuccessorRollback(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("rollback healthy successor", func(t *testing.T) {
		h, err := hotswap.StartSuccessor(hotswap.Options{
			Exe:          exe,
			Session:      hotswap.Session{"mcp_protocol": "2025-06-18"},
			Generation:   hotswap.GenerationFirst,
			Env:          []string{testRoleEnv + "=echo"},
			ReadyTimeout: 5 * time.Second,
			PollInterval: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != "v-test-echo" {
			t.Fatalf("successor version = %q want v-test-echo", h.Version)
		}
		withDeadline(t, "Rollback", 5*time.Second, func() error { return h.Rollback() })
	})

	t.Run("ready timeout fails closed", func(t *testing.T) {
		_, err := hotswap.StartSuccessor(hotswap.Options{
			Exe:          exe,
			Session:      hotswap.Session{"mcp_protocol": "2025-06-18"},
			Generation:   hotswap.GenerationFirst,
			Env:          []string{testRoleEnv + "=stuck"},
			ReadyTimeout: 300 * time.Millisecond,
			PollInterval: 20 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("stuck successor must time out")
		}
	})
}

// TestReloadServiceReport:盘上代际读取(含缺失=0)与 latest 注入的成功/
// 失败两路。
func TestReloadServiceReport(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sigPath := updater.SignalPath(exe)
	t.Cleanup(func() { os.Remove(sigPath) })

	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		Exe: exe,
		Latest: func(ctx context.Context) (string, error) {
			return "v9.9.9", nil
		},
	})

	st := rs.Report(context.Background())
	if st.DiskGeneration != 0 || st.DiskVersion != "" {
		t.Fatalf("no signal on disk yet: %+v", st)
	}
	if st.Latest != "v9.9.9" || st.LatestError != "" {
		t.Fatalf("latest: %+v", st)
	}

	if _, err := updater.WriteGenerationSignal(exe, "v-report-test"); err != nil {
		t.Fatal(err)
	}
	st = rs.Report(context.Background())
	if st.DiskVersion != "v-report-test" || st.DiskGeneration <= 0 {
		t.Fatalf("signal must show up: %+v", st)
	}

	failing := hotswap.NewReloadService(hotswap.ReloadConfig{
		Exe: exe,
		Latest: func(ctx context.Context) (string, error) {
			return "", context.DeadlineExceeded
		},
	})
	st = failing.Report(context.Background())
	if st.Latest != "" || st.LatestError == "" {
		t.Fatalf("latest failure must be reported verbatim: %+v", st)
	}
	if st.DiskVersion != "v-report-test" {
		t.Fatalf("other fields must survive a latest failure: %+v", st)
	}
}

// TestReloadServiceArmAndDance:库内闭环跑通整条退位编排——真 MCP 会话
// (内存传输握手)供 Arm 快照参数,echo 继任就绪后,Hold→配平→交割→
// 泵化,宿主字节直通继任;宿主断开后全链收场,WaitTerminal 返回。
func TestReloadServiceArmAndDance(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// 真握手会话(服务器先连,客户端握手)。
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
	defer cliSess.Close()

	// 字节面接管(测试管道扮演真实标准输入输出)。
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
	defer bio.Close()

	sessionEnded := make(chan struct{})
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:                bio,
		Srv:               srv,
		Exe:               exe,
		Env:               []string{testRoleEnv + "=echo"},
		SessionEnded:      sessionEnded,
		SessionEndTimeout: 300 * time.Millisecond,
		QuiesceTimeout:    5 * time.Second,
		ReadyTimeout:      5 * time.Second,
		PollInterval:      20 * time.Millisecond,
	})

	// 先连接 SDK 侧传输:退位交割(Feeder 关 SDK 读侧写端)会在此表现为
	// EOF——它就是本测试的「交割已发生」同步点。
	sdkConn, err := bio.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	arm := rs.Arm()
	if arm.State != hotswap.ArmArmed {
		t.Fatalf("Arm = %+v", arm)
	}
	if arm.SuccessorVersion != "v-test-echo" {
		t.Fatalf("successor version = %q", arm.SuccessorVersion)
	}
	again := rs.Arm()
	if again.State != hotswap.ArmAlreadyArmed {
		t.Fatalf("second Arm must be single-flight, got %+v", again)
	}
	withDeadline(t, "wait for retire handoff (SDK-side EOF)", 5*time.Second, func() error {
		_, err := sdkConn.Read(context.Background())
		if err == nil {
			return errEOFExpected
		}
		return nil
	})

	// 宿主字节(退位后)应直通继任并被回声。
	if _, err := inW.WriteString("ping-through-pump\n"); err != nil {
		t.Fatal(err)
	}
	got := readLineWithTimeout(t, "echo through pump", outR, 5*time.Second)
	if got != "ping-through-pump\n" {
		t.Fatalf("pump round-trip = %q", got)
	}

	// 宿主断开:feeder EOF → 泵收干 → 继任退出 → 编排终态。
	inW.Close()
	withDeadline(t, "WaitTerminal", 10*time.Second, func() error {
		rs.WaitTerminal()
		return nil
	})
}

// errEOFExpected 标记「读到了 EOF 才对」的哨兵(非错误路径)。
var errEOFExpected = errors.New("expected EOF after retire")

// TestReloadServiceArmAdoptedRefused:显式标记为不可换手的桥(Adopted,
// 如取不到自身可执行路径)Arm 一律拒绝。
func TestReloadServiceArmAdoptedRefused(t *testing.T) {
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{Adopted: true})
	arm := rs.Arm()
	if arm.State != hotswap.ArmFailed || arm.Err == nil {
		t.Fatalf("adopted bridge Arm must fail: %+v", arm)
	}
	rs.WaitTerminal() // 未启动编排:立即返回
}

// 后续代(GenerationLater)换手的库内闭环说明:继任直接继承 os.Stdin/
// os.Stdout 句柄,而本进程自建 os.Pipe 读端被继任并发读时,Windows 上
// PeekNamedPipe 会不返回(见 feeder.loop 的 noPeek 兜底)——库内管道拓扑
// 无法真实还原生产形态(继承的标准输入句柄)。该路径的验收由进程边界
// 真实的端到端覆盖:internal/mcpserver/reload_later_e2e_test.go 的三代链
// (TestGenerationChainE2EDirect)。

// TestReloadServiceQuiesceTimeoutRollback:静默判据到不了(一笔已送达 SDK
// 的请求永不应答)→ QuiesceTimeout 到期 → ReleaseHold + Rollback:旧桥
// 字节面解冻,扣住的请求回到 SDK 侧(旧桥继续应答),编排终态返回。
func TestReloadServiceQuiesceTimeoutRollback(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
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
	defer cliSess.Close()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	})
	bio, err := hotswap.NewBridgeIO(inR, outW, true)
	if err != nil {
		t.Fatal(err)
	}
	defer bio.Close()

	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:             bio,
		Srv:            srv,
		Exe:            exe,
		Env:            []string{testRoleEnv + "=echo"},
		QuiesceTimeout: 400 * time.Millisecond,
		ReadyTimeout:   5 * time.Second,
		PollInterval:   20 * time.Millisecond,
	})

	sdkConn, err := bio.Transport().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 一笔永不应答的请求:送达 SDK(conn.Read 读到,进账+1)但不写应答
	// ——静默判据永远到不了。
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":77,"method":"tools/call"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	m, err := sdkConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if req, ok := m.(*jsonrpc.Request); !ok || req.Method != "tools/call" {
		t.Fatalf("unexpected first message: %#v", m)
	}

	arm := rs.Arm()
	if arm.State != hotswap.ArmArmed {
		t.Fatalf("Arm = %+v", arm)
	}
	// 等 Hold 生效(空闲+边界,毫秒级)后写第二笔:应被扣住不进 SDK。
	// 探测读可取消(jsonrpc2 连接单读者,探测 goroutine 不得滞留)。
	time.Sleep(150 * time.Millisecond)
	if _, err := inW.WriteString(`{"jsonrpc":"2.0","id":78,"method":"tools/list"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancelProbe := context.WithCancel(ctx)
	held := make(chan error, 1)
	go func() { _, err := sdkConn.Read(probeCtx); held <- err }()
	select {
	case err := <-held:
		t.Fatalf("held request leaked into the SDK side during quiesce wait: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	cancelProbe()
	<-held

	// 编排终态:静默超时 → ReleaseHold + Rollback(继任被杀),旧桥继续。
	withDeadline(t, "WaitTerminal after quiesce timeout", 10*time.Second, func() error {
		rs.WaitTerminal()
		return nil
	})

	// 回退后字节面解冻:扣住的 id=78 回到 SDK 侧(旧桥照常应答的证明)。
	m2, err := sdkConn.Read(ctx)
	if err != nil {
		t.Fatalf("released request must reach the SDK side after rollback: %v", err)
	}
	if req2, ok := m2.(*jsonrpc.Request); !ok || req2.Method != "tools/list" {
		t.Fatalf("unexpected released message: %#v", m2)
	}
}
