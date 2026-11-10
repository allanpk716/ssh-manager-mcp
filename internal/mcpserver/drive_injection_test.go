package mcpserver

import "testing"

// TestNetworkDriveStatusReason 钉住远端映射结果到警告原因的分类：认证失败
// 是 86 与 1326；85 是盘符被占用；53、64、67 与超时是网络类；1219 表示同
// 一台服务器已有不同凭据的连接；其余保留系统错误编号。
func TestNetworkDriveStatusReason(t *testing.T) {
	cases := []struct {
		status string
		drive  string
		want   string
	}{
		{
			status: "timeout", drive: "S",
			want: "映射超时（3 秒上限）：提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制",
		},
		{status: "error-85", drive: "S", want: "盘符 S: 在这台服务器上已被占用"},
		{
			status: "error-53", drive: "S",
			want: "网络类失败（系统错误 53）：提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制",
		},
		{
			status: "error-64", drive: "T",
			want: "网络类失败（系统错误 64）：提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制",
		},
		{
			status: "error-67", drive: "S",
			want: "网络类失败（系统错误 67）：提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制",
		},
		{
			status: "error-1219", drive: "S",
			want: "同一服务器已有不同凭据的连接（系统错误 1219）",
		},
		{status: "error-86", drive: "S", want: "共享密码认证失败，已暂停注入，请同步或检查机密项"},
		{status: "error-1326", drive: "S", want: "共享密码认证失败，已暂停注入，请同步或检查机密项"},
		{status: "error-87", drive: "S", want: "其他错误（系统错误 87）"},
		{status: "error-wrapper", drive: "S", want: "注入封装执行失败"},
		{status: "garbage", drive: "S", want: networkDriveReasonStatusUnreadable},
	}
	for _, testCase := range cases {
		if got := networkDriveStatusReason(testCase.status, testCase.drive); got != testCase.want {
			t.Fatalf("networkDriveStatusReason(%q, %q) = %q, want %q",
				testCase.status, testCase.drive, got, testCase.want)
		}
	}
}
