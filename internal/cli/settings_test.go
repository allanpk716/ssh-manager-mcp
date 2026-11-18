package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func TestSettingsCommandsListGetSetUnset(t *testing.T) {
	path, key := withCliStoreEnv(t)
	st, err := store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	list := runCli(t, "settings", "ls")
	for _, want := range []string{
		"serve.pairing", "serve.discovery", "serve.metadata_edit", "serve.ui_listen",
		"serve.ui_port", "serve.discovery_name", "pair.default_profile",
		"pair.default_max_offline", "serve.public_addresses", "默认值", "取值来源", "生效时机",
	} {
		if !strings.Contains(list, want) {
			t.Fatalf("settings ls 缺少 %q：\n%s", want, list)
		}
	}

	got := runCli(t, "settings", "get", "serve.pairing")
	if !strings.Contains(got, "serve.pairing") || !strings.Contains(got, "true") ||
		!strings.Contains(got, "默认值") {
		t.Fatalf("settings get 输出不完整：\n%s", got)
	}

	set := runCli(t, "settings", "set", "serve.pairing", "false")
	if !strings.Contains(set, "已保存") {
		t.Fatalf("settings set 应输出保存结果：\n%s", set)
	}
	st, err = store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	value, ok, err := st.GetSetting("serve.pairing")
	if err != nil || !ok || value != "false" {
		t.Fatalf("settings set 后设置存储值 = (%q,%v,%v)，应为 false", value, ok, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	publicSet := runCli(t, "settings", "set", "serve.public_addresses", "Example.com, [2001:db8::1]:18178")
	if !strings.Contains(publicSet, "已保存") {
		t.Fatalf("settings set 对外地址应保存：%s", publicSet)
	}
	publicGet := runCli(t, "settings", "get", "serve.public_addresses")
	if !strings.Contains(publicGet, "example.com,[2001:db8::1]:18178") {
		t.Fatalf("settings get 应显示规范化后的对外地址：%s", publicGet)
	}
	publicProblem := runCliErr(t, "settings", "set", "serve.public_addresses", "example.com/path")
	if !strings.Contains(publicProblem, "serve.public_addresses") {
		t.Fatalf("非法对外地址应被拒绝：%s", publicProblem)
	}

	unset := runCli(t, "settings", "unset", "serve.pairing")
	if !strings.Contains(unset, "已恢复默认值") {
		t.Fatalf("settings unset 应输出恢复结果：\n%s", unset)
	}
	st, err = store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetSetting("serve.pairing"); err != nil || ok {
		t.Fatalf("settings unset 后设置仍存在：ok=%v err=%v", ok, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	problem := runCliErr(t, "settings", "set", "unknown.key", "value")
	if !strings.Contains(problem, "未知设置项") {
		t.Fatalf("settings set 应拒绝未知键：%s", problem)
	}
}

func TestRoleShowDisplaysRoleFile(t *testing.T) {
	path, _ := withCliStoreEnv(t)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	out := runCli(t, "role", "show")
	for _, want := range []string{"standalone", "已完成", filepath.Join(filepath.Dir(path), "role.json")} {
		if !strings.Contains(out, want) {
			t.Fatalf("role show 缺少 %q：\n%s", want, out)
		}
	}
}
