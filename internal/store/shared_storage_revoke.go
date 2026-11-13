package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

const sharedStorageRevokeChangeNote = "共享已从服务器撤销"

// RevokeSharedStorageMount 在同一个事务里把服务器条目移出共享存储挂载
// 密码机密项的可写入列表，并写机密项取消授权审计与共享撤销 owner 审计。
// 返回提交事务后的共享存储登记，供调用方用新版本号清理远端。
func (s *Store) RevokeSharedStorageMount(
	shareID, serverID string, unmount, force bool, source WriteSource,
) (*models.SharedStorage, error) {
	if s.readOnly {
		return nil, ErrReadOnly
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
	if _, err := tx.Exec(
		`UPDATE shared_storages SET change_note=? WHERE id=?`,
		sharedStorageRevokeChangeNote, shareID,
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
	at := s.nowTime()
	if err := writeSecretAuditTx(tx, at, "secret-disallow", map[string]any{
		"name": secretName, "before": strings.Join(before, ","),
		"after": strings.Join(after, ","),
	}, source); err != nil {
		return nil, err
	}
	auditRow := OwnerAuditRowAt(at, "share-revoke", map[string]any{
		"share": loaded.Name, "server": serverName, "version": loaded.Version,
		"unmount": unmount, "force": force, "password_changed": false,
	})
	auditRow.Source = source.kind
	auditRow.SourceIP = source.address
	if err := writeAuditTx(tx, auditRow); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return loaded, nil
}
