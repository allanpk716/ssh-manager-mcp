//go:build windows

package sshbroker

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"

	"ssh-manager-mcp/internal/models"
)

// TestClassifyProbeDialErrorWinsockRefused 钉住 Windows 上 connectex 返回
// WSAECONNREFUSED（目标机器主动拒绝）时必须归类为连接被拒绝；该错误码与
// syscall.ECONNREFUSED 数值不同，漏判会把端口不通错报成其他错误。
func TestClassifyProbeDialErrorWinsockRefused(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want models.ProbeErrorCategory
	}{
		{
			name: "connectex 拒绝",
			err: &os.SyscallError{Syscall: "connectex",
				Err: windows.WSAECONNREFUSED},
			want: models.ProbeErrorConnectionRefused,
		},
		{
			name: "主机不可达",
			err: &os.SyscallError{Syscall: "connectex",
				Err: windows.WSAEHOSTUNREACH},
			want: models.ProbeErrorNoRoute,
		},
		{
			name: "网络不可达",
			err: &os.SyscallError{Syscall: "connectex",
				Err: windows.WSAENETUNREACH},
			want: models.ProbeErrorNoRoute,
		},
	}
	for _, tc := range cases {
		if got := ClassifyProbeDialError(tc.err); got != tc.want {
			t.Errorf("%s：分类错误，得到 %q，想要 %q", tc.name, got, tc.want)
		}
	}
}
