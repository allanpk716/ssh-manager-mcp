package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SecretAllowCell 表示机密项与服务器条目的一个交叉格。授权矩阵按这种
// 交叉格计算差异与摘要。
type SecretAllowCell struct {
	SecretID string
	ServerID string
}

// SecretAllowChange 是一个交叉格的目标状态：Allow 为 true 表示授权，false
// 表示取消授权。
type SecretAllowChange struct {
	SecretID string
	ServerID string
	Allow    bool
}

// SecretAllowState 是一个交叉格的当前状态，附带机密项名与服务器条目名供
// 预览展示；摘要计算只使用两个标识符与布尔状态。
type SecretAllowState struct {
	SecretID   string
	SecretName string
	ServerID   string
	ServerName string
	Allow      bool
}

// SecretAllowTargetMissingError 表示交叉格涉及的机密项或服务器条目已经被
// 删除。确认授权时调用方可按冲突处理，再按剩余格子重新计算预览。
type SecretAllowTargetMissingError struct {
	SecretID string
	ServerID string
}

// Error 返回可识别的缺失原因，只含标识符，不包含任何机密项值。
func (e *SecretAllowTargetMissingError) Error() string {
	if e.ServerID != "" {
		return fmt.Sprintf("服务器条目 %q 不存在", e.ServerID)
	}
	return fmt.Sprintf("机密项 %q 不存在", e.SecretID)
}

// SecretAllowChangeResult 是一个发生变化 的机密项在授权变更后的结果，
// AddedServers 与 RemovedServers 只含服务器条目名。
type SecretAllowChangeResult struct {
	SecretID       string
	SecretName     string
	AddedServers   []string
	RemovedServers []string
}

// SecretAllowConflictError 表示确认授权变更时，相关交叉格的现状与预览时
// 不一致。Current 是这些交叉格的最新状态；调用方应重新计算预览后再确认。
type SecretAllowConflictError struct {
	Current []SecretAllowState
}

// Error 返回可识别的冲突原因，不包含任何机密项值。
func (e *SecretAllowConflictError) Error() string {
	return "机密项可写入服务器列表已被其他入口修改，整体未生效；请使用最新预览重新确认"
}

// normalizeSecretAllowCells 把交叉格去重并按标识符排序，保证集合比较与
// 摘要计算都确定。
func normalizeSecretAllowCells(cells []SecretAllowCell) []SecretAllowCell {
	seen := make(map[SecretAllowCell]bool, len(cells))
	out := make([]SecretAllowCell, 0, len(cells))
	for _, cell := range cells {
		if cell.SecretID == "" || cell.ServerID == "" || seen[cell] {
			continue
		}
		seen[cell] = true
		out = append(out, cell)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SecretID != out[j].SecretID {
			return out[i].SecretID < out[j].SecretID
		}
		return out[i].ServerID < out[j].ServerID
	})
	return out
}

// normalizeSecretAllowChanges 把目标状态按交叉格去重。同一格给出两个不同
// 目标时直接报错，避免调用方把矛盾表单提交成半份改动。
func normalizeSecretAllowChanges(changes []SecretAllowChange) ([]SecretAllowChange, error) {
	seenCell := make(map[SecretAllowCell]bool, len(changes))
	seenTarget := make(map[SecretAllowChange]bool, len(changes))
	out := make([]SecretAllowChange, 0, len(changes))
	for _, change := range changes {
		cell := SecretAllowCell{SecretID: change.SecretID, ServerID: change.ServerID}
		if cell.SecretID == "" || cell.ServerID == "" {
			continue
		}
		if seenTarget[change] {
			continue
		}
		if seenCell[cell] {
			return nil, fmt.Errorf("机密项 %s 与服务器条目 %s 的目标状态重复且不一致", cell.SecretID, cell.ServerID)
		}
		seenCell[cell] = true
		seenTarget[change] = true
		out = append(out, change)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SecretID != out[j].SecretID {
			return out[i].SecretID < out[j].SecretID
		}
		return out[i].ServerID < out[j].ServerID
	})
	return out, nil
}

