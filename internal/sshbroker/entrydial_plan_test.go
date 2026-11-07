package sshbroker

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func membershipAt(deviceID, networkID, networkName string, networkType models.NetworkType, address string, order int64) *models.NetworkMembership {
	return &models.NetworkMembership{
		DeviceID: deviceID, NetworkID: networkID, NetworkName: networkName,
		NetworkType: networkType, Address: address, RegistrationOrder: order,
	}
}

// TestPlanServerDialAddressesOrdersAndDeduplicates: 候选地址只取两台设备共同
// 所在的网络；物理局域网优先于虚拟局域网，兜底主机最后；同级按登记序号；
// 重复地址只保留最高优先级的一份。
func TestPlanServerDialAddressesOrdersAndDeduplicates(t *testing.T) {
	target := []*models.NetworkMembership{
		membershipAt("device-target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 1),
		membershipAt("device-target", "network-office", "办公室局域网", models.NetworkPhysical, "192.0.2.20", 3),
		membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 2),
		// 与办公室地址相同：物理局域网优先，应保留办公室这一份。
		membershipAt("device-target", "network-virtual-copy", "重复虚拟局域网", models.NetworkVirtual, "192.0.2.20", 4),
	}
	dialer := []*models.NetworkMembership{
		membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 5),
		membershipAt("device-dialer", "network-office", "办公室局域网", models.NetworkPhysical, "192.0.2.3", 6),
		membershipAt("device-dialer", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.2", 7),
		membershipAt("device-dialer", "network-virtual-copy", "重复虚拟局域网", models.NetworkVirtual, "100.64.10.3", 8),
		membershipAt("device-dialer", "network-unrelated", "无关网络", models.NetworkPhysical, "203.0.113.1", 9),
	}

	got := PlanServerDialAddresses(target, dialer, "198.51.100.10")

	type wanted struct {
		address    string
		network    string
		sequence   int
		sourceName string
	}
	want := []wanted{
		{"192.0.2.10", "家A局域网", 1, "physical"},
		{"192.0.2.20", "办公室局域网", 1, "physical"},
		{"100.64.10.5", "虚拟局域网", 1, "virtual"},
		{"198.51.100.10", "", 0, "fallback"},
	}
	if len(got.Candidates) != len(want) {
		t.Fatalf("candidates = %+v, want %d entries", got.Candidates, len(want))
	}
	for i, wantItem := range want {
		gotItem := got.Candidates[i]
		if gotItem.Address != wantItem.address || gotItem.NetworkName != wantItem.network ||
			gotItem.NetworkSequence != wantItem.sequence || string(gotItem.Source) != wantItem.sourceName {
			t.Fatalf("candidate %d = %+v, want %+v", i, gotItem, wantItem)
		}
	}
	if got.FallbackOnlyReason != "" {
		t.Fatalf("common networks must produce topology candidates, reason = %q", got.FallbackOnlyReason)
	}
}

// TestPlanServerDialAddressesFallbackOnlyReasons: 拨号设备未知或没有共同网络
// 时只保留兜底主机并给出原因；目标本来就没有拓扑登记时保持旧部署行为，
// 不附任何原因。
func TestPlanServerDialAddressesFallbackOnlyReasons(t *testing.T) {
	target := []*models.NetworkMembership{
		membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
	}
	otherNetwork := []*models.NetworkMembership{
		membershipAt("device-dialer", "network-other", "其他局域网", models.NetworkPhysical, "203.0.113.1", 2),
	}

	cases := []struct {
		name       string
		dialer     []*models.NetworkMembership
		wantReason string
	}{
		{"dialer unknown", nil, "dialer-unknown"},
		{"no common network", otherNetwork, "no-common-network"},
		{"target has no topology", nil, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inputTarget := target
			if testCase.name == "target has no topology" {
				inputTarget = nil
			}
			got := PlanServerDialAddresses(inputTarget, testCase.dialer, "198.51.100.10")
			if len(got.Candidates) != 1 || got.Candidates[0].Address != "198.51.100.10" ||
				got.Candidates[0].Source != DialAddressFallback {
				t.Fatalf("candidates = %+v, want only the fallback address", got.Candidates)
			}
			if string(got.FallbackOnlyReason) != testCase.wantReason {
				t.Fatalf("reason = %q, want %q", got.FallbackOnlyReason, testCase.wantReason)
			}
		})
	}
}

// TestPlanServerDialAddressesFallbackDuplicateKeepsTopologySource: 成员地址
// 与兜底主机重复时只试一次，但保留物理局域网来源；这不是「没有共同网络」，
// 失败时不该附补登记拓扑提示。
func TestPlanServerDialAddressesFallbackDuplicateKeepsTopologySource(t *testing.T) {
	target := []*models.NetworkMembership{
		membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
	}
	dialer := []*models.NetworkMembership{
		membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 2),
	}

	got := PlanServerDialAddresses(target, dialer, "192.0.2.10")
	if len(got.Candidates) != 1 || got.Candidates[0].Source != DialAddressPhysical ||
		got.Candidates[0].NetworkName != "家A局域网" || got.FallbackOnlyReason != "" {
		t.Fatalf("plan = %+v, want one physical candidate and no fallback-only reason", got)
	}
}
