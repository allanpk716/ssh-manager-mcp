package mcpserver

// reloadself_test.go — reload_self 处理器主体的单元测试(忙拒绝+活跃清单、
// 无新代际、被领养桥、GitHub 查询失败如实带错误、换手失败带原因)。跨进程
// 端到端见 reload_e2e_test.go。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/updater"
)

// newReloadTestServer 建一个有真握手的内存服务(Arm 需要从会话快照参数)。
func newReloadTestServer(t *testing.T) *mcp.Server {
	t.Helper()
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
	return srv
}

func newReloadTestIO(t *testing.T) *hotswap.BridgeIO {
	t.Helper()
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
	t.Cleanup(func() {
		inW.Close()
		outR.Close()
		bio.Close()
	})
	return bio
}

// withSignal 在测试二进制旁写一个新代际信号(出生代际 0),返回清理函数。
func withSignal(t *testing.T, version string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updater.WriteGenerationSignal(exe, version); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
}

func TestReloadSelfHandlerNoNewGeneration(t *testing.T) {
	t.Setenv("SSHMGR_TEST_VERSION", "v-unit")
	srv := newReloadTestServer(t)
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:  newReloadTestIO(t),
		Srv: srv,
		Exe: testBinaryPath(t),
		Latest: func(ctx context.Context) (string, error) {
			return "v9.9.9", nil
		},
	})
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })

	out := reloadSelf(context.Background(), busy, rs)
	if out.Handover != "no_new_generation" {
		t.Fatalf("handover = %q want no_new_generation", out.Handover)
	}
	if out.Version != "v-unit" {
		t.Fatalf("version = %q", out.Version)
	}
	if out.Latest != "v9.9.9" || out.LatestError != "" {
		t.Fatalf("latest: %+v", out)
	}
	if out.DiskGeneration != 0 || out.DiskVersion != "" {
		t.Fatalf("no signal on disk: %+v", out)
	}
}

func TestReloadSelfHandlerBusyDeclines(t *testing.T) {
	t.Setenv("SSHMGR_TEST_VERSION", "v-unit")
	withSignal(t, "v-busy-test")
	srv := newReloadTestServer(t)
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:  newReloadTestIO(t),
		Srv: srv,
		Exe: testBinaryPath(t),
	})
	// 伪造活跃隧道 2 条:忙 → 拒绝 + 活跃清单,不换手(Arm 未被调用——
	// 若被调用,会在 ReadyTimeout 内挂住本测试,超时即证伪)。
	busy := NewBusyTracker(func() int { return 2 }, func() int { return 0 })

	out := reloadSelf(context.Background(), busy, rs)
	if out.Handover != "declined_busy" {
		t.Fatalf("handover = %q want declined_busy", out.Handover)
	}
	if out.Busy == nil || !out.Busy.Busy || out.Busy.ActiveTunnels != 2 || out.Busy.RunningTasks != 0 {
		t.Fatalf("active list missing or wrong: %+v", out.Busy)
	}
	if out.DiskGeneration <= 0 || out.DiskVersion != "v-busy-test" {
		t.Fatalf("versions must still be reported when busy: %+v", out)
	}
}

func TestReloadSelfHandlerAdoptedDeclines(t *testing.T) {
	withSignal(t, "v-adopted-test")
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:      newReloadTestIO(t),
		Srv:     newReloadTestServer(t),
		Exe:     testBinaryPath(t),
		Adopted: true,
	})
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })
	out := reloadSelf(context.Background(), busy, rs)
	if out.Handover != "not_first_generation" {
		t.Fatalf("handover = %q want not_first_generation", out.Handover)
	}
}

