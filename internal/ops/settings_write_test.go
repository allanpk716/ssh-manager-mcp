package ops

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestSettingValidationAndWrites(t *testing.T) {
	st, _ := newSettingsTestStore(t)
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}
	manager := New(st)

	valid := map[string]string{
		"serve.pairing":            "false",
		"serve.discovery":          "true",
		"serve.metadata_edit":      "false",
		"serve.ui_listen":          "127.0.0.1, 192.0.2.10",
		"serve.ui_port":            "65535",
		"serve.discovery_name":     "家 A",
		"pair.default_profile":     "ops",
		"pair.default_max_offline": "24h",
	}
	for key, value := range valid {
		if err := manager.SetSetting(key, value, CLISource()); err != nil {
			t.Fatalf("写入合法设置 %s=%q 失败：%v", key, value, err)
		}
		got, ok, err := st.GetSetting(key)
		if err != nil || !ok || got != value {
			t.Fatalf("设置 %s 写入后 = (%q,%v,%v)，应为 (%q,true,nil)", key, got, ok, err, value)
		}
	}

	invalid := map[string]string{
		"serve.pairing":            "on",
		"serve.discovery":          "yes",
		"serve.metadata_edit":      "0",
		"serve.ui_listen":          "127.0.0.1,not-an-ip",
		"serve.ui_port":            "65536",
		"serve.discovery_name":     "-name",
		"pair.default_profile":     "ghost",
		"pair.default_max_offline": "30m",
	}
	for key, value := range invalid {
		before, _, err := st.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.SetSetting(key, value, CLISource()); err == nil {
			t.Fatalf("非法设置 %s=%q 应被拒绝", key, value)
		} else if !strings.Contains(err.Error(), key) && !strings.Contains(err.Error(), "设置") {
			t.Fatalf("非法设置 %s 的错误应说明键名：%v", key, err)
		}
		after, _, err := st.GetSetting(key)
		if err != nil || after != before {
			t.Fatalf("非法写入后设置 %s 被改动：before=%q after=%q err=%v", key, before, after, err)
		}
	}

	for key := range valid {
		if err := manager.UnsetSetting(key, CLISource()); err != nil {
			t.Fatalf("删除设置 %s 失败：%v", key, err)
		}
		if _, ok, err := st.GetSetting(key); err != nil || ok {
			t.Fatalf("删除设置 %s 后仍存在：ok=%v err=%v", key, ok, err)
		}
	}

	if err := manager.SetSetting("unknown.key", "value", CLISource()); err == nil ||
		!strings.Contains(err.Error(), "未知设置项") {
		t.Fatalf("未知设置项应被拒绝，错误=%v", err)
	}
	if err := manager.UnsetSetting("unknown.key", CLISource()); err == nil ||
		!strings.Contains(err.Error(), "未知设置项") {
		t.Fatalf("删除未知设置项应被拒绝，错误=%v", err)
	}
}

func TestSettingWritesAreAuditedWithOldAndNewValues(t *testing.T) {
	st, _ := newSettingsTestStore(t)
	manager := New(st)
	if err := manager.SetSetting("serve.pairing", "false", WebUISource("192.0.2.23")); err != nil {
		t.Fatal(err)
	}
	if err := manager.UnsetSetting("serve.pairing", WebUISource("192.0.2.23")); err != nil {
		t.Fatal(err)
	}

	rows, err := st.AuditRows(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("设置写入审计行数 = %d，应为 2：%+v", len(rows), rows)
	}
	unset := rows[0]
	set := rows[1]
	if set.Action != "setting.set" || set.Status != "ok" ||
		!strings.Contains(set.Command, `"key":"serve.pairing"`) ||
		!strings.Contains(set.Command, `"old":"true"`) ||
		!strings.Contains(set.Command, `"new":"false"`) ||
		set.Source != store.AuditSourceWebUI || set.SourceIP != "192.0.2.23" {
		t.Fatalf("写入设置审计行不符：%+v", set)
	}
	if unset.Action != "setting.unset" || unset.Status != "ok" ||
		!strings.Contains(unset.Command, `"key":"serve.pairing"`) ||
		!strings.Contains(unset.Command, `"old":"false"`) ||
		!strings.Contains(unset.Command, `"new":"true"`) {
		t.Fatalf("删除设置审计行不符：%+v", unset)
	}
}
