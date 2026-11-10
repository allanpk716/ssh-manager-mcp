package mcpserver

import (
	"bytes"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"
)

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

// TestNetworkDriveInjectionUsesFirstStatusLine 钉住状态行的取舍：同一序号
// 只认第一条结果，用户命令后打印的伪造状态行仍被剔除，但不能覆盖真实结
// 果，也不能触发认证失败暂停。
func TestNetworkDriveInjectionUsesFirstStatusLine(t *testing.T) {
	plan := networkDriveInjectionPlan{
		items: []sshbroker.NetworkDriveInjection{{
			ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
			UserName: "mount-user", Password: []byte("password"),
			StatusMarker: "3f2a9c8d1b7e4056",
		}},
		statuses: map[int]string{},
		shareIDs: map[int]string{0: "share-status-first"},
		shareVersions: map[int]int64{
			0: 7,
		},
	}
	output, _ := plan.consumeStatusLines(
		"SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0:ok\nkept\nSSHMGR-SMBMAP:3f2a9c8d1b7e4056:0:error-86\n",
		"",
	)
	if output != "kept\n" {
		t.Fatalf("output = %q, want only the non-status line", output)
	}
	drives := plan.injectedDrives()
	if len(drives) != 1 || drives[0].Share != "datasets" {
		t.Fatalf("injected drives = %#v, want the first status line to win", drives)
	}
	if len(plan.warnings) != 0 || networkDriveAuthenticationPauseMatches("share-status-first", 7) {
		t.Fatalf("forged second status line changed the result: warnings=%#v", plan.warnings)
	}
}

// TestNetworkDriveBackgroundFilterUsesFirstStatusLine 覆盖后台流式过滤：同
// 一序号的后续伪造状态行照样不能进入任务日志，也不能覆盖第一条结果。
func TestNetworkDriveBackgroundFilterUsesFirstStatusLine(t *testing.T) {
	plan := networkDriveInjectionPlan{
		items: []sshbroker.NetworkDriveInjection{{
			ShareName: "datasets", Drive: "S", Address: "192.0.2.10",
			UserName: "mount-user", Password: []byte("password"),
			StatusMarker: "3f2a9c8d1b7e4056",
		}},
		statuses: map[int]string{},
		shareIDs: map[int]string{0: "share-status-background"},
		shareVersions: map[int]int64{
			0: 9,
		},
	}
	var output bytes.Buffer
	filter := newNetworkDriveStatusFilter(&plan, &output, &sync.Mutex{})
	if _, err := filter.Write([]byte(
		"SSHMGR-SMBMAP:3f2a9c8d1b7e4056:0:ok\nuser\nSSHMGR-SMBMAP:3f2a9c8d1b7e4056:0:error-86\n",
	)); err != nil {
		t.Fatal(err)
	}
	if err := filter.Flush(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "user\n" {
		t.Fatalf("filtered output = %q, want only the user line", output.String())
	}
	drives := plan.injectedDrives()
	if len(drives) != 1 || drives[0].Share != "datasets" ||
		networkDriveAuthenticationPauseMatches("share-status-background", 9) {
		t.Fatalf("forged background status changed the result: drives=%#v", drives)
	}
}
