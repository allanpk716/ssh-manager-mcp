package store

import (
	"strings"
	"testing"
	"time"
)

func TestBindHostsCRUDAndCanonical(t *testing.T) {
	st := openTestStore(t)
	// 全写形式入、缩写规范形存
	if err := st.AddForwardBindHost("2001:0db8::0001"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddForwardBindHost("192.168.50.10"); err != nil {
		t.Fatalf("add v4: %v", err)
	}
	hosts, err := st.ListForwardBindHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0] != "192.168.50.10" || hosts[1] != "2001:db8::1" {
		t.Fatalf("canonical form wrong: %v", hosts)
	}
	// 幂等 add
	if err := st.AddForwardBindHost("2001:db8::1"); err != nil {
		t.Fatalf("idempotent add: %v", err)
	}
	if hosts, _ := st.ListForwardBindHosts(); len(hosts) != 2 {
		t.Fatalf("idempotent add duplicated: %v", hosts)
	}
	// 拒绝:环回/通配/hostname/带 zone
	for _, bad := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "example.com", "fe80::1%eth0", "10.0.0.0/8"} {
		if err := st.AddForwardBindHost(bad); err == nil {
			t.Fatalf("add %q must be rejected", bad)
		}
	}
	// rm 用等价形式命中同一行
	ok, err := st.RemoveForwardBindHost("2001:0DB8::1")
	if err != nil || !ok {
		t.Fatalf("rm equivalent form: %v %v", ok, err)
	}
	if ok, _ := st.RemoveForwardBindHost("2001:db8::1"); ok {
		t.Fatal("rm twice must report false")
	}
}

func TestTunnelOrderLifecycle(t *testing.T) {
	st := openTestStore(t)
	id, err := st.CreateTunnelOrder("tun-1", "", "owner\\alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTunnelOrder("", "", "x"); err == nil {
		t.Fatal("both-empty must be rejected")
	}
	if _, err := st.CreateTunnelOrder("a", "p", "x"); err == nil {
		t.Fatal("both-set must be rejected")
	}
	pend, err := st.PendingTunnelOrders()
	if err != nil || len(pend) != 1 || pend[0].TunnelID != "tun-1" || pend[0].CreatedBy != "owner\\alice" {
		t.Fatalf("pending: %v %v", pend, err)
	}
	if ok, _ := st.MarkTunnelOrderApplied(id); !ok {
		t.Fatal("mark applied must win")
	}
	if ok, _ := st.MarkTunnelOrderApplied(id); ok {
		t.Fatal("second mark must lose (outcome IS NULL guard)")
	}
	if pend, _ := st.PendingTunnelOrders(); len(pend) != 0 {
		t.Fatal("applied order must leave pending set")
	}
	got, err := st.GetTunnelOrder(id)
	if err != nil || got == nil || got.AppliedAt == nil || *got.Outcome != "applied" {
		t.Fatalf("get: %v %v", got, err)
	}
}

func TestTunnelOrderCleanupTargetAware(t *testing.T) {
	st := openTestStore(t)
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	// applied 老行 → 清
	idA, _ := st.CreateTunnelOrder("t-a", "", "o")
	_, _ = st.MarkTunnelOrderApplied(idA) // brief 原文 `_ =` 单赋值对两返回值不编译,改 `_, _ =`
	st.db.Exec(`UPDATE tunnel_orders SET created_at=? WHERE id=?`, old, idA)
	// pending + 目标行在(模拟持续重开中的 project 单) → 不清
	idP, _ := st.CreateTunnelOrder("", "proj-1", "o")
	st.db.Exec(`UPDATE tunnel_orders SET created_at=? WHERE id=?`, old, idP)
	_ = st.InsertTunnelRegistry(TunnelRegistryRow{TunnelID: "t-x", ProjectID: "proj-1", ServerID: "s", Remote: "r", LocalAddr: "a", ListenHost: "h", OpenedAt: old, LastRenewed: old})
	// pending + 目标缺席(tunnel 单) → 清
	idT, _ := st.CreateTunnelOrder("t-gone", "", "o")
	st.db.Exec(`UPDATE tunnel_orders SET created_at=? WHERE id=?`, old, idT)

	if err := st.CleanupTunnelOrders(); err != nil {
		t.Fatal(err)
	}
	// (brief 原句 `_, err := GetTunnelOrder(idA); err == nil` 与接口 "缺行返回
	// (nil,nil)" 矛盾——缺行永远 err==nil,测试永红;按同测试两行下方兄弟断言的
	// got==nil/got!=nil 模式修正,断言意图不变:applied 老行必须已被清掉。)
	if got, _ := st.GetTunnelOrder(idA); got != nil {
		t.Fatal("old applied row must be cleaned")
	}
	if got, _ := st.GetTunnelOrder(idP); got == nil {
		t.Fatal("pending order with live target rows must NOT be cleaned")
	}
	if got, _ := st.GetTunnelOrder(idT); got != nil {
		t.Fatal("pending order with absent target must be cleaned")
	}
}

