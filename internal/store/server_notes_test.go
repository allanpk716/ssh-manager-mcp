package store

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// seedNoteServers 建一台设备的两个账号条目，返回两台的服务器标识。
func seedNoteServers(t *testing.T, st *Store) (string, string) {
	t.Helper()
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	web, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := st.AddServer(&models.Server{
		Name: "backup", Host: "192.0.2.10", Port: 22, User: "backup",
		Description: "旧说明", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return web, backup
}

// TestEditServerNotesWritesEachEntryWithAudit 校验多台条目的说明在一个事
// 务内全部写入：说明去掉首尾空白、修订号各加一、每台一条 server.edit 审计
// 与变更同事务落库，摘要格式与单台编辑一致。
func TestEditServerNotesWritesEachEntryWithAudit(t *testing.T) {
	st := newTestStore(t)
	web, backup := seedNoteServers(t, st)

	err := st.EditServerNotes([]ServerNoteEdit{
		{ServerID: web, Description: "  主入口  ", ExpectedRevision: 0},
		{ServerID: backup, Description: "夜里跑备份", ExpectedRevision: 0},
	}, WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatalf("同机账号说明原子保存失败：%v", err)
	}
	webRow, err := st.GetServer(web)
	if err != nil {
		t.Fatal(err)
	}
	backupRow, err := st.GetServer(backup)
	if err != nil {
		t.Fatal(err)
	}
	if webRow.Description != "主入口" || webRow.Revision != 1 {
		t.Fatalf("第一台说明应去空白写入并推进修订号：%+v", webRow)
	}
	if backupRow.Description != "夜里跑备份" || backupRow.Revision != 1 {
		t.Fatalf("第二台说明应写入并推进修订号：%+v", backupRow)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"server.edit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("成功路径每台应有一条 server.edit 审计：%+v", rows)
	}
	byCommand := map[string]AuditRow{}
	for _, row := range rows {
		byCommand[row.Command] = row
	}
	for _, want := range []string{
		`{"changed":["description"],"name":"web"}`,
		`{"changed":["description"],"name":"backup"}`,
	} {
		row, ok := byCommand[want]
		if !ok {
			t.Fatalf("缺少摘要为 %s 的审计行，实际：%+v", want, rows)
		}
		if row.Status != "ok" || row.Source != AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("审计行状态或来源不正确（摘要 %s）：%+v", want, row)
		}
	}
}

// TestEditServerNotesStaleSecondEntryRollsBackAll 校验第二台修订号不符时整
// 批回滚：两台的说明与修订号都保持原状，没有成功审计；冲突错误能指明是第
// 二台条目；事务外每台另写一条失败审计。
func TestEditServerNotesStaleSecondEntryRollsBackAll(t *testing.T) {
	st := newTestStore(t)
	web, backup := seedNoteServers(t, st)
	// 表单打开后，别的入口先改了第二台。
	if err := st.EditServerNotes([]ServerNoteEdit{
		{ServerID: backup, Description: "别的入口写的", ExpectedRevision: 0},
	}, CLISource()); err != nil {
		t.Fatal(err)
	}

	err := st.EditServerNotes([]ServerNoteEdit{
		{ServerID: web, Description: "主入口", ExpectedRevision: 0},
		{ServerID: backup, Description: "夜里跑备份", ExpectedRevision: 0},
	}, WebUISource("192.0.2.7"))
	var stale *ErrStaleRevision
	if !errors.As(err, &stale) {
		t.Fatalf("第二台修订号不符应返回 ErrStaleRevision，实际：%v", err)
	}
	if stale.ServerID != backup || stale.ServerName != "backup" || stale.Current != 1 {
		t.Fatalf("冲突错误应指明第二台条目，实际：%+v", stale)
	}
	webRow, _ := st.GetServer(web)
	if webRow.Description != "" || webRow.Revision != 0 {
		t.Fatalf("整批回滚后第一台应保持原状：%+v", webRow)
	}
	backupRow, _ := st.GetServer(backup)
	if backupRow.Description != "别的入口写的" || backupRow.Revision != 1 {
		t.Fatalf("整批回滚后第二台应保持别的入口写入的值：%+v", backupRow)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"server.edit"}, Source: AuditSourceWebUI})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("冲突后事务外每台应有一条失败审计，实际 %d 条：%+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Status != "error" {
			t.Fatalf("失败审计状态应为 error：%+v", row)
		}
		seen[row.Command] = true
	}
	if !seen[`{"changed":["description"],"name":"web"}`] ||
		!seen[`{"changed":["description"],"name":"backup"}`] {
		t.Fatalf("失败审计应每台一条、摘要不变：%+v", rows)
	}
}

// TestEditServerNotesMidWriteFailureRollsBackAll 用触发器让第二台的更新失
// 败，校验整批回滚且事务外每台一条失败审计。
func TestEditServerNotesMidWriteFailureRollsBackAll(t *testing.T) {
	st := newTestStore(t)
	web, backup := seedNoteServers(t, st)
	if _, err := st.db.Exec(`CREATE TRIGGER block_backup_note_update
BEFORE UPDATE ON servers
WHEN NEW.id = '` + backup + `'
BEGIN
	SELECT RAISE(ABORT, 'note write rejected');
END`); err != nil {
		t.Fatal(err)
	}

	err := st.EditServerNotes([]ServerNoteEdit{
		{ServerID: web, Description: "主入口", ExpectedRevision: 0},
		{ServerID: backup, Description: "夜里跑备份", ExpectedRevision: 0},
	}, WebUISource("192.0.2.7"))
	if err == nil || !strings.Contains(err.Error(), "note write rejected") {
		t.Fatalf("第二台写入失败应返回注入错误：%v", err)
	}
	webRow, _ := st.GetServer(web)
	if webRow.Description != "" || webRow.Revision != 0 {
		t.Fatalf("写入中途失败后第一台也应回滚：%+v", webRow)
	}
	backupRow, _ := st.GetServer(backup)
	if backupRow.Description != "旧说明" || backupRow.Revision != 0 {
		t.Fatalf("写入中途失败后第二台应保持原状：%+v", backupRow)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"server.edit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("失败后事务外每台应有一条失败审计，实际 %d 条：%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Status != "error" {
			t.Fatalf("失败审计状态应为 error：%+v", row)
		}
	}
}
