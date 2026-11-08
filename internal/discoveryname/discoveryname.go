// Package discoveryname 保存网络发现展示名称的共享解析规则。
package discoveryname

import (
	"regexp"
	"unicode"
)

const fallback = "sshmgr"

var pattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)

// DisplayName 返回网络发现实际使用的展示名称：配置名合法时直用；否则依次
// 尝试主机名、把主机名压缩成合法字符，最后使用固定兜底值。
func DisplayName(configured, hostname string) string {
	if pattern.MatchString(configured) {
		return configured
	}
	if pattern.MatchString(hostname) {
		return hostname
	}
	return derive(hostname)
}

func derive(hostname string) string {
	kept := make([]rune, 0, len(hostname))
	for _, symbol := range hostname {
		switch {
		case unicode.IsLetter(symbol) || unicode.IsNumber(symbol):
			kept = append(kept, symbol)
		case symbol == ' ' || symbol == '.' || symbol == '_' || symbol == '-':
			kept = append(kept, symbol)
		}
	}
	start := 0
	for start < len(kept) && !unicode.IsLetter(kept[start]) && !unicode.IsNumber(kept[start]) {
		start++
	}
	kept = kept[start:]
	if len(kept) > 32 {
		kept = kept[:32]
	}
	if len(kept) == 0 {
		return fallback
	}
	return string(kept)
}
