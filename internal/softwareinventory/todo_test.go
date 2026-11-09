package softwareinventory

import (
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// TestTodosListsThreeCategories 覆盖三类待办的领域规则：待确认暴露按设备分
// 组，未认领的未识别进程单独列出，对不上与清单过期同在一个待办分类里。
func TestTodosListsThreeCategories(t *testing.T) {
	manager, st := newTestManager(t)
	manager.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	mustDevice(t, st, "工作站", true)
	mustDevice(t, st, "没有采集的受管设备", true)
	mustDevice(t, st, "不受管设备", false)

	mustObservedItems(t, st, "工作站",
		models.SoftwareObservedItem{
			Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 80, Protocol: "tcp", Exposure: ExposureAllAddresses,
			}},
		},
		models.SoftwareObservedItem{
			Category: CategoryUnidentified, Name: "已认领进程", Version: "",
			ExecutablePath: "/opt/custom-service/service", ProcessName: "service",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposureAllAddresses,
			}},
		},
		models.SoftwareObservedItem{
			Category: CategoryUnidentified, Name: "未认领进程", Version: "",
			ExecutablePath: "/opt/legacy-tool/tool", ProcessName: "tool",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 8080, Protocol: "tcp", Exposure: ExposureAllAddresses,
			}},
		},
	)
	observed, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	observedByKey := map[string]models.SoftwareObservedItem{}
	for _, item := range observed {
		observedByKey[item.Name] = item
	}
	claimID, err := manager.AddDeclaration(Declaration{
		Device: "工作站", Category: CategoryServiceProgram,
		Name: "自定义网站服务", Version: "1.0",
		ListeningPorts: []ListeningPort{{
			Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposurePublicNetwork,
		}},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ClaimDeclaration(
		claimID, observedByKey["已认领进程"].StableKey, CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddDeclaration(Declaration{
		Device: "工作站", Category: CategoryServiceProgram,
		Name: "对不上的声明", Version: "2.0",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddDeclaration(Declaration{
		Device: "不受管设备", Category: CategoryOperatingSystem,
		Name: "网络存储系统", Version: "7.2",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}

	todos, err := manager.Todos(AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(todos.PendingExposure) != 1 ||
		todos.PendingExposure[0].DeviceName != "工作站" ||
		len(todos.PendingExposure[0].Ports) != 2 {
		t.Fatalf("待确认暴露待办 = %+v", todos.PendingExposure)
	}
	if len(todos.UnidentifiedProcesses) != 1 ||
		todos.UnidentifiedProcesses[0].DeviceName != "工作站" ||
		todos.UnidentifiedProcesses[0].ExecutablePath != "/opt/legacy-tool/tool" ||
		todos.UnidentifiedProcesses[0].ObservedStableKey != observedByKey["未认领进程"].StableKey {
		t.Fatalf("未识别进程待办 = %+v", todos.UnidentifiedProcesses)
	}
	if len(todos.Mismatches) != 1 || todos.Mismatches[0].DeviceName != "工作站" ||
		todos.Mismatches[0].Name != "对不上的声明" {
		t.Fatalf("对不上待办 = %+v", todos.Mismatches)
	}
	if len(todos.StaleDevices) != 2 {
		t.Fatalf("清单过期待办 = %+v", todos.StaleDevices)
	}
	var neverCollected *StaleDeviceTodo
	for i := range todos.StaleDevices {
		if todos.StaleDevices[i].DeviceName == "没有采集的受管设备" {
			neverCollected = &todos.StaleDevices[i]
		}
	}
	if neverCollected == nil || !neverCollected.NeverCollected {
		t.Fatalf("从未采集的受管设备应出现在清单过期待办：%+v", todos.StaleDevices)
	}
}
