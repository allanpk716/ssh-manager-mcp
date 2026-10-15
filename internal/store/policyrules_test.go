package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
)

// 自定义规则与停用记录的 vault 存储(ticket #15):权威库两张新表
// (policy_rules / policy_disabled_defaults)、增删查接口,以及只读缓存
// 库上结构性拒绝(ErrReadOnly)。只断言外部行为:行进出入库的字段、
// 停用记录的有无。

// seedRule 构造一条带全部结构化字段的自定义规则。
func seedRule(id string) policy.CustomRule {
	return policy.CustomRule{
		ID:           id,
		Shells:       []models.ShellType{models.ShellPOSIX},
		Commands:     []string{"reboot", "shutdown"},
		Args:         []string{"--force"},
		PathCategory: policy.PathCategoryCritical,
		Regex:        `reboot\s+now`,
		Disposition:  policy.DispositionBlock,
		Explanation:  "no unattended reboots",
		Alternative:  "ask the owner and schedule a window",
	}
}

// TestPolicyRulesCRUD:增加一条自定义规则后逐字段读回;删除后表空;
// 删除不存在的编号返回未删除而不是报错。
func TestPolicyRulesCRUD(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.ListPolicyRules(); err != nil {
		t.Fatalf("ListPolicyRules on empty table: %v", err)
	}
	rule := seedRule("NO-REBOOT")
	if err := s.AddPolicyRule(rule); err != nil {
		t.Fatalf("AddPolicyRule: %v", err)
	}
	rules, err := s.ListPolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	got := rules[0]
	if got.ID != rule.ID || got.Disposition != rule.Disposition ||
		got.PathCategory != rule.PathCategory || got.Regex != rule.Regex ||
		got.Explanation != rule.Explanation || got.Alternative != rule.Alternative {
		t.Fatalf("rule fields not round-tripped: %+v", got)
	}
	if len(got.Shells) != 1 || got.Shells[0] != models.ShellPOSIX ||
		len(got.Commands) != 2 || got.Commands[0] != "reboot" || got.Commands[1] != "shutdown" ||
		len(got.Args) != 1 || got.Args[0] != "--force" {
		t.Fatalf("rule list fields not round-tripped: %+v", got)
	}

	deleted, err := s.DeletePolicyRule("NO-REBOOT")
	if err != nil || !deleted {
		t.Fatalf("DeletePolicyRule = %v, %v; want true, nil", deleted, err)
	}
	if rules, _ = s.ListPolicyRules(); len(rules) != 0 {
		t.Fatalf("rules after delete = %d, want 0", len(rules))
	}
	if deleted, err = s.DeletePolicyRule("NO-REBOOT"); err != nil || deleted {
		t.Fatalf("DeletePolicyRule of unknown id = %v, %v; want false, nil", deleted, err)
	}
}

// TestPolicyRulesDuplicateID:同编号的自定义规则不能加第二条(主键约束
// 转成可读错误)。
func TestPolicyRulesDuplicateID(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddPolicyRule(seedRule("DUP")); err != nil {
		t.Fatal(err)
	}
	err := s.AddPolicyRule(seedRule("DUP"))
	if err == nil || !strings.Contains(err.Error(), "DUP") {
		t.Fatalf("duplicate add must error naming the id, got %v", err)
	}
}

