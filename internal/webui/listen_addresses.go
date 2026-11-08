package webui

import "ssh-manager-mcp/internal/listenplan"

// ListenPlan 是监听地址推导结果。NeedsManualListen 为 true 时，日志与命令
// 行应提示 owner 使用 serve.ui_listen 手动指定地址。
type ListenPlan = listenplan.Plan

// DeriveListenAddresses 推导权威端 Web UI 的监听地址：未设置手动地址时取
// 拓扑虚拟局域网成员地址与本机网卡地址的交集；无论哪种情况都并入回环地址。
func DeriveListenAddresses(topologyAddresses, interfaceAddresses []string, manualListen string) (ListenPlan, error) {
	return listenplan.Derive(topologyAddresses, interfaceAddresses, manualListen)
}

func canonicalIPSet(values []string) map[string]struct{} {
	return listenplan.CanonicalSet(values)
}
