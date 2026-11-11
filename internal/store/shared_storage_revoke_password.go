package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// RevokeSharedStorageMountAndPassword 在同一个事务里更换共享存储挂载密码、
// 把服务器条目移出可写入列表，并写三类审计。密码换值与移出列表都会触发
// 共享存储版本号加一；事务末尾把这一次合并变更收敛为只加一。
func (s *Store) RevokeSharedStorageMountAndPassword(
	shareID, serverID string, unmount, force bool, newPassword []byte, source WriteSource,
) (*models.SharedStorage, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if err := validateSecretValue(newPassword); err != nil {
		return nil, err
	}
	blob, err := seal(s.masterKey, newPassword)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var secretID, secretName, serverName string
	err = tx.QueryRow(`
		SELECT share.password_secret_id, secret.name, server.name
		FROM shared_storages share
		JOIN secrets secret ON secret.id=share.password_secret_id
		JOIN servers server ON server.id=?
		WHERE share.id=?`, serverID, shareID,
	).Scan(&secretID, &secretName, &serverName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSharedStorageNotFound
	}
	if err != nil {
		return nil, err
	}
	var one int
	err = tx.QueryRow(`
		SELECT 1 FROM secret_server_allow WHERE secret_id=? AND server_id=?`,
		secretID, serverID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("服务器条目 %s 不在共享存储的可挂载范围内", serverName)
	}
	if err != nil {
		return nil, err
	}
	before, err := secretAllowServerNames(tx, secretID)
	if err != nil {
		return nil, err
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		UPDATE secrets
		   SET previous_value_blob=value_blob, value_blob=?, value_version=value_version+1,
		       value_updated_at=?
		 WHERE id=?`, blob, at.Unix(), secretID,
	); err != nil {
		return nil, err
	}
	result, err := tx.Exec(
		`DELETE FROM secret_server_allow WHERE secret_id=? AND server_id=?`,
		secretID, serverID,
	)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, fmt.Errorf("共享存储撤销没有移除服务器条目 %s", serverName)
	}
	if _, err := tx.Exec(`
		UPDATE shared_storages
		   SET version=version-1, change_note='', updated_at=?
		 WHERE password_secret_id=?`, at.Unix(), secretID,
	); err != nil {
		return nil, err
	}
	after, err := secretAllowServerNames(tx, secretID)
	if err != nil {
		return nil, err
	}
	loaded, err := s.sharedStorageByIDTx(tx, shareID)
	if err != nil {
		return nil, err
	}
	if err := writeSecretAuditTx(tx, at, "secret-set", map[string]any{
		"name": secretName, "change": "值已更改",
	}, source); err != nil {
		return nil, err
	}
	if err := writeSecretAuditTx(tx, at, "secret-disallow", map[string]any{
		"name": secretName, "before": strings.Join(before, ","),
		"after": strings.Join(after, ","),
	}, source); err != nil {
		return nil, err
	}
	if err := writeAuditTx(tx, OwnerAuditRowAt(at, "share-revoke", map[string]any{
		"share": loaded.Name, "server": serverName, "version": loaded.Version,
		"unmount": unmount, "force": force, "password_changed": true,
	})); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.secretValuesForMaskingVersion.Add(1)
	return loaded, nil
}
