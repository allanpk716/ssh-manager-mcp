//go:build !windows

package sshbroker

import (
	"errors"
	"syscall"
)

// isConnRefusedErrno 判断拨号错误是否为「目标端口明确拒绝连接」的系统错误码。
func isConnRefusedErrno(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// isNoRouteErrno 判断拨号错误是否为「目标主机或网络不可达 / 已下线」的系统
// 错误码。
func isNoRouteErrno(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTDOWN) || errors.Is(err, syscall.ENETDOWN)
}
