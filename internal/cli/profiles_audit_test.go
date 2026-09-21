package cli

// profiles 三命令(owner 变更面)的审计行测试:add / grant / remove 各自在
// vault 审计表留一行,动作名 profile.add / profile.grant / profile.rm,摘要
// 逐字钉在白名单上——profile.add / profile.rm 只记 name,profile.grant 记
// profile 与请求的条目名列表(计数:名1,名2,命令行顺序);失败路径(名称
// 重复、轮廓不存在、未知条目导致 grant 中止)也留行,状态 error;只读的
// profiles ls 不留行。

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// newProfilesAuditEnv 把 SSHMGR_STORE 钉到一个全新临时 vault(servers_audit_test.go
// 同形:SSHMGR_FILEKEY_PATH 指向不存在的文件,开发机的真 master.key 永远不会
// 被捡到),并返回 rowsFor:重开 vault 读回若干动作名的全部 owner 审计行
// (最新在前)。
func newProfilesAuditEnv(t *testing.T) (dbPath string, mk []byte, rowsFor func(t *testing.T, actions ...string) []store.AuditRow) {
	t.Helper()
	dir := t.TempDir()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(key),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	rowsFor = func(t *testing.T, actions ...string) []store.AuditRow {
		t.Helper()
		st, err := store.Open(dbPath, key)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: actions})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	return dbPath, key, rowsFor
}

// assertProfileAuditRow 钉住一行 owner 审计行的形状:动作名、状态、摘要字
// 符串逐字相等(白名单外零字段)、不挂项目/条目、ExitCode/DurationMS 取
// owner 行惯例零值。
func assertProfileAuditRow(t *testing.T, r store.AuditRow, wantAction, wantStatus, wantCommand string) {
	t.Helper()
	if r.Action != wantAction {
		t.Fatalf("action = %q, want %q", r.Action, wantAction)
	}
	if r.Status != wantStatus {
		t.Fatalf("%s: status = %q, want %q", wantAction, r.Status, wantStatus)
	}
	if r.ProjectID != "" || r.ServerID != "" || r.Sudo {
		t.Fatalf("%s: owner row must carry no project/server/sudo, got %+v", wantAction, r)
	}
	if r.Command != wantCommand {
		t.Fatalf("%s: command summary = %s, want %s (whitelist: no extra fields)", wantAction, r.Command, wantCommand)
	}
	if r.ExitCode != 0 || r.DurationMS != 0 {
		t.Fatalf("%s: owner rows keep ExitCode/DurationMS at zero, got %d/%d", wantAction, r.ExitCode, r.DurationMS)
	}
}

// TestProfilesAudit_AddGrantRemoveRows:三命令成功各留一行,摘要与白名单逐字
// 一致;只读的 profiles ls 不加行。
func TestProfilesAudit_AddGrantRemoveRows(t *testing.T) {
	_, _, rowsFor := newProfilesAuditEnv(t)

	runCli(t, "profiles", "add", "team-a")
	runCli(t, "servers", "add", "--name", "web1", "--host", "192.0.2.10", "--user", "u")
	runCli(t, "servers", "add", "--name", "web2", "--host", "192.0.2.11", "--user", "u")
	runCli(t, "profiles", "grant", "team-a", "web1", "web2")
	runCli(t, "profiles", "remove", "team-a")

	rows := rowsFor(t, "profile.add", "profile.grant", "profile.rm")
	if len(rows) != 3 {
		t.Fatalf("add+grant+remove must leave exactly three owner rows, got %d: %+v", len(rows), rows)
	}
	// 最新在前,即执行顺序的倒序。
	assertProfileAuditRow(t, rows[0], "profile.rm", "ok", `{"name":"team-a"}`)
	assertProfileAuditRow(t, rows[1], "profile.grant", "ok", `{"profile":"team-a","servers":"2台:web1,web2"}`)
	assertProfileAuditRow(t, rows[2], "profile.add", "ok", `{"name":"team-a"}`)

	// 只读命令不留行:ls 前后行数不变。
	runCli(t, "profiles", "ls")
	if n := len(rowsFor(t, "profile.add", "profile.grant", "profile.rm")); n != 3 {
		t.Fatalf("read-only profiles ls must leave no audit row, still want 3 rows, got %d", n)
	}
}

// TestProfilesAudit_ErrorRows:名称重复的 add、轮廓不存在的 remove、轮廓不
// 存在 / 未知条目中止的 grant 都留 Status=error 行,摘要按命令行白名单能取
// 多少取多少(未知名也照记)。
func TestProfilesAudit_ErrorRows(t *testing.T) {
	_, _, rowsFor := newProfilesAuditEnv(t)

	runCli(t, "profiles", "add", "team-a")
	runCliErr(t, "profiles", "add", "team-a")                             // 名称重复
	runCliErr(t, "profiles", "remove", "ghost")                           // 轮廓不存在
	runCliErr(t, "profiles", "grant", "ghost", "web1")                    // 轮廓不存在
	runCliErr(t, "profiles", "grant", "team-a", "no-such-server")         // 条目不存在,grant 中止
	runCliErr(t, "profiles", "grant", "team-a", "web1", "no-such-server") // 已知+未知混合,整体中止

	rows := rowsFor(t, "profile.add", "profile.grant", "profile.rm")
	if len(rows) != 6 {
		t.Fatalf("want 1 ok add row + 5 error rows, got %d: %+v", len(rows), rows)
	}
	// 最新在前,即执行顺序的倒序。
	assertProfileAuditRow(t, rows[0], "profile.grant", "error", `{"profile":"team-a","servers":"2台:web1,no-such-server"}`)
	assertProfileAuditRow(t, rows[1], "profile.grant", "error", `{"profile":"team-a","servers":"1台:no-such-server"}`)
	assertProfileAuditRow(t, rows[2], "profile.grant", "error", `{"profile":"ghost","servers":"1台:web1"}`)
	assertProfileAuditRow(t, rows[3], "profile.rm", "error", `{"name":"ghost"}`)
	assertProfileAuditRow(t, rows[4], "profile.add", "error", `{"name":"team-a"}`)
	assertProfileAuditRow(t, rows[5], "profile.add", "ok", `{"name":"team-a"}`)
}
