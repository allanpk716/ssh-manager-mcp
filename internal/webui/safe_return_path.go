package webui

import "strings"

// safeReturnPath 只返回 Web UI 内部路径。所有返回路径集中在这里套用同一
// 份规则，拒绝协议头、双斜杠与反斜杠，防止跳转到站外地址。
func safeReturnPath(path, fallback string, allowedPrefixes ...string) string {
	if !strings.HasPrefix(path, "/ui/") {
		return fallback
	}
	if strings.Contains(path, "//") || strings.Contains(path, `\`) {
		return fallback
	}
	if scheme, _, found := strings.Cut(path, ":"); found && strings.Contains(scheme, "/") {
		return fallback
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return path
		}
	}
	return fallback
}
