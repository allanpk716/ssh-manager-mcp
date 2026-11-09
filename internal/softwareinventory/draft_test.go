package softwareinventory

import (
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestDeclarationDraftsHiddenFromQueriesAndTodos 钉住草稿的隔离性：提交后
// 不影响统一查询与三类待办；确认后才进入结果，认领的未识别进程也从待办
// 里消失。
func TestDeclarationDraftsHiddenFromQueriesAndTodos(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			StableKey: "unidentified-1", Category: CategoryUnidentified,
			Name: "unknown-service", Version: "",
			ExecutablePath: "/opt/gateway/bin/gateway", ProcessName: "gateway",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
				Exposure: ExposureAllAddresses,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	ids, err := manager.SubmitDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "工作站", Category: CategoryServiceProgram,
			Name: "业务网关", Version: "2.1",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
				Exposure: ExposureVirtualLocalAreaNetwork,
			}},
		},
		ClaimedObservedKey: "unidentified-1",
	}}, AllDevicesVisible(), AgentSource())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("应返回一个草稿编号：%v", ids)
	}

	query, err := manager.Query(QueryOptions{Name: "业务网关", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(query.Devices) != 0 {
		t.Fatalf("草稿确认前不应出现在统一查询里：%+v", query.Devices)
	}
	todos, err := manager.Todos(AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(todos.UnidentifiedProcesses) != 1 || len(todos.PendingExposure) != 1 {
		t.Fatalf("草稿确认前待办应保持不变：%+v", todos)
	}

	drafts, err := manager.DeclarationDrafts(AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].ID != ids[0] ||
		drafts[0].ClaimedObservedKey != "unidentified-1" {
		t.Fatalf("领域模块应列出草稿：%+v", drafts)
	}

	if err := manager.ConfirmDeclarationDraft(ids[0], nil, CLISource()); err != nil {
		t.Fatal(err)
	}
	query, err = manager.Query(QueryOptions{Name: "业务网关", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(query.Devices) != 1 || len(query.Devices[0].Items) != 1 ||
		query.Devices[0].Items[0].Source != SourceCombined {
		t.Fatalf("确认后认领草稿应合成为一条软件条目：%+v", query.Devices)
	}
	todos, err = manager.Todos(AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(todos.UnidentifiedProcesses) != 0 || len(todos.PendingExposure) != 0 {
		t.Fatalf("确认后未识别进程与待确认暴露应从待办消失：%+v", todos)
	}
}

// TestDeclarationDraftVisibilityAndReject 覆盖领域模块对可见范围的核对与
// 驳回语义。
func TestDeclarationDraftVisibilityAndReject(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "可见设备", true)
	mustDevice(t, st, "授权外设备", true)
	server, err := st.GetServerByName("可见设备-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	profileID, err := st.AddProfile("草稿可见范围")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	visibility := DevicesVisibleThroughServers(server.ID)

	if _, err := manager.SubmitDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "授权外设备", Category: CategoryServiceProgram,
			Name: "redis", Version: "7.2",
		},
	}}, visibility, AgentSource()); err != ErrDeviceNotVisible {
		t.Fatalf("越界设备应被拒：%v", err)
	}

	ids, err := manager.SubmitDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "可见设备", Category: CategoryServiceProgram,
			Name: "nginx", Version: "1.25.3",
		},
	}}, visibility, ForwardedSource("mini6"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RejectDeclarationDraft(ids[0], CLISource()); err != nil {
		t.Fatal(err)
	}
	drafts, err := manager.DeclarationDrafts(visibility)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("驳回后的草稿应消失：%+v", drafts)
	}
}
