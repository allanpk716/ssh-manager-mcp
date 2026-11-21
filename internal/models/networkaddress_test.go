package models

import "testing"

// TestPreferredNetworkAddress 覆盖首选网络地址的选取规则：只认物理或虚拟
// 局域网候选，按列表顺序取第一个；兜底主机候选与空列表都不产生结果。
func TestPreferredNetworkAddress(t *testing.T) {
	cases := []struct {
		name       string
		candidates []NetworkAddressCandidate
		want       string
	}{
		{
			name: "物理局域网候选",
			candidates: []NetworkAddressCandidate{
				{Address: "192.0.2.10", Source: DialAddressSourcePhysical},
			},
			want: "192.0.2.10",
		},
		{
			name: "虚拟局域网候选",
			candidates: []NetworkAddressCandidate{
				{Address: "100.64.0.10", Source: DialAddressSourceVirtual},
			},
			want: "100.64.0.10",
		},
		{
			name: "物理局域网候选排在虚拟局域网候选之前",
			candidates: []NetworkAddressCandidate{
				{Address: "192.0.2.10", Source: DialAddressSourcePhysical},
				{Address: "100.64.0.10", Source: DialAddressSourceVirtual},
			},
			want: "192.0.2.10",
		},
		{
			name: "只有兜底主机候选时返回空",
			candidates: []NetworkAddressCandidate{
				{Address: "203.0.113.9", Source: DialAddressSourceFallback},
			},
			want: "",
		},
		{
			name:       "没有候选时返回空",
			candidates: nil,
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreferredNetworkAddress(tc.candidates); got != tc.want {
				t.Fatalf("首选网络地址 = %q，应为 %q", got, tc.want)
			}
		})
	}
}
