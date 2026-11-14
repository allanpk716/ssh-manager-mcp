package store

import (
	"database/sql"
)

// GetSetting returns the raw value stored under key. ok=false when the key is
// absent. Settings are plain operator preferences (Plan 42 三态开关的持久层,
// 批2 Settings 页同表同模式) — deliberately NOT encrypted: they hold no
// secrets, and the resolve path must read them without unsealing anything.
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting upserts key=value; an empty value DELETES the key — the
// tri-state "explicitly unset" form that lets an operator clear a preference
// instead of pinning it. Read-only (offline cache) stores reject the
// mutation: preferences are written on the authoritative broker, the cache
// only reads them.
func (s *Store) SetSetting(key, value string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if value == "" {
		_, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
		return err
	}
	// INSERT OR REPLACE (brief-sanctioned form): settings has no FK children,
	// so REPLACE's delete+reinsert is harmless, and updated_at rides along.
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO settings(key, value, updated_at) VALUES(?, ?, ?)`,
		key, value, now())
	return err
}

// snapshotSettings 读取设置存储中可跨机迁移的行。当前设置表只保存操作偏
// 好，不保存机密值；一次性迁移完成标记属于目标库本机状态，不随快照迁移。
func (s *Store) snapshotSettings() ([]SnapshotSetting, error) {
	rows, err := s.db.Query(`
		SELECT key,value,COALESCE(updated_at,0)
		FROM settings WHERE key<>? ORDER BY key`, serverDeviceMigrationSetting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SnapshotSetting, 0)
	for rows.Next() {
		var row SnapshotSetting
		if err := rows.Scan(&row.Key, &row.Value, &row.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