func TestTunnelRegistryOps(t *testing.T) {
	st := openTestStore(t)
	row := TunnelRegistryRow{TunnelID: "t1", ProjectID: "p1", ServerID: "s1", Remote: "127.0.0.1:5432", LocalAddr: "127.0.0.1:9000", ListenHost: "127.0.0.1", OpenedAt: 1, LastRenewed: 1}
	if err := st.InsertTunnelRegistry(row); err != nil {
		t.Fatal(err)
	}
	has, _ := st.HasTunnelRegistryRow("t1")
	if !has {
		t.Fatal("row must exist")
	}
	if ok, _ := st.RenewTunnelHeartbeat("t1", 99); !ok {
		t.Fatal("renew must hit")
	}
	if ok, _ := st.RenewTunnelHeartbeat("nope", 99); ok {
		t.Fatal("renew missing row must miss (zero-row signal)")
	}
	rows, _ := st.ListTunnelRegistry()
	if len(rows) != 1 || rows[0].LastRenewed != 99 {
		t.Fatalf("rows: %+v", rows)
	}
	n, _ := st.CountTunnelRegistryProject("p1")
	if n != 1 {
		t.Fatal("count by project")
	}
	_ = st.GCTunnelRegistry(50) // cutoff=50 → last_renewed(99) 不删
	if rows, _ = st.ListTunnelRegistry(); len(rows) != 1 {
		t.Fatal("fresh row must survive GC")
	}
	_ = st.GCTunnelRegistry(200)
	if rows, _ = st.ListTunnelRegistry(); len(rows) != 0 {
		t.Fatal("stale row must be GC'd")
	}
	_ = st.DeleteTunnelRegistry([]string{"t1"})
	if has, _ := st.HasTunnelRegistryRow("t1"); has {
		t.Fatal("delete")
	}
}

func TestTunnelStoreReadOnlyGates(t *testing.T) {
	st := openTestStore(t)
	st.SetReadOnly(nil)
	if !st.IsReadOnly() {
		t.Fatal("IsReadOnly must reflect SetReadOnly")
	}
	if err := st.AddForwardBindHost("10.1.2.3"); err != ErrReadOnly {
		t.Fatalf("write on read-only: %v", err)
	}
	if _, err := st.CreateTunnelOrder("t", "", "o"); err != ErrReadOnly {
		t.Fatalf("order write on read-only: %v", err)
	}
	if err := st.InsertTunnelRegistry(TunnelRegistryRow{TunnelID: "t"}); err != ErrReadOnly {
		t.Fatalf("registry write on read-only: %v", err)
	}
	// 读不受影响
	if _, err := st.ListForwardBindHosts(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalBindIP(t *testing.T) {
	if c, err := CanonicalBindIP(" 2001:0db8::0001 "); err != nil || c != "2001:db8::1" {
		t.Fatalf("canonical: %q %v", c, err)
	}
	for _, bad := range []string{"example.com", "fe80::1%eth0", "10.0.0.0/8", ""} {
		if _, err := CanonicalBindIP(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
	if !strings.Contains(func() string { _, err := CanonicalBindIP("x"); return err.Error() }(), "IP literal") {
		t.Fatal("error wording should mention IP literal")
	}
}
