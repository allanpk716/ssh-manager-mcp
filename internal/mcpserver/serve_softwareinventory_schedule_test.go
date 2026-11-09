package mcpserver

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// swapSoftwareCollectionScheduler 替换 serve 里软件清单采集调度服务的启动
// 缝，返回恢复函数。
func swapSoftwareCollectionScheduler(
	fake func(ctx context.Context, st *store.Store) func(),
) func() {
	previous := startSoftwareCollectionScheduler
	startSoftwareCollectionScheduler = fake
	return func() { startSoftwareCollectionScheduler = previous }
}

// TestRunServeStartsAndStopsSoftwareCollectionScheduler 验证权威端 serve 启
// 动时启动软件清单采集调度服务，退出时干净收尾。
func TestRunServeStartsAndStopsSoftwareCollectionScheduler(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleServer)
	if err := st.SetSetting(settingWebUIPort, strconv.Itoa(reserveLocalPort(t))); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)
	restore := swapSoftwareCollectionScheduler(func(ctx context.Context, got *store.Store) func() {
		if got != st {
			t.Errorf("调度服务应绑定 serve 的存储")
		}
		started <- struct{}{}
		go func() { <-ctx.Done() }()
		return func() { stopped <- struct{}{} }
	})
	t.Cleanup(restore)

	mainPort := reserveLocalPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunServe(ctx, st, net.JoinHostPort("127.0.0.1", strconv.Itoa(mainPort)), "", "", ServeOpts{})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("serve 启动时没有启动软件清单采集调度服务")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe 退出应返回空错误，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunServe 取消后没有及时退出")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("serve 收尾时没有停止软件清单采集调度服务")
	}
}

// TestRunServeSkipsSoftwareCollectionSchedulerOnStandalone 验证单机角色不启
// 动采集调度服务：单机由 owner 用系统计划任务调用子命令。
func TestRunServeSkipsSoftwareCollectionSchedulerOnStandalone(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleStandalone)
	started := make(chan struct{}, 1)
	restore := swapSoftwareCollectionScheduler(func(context.Context, *store.Store) func() {
		started <- struct{}{}
		return func() {}
	})
	t.Cleanup(restore)

	mainPort := reserveLocalPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	mainAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(mainPort))
	go func() { done <- RunServe(ctx, st, mainAddress, "", "", ServeOpts{}) }()
	waitTCPListener(t, mainAddress, done)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe 退出应返回空错误，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunServe 取消后没有及时退出")
	}
	select {
	case <-started:
		t.Fatal("单机角色的 serve 不应启动软件清单采集调度服务")
	default:
	}
}
