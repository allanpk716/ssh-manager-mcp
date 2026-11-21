package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ServerNoteEdit 是一次同机账号说明保存里的一台条目：Description 是显示说
// 明的最终值（首尾空白在这里去除），ExpectedRevision 是表单打开时的修订
// 号，写入按它做条件更新。
type ServerNoteEdit struct {
	ServerID         string
	Description      string
	ExpectedRevision int64
}

// EditServerNotes 在一个事务内按修订号编辑多台服务器条目的显示说明：逐台
// 读行、比对修订号，WHERE 条件同时比对修订号做更新，每台一条 server.edit
// 审计与变更在同一事务写入。任一台修订号不符或任一步失败，整批回滚，并
// 在事务外为每台另写一条失败审计。冲突错误是带 ServerID 与 ServerName 的
// ErrStaleRevision；Error() 文字与单台路径一致，409 响应不受影响。
func (s *Store) EditServerNotes(edits []ServerNoteEdit, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := s.editServerNotesTx(edits, source); err != nil {
		s.writeServerNotesFailureAudit(edits, source)
		return err
	}
	return nil
}

// editServerNotesTx 是 EditServerNotes 的事务体，事务形状照
// ReplaceDeviceAddressList：defer 的回滚在提交后是空操作，任一步失败都整
// 批回滚。
func (s *Store) editServerNotesTx(edits []ServerNoteEdit, source WriteSource) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, edit := range edits {
		srv, err := getServerTx(tx, edit.ServerID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("server id %q not found", edit.ServerID)
			}
			return err
		}
		if srv.Revision != edit.ExpectedRevision {
			return &ErrStaleRevision{
				Current: srv.Revision, ServerID: srv.ID, ServerName: srv.Name,
			}
		}
		note := strings.TrimSpace(edit.Description)
		srv.Description = note
		if err := validateServerText(srv); err != nil {
			return err
		}
		result, err := tx.Exec(
			`UPDATE servers SET description=?,updated_at=?,revision=revision+1 WHERE id=? AND revision=?`,
			note, now(), srv.ID, srv.Revision,
		)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			currentRevision := int64(-1)
			if err := tx.QueryRow(`SELECT revision FROM servers WHERE id=?`, srv.ID).
				Scan(&currentRevision); err != nil {
				return err
			}
			return &ErrStaleRevision{
				Current: currentRevision, ServerID: srv.ID, ServerName: srv.Name,
			}
		}
		audit := OwnerAuditRowAt(s.nowTime(), "server.edit", map[string]any{
			"name": srv.Name, "changed": []string{"description"},
		})
		audit.Source, audit.SourceIP = source.kind, source.address
		if err := writeAuditTx(tx, audit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// writeServerNotesFailureAudit 在整批回滚后为每台条目补一条失败的
// server.edit 审计（事务外，形状与服务器管理模块的失败审计一致：状态为
// error、摘要只含条目名与字段名）。行已不存在的台用服务器标识兜底；审计
// 写失败不覆盖原始错误。
func (s *Store) writeServerNotesFailureAudit(edits []ServerNoteEdit, source WriteSource) {
	for _, edit := range edits {
		name := edit.ServerID
		if srv, err := s.GetServer(edit.ServerID); err == nil && srv != nil {
			name = srv.Name
		}
		row := OwnerAuditRow("server.edit", map[string]any{
			"name": name, "changed": []string{"description"},
		})
		row.Source, row.SourceIP = source.kind, source.address
		row.Status = "error"
		_ = s.WriteAudit(row)
	}
}