// loadSecretAllowStates 在事务里解析交叉格的名字并读回当前授权状态。任
// 意一个标识符不存在都整体报错；这个方法只读授权，不读机密项值。
func loadSecretAllowStates(tx *sql.Tx, cells []SecretAllowCell) ([]SecretAllowState, error) {
	normalized := normalizeSecretAllowCells(cells)
	out := make([]SecretAllowState, 0, len(normalized))
	for _, cell := range normalized {
		state := SecretAllowState{SecretID: cell.SecretID, ServerID: cell.ServerID}
		err := tx.QueryRow(`SELECT name FROM secrets WHERE id=?`, cell.SecretID).Scan(&state.SecretName)
		if err == sql.ErrNoRows {
			return nil, &SecretAllowTargetMissingError{SecretID: cell.SecretID}
		}
		if err != nil {
			return nil, err
		}
		err = tx.QueryRow(`SELECT name FROM servers WHERE id=?`, cell.ServerID).Scan(&state.ServerName)
		if err == sql.ErrNoRows {
			return nil, &SecretAllowTargetMissingError{SecretID: cell.SecretID, ServerID: cell.ServerID}
		}
		if err != nil {
			return nil, err
		}
		var one int
		err = tx.QueryRow(
			`SELECT 1 FROM secret_server_allow WHERE secret_id=? AND server_id=?`,
			cell.SecretID, cell.ServerID,
		).Scan(&one)
		if err == sql.ErrNoRows {
			state.Allow = false
		} else if err != nil {
			return nil, err
		} else {
			state.Allow = true
		}
		out = append(out, state)
	}
	return out, nil
}

// SecretAllowStates 读回一组交叉格的当前授权状态。它只暴露授权关系与名
// 字，不提供任何读取机密项值的入口。
func (s *Store) SecretAllowStates(cells []SecretAllowCell) ([]SecretAllowState, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	states, err := loadSecretAllowStates(tx, cells)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return states, nil
}

// ChangeSecretAllowsForNames 是命令行 allow / disallow 使用的授权变更入
// 口：目标全部是授权或全部是取消授权，不做预览核对。名字缺失时先合并报
// 出全部问题并整体不生效；名字齐全后仍由 ChangeSecretAllows 在一个事务里
// 执行全部插入或删除，并为每个发生变化的机密项写审计。
func (s *Store) ChangeSecretAllowsForNames(
	secretNames, serverNames []string,
	allow bool,
	source WriteSource,
) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	secrets, secretMissing, err := resolveSecretAllowTargets(tx, `SELECT id, name FROM secrets WHERE name=?`, secretNames, "机密项")
	if err != nil {
		return err
	}
	servers, serverMissing, err := resolveSecretAllowTargets(tx, `SELECT id, name FROM servers WHERE name=?`, serverNames, "服务器条目")
	if err != nil {
		return err
	}
	if missing := append(secretMissing, serverMissing...); len(missing) > 0 {
		return fmt.Errorf("以下名称不存在，整体未生效：\n%s", strings.Join(missing, "\n"))
	}
	changes := make([]SecretAllowChange, 0, len(secrets)*len(servers))
	for _, secret := range secrets {
		for _, server := range servers {
			changes = append(changes, SecretAllowChange{
				SecretID: secret.id, ServerID: server.id, Allow: allow,
			})
		}
	}
	if err := tx.Rollback(); err != nil {
		return err
	}
	_, err = s.ChangeSecretAllows(changes, nil, source)
	return err
}

