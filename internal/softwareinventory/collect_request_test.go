package softwareinventory

import (
	"context"
	"errors"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestCollectForScopeVisibilityAndDelegation 立即采集入口先核对可见设备范
// 围，再把采集交给注入的服务执行。
func TestCollectForScopeVisibilityAndDelegation(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "可见设备", true)
	mustDevice(t, st, "授权外设备", true)
	server, err := st.GetServerByName("可见设备-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	profileID, err := st.AddProfile("采集可见范围")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}

	_, err = manager.CollectForScope(context.Background(), "授权外设备",
		DevicesVisibleThroughServers(server.ID), &Service{Store: st}, AgentSource())
	if !errors.Is(err, ErrDeviceNotVisible) {
		t.Fatalf("越界设备应被拒：%v", err)
	}

	called := false
	service := &Service{Store: st, RemoteExec: func(
		_ context.Context, _ *models.Server, _ models.ShellType, _ string, _ bool,
	) (RemoteExecResult, error) {
		called = true
		return RemoteExecResult{Stdout: fixtureOutput(t, "ubuntu2404-full.txt")}, nil
	}}
	outcome, err := manager.CollectForScope(context.Background(), "可见设备",
		DevicesVisibleThroughServers(server.ID), service, AgentSource())
	if err != nil {
		t.Fatal(err)
	}
	if !called || outcome.Report == nil || outcome.Report.DeviceName != "可见设备" ||
		outcome.Report.CommandSetVersion != "posix-v2" || outcome.Requested {
		t.Fatalf("可见设备应经注入的采集服务执行：%+v", outcome)
	}
	// 采集落库的审计来源跟随调用方：agent 工具触发时不是命令行来源。
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceAgent {
		t.Fatalf("立即采集审计应记录 agent 来源：%+v", rows)
	}
}

// TestCollectForScopeRegistersClientCollector 采集执行者是客户端实例时，
// 立即采集只登记请求，等待该实例下一轮分派执行。
func TestCollectForScopeRegistersClientCollector(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "客户端采集设备", true)
	server, err := st.GetServerByName("客户端采集设备-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	profileID, err := st.AddProfile("客户端采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	_, _, err = st.AddCacheToken("mini6", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(
		"客户端采集设备", "mini6", "", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	executed := false
	service := &Service{Store: st, RemoteExec: func(
		_ context.Context, _ *models.Server, _ models.ShellType, _ string, _ bool,
	) (RemoteExecResult, error) {
		executed = true
		return RemoteExecResult{Stdout: fixtureOutput(t, "ubuntu2404-full.txt")}, nil
	}}
	outcome, err := manager.CollectForScope(context.Background(), "客户端采集设备",
		DevicesVisibleThroughServers(server.ID), service, AgentSource())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Requested || outcome.Collector != "mini6" || outcome.Report != nil || executed {
		t.Fatalf("客户端实例采集的设备只应登记请求：%+v", outcome)
	}
	status, err := st.SoftwareCollectionStatusForDevice("客户端采集设备")
	if err != nil || !status.PendingImmediate {
		t.Fatalf("立即采集请求应已登记：%+v %v", status, err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect-request"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceAgent {
		t.Fatalf("立即采集请求应记录来源：%+v", rows)
	}
}