// TestReloadSelfHandlerLatestFailureCoversUpdater:经 run.go 的真实
// latestReleaseTag + SSHMGR_UPDATE_BASE 测试缝驱动 updater 全链——
// 查询失败如实带错误,其余字段正常返回。
func TestReloadSelfHandlerLatestFailureCoversUpdater(t *testing.T) {
	t.Setenv("SSHMGR_TEST_VERSION", "v-unit")
	// 指向一个必然拒绝连接的环回地址:失败来得快,不联网。
	t.Setenv("SSHMGR_UPDATE_BASE", "http://127.0.0.1:1")
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:     newReloadTestIO(t),
		Srv:    newReloadTestServer(t),
		Exe:    testBinaryPath(t),
		Latest: latestReleaseTag,
	})
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })
	out := reloadSelf(context.Background(), busy, rs)
	if out.Latest != "" || out.LatestError == "" {
		t.Fatalf("latest failure must be reported verbatim: %+v", out)
	}
	if out.Version != "v-unit" || out.DiskGeneration != 0 {
		t.Fatalf("other fields must survive a latest failure: %+v", out)
	}
}

// TestReloadSelfHandlerLatestSuccessCoversUpdater:httptest 假 GitHub,
// 返回带本平台资产的合法 release 文档,latest 解析出 tag。
func TestReloadSelfHandlerLatestSuccessCoversUpdater(t *testing.T) {
	asset, err := updater.AssetName("v9.9.9", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"tag_name": "v9.9.9",
			"assets": []map[string]any{
				{"name": asset, "browser_download_url": "http://127.0.0.1:1/" + asset},
			},
		}
		json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()
	t.Setenv("SSHMGR_UPDATE_BASE", srv.URL)

	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:     newReloadTestIO(t),
		Srv:    newReloadTestServer(t),
		Exe:    testBinaryPath(t),
		Latest: latestReleaseTag,
	})
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })
	out := reloadSelf(context.Background(), busy, rs)
	if out.Latest != "v9.9.9" || out.LatestError != "" {
		t.Fatalf("latest = %+v", out)
	}
}

// TestReloadSelfHandlerHandoverFailed:继任永不就绪 → 应答带失败与原因,
// 且 armed 复位(再次调用可再试)。
func TestReloadSelfHandlerHandoverFailed(t *testing.T) {
	t.Setenv("SSHMGR_TEST_VERSION", "v-unit")
	withSignal(t, "v-fail-test")
	srv := newReloadTestServer(t)
	rs := hotswap.NewReloadService(hotswap.ReloadConfig{
		IO:           newReloadTestIO(t),
		Srv:          srv,
		Exe:          testBinaryPath(t),
		Env:          []string{reloadRoleEnv + "=" + roleNeverReady},
		ReadyTimeout: 700 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })

	out := reloadSelf(context.Background(), busy, rs)
	if out.Handover != "failed" {
		t.Fatalf("handover = %q want failed", out.Handover)
	}
	if !strings.Contains(out.Error, "not ready in time") {
		t.Fatalf("failure reason must carry the ready-timeout identity: %q", out.Error)
	}
	if out.DiskVersion != "v-fail-test" {
		t.Fatalf("versions must still be reported: %+v", out)
	}
	// armed 已复位:再触发仍是 failed(而非 already_armed),且不挂。
	out2 := reloadSelf(context.Background(), busy, rs)
	if out2.Handover != "failed" {
		t.Fatalf("second call handover = %q want failed (armed must reset)", out2.Handover)
	}
}

// TestReloadSelfRegisteredOnBridgeFaces:两形态桥面(run.go serveBridge 的
// 注册形态)在 BrokerTools 上的占位与豁免注册的 grep 不变量——注册函数
// 幂等(继任接管后的再注册),重复注册不产生重复工具。
func TestReloadSelfRegisteredOnBridgeFaces(t *testing.T) {
	if BrokerTools[13] != hotswap.ToolReloadSelf {
		t.Fatalf("BrokerTools[13] = %q want %q", BrokerTools[13], hotswap.ToolReloadSelf)
	}
	if !bridgeOnlyTools[BrokerTools[13]] {
		t.Fatal("reload_self must be bridge-only")
	}
	if authorityTools[len(authorityTools)-1] == hotswap.ToolReloadSelf {
		t.Fatal("reload_self must not leak into authorityTools (NewServer's face)")
	}
}
