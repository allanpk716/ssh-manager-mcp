package snapshotdiff

import (
	"reflect"
	"sort"

	"ssh-manager-mcp/internal/store"
)

// policyChangedText 是命令策略有差异时的固定分句。
const policyChangedText = "命令策略已变化，此前被拒绝或允许的命令可能不同"

// CommandPolicy 比较命令策略:自定义规则按规则编号与语义字段比较,默认
// 规则停用清单按编号集合比较。时间戳不算语义字段,忽略;只要有差异就
// 返回同一条分句。
type CommandPolicy struct{}

// Describe 在命令策略有差异时返回一条分句,没有差异时返回空。
func (CommandPolicy) Describe(old, fresh *store.Snapshot) []string {
	if !reflect.DeepEqual(policyRulesByID(old), policyRulesByID(fresh)) ||
		!reflect.DeepEqual(disabledDefaults(old), disabledDefaults(fresh)) {
		return []string{policyChangedText}
	}
	return nil
}

// policyRulesByID 把自定义规则整理成「编号 → 语义字段」的映射,刻意不
// 带 CreatedAt / UpdatedAt:同一语义的规则只改时间戳不算策略变化。
func policyRulesByID(snap *store.Snapshot) map[string]store.SnapshotPolicyRule {
	if snap == nil {
		return nil
	}
	out := make(map[string]store.SnapshotPolicyRule, len(snap.PolicyRules))
	for _, rule := range snap.PolicyRules {
		rule.CreatedAt = 0
		rule.UpdatedAt = 0
		out[rule.ID] = rule
	}
	return out
}

func disabledDefaults(snap *store.Snapshot) []string {
	if snap == nil {
		return nil
	}
	out := append([]string(nil), snap.PolicyDisabledDefaults...)
	sort.Strings(out)
	return out
}
