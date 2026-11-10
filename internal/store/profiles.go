package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

func (s *Store) AddProfile(name string) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	id := newID()
	ts := now()
	_, err := s.db.Exec(
		`INSERT INTO profiles (id,name,created_at,updated_at) VALUES (?,?,?,?)`,
		id, name, ts, ts,
	)
	if err != nil {
		// Localize the name-collision error — the raw driver text leaks SQLite
		// jargon into management surfaces (same wrap as AddServer).
		if strings.Contains(err.Error(), "UNIQUE constraint failed: profiles.name") {
			return "", fmt.Errorf("profile name %q already exists", name)
		}
		return "", err
	}
	return id, nil
}

func (s *Store) GetProfile(id string) (*models.Profile, error) {
	var (
		p       models.Profile
		created int64
		updated int64
	)
	// v0.8.5: created_at/updated_at WERE never selected — every profile
	// rendered 0001-01-01 (Go zero time) on listing. Unix-epoch int64 scan,
	// the same pattern ListCacheTokens uses.
	err := s.db.QueryRow(
		`SELECT id,name,created_at,updated_at FROM profiles WHERE id=?`, id,
	).Scan(&p.ID, &p.Name, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	ids, err := s.ServersForProfile(id)
	if err != nil {
		return nil, err
	}
	p.ServerIDs = ids
	return &p, nil
}

// requireProfile is the shared existence precheck for every mutation path that
// binds a profile: issuing a device code, repair-binding one, and the
// per-profile snapshot export. q takes *sql.DB or *sql.Tx (the dbtx surface in
// tx.go) so the check runs in the caller's context — addCacheTokenTx validates
// INSIDE its single transaction, never against the pool under it.
func requireProfile(q dbtx, profileID string) error {
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM profiles WHERE id=?`, profileID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("profile %q not found", profileID)
	}
	return nil
}

func (s *Store) ListProfiles() ([]*models.Profile, error) {
	rows, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Profile
	for rows.Next() {
		var (
			p       models.Profile
			created int64
			updated int64
		)
		if err := rows.Scan(&p.ID, &p.Name, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// GrantServers adds serverIDs to the profile inside one transaction.
// Duplicate (profile_id, server_id) pairs are ignored (INSERT OR IGNORE).
// Unknown server ids are rejected by an in-transaction precheck BEFORE any
// INSERT runs — fail-fast with the offending id named, so nothing is granted
// and no half-granted profile is left for a caller retry to orphan. The FK
// constraint stays as the fail-closed backstop behind the precheck.
// v0.8.6: an ACTUAL insert (not an ignored duplicate) bumps
// profiles.updated_at in the same tx — the grant set IS profile state, and
// listing surfaces' 更新 column must reflect grant changes.
func (s *Store) GrantServers(profileID string, serverIDs []string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sid := range serverIDs {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM servers WHERE id=?`, sid).Scan(&one); err == sql.ErrNoRows {
			return fmt.Errorf("server %s not found (grant aborted, nothing changed)", sid)
		} else if err != nil {
			return err
		}
	}
	added := 0
	for _, sid := range serverIDs {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, sid,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if added > 0 {
		if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ServersForProfile(profileID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SyncServers REPLACES the profile's grant set with ids in one transaction:
// unchecked rows (present now, absent from ids) are removed, missing ones
// inserted — the owner's v0.8.4 ruling on grant semantics (取消勾选=移除).
// Unknown server ids fail fast exactly like GrantServers (nothing changes);
// an empty ids slice clears the grant set (a deliberate revoke-all).
// Returns (added, removed).
func (s *Store) SyncServers(profileID string, ids []string) (int, int, error) {
	if s.readOnly {
		return 0, 0, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var one int
	for _, sid := range ids {
		if err := tx.QueryRow(`SELECT 1 FROM servers WHERE id=?`, sid).Scan(&one); err == sql.ErrNoRows {
			return 0, 0, fmt.Errorf("server %s not found (sync aborted, nothing changed)", sid)
		} else if err != nil {
			return 0, 0, err
		}
	}
	added := 0
	for _, sid := range ids {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, sid,
		)
		if err != nil {
			return 0, 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	// Go-side diff (no JSON1 dependency): delete the current rows absent
	// from ids — the removal half, the reason SyncServers exists.
	want := make(map[string]bool, len(ids))
	for _, sid := range ids {
		want[sid] = true
	}
	rows, err := tx.Query(`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID)
	if err != nil {
		return 0, 0, err
	}
	var stale []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if !want[sid] {
			stale = append(stale, sid)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	removed := 0
	for _, sid := range stale {
		if _, err := tx.Exec(`DELETE FROM profile_servers WHERE profile_id=? AND server_id=?`, profileID, sid); err != nil {
			return 0, 0, err
		}
		removed++
	}
	// v0.8.6: the grant set IS profile state — a real change (either
	// direction) bumps updated_at in the same tx; a no-op sync keeps the old
	// timestamp (nothing changed, nothing to advertise).
	if added+removed > 0 {
		if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
			return 0, 0, err
		}
	}
	return added, removed, tx.Commit()
}

// DeleteProfile 在一个事务里删除 profile 及其全部授权行。仍被未吊销项目
// 引用时拒绝（错误里点名这些项目）：静默解绑会让这些项目的 agent 一台
// 服务器都看不到，必须先删除或换绑这些项目。已吊销项目的绑定是惰性绑
// 定（令牌已失效），在同一事务里置空即可。仍被活跃设备码绑定时同样拒
// 绝（Plan 39）：设备码拉取快照的授权范围就是这个 profile，删掉它会困
// 住设备；已吊销设备码的绑定同样置空后随 profile 退场。
func (s *Store) DeleteProfile(profileID string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT name FROM projects WHERE profile_id=? AND status<>'revoked'`, profileID)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(names) > 0 {
		return fmt.Errorf("profile 仍被 %d 个未吊销项目引用(%s):先删除、吊销或换绑这些项目,再删除 profile",
			len(names), strings.Join(names, ", "))
	}
	// 已吊销项目令牌的绑定是惰性绑定：置空即可，随 profile 一并退场。
	if _, err := tx.Exec(`UPDATE projects SET profile_id=NULL WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	// Plan 39: same refusal for ACTIVE device bindings, by device name.
	rows, err = tx.Query(`SELECT name FROM cache_tokens WHERE profile_id=? AND status='active'`, profileID)
	if err != nil {
		return err
	}
	var devices []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		devices = append(devices, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(devices) > 0 {
		return fmt.Errorf("profile 仍被 %d 个设备码绑定(%s):先吊销或换绑(cache-tokens bind)这些设备码,再删除 profile",
			len(devices), strings.Join(devices, ", "))
	}
	// Inert bindings (revoked codes) would trip the FK on DELETE; clear them.
	if _, err := tx.Exec(`UPDATE cache_tokens SET profile_id=NULL WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM profile_servers WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM profiles WHERE id=?`, profileID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("profile %s not found", profileID)
	}
	return tx.Commit()
}

// RevokeServers 按 serverIDs 逐台删除 profile 的授权行，返回真正删除的
// serverID 清单；本来就没授权的服务器只是没有对应行可删，不报错（调用
// 方据此提示「未授权，跳过」）。实际删除发生时同步推进 profile 的更新
// 时间——授权集合是 profile 的状态。
func (s *Store) RevokeServers(profileID string, serverIDs []string) ([]string, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var removed []string
	for _, sid := range serverIDs {
		res, err := tx.Exec(`DELETE FROM profile_servers WHERE profile_id=? AND server_id=?`, profileID, sid)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed = append(removed, sid)
		}
	}
	if len(removed) > 0 {
		if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
			return nil, err
		}
	}
	return removed, tx.Commit()
}

// GrantSetAdjustment 是「按完整集合调整授权」的结果：真正新增与撤销的服
// 务器标识符；发生冲突时 Current 带当前授权集合供调用方重新渲染表单。
type GrantSetAdjustment struct {
	Added   []string
	Removed []string
	Current []string
}

// AdjustGrantSet 在一个事务里按完整集合调整 profile 的授权：先比对调用
// 方打开表单时看到的集合（expected）与当前集合，不一致就返回冲突（不写
// 任何数据）；一致则按 desired 与当前集合的差异逐项新增或删除授权行。
// 没有变化时不写任何行（不推进快照修订号）。desired 里未知的服务器标
// 识符整体报错，保持与 GrantServers 相同的失败即回滚语义。
func (s *Store) AdjustGrantSet(profileID string, expected, desired []string) (GrantSetAdjustment, bool, error) {
	if s.readOnly {
		return GrantSetAdjustment{}, false, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return GrantSetAdjustment{}, false, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID)
	if err != nil {
		return GrantSetAdjustment{}, false, err
	}
	var current []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return GrantSetAdjustment{}, false, err
		}
		current = append(current, sid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GrantSetAdjustment{}, false, err
	}
	rows.Close()
	currentSorted := normalizeIDSet(current)
	expectedSorted := normalizeIDSet(expected)
	if !idSetsEqual(currentSorted, expectedSorted) {
		return GrantSetAdjustment{Current: currentSorted}, true, nil
	}
	desiredSorted := normalizeIDSet(desired)
	currentSet := make(map[string]bool, len(currentSorted))
	for _, sid := range currentSorted {
		currentSet[sid] = true
	}
	desiredSet := make(map[string]bool, len(desiredSorted))
	for _, sid := range desiredSorted {
		desiredSet[sid] = true
	}
	var one int
	for _, sid := range desiredSorted {
		if err := tx.QueryRow(`SELECT 1 FROM servers WHERE id=?`, sid).Scan(&one); err == sql.ErrNoRows {
			return GrantSetAdjustment{}, false, fmt.Errorf("server %s not found (adjust aborted, nothing changed)", sid)
		} else if err != nil {
			return GrantSetAdjustment{}, false, err
		}
	}
	adjustment := GrantSetAdjustment{}
	for _, sid := range desiredSorted {
		if !currentSet[sid] {
			adjustment.Added = append(adjustment.Added, sid)
		}
	}
	for _, sid := range currentSorted {
		if !desiredSet[sid] {
			adjustment.Removed = append(adjustment.Removed, sid)
		}
	}
	changed := false
	for _, sid := range adjustment.Added {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`, profileID, sid); err != nil {
			return GrantSetAdjustment{}, false, err
		}
		changed = true
	}
	for _, sid := range adjustment.Removed {
		if _, err := tx.Exec(`DELETE FROM profile_servers WHERE profile_id=? AND server_id=?`, profileID, sid); err != nil {
			return GrantSetAdjustment{}, false, err
		}
		changed = true
	}
	if changed {
		if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
			return GrantSetAdjustment{}, false, err
		}
	}
	return adjustment, false, tx.Commit()
}

// normalizeIDSet 把标识符清单去重并按字典序排序，保证集合比较与结果
// 顺序都确定。
func normalizeIDSet(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// idSetsEqual 判断两个排序切片是否包含同一组标识符。
func idSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
