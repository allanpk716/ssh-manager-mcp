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

// TestSchedulerDoesNotCollectImmediatelyAfterMovingDailyTimeEarlier 验证把
// 每日时间提前后，修改前已经到达的当天新时间不算到期；下一次新时间到达
// 才再次采集。
func TestSchedulerDoesNotCollectImmediatelyAfterMovingDailyTimeEarlier(t *testing.T) {
	changedAt := time.Date(2026, 1, 2, 2, 0, 0, 0, time.Local)
	scheduler, st := newScheduleFixture(t, changedAt, "工作站")
	collectSuccessAtTime(
		t, st, "工作站", time.Date(2026, 1, 1, 3, 0, 0, 0, time.Local),
	)
	if err := st.SetSoftwareCollectionDailyTime("01:00", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	fake := &fakeCollect{}
	scheduler.Collect = fake.fn()
	report, err := scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 0 || len(fake.names()) != 0 {
		t.Fatalf("把时间提前后不应立即采集：%+v %v", report, fake.names())
	}

	scheduler.Now = func() time.Time {
		return time.Date(2026, 1, 3, 1, 0, 0, 0, time.Local)
	}
	report, err = scheduler.ExecuteRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 1 || len(fake.names()) != 1 {
		t.Fatalf("到下一次新每日采集时间后应采集：%+v %v", report, fake.names())
	}
}

// TestSchedulerCatchesMissedDailyTime 验证服务错过昨天采集时间后，在今
// 天时间到来之前启动也会补采已经到期的设备。
func TestSchedulerCatchesMissedDailyTime(t *testing.T) {
	now := time.Date(2026, 1, 3, 0, 30, 0, 0, time.Local)
	scheduler, st := newScheduleFixture(t, now, "工作站")
	setAt := time.Date(2026, 1, 1, 0, 30, 0, 0, time.Local)
	st.NowFn = func() time.Time { return setAt }
	if err := st.SetSoftwareCollectionDailyTime("02:30", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return now }
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