// TestDisabledDefaultsCRUD:停用默认规则按编号落行;重复停用幂等返回
// 未插入;恢复返回已移除,再恢复返回未移除。
func TestDisabledDefaultsCRUD(t *testing.T) {
	s := newTestStore(t)

	inserted, err := s.DisableDefaultRule("POSIX-FORK-BOMB")
	if err != nil || !inserted {
		t.Fatalf("DisableDefaultRule = %v, %v; want true, nil", inserted, err)
	}
	ids, err := s.ListDisabledDefaultRuleIDs()
	if err != nil || len(ids) != 1 || ids[0] != "POSIX-FORK-BOMB" {
		t.Fatalf("disabled ids = %v, %v", ids, err)
	}
	if inserted, err = s.DisableDefaultRule("POSIX-FORK-BOMB"); err != nil || inserted {
		t.Fatalf("re-disable must be a no-op, got %v, %v", inserted, err)
	}

	removed, err := s.EnableDefaultRule("POSIX-FORK-BOMB")
	if err != nil || !removed {
		t.Fatalf("EnableDefaultRule = %v, %v; want true, nil", removed, err)
	}
	if ids, _ = s.ListDisabledDefaultRuleIDs(); len(ids) != 0 {
		t.Fatalf("disabled ids after enable = %v, want empty", ids)
	}
	if removed, err = s.EnableDefaultRule("POSIX-FORK-BOMB"); err != nil || removed {
		t.Fatalf("enable of a not-disabled rule must report nothing removed, got %v, %v", removed, err)
	}
}

// TestPolicyTablesReadOnly:客户端水合出的只读缓存库上,一切策略变更
// 结构性返回 ErrReadOnly——「客户端只读」不靠自觉。
func TestPolicyTablesReadOnly(t *testing.T) {
	s := newTestStore(t)
	s.SetReadOnly(nil)

	if err := s.AddPolicyRule(seedRule("X")); err != ErrReadOnly {
		t.Fatalf("AddPolicyRule on read-only = %v, want ErrReadOnly", err)
	}
	if _, err := s.DeletePolicyRule("X"); err != ErrReadOnly {
		t.Fatalf("DeletePolicyRule on read-only = %v, want ErrReadOnly", err)
	}
	if _, err := s.DisableDefaultRule("POSIX-FORK-BOMB"); err != ErrReadOnly {
		t.Fatalf("DisableDefaultRule on read-only = %v, want ErrReadOnly", err)
	}
	if _, err := s.EnableDefaultRule("POSIX-FORK-BOMB"); err != ErrReadOnly {
		t.Fatalf("EnableDefaultRule on read-only = %v, want ErrReadOnly", err)
	}
	// 读取不受影响。
	if _, err := s.ListPolicyRules(); err != nil {
		t.Fatalf("ListPolicyRules on read-only: %v", err)
	}
	if _, err := s.ListDisabledDefaultRuleIDs(); err != nil {
		t.Fatalf("ListDisabledDefaultRuleIDs on read-only: %v", err)
	}
}

