package store

import (
	"strings"
	"testing"
)

// TestNetworkMembershipPhysicalVirtualAddressConflictWarnsButWrites 覆盖新
// 增、修改与移动：同一设备的物理局域网成员地址与虚拟局域网成员地址等价
// 时照常写入，只返回警告；等价判断使用统一的地址规范化规则。
func TestNetworkMembershipPhysicalVirtualAddressConflictWarnsButWrites(t *testing.T) {
	assertConflict := func(t *testing.T, write *NetworkMembershipWriteResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("冲突时网络成员仍必须登记成功：%v", err)
		}
		if write.Membership == nil {
			t.Fatal("冲突时返回的网络成员不能为空")
		}
		for _, want := range []string{
			"警告：地址", "同时登记在物理局域网", "与虚拟局域网",
			"无法自动首次锚定", "请把其中一个成员改成所在网络自己的地址",
			"servers pin-hostkey", "--fingerprint",
		} {
			if !strings.Contains(write.Warning, want) {
				t.Fatalf("警告必须包含 %q，得到：%s", want, write.Warning)
			}
		}
	}

	t.Run("add virtual then physical", func(t *testing.T) {
		st := newTestStore(t)
		seedMembershipNetworks(t, st)
		if _, err := st.AddDevice("工作站", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("工作站", "虚拟网", "2001:db8::1"); err != nil {
			t.Fatal(err)
		}
		write, err := st.AddNetworkMembershipWithWarning(
			"工作站", "家A局域网", "2001:0db8:0000:0000:0000:0000:0000:0001",
		)
		assertConflict(t, write, err)
		if !strings.Contains(write.Warning,
			"同时登记在物理局域网 家A局域网 与虚拟局域网 虚拟网") {
			t.Fatalf("新增冲突警告必须按网络类型显示名称：%s", write.Warning)
		}
		if !strings.Contains(write.Warning, "2001:0db8:0000:0000:0000:0000:0000:0001") {
			t.Fatalf("新增冲突警告应使用本次登记的地址：%s", write.Warning)
		}
	})

	t.Run("add physical then virtual", func(t *testing.T) {
		st := newTestStore(t)
		seedMembershipNetworks(t, st)
		if _, err := st.AddDevice("工作站", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("工作站", "家A局域网", "2001:db8::1"); err != nil {
			t.Fatal(err)
		}
		write, err := st.AddNetworkMembershipWithWarning(
			"工作站", "虚拟网", "2001:db8::1",
		)
		assertConflict(t, write, err)
		if !strings.Contains(write.Warning,
			"同时登记在物理局域网 家A局域网 与虚拟局域网 虚拟网") {
			t.Fatalf("反向新增冲突警告必须按网络类型显示名称：%s", write.Warning)
		}
	})

	t.Run("edit", func(t *testing.T) {
		st := newTestStore(t)
		seedMembershipNetworks(t, st)
		if _, err := st.AddDevice("工作站", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.0.2.10"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("工作站", "虚拟网", "2001:db8::1"); err != nil {
			t.Fatal(err)
		}
		write, err := st.EditNetworkMembershipWithWarning("工作站", "家A局域网", "2001:db8::1")
		assertConflict(t, write, err)
		if write.Membership.Address != "2001:db8::1" {
			t.Fatalf("修改冲突时也应写入新地址：%+v", write.Membership)
		}
	})

	t.Run("move", func(t *testing.T) {
		st := newTestStore(t)
		seedMembershipNetworks(t, st)
		if _, err := st.AddDevice("工作站", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.0.2.10"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership("工作站", "虚拟网", "2001:db8::1"); err != nil {
			t.Fatal(err)
		}
		write, err := st.MoveNetworkMembershipWithWarning(
			"工作站", "家A局域网", "机房局域网", "2001:db8::1",
		)
		assertConflict(t, write, err)
		if write.Membership.NetworkName != "机房局域网" || write.Membership.Address != "2001:db8::1" {
			t.Fatalf("移动冲突时也应写入目标网络成员：%+v", write.Membership)
		}
	})
}

// TestNetworkMembershipPhysicalVirtualAddressConflictNoWarning 确认普通物理
// 局域网地址与虚拟局域网地址不同时不产生警告。
func TestNetworkMembershipPhysicalVirtualAddressConflictNoWarning(t *testing.T) {
	st := newTestStore(t)
	seedMembershipNetworks(t, st)
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}

	added, err := st.AddNetworkMembershipWithWarning("工作站", "虚拟网", "100.64.0.1")
	if err != nil {
		t.Fatalf("新增虚拟局域网成员失败：%v", err)
	}
	physical, err := st.AddNetworkMembershipWithWarning("工作站", "家A局域网", "192.0.2.1")
	if err != nil {
		t.Fatalf("新增物理局域网成员失败：%v", err)
	}
	if added.Warning != "" || physical.Warning != "" {
		t.Fatalf("地址不同时不应警告：虚拟=%q 物理=%q", added.Warning, physical.Warning)
	}

	edited, err := st.EditNetworkMembershipWithWarning("工作站", "家A局域网", "192.0.2.2")
	if err != nil {
		t.Fatalf("修改物理局域网成员失败：%v", err)
	}
	if edited.Warning != "" {
		t.Fatalf("地址仍不同时不应警告：%s", edited.Warning)
	}

	moved, err := st.MoveNetworkMembershipWithWarning(
		"工作站", "家A局域网", "机房局域网", "192.0.2.3",
	)
	if err != nil {
		t.Fatalf("移动物理局域网成员失败：%v", err)
	}
	if moved.Warning != "" {
		t.Fatalf("地址仍不同时移动也不应警告：%s", moved.Warning)
	}
}
