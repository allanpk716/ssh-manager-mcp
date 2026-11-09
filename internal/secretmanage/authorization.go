package secretmanage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// AuthorizationCell 是授权矩阵提交的一个目标格：机密项标识符、服务器条
// 目标识符，以及这个格最终应该授权还是取消授权。调用方只需提交页面上可
// 见的格子；不可见格子不会被改动。
type AuthorizationCell struct {
	SecretID string
	ServerID string
	Allow    bool
}

// ServerRef 是授权预览里对一台服务器条目的引用。
type ServerRef struct {
	ID   string
	Name string
}

// AuthorizationChange 是一个机密项在这次授权变更中的差异。
type AuthorizationChange struct {
	SecretID   string
	SecretName string
	Added      []ServerRef
	Removed    []ServerRef
}

// AuthorizationPreview 是按当前授权状态算出的确认依据。Summary 是涉及的
// 每个授权格子（某个机密项与某个服务器条目交叉处的授权格子）的当前状态
// 加目标集合的确定性哈希；确认时必须携带同一个值。
type AuthorizationPreview struct {
	Summary      string
	Changes      []AuthorizationChange
	TotalAdded   int
	TotalRemoved int
}

// AuthorizationApplyResult 表示一次确认执行的结果。差异为空时 Applied 为
// false，数据与审计都不写。
type AuthorizationApplyResult struct {
	Preview AuthorizationPreview
	Applied bool
}

// AuthorizationConflictError 表示预览之后相关授权被其他入口改过。Latest
// 是按最新状态重新计算的预览，调用方应把它展示给操作者后重新确认。
type AuthorizationConflictError struct {
	Latest AuthorizationPreview
}

// Error 返回冲突原因，不包含任何机密项值。
func (e *AuthorizationConflictError) Error() string {
	return "机密项授权状态已被其他入口修改，已拒绝提交；请核对最新预览后再确认"
}

// PreviewAuthorizationChanges 按“目标授权集合”计算差异。输入只需要覆盖
// 调用方可见的格子；结果按机密项分组列出新增与移除的服务器条目，并附总
// 数与确认摘要。
func (m *Manager) PreviewAuthorizationChanges(target []AuthorizationCell) (AuthorizationPreview, error) {
	states, normalizedTarget, err := m.authorizationStates(target)
	if err != nil {
		return AuthorizationPreview{}, err
	}
	return buildAuthorizationPreview(states, normalizedTarget), nil
}

// ApplyAuthorizationChanges 确认一次授权变更。它先按当前状态重算预览；
// 摘要一致才把目标状态与预览状态交给存储层在同一个事务里执行。摘要不
// 一致时返回 AuthorizationConflictError 并附最新预览；差异为空时不写数据
// 也不写审计。
func (m *Manager) ApplyAuthorizationChanges(
	target []AuthorizationCell,
	summary string,
	src Source,
) (AuthorizationApplyResult, error) {
	states, normalizedTarget, err := m.authorizationStates(target)
	if err != nil {
		var missing *store.SecretAllowTargetMissingError
		if errors.As(err, &missing) {
			latest, latestErr := m.latestPreviewWithoutMissingTarget(
				dropMissingAuthorizationCells(target, missing),
			)
			if latestErr != nil {
				return AuthorizationApplyResult{}, latestErr
			}
			return AuthorizationApplyResult{}, &AuthorizationConflictError{Latest: latest}
		}
		return AuthorizationApplyResult{}, err
	}
	preview := buildAuthorizationPreview(states, normalizedTarget)
	if preview.Summary != summary {
		return AuthorizationApplyResult{}, &AuthorizationConflictError{Latest: preview}
	}
	if len(preview.Changes) == 0 {
		return AuthorizationApplyResult{Preview: preview}, nil
	}
	if _, err := m.st.ChangeSecretAllows(authorizationAllowChanges(normalizedTarget), states, src.origin); err != nil {
		var conflict *store.SecretAllowConflictError
		if errors.As(err, &conflict) {
			latest, latestErr := m.PreviewAuthorizationChanges(target)
			if latestErr != nil {
				return AuthorizationApplyResult{}, latestErr
			}
			return AuthorizationApplyResult{}, &AuthorizationConflictError{Latest: latest}
		}
		var missing *store.SecretAllowTargetMissingError
		if errors.As(err, &missing) {
			latest, latestErr := m.latestPreviewWithoutMissingTarget(
				dropMissingAuthorizationCells(target, missing),
			)
			if latestErr != nil {
				return AuthorizationApplyResult{}, latestErr
			}
			return AuthorizationApplyResult{}, &AuthorizationConflictError{Latest: latest}
		}
		return AuthorizationApplyResult{}, err
	}
	return AuthorizationApplyResult{Preview: preview, Applied: true}, nil
}

// latestPreviewWithoutMissingTarget 按仍存在的交叉格重新计算最新预览。多
// 个条目被删除时逐个剔除后再计算；全部被删除则得到空差异预览。
func (m *Manager) latestPreviewWithoutMissingTarget(target []AuthorizationCell) (AuthorizationPreview, error) {
	for {
		states, normalized, err := m.authorizationStates(target)
		if err == nil {
			return buildAuthorizationPreview(states, normalized), nil
		}
		var missing *store.SecretAllowTargetMissingError
		if !errors.As(err, &missing) {
			return AuthorizationPreview{}, err
		}
		next := dropMissingAuthorizationCells(target, missing)
		if len(next) == len(target) {
			return AuthorizationPreview{}, err
		}
		target = next
	}
}

