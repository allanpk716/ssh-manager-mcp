package softwareinventory

import (
	"context"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// TestSchedulerUsesPersistedDailyCollectionTime 钉住全局每日采集时间：未配
// 置时使用 01:00；持久保存新时间后，晚于旧时间而早于新时间的上次成功结果
// 要等到新时间才再次到期。
func TestSchedulerUsesPersistedDailyCollectionTime(t *testing.T) {
	beforeNewTime := time.Date(2026, 1, 2, 2, 0, 0, 0, time.Local)
	scheduler, st := newScheduleFixture(t, beforeNewTime, "工作站")
	collectSuccessAtTime(
		t, st, "工作站", time.Date(2026, 1, 2, 1, 30, 0, 0, time.Local),
	)

	dailyTime, err := st.SoftwareCollectionDailyTime()
	if err != nil {
		t.Fatal(err)
	}
	if dailyTime != "01:00" {
		t.Fatalf("未配置时每日采集时间应为 01:00，得到 %s", dailyTime)
	}
	if err := st.SetSoftwareCollectionDailyTime("02:30", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.SoftwareCollectionDailyTime()
	if err != nil {
		t.Fatal(err)
	}
	if persisted != "02:30" {
		t.Fatalf("每日采集时间应持久保存为 02:30，得到 %s", persisted)
	}

	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()
	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 0 || len(fake.names()) != 0 {
		t.Fatalf("未到新每日采集时间时不应采集：%+v %v", report, fake.names())
	}

	newTime := beforeNewTime.Add(30 * time.Minute)
	scheduler.Now = func() time.Time { return newTime }
	report, err = scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 1 || len(fake.names()) != 1 || fake.names()[0] != "工作站" {
		t.Fatalf("到新每日采集时间后应采集：%+v %v", report, fake.names())
	}
}

// TestSchedulerCatchesMissedDailyTime 验证服务错过昨天采集时间后，在今
// 天时间到来之前启动也会补采已经到期的设备。
func TestSchedulerCatchesMissedDailyTime(t *testing.T) {
	now := time.Date(2026, 1, 3, 0, 30, 0, 0, time.Local)
	scheduler, st := newScheduleFixture(t, now, "工作站")
	if err := st.SetSoftwareCollectionDailyTime("02:30", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	collectSuccessAtTime(
		t, st, "工作站", time.Date(2026, 1, 1, 1, 0, 0, 0, time.Local),
	)
	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()

	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 1 || len(fake.names()) != 1 {
		t.Fatalf("错过采集时间后应补采：%+v %v", report, fake.names())
	}
}
