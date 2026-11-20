//go:build windows

package sshbroker

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isConnRefusedErrno 判断拨号错误是否为「目标端口明确拒绝连接」的系统错误码。
// Windows 上 TCP 拨号失败返回 Winsock 错误码 WSAECONNREFUSED，它与
// syscall.ECONNREFUSED 的数值不同，errors.Is 不会互相命中，必须单独判断。
func isConnRefusedErrno(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED)
}

// isNoRouteErrno 判断拨号错误是否为「目标主机或网络不可达 / 已下线」的系统
// 错误码。Windows 上同样要先判 Winsock 错误码（WSAEHOSTUNREACH、
// WSAENETUNREACH），再保留通用错误码兜底。
func isNoRouteErrno(err error) bool {
	return errors.Is(err, windows.WSAEHOSTUNREACH) || errors.Is(err, windows.WSAENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTDOWN) || errors.Is(err, syscall.ENETDOWN)
}