// dropMissingAuthorizationCells 从目标集合里去掉已不存在的交叉格：机密项
// 被删时去掉它的全部格子，服务器条目被删时只去掉对应格子。
func dropMissingAuthorizationCells(target []AuthorizationCell, missing *store.SecretAllowTargetMissingError) []AuthorizationCell {
	out := make([]AuthorizationCell, 0, len(target))
	for _, cell := range target {
		if cell.SecretID == missing.SecretID &&
			(missing.ServerID == "" || cell.ServerID == missing.ServerID) {
			continue
		}
		out = append(out, cell)
	}
	return out
}

// authorizationStates 读取目标格的当前状态，并把目标集合整理成确定的顺
// 序。存储层只返回授权关系与名字，不会读取机密项值。
func (m *Manager) authorizationStates(target []AuthorizationCell) ([]store.SecretAllowState, []AuthorizationCell, error) {
	normalizedChanges, err := store.NormalizeSecretAllowChanges(authorizationAllowChanges(target))
	if err != nil {
		return nil, nil, err
	}
	cells := make([]store.SecretAllowCell, 0, len(normalizedChanges))
	for _, change := range normalizedChanges {
		cells = append(cells, store.SecretAllowCell{SecretID: change.SecretID, ServerID: change.ServerID})
	}
	states, err := m.st.SecretAllowStates(cells)
	if err != nil {
		return nil, nil, err
	}
	return states, authorizationCellsFromChanges(normalizedChanges), nil
}

// authorizationAllowChanges 把界面层的目标格转成存储层的目标状态。
func authorizationAllowChanges(target []AuthorizationCell) []store.SecretAllowChange {
	changes := make([]store.SecretAllowChange, 0, len(target))
	for _, cell := range target {
		changes = append(changes, store.SecretAllowChange{
			SecretID: cell.SecretID, ServerID: cell.ServerID, Allow: cell.Allow,
		})
	}
	return changes
}

// authorizationCellsFromChanges 把存储层排序后的目标状态转回界面层格子。
func authorizationCellsFromChanges(changes []store.SecretAllowChange) []AuthorizationCell {
	cells := make([]AuthorizationCell, 0, len(changes))
	for _, change := range changes {
		cells = append(cells, AuthorizationCell{
			SecretID: change.SecretID, ServerID: change.ServerID, Allow: change.Allow,
		})
	}
	return cells
}

// buildAuthorizationPreview 从当前状态与目标集合构造差异和确认摘要。摘要
// 输入先排序再哈希，因此同一状态与同一目标集合不随输入顺序改变。
func buildAuthorizationPreview(states []store.SecretAllowState, target []AuthorizationCell) AuthorizationPreview {
	current := make(map[store.SecretAllowCell]store.SecretAllowState, len(states))
	for _, state := range states {
		key := store.SecretAllowCell{SecretID: state.SecretID, ServerID: state.ServerID}
		current[key] = state
	}
	targetByKey := make(map[store.SecretAllowCell]bool, len(target))
	for _, cell := range target {
		key := store.SecretAllowCell{SecretID: cell.SecretID, ServerID: cell.ServerID}
		targetByKey[key] = cell.Allow
	}

	changesBySecret := make(map[string]*AuthorizationChange)
	for _, state := range states {
		key := store.SecretAllowCell{SecretID: state.SecretID, ServerID: state.ServerID}
		if state.Allow == targetByKey[key] {
			continue
		}
		change, exists := changesBySecret[state.SecretID]
		if !exists {
			change = &AuthorizationChange{SecretID: state.SecretID, SecretName: state.SecretName}
			changesBySecret[state.SecretID] = change
		}
		ref := ServerRef{ID: state.ServerID, Name: state.ServerName}
		if targetByKey[key] {
			change.Added = append(change.Added, ref)
		} else {
			change.Removed = append(change.Removed, ref)
		}
	}
	preview := AuthorizationPreview{Changes: make([]AuthorizationChange, 0, len(changesBySecret))}
	for _, change := range changesBySecret {
		preview.TotalAdded += len(change.Added)
		preview.TotalRemoved += len(change.Removed)
		preview.Changes = append(preview.Changes, *change)
	}
	sort.Slice(preview.Changes, func(i, j int) bool {
		return preview.Changes[i].SecretName < preview.Changes[j].SecretName
	})
	preview.Summary = authorizationSummary(states, targetByKey)
	return preview
}

// authorizationSummary 对涉及格子的当前状态与目标集合做确定性哈希。这里
// 只进入标识符与布尔授权状态，绝不进入机密项值。
func authorizationSummary(states []store.SecretAllowState, target map[store.SecretAllowCell]bool) string {
	lines := make([]string, 0, len(states))
	for _, state := range states {
		key := store.SecretAllowCell{SecretID: state.SecretID, ServerID: state.ServerID}
		currentValue, targetValue := 0, 0
		if state.Allow {
			currentValue = 1
		}
		if target[key] {
			targetValue = 1
		}
		lines = append(lines, fmt.Sprintf(
			"secret=%s\x00server=%s\x00current=%d\x00target=%d",
			state.SecretID, state.ServerID, currentValue, targetValue,
		))
	}
	sort.Strings(lines)
	digest := sha256.Sum256([]byte("sshmgr-secret-authorization-v1\n" + strings.Join(lines, "\n")))
	return hex.EncodeToString(digest[:])
}
