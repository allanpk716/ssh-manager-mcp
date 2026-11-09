package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// AddSoftwareDeclarationDrafts 一次提交多条声明草稿，按输入顺序返回每条
// 草稿编号。草稿行与审计行在同一个事务里提交；任何一条不合法或设备不
// 在可见范围内时，整批拒绝，不留下部分草稿。来源是受审计转发写时，审
// 计摘要会记录设备码编号。
func (s *Store) AddSoftwareDeclarationDrafts(
	drafts []models.SoftwareDeclarationDraft,
	scope SoftwareDeviceScope,
	source WriteSource,
) ([]string, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if len(drafts) == 0 {
		return nil, fmt.Errorf("至少要提交一条声明草稿")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	at := s.nowTime()
	ids := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		draft.Device = strings.TrimSpace(draft.Device)
		draft.Name = strings.TrimSpace(draft.Name)
		draft.ClaimedObservedKey = strings.TrimSpace(draft.ClaimedObservedKey)
		if err := models.ValidateSoftwareDeclaration(draft.SoftwareDeclaration); err != nil {
			return nil, err
		}
		device, err := deviceByNameTx(tx, draft.Device)
		if err != nil {
			return nil, err
		}
		if err := ensureSoftwareDeviceVisibleTx(tx, device.ID, scope); err != nil {
			return nil, err
		}
		if draft.ClaimedObservedKey != "" {
			if err := ensureSoftwareObservedKeyTx(tx, device, draft.ClaimedObservedKey); err != nil {
				return nil, err
			}
		}
		id := newID()
		if err := insertSoftwareDeclarationDraftTx(tx, id, device.ID, draft, at, source); err != nil {
			return nil, err
		}
		summary := map[string]any{
			"device": device.Name, "category": draft.Category.Label(), "name": draft.Name,
		}
		if draft.ClaimedObservedKey != "" {
			summary["observed_key"] = draft.ClaimedObservedKey
		}
		if err := writeSoftwareAuditTx(tx, at, "software-declaration-draft", summary, source); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, tx.Commit()
}

// SoftwareDeclarationDrafts 列出可见范围内的声明草稿。草稿按设备名、类
// 别、名称与版本稳定排序。
func (s *Store) SoftwareDeclarationDrafts(
	scope SoftwareDeviceScope,
) ([]SoftwareDeclarationView, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if scope.all {
		return softwareDeclarationDraftsWhereTx(tx, "1=1")
	}
	deviceIDs, err := visibleSoftwareDeviceIDsTx(tx, scope)
	if err != nil {
		return nil, err
	}
	if len(deviceIDs) == 0 {
		return []SoftwareDeclarationView{}, tx.Commit()
	}
	return softwareDeclarationDraftsWhereTx(tx, `declaration.device_id IN (`+
		placeholdersForIDs(mapFromSlice(deviceIDs))+`)`, idsToAny(deviceIDs)...)
}

// ConfirmSoftwareDeclarationDraft 确认一条声明草稿。edit 为空表示按草稿
// 原样确认；非空表示 owner 修改后确认，修改字段与审计在同一个事务里落
// 库。认领了实测条目的草稿在确认时执行与认领命令相同的核对：实测条目
// 必须仍在同一设备上，且没有另一条生效声明认领它。
func (s *Store) ConfirmSoftwareDeclarationDraft(
	draftID string,
	edit *models.SoftwareDeclarationEdit,
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
	before, err := softwareDeclarationDraftByIDTx(tx, strings.TrimSpace(draftID))
	if err != nil {
		return nil, err
	}
	desired, changed := mergeSoftwareDeclarationEdit(*before, edit)
	if err := models.ValidateSoftwareDeclaration(desired); err != nil {
		return nil, err
	}
	targetDevice, err := deviceByNameTx(tx, desired.Device)
	if err != nil {
		return nil, err
	}
	claimKey := before.ClaimedObservedKey
	if claimKey != "" {
		if err := ensureSoftwareObservedKeyTx(tx, targetDevice, claimKey); err != nil {
			return nil, err
		}
		var count int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM software_declarations
			WHERE device_id=? AND claimed_observed_key=? AND status='active' AND id<>?`,
			targetDevice.ID, claimKey, before.ID,
		).Scan(&count); err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, fmt.Errorf("这个实测条目已经被另一条声明认领")
		}
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		UPDATE software_declarations
		SET device_id=?,category=?,name=?,name_lower=?,version=?,package_url=?,cpe=?,
		    belongs_to=?,note=?,claimed_observed_key=?,status='active',confirmed_at=?,
		    last_source=?,updated_at=?
		WHERE id=? AND status='draft'`,
		targetDevice.ID, desired.Category, desired.Name, strings.ToLower(desired.Name),
		desired.Version, desired.PackageURL, desired.CPE, desired.BelongsTo, desired.Note,
		claimKey, at.Unix(), source.kind, at.Unix(), before.ID,
	); err != nil {
		return nil, err
	}
	if targetDevice.ID != before.DeviceID {
		if _, err := tx.Exec(`
			UPDATE software_declaration_ports SET device_id=?,updated_at=? WHERE declaration_id=?`,
			targetDevice.ID, at.Unix(), before.ID,
		); err != nil {
			return nil, err
		}
	}
	if edit != nil && edit.ListeningPorts != nil {
		if err := replaceSoftwarePortsTx(
			tx, before.ID, targetDevice.ID, desired.ListeningPorts, before.CreatedAt, at,
		); err != nil {
			return nil, err
		}
	}
	action := "software-declaration-confirm"
	summary := map[string]any{
		"device": targetDevice.Name, "category": desired.Category.Label(),
		"name": desired.Name,
	}
	if len(changed) > 0 {
		action = "software-declaration-confirm-edit"
		summary["fields"] = changed
	}
	if claimKey != "" {
		summary["observed_key"] = claimKey
	}
	if err := writeSoftwareAuditTx(tx, at, action, summary, source); err != nil {
		return nil, err
	}
	after, err := softwareDeclarationByIDTx(tx, before.ID)
	if err != nil {
		return nil, err
	}
	return after, tx.Commit()
}

// RejectSoftwareDeclarationDraft 驳回一条声明草稿：草稿行与监听端口一起
// 删除，不会成为生效声明。
func (s *Store) RejectSoftwareDeclarationDraft(
	draftID string,
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
	before, err := softwareDeclarationDraftByIDTx(tx, strings.TrimSpace(draftID))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM software_declarations WHERE id=?`, before.ID); err != nil {
		return nil, err
	}
	if err := writeSoftwareAuditTx(tx, s.nowTime(), "software-declaration-reject", map[string]any{
		"device": before.DeviceName, "category": before.Category.Label(),
		"name": before.Name,
	}, source); err != nil {
		return nil, err
	}
	return before, tx.Commit()
}

// insertSoftwareDeclarationDraftTx 写入一条草稿行。草稿与生效声明共用同
// 一张表，靠 status 列区分；确认沿用与命令行生效写相同的表结构。
func insertSoftwareDeclarationDraftTx(
	tx *sql.Tx,
	id, deviceID string,
	draft models.SoftwareDeclarationDraft,
	at time.Time,
	source WriteSource,
) error {
	if _, err := tx.Exec(`
		INSERT INTO software_declarations(
			id,device_id,category,name,name_lower,version,package_url,cpe,belongs_to,note,
			claimed_observed_key,status,confirmed_at,created_source,last_source,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, deviceID, draft.Category, draft.Name, strings.ToLower(draft.Name),
		draft.Version, draft.PackageURL, draft.CPE, draft.BelongsTo, draft.Note,
		draft.ClaimedObservedKey, "draft", 0, source.kind, source.kind,
		at.Unix(), at.Unix(),
	); err != nil {
		return err
	}
	return replaceSoftwarePortsTx(tx, id, deviceID, draft.ListeningPorts, at, at)
}

// mergeSoftwareDeclarationEdit 把草稿与 owner 的修改合并成确认后的声明
// 形态，并返回按字母排序的被修改字段名。
func mergeSoftwareDeclarationEdit(
	before SoftwareDeclarationView,
	edit *models.SoftwareDeclarationEdit,
) (models.SoftwareDeclaration, []string) {
	desired := models.SoftwareDeclaration{
		Device: before.DeviceName, Category: before.Category, Name: before.Name,
		Version: before.Version, PackageURL: before.PackageURL, CPE: before.CPE,
		ListeningPorts: before.ListeningPorts, BelongsTo: before.BelongsTo, Note: before.Note,
	}
	if edit == nil {
		if desired.ListeningPorts == nil {
			desired.ListeningPorts = []models.SoftwareListeningPort{}
		}
		return desired, nil
	}
	changed := map[string]bool{}
	if edit.Device != nil {
		desired.Device = strings.TrimSpace(*edit.Device)
		changed["device"] = true
	}
	if edit.Category != nil {
		desired.Category = *edit.Category
		changed["category"] = true
	}
	if edit.Name != nil {
		desired.Name = strings.TrimSpace(*edit.Name)
		changed["name"] = true
	}
	if edit.Version != nil {
		desired.Version = *edit.Version
		changed["version"] = true
	}
	if edit.PackageURL != nil {
		desired.PackageURL = *edit.PackageURL
		changed["purl"] = true
	}
	if edit.CPE != nil {
		desired.CPE = *edit.CPE
		changed["cpe"] = true
	}
	if edit.ListeningPorts != nil {
		desired.ListeningPorts = *edit.ListeningPorts
		changed["ports"] = true
	}
	if edit.BelongsTo != nil {
		desired.BelongsTo = *edit.BelongsTo
		changed["belongs_to"] = true
	}
	if edit.Note != nil {
		desired.Note = *edit.Note
		changed["note"] = true
	}
	keys := make([]string, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return desired, keys
}

// softwareDeclarationDraftsWhereTx 按条件读取草稿行。
func softwareDeclarationDraftsWhereTx(tx *sql.Tx, where string, args ...any) ([]SoftwareDeclarationView, error) {
	rows, err := tx.Query(`
		SELECT declaration.id,declaration.device_id,device.name,declaration.category,declaration.name,
		       declaration.version,declaration.package_url,declaration.cpe,declaration.belongs_to,declaration.note,
		       declaration.claimed_observed_key,declaration.status,declaration.created_at,declaration.updated_at
		FROM software_declarations declaration
		JOIN devices device ON device.id=declaration.device_id
		WHERE declaration.status='draft' AND `+where+`
		ORDER BY device.name,declaration.name,declaration.version,declaration.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SoftwareDeclarationView, 0)
	for rows.Next() {
		var view SoftwareDeclarationView
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&view.ID, &view.DeviceID, &view.DeviceName, &view.Category, &view.Name,
			&view.Version, &view.PackageURL, &view.CPE, &view.BelongsTo, &view.Note,
			&view.ClaimedObservedKey, &view.Status, &createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		view.CreatedAt, view.UpdatedAt = time.Unix(createdAt, 0), time.Unix(updatedAt, 0)
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := attachSoftwarePortsTx(tx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// softwareDeclarationDraftByIDTx 按编号读取一条草稿；编号不存在或不是草
// 稿时返回错误。
func softwareDeclarationDraftByIDTx(tx *sql.Tx, id string) (*SoftwareDeclarationView, error) {
	rows, err := softwareDeclarationDraftsWhereTx(tx, `declaration.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("声明草稿 %q 不存在", id)
	}
	return &rows[0], nil
}

// ensureSoftwareObservedKeyTx 核对一台设备上存在给定稳定键的实测条目。
func ensureSoftwareObservedKeyTx(tx *sql.Tx, device *models.Device, stableKey string) error {
	var count int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM software_observed_items
		WHERE device_id=? AND stable_key=?`, device.ID, stableKey,
	).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("设备 %q 上没有稳定键为 %q 的实测条目", device.Name, stableKey)
	}
	return nil
}

// mapFromSlice 把字符串切片转成集合，供占位符构造使用。
func mapFromSlice(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// idsToAny 把字符串切片转换为 SQL 参数切片。
func idsToAny(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}
