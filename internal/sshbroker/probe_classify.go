package sshbroker

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"

	"ssh-manager-mcp/internal/models"
)

// ClassifyProbeDialError 把本机 TCP 拨号或 SSH 低层连接错误归入连通探测的
// 共用错误分类。err 为 nil 表示连接成功，返回空分类。
func ClassifyProbeDialError(err error) models.ProbeErrorCategory {
	if err == nil {
		return models.ProbeErrorNone
	}
	if errors.Is(err, ErrHostKeyMismatch) {
		return models.ProbeErrorOther
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return models.ProbeErrorTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return models.ProbeErrorTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return models.ProbeErrorDNSFailure
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return models.ProbeErrorConnectionRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.EHOSTDOWN), errors.Is(err, syscall.ENETDOWN):
		return models.ProbeErrorNoRoute
	}
	return models.ProbeErrorOther
}
