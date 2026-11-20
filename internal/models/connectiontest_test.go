package models

import "testing"

// TestConnectionTestFailureReasonLabel 覆盖失败原因代号到 owner 白话文案
// 的映射；未知与空代号都回落到其他连接失败，不回显底层错误。
func TestConnectionTestFailureReasonLabel(t *testing.T) {
	cases := map[ConnectionTestFailureReason]string{
		ConnectionTestFailurePortUnreachable:     "端口不通",
		ConnectionTestFailureAuthentication:      "认证失败",
		ConnectionTestFailureHostKeyMismatch:     "主机密钥不符",
		ConnectionTestFailureTimeout:             "超时",
		ConnectionTestFailureConnectedSaveFailed: "已连上，但保存连接结果失败",
		ConnectionTestFailureOther:               "其他连接失败",
		"":                                       "其他连接失败",
		"unknown-reason":                         "其他连接失败",
	}
	for reason, want := range cases {
		if got := reason.Label(); got != want {
			t.Fatalf("失败原因 %q 的文案 = %q，应为 %q", reason, got, want)
		}
	}
}
