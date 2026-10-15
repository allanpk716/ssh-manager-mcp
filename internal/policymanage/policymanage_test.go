package policymanage

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

// 管理层(ticket #15)的行为测试:这是 CLI 与(后续票的)TUI 共用的
// 可复用层,不在 cobra 处理器里。断言外部行为——
//   - 生效规则集组装:停用的默认规则不再命中、自定义规则按它判定;
//   - 增、删、停用、恢复各写一条 owner 审计行(动作名与摘要字段);
//   - 校验:与默认规则编号冲突、编号重复、非法处置、未知默认编号都拒。

func newStore(t *testing.T) *store.Store {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir()+"/pm.db", mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func rebootRule(id string) policy.CustomRule {
	return policy.CustomRule{
		ID:          id,
		Shells:      []policy.ShellType{policy.ShellPOSIX},
		Commands:    []string{"reboot"},
		Disposition: policy.DispositionBlock,
		Explanation: "no unattended reboots",
		Alternative: "ask the owner and schedule a window",
	}
}

// soleOwnerRow 取全部指定动作的 owner 审计行(计数防双写)。
func soleOwnerRows(t *testing.T, st *store.Store, action string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestEffectiveRuleSet_FromStore:管理层把权威库的两张表组装成判定内核
// 的生效规则集——停用的默认规则不再命中,自定义规则开始命中。
func TestEffectiveRuleSet_FromStore(t *testing.T) {
	st := newStore(t)

	// 空库:与 nil 等价,内置默认规则全部生效。
	rules, err := EffectiveRuleSet(st)
	if err != nil {
		t.Fatal(err)
	}
	if d := policy.Evaluate(policy.ShellPOSIX, "mkfs.ext4 /dev/sdb", rules); d.Disposition != policy.DispositionBlock || d.RuleID != "POSIX-FORMAT" {
		t.Fatalf("empty-store judge = %+v, want block/POSIX-FORMAT", d)
	}
	if d := policy.Evaluate(policy.ShellPOSIX, "reboot", rules); d.Disposition != policy.DispositionAllow {
		t.Fatalf("reboot with no custom rule = %+v, want allow", d)
	}

	if err := AddCustomRule(st, rebootRule("NO-REBOOT")); err != nil {
		t.Fatalf("AddCustomRule: %v", err)
	}
	if _, err := DisableDefaultRule(st, "POSIX-FORMAT"); err != nil {
		t.Fatalf("DisableDefaultRule: %v", err)
	}
	rules, err = EffectiveRuleSet(st)
	if err != nil {
		t.Fatal(err)
	}
	if d := policy.Evaluate(policy.ShellPOSIX, "mkfs.ext4 /dev/sdb", rules); d.Disposition != policy.DispositionAllow {
		t.Fatalf("disabled default must stop matching, got %+v", d)
	}
	if d := policy.Evaluate(policy.ShellPOSIX, "reboot", rules); d.Disposition != policy.DispositionBlock || d.RuleID != "NO-REBOOT" {
		t.Fatalf("custom rule must fire, got %+v", d)
	}
}

// TestEffectiveRuleSetFromSnapshot:客户端从快照段组装同样的生效规则集
// (客户端只读镜像的数据面)。
func TestEffectiveRuleSetFromSnapshot(t *testing.T) {
	st := newStore(t)
	if err := AddCustomRule(st, rebootRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	if _, err := DisableDefaultRule(st, "POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rules := EffectiveRuleSetFromSnapshot(snap)
	if d := policy.Evaluate(policy.ShellPOSIX, "reboot", rules); d.Disposition != policy.DispositionBlock || d.RuleID != "NO-REBOOT" {
		t.Fatalf("custom rule from snapshot = %+v, want block/NO-REBOOT", d)
	}
	if d := policy.Evaluate(policy.ShellPOSIX, ":(){ :|:& };:", rules); d.Disposition != policy.DispositionAllow {
		t.Fatalf("fork bomb with disabled default = %+v, want allow", d)
	}
	// 旧快照(无策略段):nil 段 → 默认规则全部生效。
	old := EffectiveRuleSetFromSnapshot(&store.Snapshot{})
	if d := policy.Evaluate(policy.ShellPOSIX, ":(){ :|:& };:", old); d.Disposition != policy.DispositionBlock {
		t.Fatalf("old snapshot fork bomb = %+v, want block", d)
	}
}

// TestListRules:列出全部规则——默认规则(带停用标注)在前、自定义规则
// 在后,自定义规则字段完整带出。
func TestListRules(t *testing.T) {
	st := newStore(t)
	if err := AddCustomRule(st, rebootRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	if _, err := DisableDefaultRule(st, "POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	views, err := ListRules(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(policy.DefaultRuleIDs())+1 {
		t.Fatalf("views = %d, want %d defaults + 1 custom", len(views), len(policy.DefaultRuleIDs()))
	}
	var forkDisabled, formatEnabled, custom *RuleView
	for i, v := range views {
		switch v.ID {
		case "POSIX-FORK-BOMB":
			if !v.Custom {
				forkDisabled = &views[i]
			}
		case "POSIX-FORMAT":
			formatEnabled = &views[i]
		case "NO-REBOOT":
			custom = &views[i]
		}
	}
	if forkDisabled == nil || !forkDisabled.Disabled {
		t.Fatalf("POSIX-FORK-BOMB must be listed as disabled: %+v", forkDisabled)
	}
	if formatEnabled == nil || formatEnabled.Disabled {
		t.Fatalf("POSIX-FORMAT must be listed as enabled: %+v", formatEnabled)
	}
	if custom == nil || !custom.Custom || custom.Disposition != policy.DispositionBlock ||
		len(custom.Commands) != 1 || custom.Commands[0] != "reboot" || custom.Explanation == "" {
		t.Fatalf("custom rule view incomplete: %+v", custom)
	}
}

// TestMutationAudit:增、删、停用、恢复各写恰一条 owner 审计行,动作名
// 与摘要字段(规则编号、处置)可追查;删除不存在的规则报错且不写成功行。
func TestMutationAudit(t *testing.T) {
	st := newStore(t)

	// 增。
	if err := AddCustomRule(st, rebootRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	rows := soleOwnerRows(t, st, "policy-rule.add")
	if len(rows) != 1 {
		t.Fatalf("policy-rule.add rows = %d, want 1", len(rows))
	}
	if rows[0].ProjectID != "" || rows[0].Status != "ok" ||
		!containsAll(rows[0].Command, `"id":"NO-REBOOT"`, `"disposition":"block"`) {
		t.Fatalf("add row = %+v (Command %q)", rows[0], rows[0].Command)
	}

	// 停用。
	if _, err := DisableDefaultRule(st, "POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	if rows := soleOwnerRows(t, st, "policy-rule.disable"); len(rows) != 1 ||
		!containsAll(rows[0].Command, `"id":"POSIX-FORK-BOMB"`) {
		t.Fatalf("disable rows = %+v", soleOwnerRows(t, st, "policy-rule.disable"))
	}

	// 恢复。
	if _, err := EnableDefaultRule(st, "POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	if rows := soleOwnerRows(t, st, "policy-rule.enable"); len(rows) != 1 ||
		!containsAll(rows[0].Command, `"id":"POSIX-FORK-BOMB"`) {
		t.Fatalf("enable rows = %+v", soleOwnerRows(t, st, "policy-rule.enable"))
	}

	// 删。
	if err := DeleteCustomRule(st, "NO-REBOOT"); err != nil {
		t.Fatal(err)
	}
	if rows := soleOwnerRows(t, st, "policy-rule.rm"); len(rows) != 1 ||
		!containsAll(rows[0].Command, `"id":"NO-REBOOT"`) {
		t.Fatalf("rm rows = %+v", soleOwnerRows(t, st, "policy-rule.rm"))
	}

	// 删除不存在的规则:报错,写 Status=error 的失败行(沿 servers/cache-tokens
	// 的失败审计模式),成功行不受影响。
	if err := DeleteCustomRule(st, "NO-REBOOT"); err == nil {
		t.Fatal("deleting an unknown rule must error")
	}
	rmRows := soleOwnerRows(t, st, "policy-rule.rm")
	if len(rmRows) != 2 {
		t.Fatalf("rm rows after failed delete = %d, want 2 (one ok + one error)", len(rmRows))
	}
	if rmRows[0].Status != "error" && rmRows[1].Status != "error" {
		t.Fatalf("failed delete must leave an error-status row: %+v", rmRows)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestAddCustomRuleValidation:管理层校验——编号撞默认规则、编号重复、
// 内核校验不过(非法处置等)都拒,库里不留行。
func TestAddCustomRuleValidation(t *testing.T) {
	st := newStore(t)
	if err := AddCustomRule(st, rebootRule("POSIX-FORMAT")); err == nil {
		t.Fatal("id colliding with a default rule must be refused")
	}
	if err := AddCustomRule(st, rebootRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	if err := AddCustomRule(st, rebootRule("NO-REBOOT")); err == nil {
		t.Fatal("duplicate id must be refused")
	}
	bad := rebootRule("BAD-DISPOSITION")
	bad.Disposition = policy.DispositionAllow
	if err := AddCustomRule(st, bad); err == nil {
		t.Fatal("invalid disposition must be refused")
	}
	rules, _ := st.ListPolicyRules()
	if len(rules) != 1 {
		t.Fatalf("rules after refusals = %d, want 1", len(rules))
	}
}

// TestDisableEnableUnknownDefaultID:停用/恢复只认内置默认规则编号,
// 未知编号(拼错)直接报错,不留死行。
func TestDisableEnableUnknownDefaultID(t *testing.T) {
	st := newStore(t)
	if _, err := DisableDefaultRule(st, "POSIX-NOPE"); err == nil {
		t.Fatal("unknown default rule id must be refused")
	}
	if _, err := EnableDefaultRule(st, "POSIX-NOPE"); err == nil {
		t.Fatal("unknown default rule id must be refused")
	}
	if ids, _ := st.ListDisabledDefaultRuleIDs(); len(ids) != 0 {
		t.Fatalf("disabled ids after refusals = %v, want empty", ids)
	}
}

// TestEnableNotDisabledRule:恢复一条未停用的默认规则是幂等成功,
// 不写审计行(无事发生)。
func TestEnableNotDisabledRule(t *testing.T) {
	st := newStore(t)
	changed, err := EnableDefaultRule(st, "POSIX-FORMAT")
	if err != nil || changed {
		t.Fatalf("enable of a not-disabled rule = %v, %v; want false, nil", changed, err)
	}
	if rows := soleOwnerRows(t, st, "policy-rule.enable"); len(rows) != 0 {
		t.Fatalf("no-op enable must not audit, got %d rows", len(rows))
	}
}

// TestListRulesFromSnapshot:快照列出面(客户端 `policy ls` 与后续 TUI 页
// 的数据源)——默认规则带停用标注、自定义规则字段完整;nil 快照只列
// 默认规则(旧快照形态)。
func TestListRulesFromSnapshot(t *testing.T) {
	snap := &store.Snapshot{
		PolicyRules: []store.SnapshotPolicyRule{{
			ID: "NO-REBOOT", Shells: []string{"posix"}, Commands: []string{"reboot"},
			Disposition: "block", Explanation: "no unattended reboots",
		}},
		PolicyDisabledDefaults: []string{"POSIX-FORK-BOMB"},
	}
	views := ListRulesFromSnapshot(snap)
	if len(views) != len(policy.DefaultRuleIDs())+1 {
		t.Fatalf("views = %d, want defaults + 1", len(views))
	}
	var custom, fork *RuleView
	for i, v := range views {
		switch v.ID {
		case "NO-REBOOT":
			custom = &views[i]
		case "POSIX-FORK-BOMB":
			fork = &views[i]
		}
	}
	if custom == nil || !custom.Custom || custom.Disposition != policy.DispositionBlock ||
		len(custom.Commands) != 1 || custom.Explanation == "" {
		t.Fatalf("custom view = %+v", custom)
	}
	if fork == nil || fork.Custom || !fork.Disabled {
		t.Fatalf("fork bomb view = %+v (want default + disabled)", fork)
	}

	// nil 快照:只列默认规则,全部生效。
	for _, v := range ListRulesFromSnapshot(nil) {
		if v.Custom || v.Disabled {
			t.Fatalf("nil snapshot must list enabled defaults only, got %+v", v)
		}
	}
}
