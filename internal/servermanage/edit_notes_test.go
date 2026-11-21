package servermanage

import (
	"errors"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// seedAccountNotes 建一台设备的两个账号条目，返回两台的服务器标识。
func seedAccountNotes(t *testing.T, manager *Manager) (string, string) {
	t.Helper()
	device, err := manager.st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	web, err := manager.st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := manager.st.AddServer(&models.Server{
		Name: "backup", Host: "192.0.2.10", Port: 22, User: "backup", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return web, backup
}

// TestManagerEditAccountNotesSavesAllOrNothing 校验多台条目的说明一次保存
// 要么全部写入要么一台都不改，冲突时 Current 指明被别人改过的那一台。
func TestManagerEditAccountNotesSavesAllOrNothing(t *testing.T) {
	manager := newTestManager(t)
	web, backup := seedAccountNotes(t, manager)

	if err := manager.EditAccountNotes([]AccountNoteEdit{
		{ServerID: web, Note: "主入口", Revision: 0},
		{ServerID: backup, Note: "夜里跑备份", Revision: 0},
	}, WebUISource("192.0.2.7")); err != nil {
		t.Fatalf("同机账号说明整批保存失败：%v", err)
	}
	webRow, _ := manager.st.GetServer(web)
	backupRow, _ := manager.st.GetServer(backup)
	if webRow.Description != "主入口" || backupRow.Description != "夜里跑备份" {
		t.Fatalf("两台说明都应写入：%+v %+v", webRow, backupRow)
	}

	// 第二台已被别的入口改过（修订号推进到 2），整批提交必须全部回滚。
	if err := manager.EditAccountNotes([]AccountNoteEdit{
		{ServerID: backup, Note: "再次更新", Revision: 1},
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	conflictErr := manager.EditAccountNotes([]AccountNoteEdit{
		{ServerID: web, Note: "改不掉的说明", Revision: 1},
		{ServerID: backup, Note: "改不掉的说明", Revision: 1},
	}, CLISource())
	var conflict *RevisionConflict
	if !errors.As(conflictErr, &conflict) {
		t.Fatalf("任一台修订号不符应返回修订号冲突，实际：%v", conflictErr)
	}
	if conflict.Current == nil || conflict.Current.ID != backup || conflict.Current.Revision != 2 {
		t.Fatalf("冲突应指明第二台条目及其当前修订号：%+v", conflict.Current)
	}
	webRow, _ = manager.st.GetServer(web)
	backupRow, _ = manager.st.GetServer(backup)
	if webRow.Description != "主入口" || backupRow.Description != "再次更新" {
		t.Fatalf("冲突后两台都应保持原状：%+v %+v", webRow, backupRow)
	}
}
