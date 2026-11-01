package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 机密项的名字与值规则。名字从严（字母、数字、下划线，首字符不是数字），
// 以后放宽不需要迁移；值的最小长度保证后续输出遮蔽有足够的区分材料。
const (
	secretNameMaxLength = 64
	secretValueMinBytes = 8
	secretValueMaxBytes = 64 * 1024
)

// 机密项操作的错误。文本会直接出现在命令行报错里，因此自带完整原因，也
// 不包含值本身。
var (
	ErrSecretNameInvalid = errors.New("机密项名字不合法：只能使用字母、数字、下划线，长度 1 到 64，且首字符不能是数字")
	ErrSecretNameExists  = errors.New("机密项名字已存在（大小写不同的名字视为同一个）")
	ErrSecretNotFound    = errors.New("机密项不存在")
)

// SecretView 是机密项的对外可见形态：只有名字、备注、可写入服务器条目名与
// 两个时间。当前值、上一个值、值的哈希、长度与版本号都不进入这个结构，
// 让「读不回值」成为类型层面的约束，而不只是界面的约束。
type SecretView struct {
	Name           string
	Note           string
	AllowedServers []string
	CreatedAt      int64
	ValueUpdatedAt int64
}

// ValidateSecretName 校验机密项名字：首字符为字母或下划线，其余为字母、
// 数字、下划线，长度 1 到 64。唯一性检查按小写形式进行，由调用方另行查询。
func ValidateSecretName(name string) error {
	if name == "" || len(name) > secretNameMaxLength {
		return ErrSecretNameInvalid
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z'):
			// 首字符与后续字符都允许。
		case i > 0 && '0' <= c && c <= '9':
			// 数字只允许出现在非首字符。
		default:
			return ErrSecretNameInvalid
		}
	}
	return nil
}

// validateSecretValue 按原始字节校验值。空值、短于 8 个字节、超过 64 KiB
// 都拒绝；末尾换行计入长度。
func validateSecretValue(value []byte) error {
	switch n := len(value); {
	case n == 0:
		return errors.New("机密项值不能为空")
	case n < secretValueMinBytes:
		return fmt.Errorf("机密项值太短：至少需要 %d 个字节，当前 %d 个字节；短值无法可靠参与后续的输出遮蔽", secretValueMinBytes, n)
	case n > secretValueMaxBytes:
		return fmt.Errorf("机密项值太大：最多 %d 个字节，当前 %d 个字节", secretValueMaxBytes, n)
	default:
		return nil
	}
}