// TestSnapshotRoundTripPolicySections:备份/恢复链路携带自定义规则与
// 停用记录——导出全量快照后导入另一个主键的空库,两张表的行原样到达。
func TestSnapshotRoundTripPolicySections(t *testing.T) {
	src := newTestStore(t)
	if err := src.AddPolicyRule(seedRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	if err := src.AddPolicyRule(policy.CustomRule{
		ID: "ASK-BEFORE-KILL", Commands: []string{"pkill"},
		Disposition: policy.DispositionConfirmRequired, Explanation: "killing daemons is the owner's call",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.DisableDefaultRule("POSIX-FORMAT"); err != nil {
		t.Fatal(err)
	}

	snap, err := src.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if len(snap.PolicyRules) != 2 {
		t.Fatalf("snapshot policy rules = %d, want 2", len(snap.PolicyRules))
	}
	if len(snap.PolicyDisabledDefaults) != 1 || snap.PolicyDisabledDefaults[0] != "POSIX-FORMAT" {
		t.Fatalf("snapshot disabled defaults = %v, want [POSIX-FORMAT]", snap.PolicyDisabledDefaults)
	}

	dst, _ := Open(filepath.Join(t.TempDir(), "dst.db"), mustMasterKey(t))
	defer dst.Close()
	if err := dst.ImportSnapshot(snap); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	rules, err := dst.ListPolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("imported rules = %d, want 2", len(rules))
	}
	var imported *policy.CustomRule
	for i, r := range rules {
		if r.ID == "NO-REBOOT" {
			imported = &rules[i]
		}
	}
	if imported == nil || imported.Disposition != policy.DispositionBlock || imported.Regex != `reboot\s+now` {
		t.Fatalf("imported rule fields drifted: %+v", imported)
	}
	if ids, _ := dst.ListDisabledDefaultRuleIDs(); len(ids) != 1 || ids[0] != "POSIX-FORMAT" {
		t.Fatalf("imported disabled defaults = %v, want [POSIX-FORMAT]", ids)
	}
}

// TestExportSnapshotForProfile_CarriesPolicySections:下发给缓存客户端的
// 裁剪快照同样携带策略段(规则全局一套,不做 profile 裁剪)。
func TestExportSnapshotForProfile_CarriesPolicySections(t *testing.T) {
	s := newTestStore(t)
	cid, _ := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, err := s.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	profID, _ := s.AddProfile("team-a")
	_ = s.GrantServers(profID, []string{srvID})
	if err := s.AddPolicyRule(seedRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisableDefaultRule("POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}

	snap, err := s.ExportSnapshotForProfile(profID)
	if err != nil {
		t.Fatalf("ExportSnapshotForProfile: %v", err)
	}
	if len(snap.PolicyRules) != 1 || snap.PolicyRules[0].ID != "NO-REBOOT" {
		t.Fatalf("scoped snapshot policy rules = %+v, want NO-REBOOT", snap.PolicyRules)
	}
	if len(snap.PolicyDisabledDefaults) != 1 || snap.PolicyDisabledDefaults[0] != "POSIX-FORK-BOMB" {
		t.Fatalf("scoped snapshot disabled defaults = %v", snap.PolicyDisabledDefaults)
	}
}

// TestImportSnapshot_OldSnapshotWithoutPolicySections:旧快照(策略字段
// 尚不存在时导出的 JSON)导入后规则表与停用表为空——内置默认规则
// 照常生效(生效面的行为断言在执行工具测试里)。
func TestImportSnapshot_OldSnapshotWithoutPolicySections(t *testing.T) {
	old := `{"version":1,"credentials":[],"servers":[],"profiles":[],"grants":[],"projects":[],"host_keys":[],"audit":[]}`
	var snap Snapshot
	if err := json.Unmarshal([]byte(old), &snap); err != nil {
		t.Fatalf("decode old snapshot: %v", err)
	}
	if snap.PolicyRules != nil || snap.PolicyDisabledDefaults != nil {
		t.Fatalf("old snapshot decoded policy sections = %v / %v, want nil", snap.PolicyRules, snap.PolicyDisabledDefaults)
	}
	dst, _ := Open(filepath.Join(t.TempDir(), "old.db"), mustMasterKey(t))
	defer dst.Close()
	if err := dst.ImportSnapshot(&snap); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if rules, _ := dst.ListPolicyRules(); len(rules) != 0 {
		t.Fatalf("old snapshot imported rules = %d, want 0", len(rules))
	}
	if ids, _ := dst.ListDisabledDefaultRuleIDs(); len(ids) != 0 {
		t.Fatalf("old snapshot imported disabled defaults = %v, want empty", ids)
	}
}

// mustMasterKey 生成一个随机主键(store 测试的既有形态)。
func mustMasterKey(t *testing.T) []byte {
	t.Helper()
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

// TestPolicyRulesSurviveReopen:规则与停用记录落盘后重开库仍在
// (权威端进程重启不丢规则)。
func TestPolicyRulesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	mk := mustMasterKey(t)
	s, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPolicyRule(seedRule("NO-REBOOT")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisableDefaultRule("POSIX-FORMAT"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if rules, _ := s2.ListPolicyRules(); len(rules) != 1 || rules[0].ID != "NO-REBOOT" {
		t.Fatalf("rules after reopen = %+v, want NO-REBOOT", rules)
	}
	if ids, _ := s2.ListDisabledDefaultRuleIDs(); len(ids) != 1 || ids[0] != "POSIX-FORMAT" {
		t.Fatalf("disabled after reopen = %v, want [POSIX-FORMAT]", ids)
	}
	_ = os.Remove(path)
}
