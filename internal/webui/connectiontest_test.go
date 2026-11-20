package webui

import (
	"testing"
	"time"
)

// TestFormatConnectionTestCardTime 覆盖服务器列表卡片使用的短时间格式；传
// 入当前时间让日期判断可测试。
func TestFormatConnectionTestCardTime(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 4, 5, 0, time.Local)
	cases := []struct {
		name        string
		completedAt time.Time
		want        string
	}{
		{"同一天", time.Date(2026, 11, 20, 10, 12, 0, 0, time.Local), "今天 10:12"},
		{"昨天", time.Date(2026, 11, 19, 9, 5, 0, 0, time.Local), "昨天 09:05"},
		{"同年更早", time.Date(2026, 3, 2, 8, 7, 0, 0, time.Local), "3月2日 08:07"},
		{"跨年", time.Date(2025, 12, 31, 23, 59, 0, 0, time.Local), "2025年12月31日"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := formatConnectionTestCardTime(testCase.completedAt, now)
			if got != testCase.want {
				t.Fatalf("短时间 = %q，应为 %q", got, testCase.want)
			}
		})
	}
}
