package models

import "time"

// DialAddressSource 标明一个候选地址来自哪一级挑选优先级。存储与页面共
// 用这份代号，避免展示层反向依赖拨号模块。
type DialAddressSource string

const (
	// DialAddressSourcePhysical 表示目标设备与拨号设备共同所在的物理局域网。
	DialAddressSourcePhysical DialAddressSource = "physical"
	// DialAddressSourceVirtual 表示目标设备与拨号设备共同所在的虚拟局域网。
	DialAddressSourceVirtual DialAddressSource = "virtual"
	// DialAddressSourceFallback 表示服务器条目登记的兜底主机。
	DialAddressSourceFallback DialAddressSource = "fallback"
)

// ConnectionTestFailureReason 是连接测试给 owner 看的白话失败原因代号。
type ConnectionTestFailureReason string

const (
	// ConnectionTestFailurePortUnreachable 表示地址与端口无法建立连接。
	ConnectionTestFailurePortUnreachable ConnectionTestFailureReason = "port-unreachable"
	// ConnectionTestFailureAuthentication 表示 SSH 登录认证未通过。
	ConnectionTestFailureAuthentication ConnectionTestFailureReason = "authentication"
	// ConnectionTestFailureHostKeyMismatch 表示主机密钥与锚不一致。
	ConnectionTestFailureHostKeyMismatch ConnectionTestFailureReason = "host-key-mismatch"
	// ConnectionTestFailureTimeout 表示候选地址在限定时间内没有连上。
	ConnectionTestFailureTimeout ConnectionTestFailureReason = "timeout"
	// ConnectionTestFailureConnectedSaveFailed 表示 SSH 已经连上，但连接后
	// 的保存步骤（例如回写 shell 探测结果或写审计）失败。
	ConnectionTestFailureConnectedSaveFailed ConnectionTestFailureReason = "connected-save-failed"
	// ConnectionTestFailureOther 表示未归入以上类别的失败。
	ConnectionTestFailureOther ConnectionTestFailureReason = "other"
)

// Label 返回连接测试失败原因的固定中文文案。未知代号按其他失败处理，
// 不回显底层错误，避免把排查文本变成秘密暴露面。
func (r ConnectionTestFailureReason) Label() string {
	switch r {
	case ConnectionTestFailurePortUnreachable:
		return "端口不通"
	case ConnectionTestFailureAuthentication:
		return "认证失败"
	case ConnectionTestFailureHostKeyMismatch:
		return "主机密钥不符"
	case ConnectionTestFailureTimeout:
		return "超时"
	case ConnectionTestFailureConnectedSaveFailed:
		return "已连上，但保存连接结果失败"
	case ConnectionTestFailureOther, "":
		return "其他连接失败"
	default:
		return "其他连接失败"
	}
}

// ServerConnectionTestAttempt 是连接测试实际试过的一个候选地址及其结果。
type ServerConnectionTestAttempt struct {
	NetworkName   string                      `json:"network_name,omitempty"`
	Address       string                      `json:"address"`
	Source        DialAddressSource           `json:"source"`
	OK            bool                        `json:"ok"`
	FailureReason ConnectionTestFailureReason `json:"failure_reason,omitempty"`
	Duration      time.Duration               `json:"duration_ns"`
}

// ServerConnectionTestResult 是按服务器条目保存的最近一次连接测试完成结果。
// 字段只允许结构化、可展示事实，不得放入登录凭据、私钥口令或提权口令。
type ServerConnectionTestResult struct {
	CompletedAt   time.Time                   `json:"completed_at"`
	Succeeded     bool                        `json:"succeeded"`
	FailureReason ConnectionTestFailureReason `json:"failure_reason,omitempty"`
	// Hint 是连接测试失败时的可操作提示。它由程序内写定的白话文本组成，
	// 不包含底层错误或任何登录凭据、私钥口令、提权口令。
	Hint               string                        `json:"hint,omitempty"`
	SuccessAddress     string                        `json:"success_address,omitempty"`
	SuccessNetworkName string                        `json:"success_network_name,omitempty"`
	SuccessSource      DialAddressSource             `json:"success_source,omitempty"`
	Duration           time.Duration                 `json:"duration_ns"`
	Attempts           []ServerConnectionTestAttempt `json:"attempts"`
}
