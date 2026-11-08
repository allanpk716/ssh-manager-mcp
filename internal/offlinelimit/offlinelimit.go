// Package offlinelimit 保存离线上限时长的共享写法。客户端配置、环境变量与
// 权威端配对默认值必须接受同一批值。
package offlinelimit

import (
	"fmt"
	"time"
)

// Minimum 是允许的最短离线上限；0 表示关闭时限。
const Minimum = time.Hour

// Parse 接受 0 或不小于 1 小时的 Go 时长；空字符串按关闭处理。
func Parse(value, source string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || (duration != 0 && duration < Minimum) {
		return 0, fmt.Errorf("invalid %s %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", source, value)
	}
	return duration, nil
}
