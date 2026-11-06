package cli

// `sshmgr sites` 命令组的行为测试。用例从根命令进入，只观察标准输出、
// 错误结果与重新打开 vault 后的可见状态。

import (
	"strings"
	"testing"
)

// TestSitesLifecycle 覆盖站点的新增、列表、修改与删除：改名后原名字不再
// 可见，备注按部分更新语义只有显式传入时才变化。
func TestSitesLifecycle(t *testing.T) {
	withCliStoreEnv(t)

	out := runCli(t, "sites", "add", "家A", "--note", "家里的物理场所")
	if !strings.Contains(out, "已登记站点 家A") {
		t.Fatalf("add output must confirm the site:\n%s", out)
	}

	errText := runCliErr(t, "sites", "add", "家A")
	if !strings.Contains(errText, "站点名字已存在") {
		t.Fatalf("duplicate site name must be refused with the reason, got:\n%s", errText)
	}

	out = runCli(t, "sites", "list")
	if !strings.Contains(out, "家A") || !strings.Contains(out, "家里的物理场所") {
		t.Fatalf("list must show the site and note:\n%s", out)
	}

	out = runCli(t, "sites", "edit", "家A", "--name", "家B", "--note", "改名后的场所")
	if !strings.Contains(out, "已修改站点 家A") {
		t.Fatalf("edit output must name the original site:\n%s", out)
	}
	out = runCli(t, "sites", "list")
	if strings.Contains(out, "家A") || !strings.Contains(out, "家B") || !strings.Contains(out, "改名后的场所") {
		t.Fatalf("list must show the renamed site and new note:\n%s", out)
	}
	runCli(t, "sites", "edit", "家B", "--note", "")
	out = runCli(t, "sites", "list")
	if strings.Contains(out, "改名后的场所") || !strings.Contains(out, "（无备注）") {
		t.Fatalf("passing an empty note must clear the note:\n%s", out)
	}

	out = runCli(t, "sites", "rm", "家B")
	if !strings.Contains(out, "已删除站点 家B") {
		t.Fatalf("remove output must confirm deletion:\n%s", out)
	}
	out = runCli(t, "sites", "list")
	if strings.Contains(out, "家B") {
		t.Fatalf("removed site must not be listed:\n%s", out)
	}
}
