//go:build !windows && !unix

package hotswap

import "os"

// peek_fallback.go — 平台不支持窥视:调用方退回阻塞读(feeder 的 Park
// 收尾由 ReloadService 的兜底路径处理)。
func peekReadable(*os.File) (readable, ok bool) { return false, false }
