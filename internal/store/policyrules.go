package store

// 自定义命令策略规则与默认规则停用记录的权威存储(ticket #15,规格
// #8「存储与下发」):两张新表 policy_rules / policy_disabled_defaults。
// 只读缓存(客户端水合库)上一切变更返回 ErrReadOnly——「客户端只读」
// 由 SetReadOnly 开关结构性保证。规则本体的判定语义在 internal/policy,
// 本文件只管行的进出。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/policy"
)

// policyRulesJSONColumns 是 shells / commands / args 三列的统一序列化
// 形态:JSON 数组字符串(与 servers.tags 的既有做法一致),空集存
// "[]",读回空切片而不是 nil,让上层不必区分「没写」与「写了空」。
func marshalStringList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		// []string 永远可序列化;此处不可达,保底存空数组。
		return "[]"
	}
	return string(b)
}

func unmarshalStringList(raw string) []string {
	var out []string
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// AddPolicyRule 插入一条自定义规则(编号唯一)。同编号已存在时返回
// 指名编号的可读错误。
func (s *Store) AddPolicyRule(rule policy.CustomRule) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(`INSERT INTO policy_rules(id,shells,commands,args,path_category,regex,disposition,explanation,alternative,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		rule.ID,
		marshalShells(rule.Shells),
		marshalStringList(rule.Commands),
		marshalStringList(rule.Args),
		rule.PathCategory,
		rule.Regex,
		rule.Disposition.Code(),
		rule.Explanation,
		rule.Alternative,
		now(), now(),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// UNIQUE 主键冲突(同编号第二条)翻译成指名编号的可读错误。
		if isUniqueViolation(err) {
			return fmt.Errorf("a custom rule with id %q already exists", rule.ID)
		}
		return err
	}
	return nil
}

// marshalShells 把 shell 类型集合序列化成 JSON 字符串数组。
func marshalShells(shells []policy.ShellType) string {
	strs := make([]string, 0, len(shells))
	for _, sh := range shells {
		strs = append(strs, string(sh))
	}
	return marshalStringList(strs)
}

// isUniqueViolation 报告一个 SQLite 错误是否是唯一约束冲突
// (modernc.org/sqlite 的错误文本含 "UNIQUE constraint failed")。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// DeletePolicyRule 按编号删除一条自定义规则;编号不存在时不报错,
// 返回 false。
func (s *Store) DeletePolicyRule(id string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(`DELETE FROM policy_rules WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListPolicyRules 返回全部自定义规则,按编号排序(与导出路径的顺序
// 一致,快照字节稳定)。
func (s *Store) ListPolicyRules() ([]policy.CustomRule, error) {
	rows, err := s.db.Query(`SELECT id,shells,commands,args,path_category,regex,disposition,explanation,alternative,created_at,updated_at FROM policy_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []policy.CustomRule
	for rows.Next() {
		var r policy.CustomRule
		var shellsRaw, commandsRaw, argsRaw, disposition string
		if err := rows.Scan(&r.ID, &shellsRaw, &commandsRaw, &argsRaw, &r.PathCategory, &r.Regex, &disposition, &r.Explanation, &r.Alternative, new(int64), new(int64)); err != nil {
			return nil, err
		}
		for _, sh := range unmarshalStringList(shellsRaw) {
			r.Shells = append(r.Shells, policy.ShellType(sh))
		}
		r.Commands = unmarshalStringList(commandsRaw)
		r.Args = unmarshalStringList(argsRaw)
		d, derr := policy.ParseDisposition(disposition)
		if derr != nil {
			return nil, fmt.Errorf("policy rule %s: %w", r.ID, derr)
		}
		r.Disposition = d
		out = append(out, r)
	}
	return out, rows.Err()
}

// DisableDefaultRule 按编号停用一条默认规则(插入停用表)。已停用时
// 幂等返回未插入。
func (s *Store) DisableDefaultRule(id string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO policy_disabled_defaults(rule_id, created_at) VALUES(?,?)`, id, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// EnableDefaultRule 按编号恢复一条已停用的默认规则(删除停用行)。
// 未停用时幂等返回未移除。
func (s *Store) EnableDefaultRule(id string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(`DELETE FROM policy_disabled_defaults WHERE rule_id=?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListDisabledDefaultRuleIDs 返回已停用的默认规则编号,按编号排序。
func (s *Store) ListDisabledDefaultRuleIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT rule_id FROM policy_disabled_defaults ORDER BY rule_id`)
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

// exportPolicySections 两条快照导出路径共用的策略段导出:自定义规则与
// 停用记录全量导出,不随 profile 裁剪(规则全局一套)。列形态刻意与
// ImportSnapshot 的 INSERT 保持一致。
func (s *Store) exportPolicySections() ([]SnapshotPolicyRule, []string, error) {
	rows, err := s.db.Query(`SELECT id,shells,commands,args,path_category,regex,disposition,explanation,alternative,created_at,updated_at FROM policy_rules ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var rules []SnapshotPolicyRule
	for rows.Next() {
		var r SnapshotPolicyRule
		var shellsRaw, commandsRaw, argsRaw string
		if err := rows.Scan(&r.ID, &shellsRaw, &commandsRaw, &argsRaw, &r.PathCategory, &r.Regex, &r.Disposition, &r.Explanation, &r.Alternative, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, nil, err
		}
		r.Shells = unmarshalStringList(shellsRaw)
		r.Commands = unmarshalStringList(commandsRaw)
		r.Args = unmarshalStringList(argsRaw)
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	disabled, err := s.ListDisabledDefaultRuleIDs()
	if err != nil {
		return nil, nil, err
	}
	return rules, disabled, nil
}
