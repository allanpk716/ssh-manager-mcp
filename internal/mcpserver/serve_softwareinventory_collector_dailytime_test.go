package mcpserver

import (
	"encoding/json"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestSoftwareInventoryAssignmentsFollowDailyTime 验证客户端实例采集分派
// 使用权威端持久保存的每日时间：新时间未到时不下发，到新时间后下发。
func TestSoftwareInventoryAssignmentsFollowDailyTime(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	gpu, err := fx.st.GetServerByName("gpu")
	if err != nil || gpu == nil {
		t.Fatalf("取 gpu 条目失败：%v %+v", err, gpu)
	}
	if err := fx.st.SetSoftwareCollectionCollector(
		"gpu-设备", "laptop", gpu.ID, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	changedAt := time.Date(2026, 1, 2, 1, 0, 0, 0, time.Local)
	fx.st.NowFn = func() time.Time { return changedAt }
	if err := fx.st.SetSoftwareCollectionDailyTime("02:30", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	beforeTime := time.Date(2026, 1, 2, 2, 0, 0, 0, time.Local)
	lastSuccess := time.Date(2026, 1, 2, 1, 30, 0, 0, time.Local)
	fx.st.NowFn = func() time.Time { return lastSuccess }
	if err := fx.st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Collector: models.ClientCollector(fx.laptopID), ServerID: gpu.ID,
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	fx.st.NowFn = func() time.Time { return beforeTime }
	_, body := postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	assertAssignmentDue(t, body, false)

	fx.st.NowFn = func() time.Time { return beforeTime.Add(30 * time.Minute) }
	_, body = postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	assertAssignmentDue(t, body, true)
}

// TestSoftwareInventoryAssignmentsHonorDailyTimeChange 验证客户端实例分派
// 与权威端调度使用同一个「修改后从下一次新时间开始生效」的判断。
func TestSoftwareInventoryAssignmentsHonorDailyTimeChange(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	gpu, err := fx.st.GetServerByName("gpu")
	if err != nil || gpu == nil {
		t.Fatalf("取 gpu 条目失败：%v %+v", err, gpu)
	}
	if err := fx.st.SetSoftwareCollectionCollector(
		"gpu-设备", "laptop", gpu.ID, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	lastSuccess := time.Date(2026, 1, 1, 3, 0, 0, 0, time.Local)
	fx.st.NowFn = func() time.Time { return lastSuccess }
	if err := fx.st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Collector: models.ClientCollector(fx.laptopID), ServerID: gpu.ID,
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	changedAt := time.Date(2026, 1, 2, 2, 0, 0, 0, time.Local)
	fx.st.NowFn = func() time.Time { return changedAt }
	if err := fx.st.SetSoftwareCollectionDailyTime("01:00", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	_, body := postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	assertAssignmentDue(t, body, false)

	nextNewTime := time.Date(2026, 1, 3, 1, 0, 0, 0, time.Local)
	fx.st.NowFn = func() time.Time { return nextNewTime }
	_, body = postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	assertAssignmentDue(t, body, true)

	missedAfterChange := time.Date(2026, 1, 3, 2, 0, 0, 0, time.Local)
	fx.st.NowFn = func() time.Time { return missedAfterChange }
	_, body = postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	assertAssignmentDue(t, body, true)
}

func assertAssignmentDue(t *testing.T, body string, wanted bool) {
	t.Helper()
	var response struct {
		Devices []struct {
			Due bool `json:"due"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Devices) != 1 || response.Devices[0].Due != wanted {
		t.Fatalf("采集分派到期状态应为 %v：%s", wanted, body)
	}
}