// ChangeSecretAllows 在一个事务里按目标状态调整机密项授权。expected 是预
// 览时这些交叉格的状态；任意一格现状与预览不一致就整体拒绝并返回
// SecretAllowConflictError，不写数据也不写审计。expected 为空表示不做预览
// 核对，供命令行 allow / disallow 这类无预览入口使用。带预览核对的变更每
// 个发生变化的机密项写一条 secret-allow-change，摘要列出新增与移除的服务
// 器条目名；无预览入口保留原有 secret-allow / secret-disallow 审计形状。
// 两种形状都不含机密项值。
func (s *Store) ChangeSecretAllows(
	changes []SecretAllowChange,
	expected []SecretAllowState,
	source WriteSource,
) ([]SecretAllowChangeResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	targets, err := normalizeSecretAllowChanges(changes)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	cells := make([]SecretAllowCell, 0, len(targets))
	for _, target := range targets {
		cells = append(cells, SecretAllowCell{SecretID: target.SecretID, ServerID: target.ServerID})
	}
	expectedCells := make([]SecretAllowCell, 0, len(expected))
	for _, state := range expected {
		expectedCells = append(expectedCells, SecretAllowCell{SecretID: state.SecretID, ServerID: state.ServerID})
	}
	if expected != nil && strings.Join(secretAllowCellKeys(cells), "\n") != strings.Join(secretAllowCellKeys(expectedCells), "\n") {
		return nil, errors.New("授权变更的目标集合与预览集合不一致，整体未生效")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := loadSecretAllowStates(tx, cells)
	if err != nil {
		return nil, err
	}
	currentByKey := make(map[SecretAllowCell]SecretAllowState, len(current))
	for _, state := range current {
		currentByKey[SecretAllowCell{SecretID: state.SecretID, ServerID: state.ServerID}] = state
	}
	expectedByKey := make(map[SecretAllowCell]bool, len(expected))
	for _, state := range expected {
		expectedByKey[SecretAllowCell{SecretID: state.SecretID, ServerID: state.ServerID}] = state.Allow
	}
	if expected != nil {
		for _, cell := range normalizeSecretAllowCells(expectedCells) {
			state, exists := currentByKey[cell]
			if !exists || state.Allow != expectedByKey[cell] {
				return nil, &SecretAllowConflictError{Current: current}
			}
		}
	}

	resultsBySecret := make(map[string]*SecretAllowChangeResult)
	beforeServers := make(map[string]string)
	for _, state := range current {
		before, beforeErr := secretAllowServerNames(tx, state.SecretID)
		if beforeErr != nil {
			return nil, beforeErr
		}
		beforeServers[state.SecretID] = strings.Join(before, ",")
	}
	for _, target := range targets {
		cell := SecretAllowCell{SecretID: target.SecretID, ServerID: target.ServerID}
		state := currentByKey[cell]
		if state.Allow == target.Allow {
			continue
		}
		result, exists := resultsBySecret[target.SecretID]
		if !exists {
			result = &SecretAllowChangeResult{SecretID: target.SecretID, SecretName: state.SecretName}
			resultsBySecret[target.SecretID] = result
		}
		var execErr error
		if target.Allow {
			_, execErr = tx.Exec(
				`INSERT OR IGNORE INTO secret_server_allow(secret_id,server_id) VALUES(?,?)`,
				target.SecretID, target.ServerID,
			)
			result.AddedServers = append(result.AddedServers, state.ServerName)
		} else {
			_, execErr = tx.Exec(
				`DELETE FROM secret_server_allow WHERE secret_id=? AND server_id=?`,
				target.SecretID, target.ServerID,
			)
			result.RemovedServers = append(result.RemovedServers, state.ServerName)
		}
		if execErr != nil {
			return nil, execErr
		}
	}

	results := make([]SecretAllowChangeResult, 0, len(resultsBySecret))
	for _, result := range resultsBySecret {
		sort.Strings(result.AddedServers)
		sort.Strings(result.RemovedServers)
		results = append(results, *result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].SecretName < results[j].SecretName })
	auditAt := s.nowTime()
	for _, result := range results {
		afterServers, afterErr := secretAllowServerNames(tx, result.SecretID)
		if afterErr != nil {
			return nil, afterErr
		}
		action := "secret-allow-change"
		summary := map[string]any{
			"name":    result.SecretName,
			"added":   strings.Join(result.AddedServers, ","),
			"removed": strings.Join(result.RemovedServers, ","),
		}
		if expected == nil && len(result.RemovedServers) == 0 {
			action = "secret-allow"
			summary = map[string]any{
				"name": result.SecretName, "before": beforeServers[result.SecretID],
				"after": strings.Join(afterServers, ","),
			}
		} else if expected == nil && len(result.AddedServers) == 0 {
			action = "secret-disallow"
			summary = map[string]any{
				"name": result.SecretName, "before": beforeServers[result.SecretID],
				"after": strings.Join(afterServers, ","),
			}
		}
		if err := writeSecretAuditTx(tx, auditAt, action, summary, source); err != nil {
			return nil, err
		}
	}
	if len(results) == 0 {
		return nil, tx.Rollback()
	}
	if err := s.commitSecretChange(tx); err != nil {
		return nil, err
	}
	return results, nil
}

// secretAllowCellKeys 把交叉格转成稳定的文本键，只用于集合覆盖范围比较。
func secretAllowCellKeys(cells []SecretAllowCell) []string {
	normalized := normalizeSecretAllowCells(cells)
	out := make([]string, 0, len(normalized))
	for _, cell := range normalized {
		out = append(out, cell.SecretID+"\x00"+cell.ServerID)
	}
	return out
}
