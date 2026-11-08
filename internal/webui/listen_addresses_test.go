package webui

import (
	"reflect"
	"testing"
)

func TestDeriveListenAddresses(t *testing.T) {
	cases := []struct {
		name       string
		topology   []string
		interfaces []string
		manual     string
		want       []string
		hint       bool
	}{
		{
			name: "有交集", topology: []string{"100.64.10.5"},
			interfaces: []string{"100.64.10.5", "192.0.2.30"},
			want:       []string{"100.64.10.5", "127.0.0.1"}, hint: false,
		},
		{
			name: "无交集只听回环", topology: []string{"100.64.10.5"},
			interfaces: []string{"192.0.2.30"}, want: []string{"127.0.0.1"}, hint: true,
		},
		{
			name: "物理局域网地址不在拓扑中时不被选中", topology: []string{"100.64.10.5"},
			interfaces: []string{"192.168.1.10"}, want: []string{"127.0.0.1"}, hint: true,
		},
		{
			name: "设置覆盖并合入回环", topology: []string{"100.64.10.5"},
			interfaces: []string{"192.0.2.30"}, manual: "100.64.10.6",
			want: []string{"100.64.10.6", "127.0.0.1"}, hint: false,
		},
		{
			name: "重复地址去重", topology: []string{"100.64.10.5", "100.64.10.05"},
			interfaces: []string{"100.64.10.5"}, want: []string{"100.64.10.5", "127.0.0.1"}, hint: false,
		},
		{
			name: "IPv6 使用规范地址", topology: []string{"100::1"},
			interfaces: []string{"100:0000:0000:0000:0000:0000:0000:0001", "192.0.2.30"},
			want:       []string{"100::1", "127.0.0.1"}, hint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := DeriveListenAddresses(
				tc.topology, tc.interfaces, tc.manual)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Addresses, tc.want) {
				t.Fatalf("监听地址 = %v，应为 %v", plan.Addresses, tc.want)
			}
			if plan.NeedsManualListen != tc.hint {
				t.Fatalf("需要手动设置提示 = %v，应为 %v", plan.NeedsManualListen, tc.hint)
			}
		})
	}
}

func TestDeriveListenAddressesRejectsInvalidManualAddress(t *testing.T) {
	if _, err := DeriveListenAddresses(nil, nil, "192.0.2.10,not-an-ip"); err == nil {
		t.Fatal("serve.ui_listen 含非 IP 地址时应返回错误")
	}
}
