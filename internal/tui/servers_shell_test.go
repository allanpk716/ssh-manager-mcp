package tui

// 命令策略·服务器 shell 类型字段(ticket #14)的 TUI 面:编辑页字段表的
// Set/Get 往返、prefill/toParts 不丢字段(否则编辑任意其它字段会把标注
// 悄悄重置回默认)、服务器页详情可见。

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestEditFieldsShellRoundTrip: shell 字段 Set/Get 往返,三类值都行。
func TestEditFieldsShellRoundTrip(t *testing.T) {
	f := fieldByKey(t, editFields(true), "shell")
	d := &serverDraft{}
	for _, v := range []string{"posix", "powershell", "cmd"} {
		f.Set(d, v)
		if got := f.Get(d); got != v {
			t.Fatalf("shell round-trip: Get = %q, want %q", got, v)
		}
	}
}

// TestPrefillToPartsPreserveShellType: 与 ExposeHost 同类的「悄悄重置」守卫
// ——updateServerTx 写全行、toParts 新建 models.Server,两个拷贝点都必须
// 携带 shell 类型,否则编辑任意其它字段会把 PowerShell 标注重置回默认。
func TestPrefillToPartsPreserveShellType(t *testing.T) {
	cur := &models.Server{
		Name: "n", Host: "h", Port: 22, User: "u",
		Description: "d", ShellType: models.ShellPowerShell,
	}
	d := prefill(cur)
	if d.Shell != "powershell" {
		t.Fatalf("prefill dropped ShellType: %q", d.Shell)
	}
	srv, _, _, err := d.toParts()
	if err != nil {
		t.Fatal(err)
	}
	if srv.ShellType != models.ShellPowerShell {
		t.Fatalf("toParts dropped ShellType: %q", srv.ShellType)
	}
	// 空草稿(新增态未碰 shell 字段)提交为默认 POSIX。
	bare, _, _, err := (&serverDraft{}).toParts()
	if err != nil {
		t.Fatal(err)
	}
	if bare.ShellType != models.ShellPOSIX {
		t.Fatalf("bare draft ShellType = %q, want posix", bare.ShellType)
	}
	// 编辑字段表往返:经 Set 改值再改回。
	f := fieldByKey(t, editFields(true), "shell")
	f.Set(d, "cmd")
	if d.Shell != "cmd" {
		t.Fatalf("Set(cmd) = %q", d.Shell)
	}
	f.Set(d, "powershell")
	if d.Shell != "powershell" {
		t.Fatalf("Set(powershell) = %q", d.Shell)
	}
}

// TestServersPageDetailShowsShellType: 服务器页详情(TUI 的查看面)可见
// shell 类型;未标注显示默认 POSIX。
func TestServersPageDetailShowsShellType(t *testing.T) {
	sp := newServersPage([]*models.Server{
		{Name: "ps", Host: "10.0.0.9", User: "u", Port: 22, CredentialID: "c", Role: "r", ShellType: models.ShellPowerShell},
		{Name: "nix", Host: "10.0.0.10", User: "u", Port: 22, CredentialID: "c", Role: "r"},
	})
	d := sp.Detail()
	if !strings.Contains(d, "shell=powershell") {
		t.Fatalf("detail missing shell=powershell:\n%s", d)
	}
	// 第二台(零值)也应显示默认 posix:把光标移过去再看。
	sp.list.Select(1)
	d2 := sp.Detail()
	if !strings.Contains(d2, "shell=posix") {
		t.Fatalf("detail missing shell=posix:\n%s", d2)
	}
}
