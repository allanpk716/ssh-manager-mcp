package mcpserver

import (
	"fmt"
	"os"
	"time"
)

// endToEndReverifyWindow 只读取端到端浏览器测试注入的重新验证窗口。生产默
// 认值在 Web UI 层；这个环境变量为空或非法时也不改变生产行为。它只能把窗
// 口缩短（更严格），不能超过默认 5 分钟，以免被用来放宽重新验证。
func endToEndReverifyWindow() time.Duration {
	value := os.Getenv("SSHMGR_E2E_REVERIFY_WINDOW")
	if value == "" {
		return 0
	}
	window, err := time.ParseDuration(value)
	if err != nil || window <= 0 || window > 5*time.Minute {
		fmt.Fprintf(os.Stderr, "sshmgr serve: ignored invalid SSHMGR_E2E_REVERIFY_WINDOW %q\n", value)
		return 0
	}
	return window
}
