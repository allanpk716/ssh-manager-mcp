package models

import (
	"testing"
	"time"
)

func TestSoftwareCollectionDueHonorsDailyTimeChange(t *testing.T) {
	location := time.FixedZone("测试时区", 8*60*60)
	tests := []struct {
		name        string
		lastSuccess time.Time
		changedAt   time.Time
		now         time.Time
		dailyTime   string
		want        bool
	}{
		{
			name:        "把时间提前后不立即采集",
			lastSuccess: time.Date(2026, 1, 1, 3, 0, 0, 0, location),
			changedAt:   time.Date(2026, 1, 2, 2, 0, 0, 0, location),
			now:         time.Date(2026, 1, 2, 2, 0, 0, 0, location),
			dailyTime:   "01:00",
			want:        false,
		},
		{
			name:        "到下一次提前后的新时间采集",
			lastSuccess: time.Date(2026, 1, 1, 3, 0, 0, 0, location),
			changedAt:   time.Date(2026, 1, 2, 2, 0, 0, 0, location),
			now:         time.Date(2026, 1, 3, 1, 0, 0, 0, location),
			dailyTime:   "01:00",
			want:        true,
		},
		{
			name:        "把时间推迟后仍按新时间采集",
			lastSuccess: time.Date(2026, 1, 2, 1, 30, 0, 0, location),
			changedAt:   time.Date(2026, 1, 2, 2, 0, 0, 0, location),
			now:         time.Date(2026, 1, 2, 3, 0, 0, 0, location),
			dailyTime:   "03:00",
			want:        true,
		},
		{
			name:        "修改之后错过新时间要补采",
			lastSuccess: time.Date(2026, 1, 1, 0, 30, 0, 0, location),
			changedAt:   time.Date(2026, 1, 2, 0, 30, 0, 0, location),
			now:         time.Date(2026, 1, 2, 2, 0, 0, 0, location),
			dailyTime:   "01:00",
			want:        true,
		},
		{
			name:        "旧数据没有修改时刻时保持原判断",
			lastSuccess: time.Date(2026, 1, 1, 3, 0, 0, 0, location),
			changedAt:   time.Time{},
			now:         time.Date(2026, 1, 2, 2, 0, 0, 0, location),
			dailyTime:   "01:00",
			want:        true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SoftwareCollectionDue(test.lastSuccess, test.changedAt, test.now, test.dailyTime)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("到期判断应为 %v，得到 %v", test.want, got)
			}
		})
	}
}
