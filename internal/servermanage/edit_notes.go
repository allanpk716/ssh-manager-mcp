package servermanage

import (
	"errors"

	"ssh-manager-mcp/internal/store"
)

// AccountNoteEdit 是一次同机账号说明保存里的一台条目：Note 是显示说明的最
// 终值，Revision 是表单打开时的修订号。
type AccountNoteEdit struct {
	ServerID string
	Note     string
	Revision int64
}

// EditAccountNotes 在一个事务内按修订号保存多台条目的显示说明：任一台修
// 订号不符或任一步失败，整批回滚（写入与审计由存储层同事务完成，失败审计
// 由存储层在事务外补写）。冲突使用服务器编辑的现有冲突类型，Current 指明
// 被别人改过的那一台。
func (m *Manager) EditAccountNotes(edits []AccountNoteEdit, src Source) error {
	request := make([]store.ServerNoteEdit, 0, len(edits))
	for _, edit := range edits {
		request = append(request, store.ServerNoteEdit{
			ServerID: edit.ServerID, Description: edit.Note, ExpectedRevision: edit.Revision,
		})
	}
	err := m.st.EditServerNotes(request, storeWriteSource(src))
	if err == nil {
		return nil
	}
	var stale *store.ErrStaleRevision
	if errors.As(err, &stale) && stale.ServerID != "" {
		current, currentErr := m.st.GetServer(stale.ServerID)
		if currentErr != nil {
			return currentErr
		}
		return &RevisionConflict{Current: current}
	}
	return err
}
