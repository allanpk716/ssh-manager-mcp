package probe

import (
	"testing"
	"time"
)

func TestObservationAgeLabel(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		observedAt time.Time
		want       string
	}{
		{name: "零值观察时间", observedAt: time.Time{}, want: "旧"},
		{name: "不到一分钟", observedAt: now.Add(-59 * time.Second), want: "刚刚"},
		{name: "一分钟", observedAt: now.Add(-time.Minute), want: "1分钟前"},
		{name: "十分钟边界", observedAt: now.Add(-10 * time.Minute), want: "10分钟前"},
		{name: "超过十分钟", observedAt: now.Add(-10*time.Minute - time.Nanosecond), want: "10分钟前 旧"},
		{name: "二十五分钟", observedAt: now.Add(-25 * time.Minute), want: "25分钟前 旧"},
		{name: "五十九分钟五十九秒", observedAt: now.Add(-time.Hour + time.Second), want: "59分钟前 旧"},
		{name: "一小时", observedAt: now.Add(-time.Hour), want: "1小时前 旧"},
		{name: "三小时", observedAt: now.Add(-3 * time.Hour), want: "3小时前 旧"},
		{name: "二十三小时五十九分五十九秒", observedAt: now.Add(-24*time.Hour + time.Second), want: "23小时前 旧"},
		{name: "一天", observedAt: now.Add(-24 * time.Hour), want: "1天前 旧"},
		{name: "三天", observedAt: now.Add(-72 * time.Hour), want: "3天前 旧"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ObservationAgeLabel(now, test.observedAt); got != test.want {
				t.Fatalf("ObservationAgeLabel() = %q, want %q", got, test.want)
			}
		})
	}
}
