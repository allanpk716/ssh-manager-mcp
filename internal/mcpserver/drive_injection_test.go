package mcpserver

import "testing"

// TestNetworkDriveStatusReason 钉住远端映射结果到警告原因的分类：85 是盘符
// 被占用；53、64、67 与超时是网络类；其余保留系统错误编号。认证失败暂停
// 与更细的提示属于后续票，这里只钉当前贯通路径需要的分类。
func TestNetworkDriveStatusReason(t *testing.T) {
	cases := []struct {
		status string
		drive  string
		want   string
	}{
		{status: "timeout", drive: "S", want: "映射超时（3 秒上限），按网络类失败处理"},
		{status: "error-85", drive: "S", want: "盘符 S: 在这台服务器上已被占用"},
		{status: "error-53", drive: "S", want: "网络类失败（系统错误 53）"},
		{status: "error-64", drive: "T", want: "网络类失败（系统错误 64）"},
		{status: "error-67", drive: "S", want: "网络类失败（系统错误 67）"},
		{status: "error-1219", drive: "S", want: "系统错误 1219"},
		{status: "error-86", drive: "S", want: "系统错误 86"},
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
