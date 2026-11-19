package store

import (
	"database/sql"
	"errors"
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// ClientProfileChange 描述一次客户端实例更换 profile 的结果。
type ClientProfileChange struct {
	ClientName    string
	OldProfileID  string
	NewProfileID  string
	PairedProject bool
}

type ClientRemoval struct {
	ClientName         string
	MachineName        string
	RevokedDeviceCode  bool
	RevokedPairProject bool
}

// ChangeClientProfile 在同一个事务里更换客户端实例绑定的 profile。配对流
// 程生成的同名访问身份必须同步换绑，否则客户端拉到新快照后本地访问身份
// 会失效。expectedProfileID 保存预览时看到的绑定，防止并发修改被覆盖。
func (s *Store) ChangeClientProfile(clientID, expectedProfileID, newProfileID string) (ClientProfileChange, error) {
	if s.readOnly {
		return ClientProfileChange{}, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ClientProfileChange{}, err
	}
	defer tx.Rollback()
	var clientName, currentProfile, status string
	err = tx.QueryRow(
		`SELECT name,COALESCE(profile_id,''),status FROM cache_tokens WHERE id=?`, clientID,
	).Scan(&clientName, &currentProfile, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ClientProfileChange{}, fmt.Errorf("%w：%s", ErrClientNotFound, clientID)
	}
	if err != nil {
		return ClientProfileChange{}, err
	}
	if models.CacheTokenStatus(status) != models.CacheTokenActive {
		return ClientProfileChange{}, fmt.Errorf("客户端实例 %s 已移除，不能更换 profile", clientName)
	}
	if currentProfile != expectedProfileID {
		return ClientProfileChange{}, fmt.Errorf("客户端实例 %s 的 profile 已被其他入口修改，请重新预览后再确认", clientName)
	}
	if err := requireProfile(tx, newProfileID); err != nil {
		return ClientProfileChange{}, err
	}
	if _, err := tx.Exec(
		`UPDATE cache_tokens SET profile_id=?,updated_at=? WHERE id=?`, newProfileID, now(), clientID,
	); err != nil {
		return ClientProfileChange{}, err
	}
	change := ClientProfileChange{ClientName: clientName, OldProfileID: currentProfile, NewProfileID: newProfileID}
	project, err := lookupPairProjectTx(tx, pairProjectName(clientName))
	if err != nil {
		return ClientProfileChange{}, err
	}
	if project != nil && project.PairGenerated {
		if _, err := tx.Exec(
			`UPDATE projects SET profile_id=?,updated_at=? WHERE id=?`, newProfileID, now(), project.ID,
		); err != nil {
			return ClientProfileChange{}, err
		}
		change.PairedProject = true
	}
	return change, tx.Commit()
}

// RemoveClient 在同一个事务里吊销客户端拉取凭据、吊销配对流程生成的访问
// 身份并清空所在机器的关联，避免留下半移除状态。
func (s *Store) RemoveClient(clientID string) (ClientRemoval, error) {
	if s.readOnly {
		return ClientRemoval{}, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ClientRemoval{}, err
	}
	defer tx.Rollback()
	var clientName, status string
	err = tx.QueryRow(`SELECT name,status FROM cache_tokens WHERE id=?`, clientID).Scan(&clientName, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ClientRemoval{}, fmt.Errorf("%w：%s", ErrClientNotFound, clientID)
	}
	if err != nil {
		return ClientRemoval{}, err
	}
	if models.CacheTokenStatus(status) != models.CacheTokenActive {
		return ClientRemoval{}, fmt.Errorf("客户端实例 %s 已移除", clientName)
	}
	removal := ClientRemoval{ClientName: clientName, RevokedDeviceCode: true}
	if err := tx.QueryRow(
		`SELECT name FROM devices WHERE client_token_id=?`, clientID,
	).Scan(&removal.MachineName); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ClientRemoval{}, err
	}
	if _, err := tx.Exec(
		`UPDATE devices SET client_token_id=NULL,client_name='',updated_at=? WHERE client_token_id=?`,
		now(), clientID,
	); err != nil {
		return ClientRemoval{}, err
	}
	if err := revokeCacheTokenTx(tx, clientName); err != nil {
		return ClientRemoval{}, err
	}
	project, err := lookupPairProjectTx(tx, pairProjectName(clientName))
	if err != nil {
		return ClientRemoval{}, err
	}
	if project != nil && project.PairGenerated {
		if _, err := tx.Exec(
			`UPDATE projects SET status=?,updated_at=? WHERE id=?`,
			string(models.ProjectRevoked), now(), project.ID,
		); err != nil {
			return ClientRemoval{}, err
		}
		removal.RevokedPairProject = true
	}
	return removal, tx.Commit()
}