// AddSecret 新增一个机密项：值用 vault 主密钥加密，值版本号为 1，创建时间
// 与值的最后更改时间相同。名字唯一性按小写形式检查。数据与审计行在同一
// 事务提交。
func (s *Store) AddSecret(name, note string, value []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	blob, err := seal(s.masterKey, value)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRow(`SELECT name FROM secrets WHERE name_lower=?`, strings.ToLower(name)).Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w：%s", ErrSecretNameExists, existing)
	}
	if err != sql.ErrNoRows {
		return err
	}
	ts := now()
	id := newID()
	if _, err := tx.Exec(
		`INSERT INTO secrets (id,name,name_lower,note,value_blob,previous_value_blob,value_version,created_at,value_updated_at)
		 VALUES (?,?,?,?,?,?,1,?,?)`,
		id, name, strings.ToLower(name), note, blob, nil, ts, ts); err != nil {
		return err
	}
	if err := writeSecretAuditTx(tx, "secret-add", map[string]any{"name": name}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSecretValue 覆盖机密项的值：旧值移入上一个值字段，值版本号加一，值的
// 最后更改时间更新；备注与可写入服务器列表不变。数据与审计行在同一事务
// 提交，审计摘要只写「值已更改」。
func (s *Store) SetSecretValue(name string, value []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	blob, err := seal(s.masterKey, value)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldBlob []byte
	var version int64
	err = tx.QueryRow(`SELECT value_blob, value_version FROM secrets WHERE name=?`, name).Scan(&oldBlob, &version)
	if err == sql.ErrNoRows {
		return ErrSecretNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE secrets SET previous_value_blob=?, value_blob=?, value_version=?, value_updated_at=? WHERE name=?`,
		oldBlob, blob, version+1, now(), name); err != nil {
		return err
	}
	if err := writeSecretAuditTx(tx, "secret-set", map[string]any{"name": name, "change": "值已更改"}); err != nil {
		return err
	}
	return tx.Commit()
}

// EditSecretNote 只改备注。数据与审计行在同一事务提交，审计不记录备注原文。
func (s *Store) EditSecretNote(name, note string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE secrets SET note=? WHERE name=?`, note, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrSecretNotFound
	}
	if err := writeSecretAuditTx(tx, "secret-edit", map[string]any{"name": name, "change": "备注已更改"}); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSecret 直接删除机密项行；关联的可写入服务器行由外键级联删除。数据
// 与审计行在同一事务提交。
func (s *Store) DeleteSecret(name string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM secrets WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrSecretNotFound
	}
	if err := writeSecretAuditTx(tx, "secret-rm", map[string]any{"name": name}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSecrets 列出全部机密项的可见形态，按名字排序；可写入服务器条目名在
// 每个机密项内按名字排序。
func (s *Store) ListSecrets() ([]SecretView, error) {
	rows, err := s.db.Query(`
		SELECT sec.name, sec.note, sec.created_at, sec.value_updated_at, srv.name
		FROM secrets sec
		LEFT JOIN secret_server_allow allow ON allow.secret_id = sec.id
		LEFT JOIN servers srv ON srv.id = allow.server_id
		ORDER BY sec.name, srv.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretView
	index := make(map[string]int)
	for rows.Next() {
		var view SecretView
		var server sql.NullString
		if err := rows.Scan(&view.Name, &view.Note, &view.CreatedAt, &view.ValueUpdatedAt, &server); err != nil {
			return nil, err
		}
		if i, ok := index[view.Name]; ok {
			if server.Valid {
				out[i].AllowedServers = append(out[i].AllowedServers, server.String)
			}
			continue
		}
		if server.Valid {
			view.AllowedServers = []string{server.String}
		}
		index[view.Name] = len(out)
		out = append(out, view)
	}
	return out, rows.Err()
}

// secretAllowTarget 是一次授权改动的目标：机密项或服务器条目的内部标识符
// 与名字。
type secretAllowTarget struct {
	id   string
	name string
}

// AllowSecrets 把若干服务器条目加入若干机密项的可写入列表。先校验全部名字，
// 任何一个不存在就一次列出全部错误并整体不生效；校验通过后在一个事务内完成
// 全部改动，并为每个发生变化的机密项写一条审计行。
func (s *Store) AllowSecrets(secretNames, serverNames []string) error {
	return s.changeSecretAllows(secretNames, serverNames, true, "")
}

// DisallowSecrets 把若干服务器条目移出若干机密项的可写入列表。校验、事务
// 与审计语义与 AllowSecrets 相同。
func (s *Store) DisallowSecrets(secretNames, serverNames []string) error {
	return s.changeSecretAllows(secretNames, serverNames, false, "")
}

// changeSecretAllows 是 allow / disallow 的共用主体。reason 非空时写进审计
// 摘要，用于删除服务器条目时的联动审计。
func (s *Store) changeSecretAllows(secretNames, serverNames []string, allow bool, reason string) error {
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
	action := "secret-allow"
	stmt := `INSERT OR IGNORE INTO secret_server_allow(secret_id,server_id) VALUES(?,?)`
	if !allow {
		action = "secret-disallow"
		stmt = `DELETE FROM secret_server_allow WHERE secret_id=? AND server_id=?`
	}
	for _, secret := range secrets {
		before, err := secretAllowServerNames(tx, secret.id)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if _, err := tx.Exec(stmt, secret.id, server.id); err != nil {
				return err
			}
		}
		after, err := secretAllowServerNames(tx, secret.id)
		if err != nil {
			return err
		}
		if strings.Join(before, ",") == strings.Join(after, ",") {
			continue // 幂等重跑：数据没有变化，不写审计行。
		}
		summary := map[string]any{
			"name":   secret.name,
			"before": strings.Join(before, ","),
			"after":  strings.Join(after, ","),
		}
		if reason != "" {
			summary["reason"] = reason
		}
		if err := writeSecretAuditTx(tx, action, summary); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// resolveSecretAllowTargets 按名字逐个解析目标，返回解析成功的目标与缺失名
// 单；缺失名单由调用方合并后一次报出，保证整体不生效。
func resolveSecretAllowTargets(tx *sql.Tx, query string, names []string, label string) ([]secretAllowTarget, []string, error) {
	seen := make(map[string]bool, len(names))
	out := make([]secretAllowTarget, 0, len(names))
	var missing []string
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		var target secretAllowTarget
		err := tx.QueryRow(query, name).Scan(&target.id, &target.name)
		if err == sql.ErrNoRows {
			missing = append(missing, fmt.Sprintf("%s %q 不存在", label, name))
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, target)
	}
	return out, missing, nil
}

// secretAllowServerNames 返回一个机密项当前的可写入服务器条目名，按名字排序。
func secretAllowServerNames(tx *sql.Tx, secretID string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT srv.name FROM secret_server_allow allow
		JOIN servers srv ON srv.id = allow.server_id
		WHERE allow.secret_id=? ORDER BY srv.name`, secretID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// secretAllowsBeforeServerDelete 在删除服务器条目前读出该条目所在的全部机密
// 项，返回机密项名字到它当时完整可写入列表的映射，供删除后补写审计。
func secretAllowsBeforeServerDelete(tx *sql.Tx, serverID string) (map[string]string, error) {
	rows, err := tx.Query(`
		SELECT DISTINCT sec.name FROM secret_server_allow allow
		JOIN secrets sec ON sec.id = allow.secret_id
		WHERE allow.server_id=? ORDER BY sec.name`, serverID)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make(map[string]string, len(names))
	for _, name := range names {
		list, err := secretAllowServerNamesForName(tx, name)
		if err != nil {
			return nil, err
		}
		out[name] = strings.Join(list, ",")
	}
	return out, nil
}

// writeServerDeleteSecretAudits 在删除服务器条目的同一事务里，为每个受影响的
// 机密项补写一条取消授权审计，摘要标明原因是条目被删除。
func writeServerDeleteSecretAudits(tx *sql.Tx, before map[string]string) error {
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		beforeList := before[name]
		after, err := secretAllowServerNamesForName(tx, name)
		if err != nil {
			return err
		}
		if err := writeSecretAuditTx(tx, "secret-disallow", map[string]any{
			"name":   name,
			"before": beforeList,
			"after":  strings.Join(after, ","),
			"reason": "服务器条目已删除",
		}); err != nil {
			return err
		}
	}
	return nil
}

// secretAllowServerNamesForName 按机密项名字取当前可写入服务器条目名。
func secretAllowServerNamesForName(tx *sql.Tx, secretName string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT srv.name FROM secret_server_allow allow
		JOIN secrets sec ON sec.id = allow.secret_id
		JOIN servers srv ON srv.id = allow.server_id
		WHERE sec.name=? ORDER BY srv.name`, secretName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// writeSecretAuditTx 在机密项数据改动的同一事务里写一条 owner 审计行。摘要
// 只含调用方传入的白名单字段；任何路径都不得把值放进摘要。
func writeSecretAuditTx(tx *sql.Tx, action string, summary map[string]any) error {
	return writeAuditTx(tx, AuditRow{
		TS:      time.Now(),
		Action:  action,
		Command: MarshalAuditSummary(summary),
		Status:  "ok",
	})
}
