package store

import (
	"fmt"
	"strings"
)

// ClaimSoftwareDeclaration 把一条生效声明认领到同一设备的实测条目。认领
// 引用实测条目的稳定键；后续整体替换实测结果时，这个关系继续保留。
func (s *Store) ClaimSoftwareDeclaration(
	declarationID, observedStableKey string,
	source WriteSource,
) (*SoftwareDeclarationView, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	declarationID = strings.TrimSpace(declarationID)
	observedStableKey = strings.TrimSpace(observedStableKey)
	if observedStableKey == "" {
		return nil, fmt.Errorf("实测条目的稳定键不能为空")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	declaration, err := softwareDeclarationByIDTx(tx, declarationID)
	if err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM software_observed_items
		WHERE device_id=? AND stable_key=?`, declaration.DeviceID, observedStableKey,
	).Scan(&count); err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("设备 %q 上没有稳定键为 %q 的实测条目", declaration.DeviceName, observedStableKey)
	}
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM software_declarations
		WHERE device_id=? AND claimed_observed_key=? AND status='active' AND id<>?`,
		declaration.DeviceID, observedStableKey, declaration.ID,
	).Scan(&count); err != nil {
		return nil, err
	}
	if count != 0 {
		return nil, fmt.Errorf("这个实测条目已经被另一条声明认领")
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		UPDATE software_declarations
		SET claimed_observed_key=?,last_source=?,updated_at=? WHERE id=?`,
		observedStableKey, source.kind, at.Unix(), declaration.ID,
	); err != nil {
		return nil, err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-declaration-claim", map[string]any{
		"device": declaration.DeviceName, "category": declaration.Category.Label(),
		"name": declaration.Name, "observed_key": observedStableKey,
	}, source); err != nil {
		return nil, err
	}
	after, err := softwareDeclarationByIDTx(tx, declaration.ID)
	if err != nil {
		return nil, err
	}
	return after, tx.Commit()
}

// UnclaimSoftwareDeclaration 取消一条声明的认领关系，并写审计。实测条目
// 本身不被删除，下一次查询会重新按「实测」来源显示。
func (s *Store) UnclaimSoftwareDeclaration(
	declarationID string,
	source WriteSource,
) (*SoftwareDeclarationView, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	declaration, err := softwareDeclarationByIDTx(tx, strings.TrimSpace(declarationID))
	if err != nil {
		return nil, err
	}
	if declaration.ClaimedObservedKey == "" {
		return nil, fmt.Errorf("软件声明 %q 没有认领实测条目", declaration.ID)
	}
	previousKey := declaration.ClaimedObservedKey
	at := s.nowTime()
	if _, err := tx.Exec(`
		UPDATE software_declarations
		SET claimed_observed_key='',last_source=?,updated_at=? WHERE id=?`,
		source.kind, at.Unix(), declaration.ID,
	); err != nil {
		return nil, err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-declaration-unclaim", map[string]any{
		"device": declaration.DeviceName, "category": declaration.Category.Label(),
		"name": declaration.Name, "observed_key": previousKey,
	}, source); err != nil {
		return nil, err
	}
	after, err := softwareDeclarationByIDTx(tx, declaration.ID)
	if err != nil {
		return nil, err
	}
	return after, tx.Commit()
}
