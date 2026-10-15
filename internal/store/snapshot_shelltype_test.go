package store

// 命令策略·服务器 shell 类型字段(ticket #14)的快照链路测试:字段随
// ExportSnapshot / ExportSnapshotForProfile 落进快照、ImportSnapshot 原样
// 恢复;旧快照(无 shell_type 字段)导入为 POSIX。备份与恢复统一走快照的
// 全量序列化与导入,没有独立表清单——这里测住快照层,备份链路即携带。

import (
	"encoding/json"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestSnapshotShellTypeRoundTrip: 三类标注经 ExportSnapshot → ImportSnapshot
// 往返不丢;裁剪版快照(ExportSnapshotForProfile,cache pull 下发的形态)
// 同样携带。
func TestSnapshotShellTypeRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ids := map[string]models.ShellType{}
	serverIDs := make([]string, 0, 4)
	for name, shell := range map[string]models.ShellType{
		"ps":   models.ShellPowerShell,
		"cmd":  models.ShellWindowsCmd,
		"nix":  models.ShellPOSIX,
		"bare": models.ShellPOSIX, // 未标注(零值)按默认 POSIX
	} {
		srv := &models.Server{Name: name, Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword}
		if name == "nix" {
			srv.ShellType = models.ShellPOSIX // 显式标注 POSIX,与“bare”的零值区分
		} else {
			srv.ShellType = shell
		}
		id, err := st.AddServer(srv)
		if err != nil {
			t.Fatal(err)
		}
		serverIDs = append(serverIDs, id)
		ids[name] = shell
	}
	profID, err := st.AddProfile("team")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profID, serverIDs); err != nil {
		t.Fatal(err)
	}

	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := st.ExportSnapshotForProfile(profID)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range ids {
		var got string
		for _, sv := range snap.Servers {
			if sv.Name == name {
				got = sv.ShellType
			}
		}
		if got != string(want) {
			t.Fatalf("ExportSnapshot: %s ShellType = %q, want %q", name, got, want)
		}
		got = ""
		for _, sv := range scoped.Servers {
			if sv.Name == name {
				got = sv.ShellType
			}
		}
		if got != string(want) {
			t.Fatalf("ExportSnapshotForProfile: %s ShellType = %q, want %q", name, got, want)
		}
	}

	// 恢复到空库:字段原样回来。
	st2 := newTestStore(t)
	if err := st2.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	for name, want := range ids {
		got, err := st2.GetServerByName(name)
		if err != nil || got == nil {
			t.Fatalf("imported %s: %v %v", name, got, err)
		}
		if got.ShellType != want {
			t.Fatalf("imported %s ShellType = %q, want %q", name, got.ShellType, want)
		}
	}
}

// TestImportOldSnapshotWithoutShellTypeIsPOSIX: 旧快照(无 shell_type 字段)
// 导入为 POSIX——与 host_keys pin 列的 v0.14 归一化同理,缺字段的 JSON
// 解码为零值,ImportSnapshot 在落库前归一。
func TestImportOldSnapshotWithoutShellTypeIsPOSIX(t *testing.T) {
	// 手造旧形态 JSON:不带 shell_type 键。
	const oldJSON = `{"version":1,"servers":[
		{"id":"s1","name":"legacy","host":"h","port":22,"user":"u","auth_method":"password"}
	]}`
	var decoded Snapshot
	if err := json.Unmarshal([]byte(oldJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(oldJSON, "shell_type") {
		t.Fatal("fixture must not carry shell_type")
	}
	st := newTestStore(t)
	if err := st.ImportSnapshot(&decoded); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetServerByName("legacy")
	if err != nil || got == nil {
		t.Fatalf("legacy import: %v %v", got, err)
	}
	if got.ShellType != models.ShellPOSIX {
		t.Fatalf("old snapshot imported ShellType = %q, want posix", got.ShellType)
	}
}
