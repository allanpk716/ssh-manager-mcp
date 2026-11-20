package store

import (
	"errors"
	"testing"
)

// TestEditDeviceSitesWithSourceWritesAllDevicesInOneTransaction 校验批量设置
// 地点的存储语义：按设备去重、一次事务全部成功，每台设备各写一条来源为
// Web UI 的现有机器地点修改审计。
func TestEditDeviceSitesWithSourceWritesAllDevicesInOneTransaction(t *testing.T) {
	st := newTestStore(t)
	company := mustAddSite(t, st, "公司", "", "")
	factory := mustAddSite(t, st, "一厂区", "", company.Name)
	for _, name := range []string{"机器一", "机器二"} {
		if _, err := st.AddDeviceAtSite(name, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	edited, err := st.EditDeviceSitesWithSource(
		[]string{"机器一", "机器二", "机器一"}, "公司 / 一厂区", WebUISource("192.0.2.9"),
	)
	if err != nil {
		t.Fatalf("批量设置设备地点失败：%v", err)
	}
	if len(edited) != 2 {
		t.Fatalf("重复设备应去重，实际返回 %d 台：%+v", len(edited), edited)
	}
	for _, name := range []string{"机器一", "机器二"} {
		device, err := st.GetDevice(name)
		if err != nil {
			t.Fatal(err)
		}
		if device.SiteID != factory.ID {
			t.Fatalf("设备 %s 的地点应为一厂区，实际 %+v", name, device)
		}
	}
	rows, err := st.QueryAudit(AuditFilter{
		Actions: []string{"device.edit"}, Source: AuditSourceWebUI, SourceIP: "192.0.2.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("两台设备应各写一条 device.edit 审计，实际 %d 条：%+v", len(rows), rows)
	}

	// 选择「未登记地点」时用空地点引用清空全部所涉设备。
	if _, err := st.EditDeviceSitesWithSource(
		[]string{"机器一", "机器二"}, "", WebUISource("192.0.2.9"),
	); err != nil {
		t.Fatalf("清空设备地点失败：%v", err)
	}
	for _, name := range []string{"机器一", "机器二"} {
		if deviceSiteID(t, st, name) != "" {
			t.Fatalf("设备 %s 的地点应已清空", name)
		}
	}
}

// TestEditDeviceSitesWithSourceKeepsAllUnchangedOnFailure 校验任一设备写入
// 失败时整批回滚：地点与审计都不出现部分提交。
func TestEditDeviceSitesWithSourceKeepsAllUnchangedOnFailure(t *testing.T) {
	st := newTestStore(t)
	company := mustAddSite(t, st, "公司", "", "")
	factory := mustAddSite(t, st, "一厂区", "", company.Name)
	for _, name := range []string{"机器一", "机器二"} {
		if _, err := st.AddDeviceAtSite(name, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	mustSetDeviceSite(t, st, "机器一", company.Name)

	if _, err := st.EditDeviceSitesWithSource(
		[]string{"机器一", "不存在的机器"}, factory.Name, WebUISource("192.0.2.9"),
	); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("设备不存在应返回 ErrDeviceNotFound，实际 %v", err)
	}
	if deviceSiteID(t, st, "机器一") != company.ID {
		t.Fatalf("整批失败后机器一的地点应保持不变，实际 %q", deviceSiteID(t, st, "机器一"))
	}
	if deviceSiteID(t, st, "机器二") != "" {
		t.Fatalf("整批失败后机器二不应被写入地点，实际 %q", deviceSiteID(t, st, "机器二"))
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"device.edit"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Source == AuditSourceWebUI {
			t.Fatalf("整批失败后不应出现 Web UI 的机器地点审计：%+v", row)
		}
	}
}
