package sshbroker

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestPlanCommonNetworkAddressesSharesDialOrderingWithoutFallback: 共享
// 规划函数与按服务器条目拨号使用同一套「共同网络判定加排序」规则，但
// 不追加兜底主机——连通探测与拨号据此不会漂移出两套地址顺序。
func TestPlanCommonNetworkAddressesSharesDialOrderingWithoutFallback(t *testing.T) {
	target := []*models.NetworkMembership{
		membershipAt("device-target", "network-virtual", "NetBird", models.NetworkVirtual, "100.64.10.5", 1),
		membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 2),
		membershipAt("device-target", "network-office", "办公室局域网", models.NetworkPhysical, "192.0.2.20", 3),
		membershipAt("device-target", "network-unrelated", "无关网络", models.NetworkPhysical, "203.0.113.9", 4),
	}
	dialer := []*models.NetworkMembership{
		membershipAt("device-dialer", "network-virtual", "NetBird", models.NetworkVirtual, "100.64.10.2", 5),
		membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 6),
		membershipAt("device-dialer", "network-office", "办公室局域网", models.NetworkPhysical, "192.0.2.3", 7),
	}

	got := PlanCommonNetworkAddresses(target, dialer)
	want := []struct {
		address string
		network string
		source  DialAddressSource
	}{
		{"192.0.2.10", "家A局域网", DialAddressPhysical},
		{"192.0.2.20", "办公室局域网", DialAddressPhysical},
		{"100.64.10.5", "NetBird", DialAddressVirtual},
	}
	if len(got.Candidates) != len(want) {
		t.Fatalf("candidates = %+v, want %d entries without the unrelated network", got.Candidates, len(want))
	}
	for i, wantItem := range want {
		if got.Candidates[i].Address != wantItem.address || got.Candidates[i].NetworkName != wantItem.network ||
			got.Candidates[i].Source != wantItem.source {
			t.Fatalf("candidate %d = %+v, want %+v", i, got.Candidates[i], wantItem)
		}
	}
	if got.FallbackOnlyReason != "" {
		t.Fatalf("common networks must not produce a fallback-only reason, got %q", got.FallbackOnlyReason)
	}

	// 没有共同网络时与拨号规划共用同一个原因值，调用方不必各自翻译。
	other := []*models.NetworkMembership{
		membershipAt("device-dialer", "network-other", "其他局域网", models.NetworkPhysical, "203.0.113.1", 8),
	}
	noCommon := PlanCommonNetworkAddresses(target, other)
	if len(noCommon.Candidates) != 0 || noCommon.FallbackOnlyReason != FallbackOnlyNoCommonNetwork {
		t.Fatalf("plan = %+v, want no candidates and the no-common-network reason", noCommon)
	}
}
