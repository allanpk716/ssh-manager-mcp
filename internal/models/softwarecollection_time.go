package models

import (
	"strings"
	"time"
)

// DefaultSoftwareCollectionDailyTime 是全局软件清单采集的默认每日时间。
const DefaultSoftwareCollectionDailyTime = "01:00"

// InvalidSoftwareCollectionDailyTimeError 表示每天采集时间的写法不合法。
type InvalidSoftwareCollectionDailyTimeError struct {
	Input string
}

func (e *InvalidSoftwareCollectionDailyTimeError) Error() string {
	return "每天采集时间必须写成 24 小时制的「小时:分钟」，当前输入 " + e.Input
}

// ParseSoftwareCollectionDailyTime 校验并解析每天采集时间。输入必须写成
// 24 小时制的「小时:分钟」，不带秒，避免同一设置出现多种保存写法。
func ParseSoftwareCollectionDailyTime(value string) (int, int, error) {
	normalized := strings.TrimSpace(value)
	parsed, err := time.Parse("15:04", normalized)
	if err != nil || parsed.Format("15:04") != normalized {
		return 0, 0, &InvalidSoftwareCollectionDailyTimeError{Input: value}
	}
	hour, minute, _ := parsed.Clock()
	return hour, minute, nil
}

// MostRecentSoftwareCollectionTime 返回 now 之前最近一次已到达的每日采集
// 时间。时间跟随 now 携带的时区计算；操作系统时区变化后，下一次读取设
// 置会按新时区解释。
func MostRecentSoftwareCollectionTime(now time.Time, value string) (time.Time, error) {
	hour, minute, err := ParseSoftwareCollectionDailyTime(value)
	if err != nil {
		return time.Time{}, err
	}
	year, month, day := now.Date()
	latest := time.Date(year, month, day, hour, minute, 0, 0, now.Location())
	if latest.After(now) {
		return latest.AddDate(0, 0, -1), nil
	}
	return latest, nil
}

// SoftwareCollectionDue 判断最近一次已到达的每日采集时间是否晚于上次成
// 功与每日时间修改时刻中较晚的一项。这样把时间提前后，不会把修改前已经
// 过去的当天窗口误判成要补采；修改之后错过的新窗口仍会补采。旧数据没有
// 修改时刻时传入零值，行为与只看上次成功一致。待执行的立即采集请求由调
// 用方处理，不属于日历规则。
func SoftwareCollectionDue(lastSuccess, changedAt, now time.Time, value string) (bool, error) {
	latest, err := MostRecentSoftwareCollectionTime(now, value)
	if err != nil {
		return false, err
	}
	effectiveAfter := changedAt
	if lastSuccess.After(effectiveAfter) {
		effectiveAfter = lastSuccess
	}
	return latest.After(effectiveAfter), nil
}
