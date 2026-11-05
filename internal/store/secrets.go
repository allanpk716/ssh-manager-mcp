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

// SecretValuesForMasking 是只供进程内部输出遮蔽使用的机密项值集合。它包含
// 解密后的当前值与上一个值，以及可写入服务器条目标识符；不能接入任何
// MCP 工具，也不能写入日志或审计。
type SecretValuesForMasking struct {
	Name              string
	Current           []byte
	Previous          []byte
	WritableServerIDs []string
}

// SecretValuesForMasking 读取全部机密项的当前值、上一个值与可写入服务器
// 条目标识符。该读取路径只服务本进程内的输出遮蔽；调用方不得把返回值
// 序列化到工具结果、日志或审计。
func (s *Store) SecretValuesForMasking() ([]SecretValuesForMasking, error) {
	rows, allows, err := s.snapshotSecrets()
	if err != nil {
		return nil, err
	}
	indexByID := make(map[string]int, len(rows))
	values := make([]SecretValuesForMasking, 0, len(rows))
	for _, row := range rows {
		indexByID[row.ID] = len(values)
		values = append(values, SecretValuesForMasking{
			Name:     row.Name,
			Current:  row.Value,
			Previous: row.PreviousValue,
		})
	}
	for _, allow := range allows {
		index, exists := indexByID[allow.SecretID]
		if !exists {
			continue
		}
		values[index].WritableServerIDs = append(values[index].WritableServerIDs, allow.ServerID)
	}
	return values, nil
}

// SecretServerScope 是机密项可见列表的服务器范围：客户端为 profile 授予
// 的服务器集合，单机与权威端为全部服务器。
type SecretServerScope struct {
	all       bool
	serverIDs map[string]bool
}

// ScopeAllServers 返回覆盖全部服务器的范围，供单机与权威端使用。
func ScopeAllServers() SecretServerScope {
	return SecretServerScope{all: true}
}

// ScopeToServers 返回只包含给定服务器条目标识符的范围，供客户端按
// profile 授予的服务器集合裁剪。
func ScopeToServers(serverIDs ...string) SecretServerScope {
	ids := make(map[string]bool, len(serverIDs))
	for _, id := range serverIDs {
		if id != "" {
			ids[id] = true
		}
	}
	return SecretServerScope{serverIDs: ids}
}

// containsServerID 判断一个服务器条目标识符是否在服务器范围内。
func (scope SecretServerScope) containsServerID(serverID string) bool {
	return scope.all || scope.serverIDs[serverID]
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
		return fmt.Errorf("机密项值太短：至少需要 %d 个字节；短值无法可靠参与后续的输出遮蔽", secretValueMinBytes)
	case n > secretValueMaxBytes:
		return fmt.Errorf("机密项值太大：最多 %d 个字节", secretValueMaxBytes)
	default:
		return nil
	}
}

// commitSecretChange 提交会改变机密项值集合的事务；提交成功后递增进程内
// 变化计数，让后台遮蔽器立即察觉本进程的改动。
func (s *Store) commitSecretChange(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	s.secretValuesForMaskingVersion.Add(1)
	return nil
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
	ts := s.nowTime()
	id := newID()
	if _, err := tx.Exec(
		`INSERT INTO secrets (id,name,name_lower,note,value_blob,previous_value_blob,value_version,created_at,value_updated_at)
		 VALUES (?,?,?,?,?,?,1,?,?)`,
		id, name, strings.ToLower(name), note, blob, nil, ts.Unix(), ts.Unix()); err != nil {
		return err
	}
	if err := writeSecretAuditTx(tx, ts, "secret-add", map[string]any{"name": name}); err != nil {
		return err
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
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
	ts := s.nowTime()
	if _, err := tx.Exec(
		`UPDATE secrets SET previous_value_blob=?, value_blob=?, value_version=?, value_updated_at=? WHERE name=?`,
		oldBlob, blob, version+1, ts.Unix(), name); err != nil {
		return err
	}
	if err := writeSecretAuditTx(tx, ts, "secret-set", map[string]any{"name": name, "change": "值已更改"}); err != nil {
		return err
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
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
	if err := writeSecretAuditTx(tx, s.nowTime(), "secret-edit", map[string]any{"name": name, "change": "备注已更改"}); err != nil {
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
	if err := writeSecretAuditTx(tx, s.nowTime(), "secret-rm", map[string]any{"name": name}); err != nil {
		return err
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
}

// ListSecretsInScope 列出服务器范围内的机密项可见列表。指定集合时只返回
// 交集非空的机密项；全部服务器范围保留空列表，供 owner 在本机排查还没有
// 配置可写入服务器的机密项。
func (s *Store) ListSecretsInScope(scope SecretServerScope) ([]SecretView, error) {
	rows, err := s.db.Query(`
		SELECT sec.id, sec.name, sec.note, sec.created_at, sec.value_updated_at,
		       srv.id, srv.name
		FROM secrets sec
		LEFT JOIN secret_server_allow allow ON allow.secret_id = sec.id
		LEFT JOIN servers srv ON srv.id = allow.server_id
		ORDER BY sec.name, srv.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	displayRows := make([]secretDisplayRow, 0)
	for rows.Next() {
		var row secretDisplayRow
		var secretID string
		var serverID, serverName sql.NullString
		if err := rows.Scan(&secretID, &row.view.Name, &row.view.Note, &row.view.CreatedAt, &row.view.ValueUpdatedAt,
			&serverID, &serverName); err != nil {
			return nil, err
		}
		row.secretID = secretID
		if serverID.Valid && serverName.Valid {
			row.hasServer = true
			row.serverID = serverID.String
			row.serverName = serverName.String
		}
		displayRows = append(displayRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return secretViewsForDisplayRows(displayRows, scope), nil
}

// secretDisplayRow 是机密项可见列表的一行原始材料：机密项本身与一条可写入
// 服务器关联。数据库读取与快照读取都先转换成它，再走同一个授权判定。
type secretDisplayRow struct {
	secretID   string
	view       SecretView
	hasServer  bool
	serverID   string
	serverName string
}

// scopedSecretServer 是一个机密项在服务器范围内的一台可写入服务器：条目
// 标识符与名字都保留，列表路径用名字，快照裁剪路径用标识符。
type scopedSecretServer struct {
	id   string
	name string
}

// secretServersInScope 是机密项按服务器范围裁剪的唯一判定：给定机密项与
// 可写入服务器关联的原始行，算出每个机密项在范围内还能写入哪些服务器。
// 指定服务器集合时交集为空的机密项不出现在结果里；全部服务器范围时空
// 列表的机密项仍出现，供 owner 排查尚未配置可写入服务器的机密项。
func secretServersInScope(rows []secretDisplayRow, scope SecretServerScope) map[string][]scopedSecretServer {
	all := make(map[string][]scopedSecretServer, len(rows))
	for _, row := range rows {
		if _, exists := all[row.secretID]; !exists {
			all[row.secretID] = nil
		}
		if row.hasServer && scope.containsServerID(row.serverID) {
			all[row.secretID] = append(all[row.secretID], scopedSecretServer{id: row.serverID, name: row.serverName})
		}
	}
	if scope.all {
		return all
	}
	inScope := make(map[string][]scopedSecretServer, len(all))
	for secretID, servers := range all {
		if len(servers) > 0 {
			inScope[secretID] = servers
		}
	}
	return inScope
}

// secretViewsForDisplayRows 把共用范围判定组装成机密项可见列表：按名字排
// 序，每个机密项内的可写入服务器条目名也按名字排序。
func secretViewsForDisplayRows(rows []secretDisplayRow, scope SecretServerScope) []SecretView {
	viewsByID := make(map[string]SecretView, len(rows))
	for _, row := range rows {
		if _, exists := viewsByID[row.secretID]; !exists {
			viewsByID[row.secretID] = row.view
		}
	}

	inScope := secretServersInScope(rows, scope)
	scoped := make([]SecretView, 0, len(inScope))
	for secretID, view := range viewsByID {
		servers, exists := inScope[secretID]
		if !exists {
			continue
		}
		view.AllowedServers = make([]string, 0, len(servers))
		for _, server := range servers {
			view.AllowedServers = append(view.AllowedServers, server.name)
		}
		sort.Strings(view.AllowedServers)
		scoped = append(scoped, view)
	}
	sort.Slice(scoped, func(i, j int) bool { return scoped[i].Name < scoped[j].Name })
	return scoped
}

// FilterSecretsByQuery 按文本筛选机密项可见列表：名字或备注包含该文本
// （不区分大小写的子串匹配）的机密项保留。空文本返回全部条目。
func FilterSecretsByQuery(views []SecretView, query string) []SecretView {
	needle := strings.ToLower(query)
	out := make([]SecretView, 0, len(views))
	for _, view := range views {
		if strings.Contains(strings.ToLower(view.Name), needle) ||
			strings.Contains(strings.ToLower(view.Note), needle) {
			out = append(out, view)
		}
	}
	return out
}

// FilterSecretsByServer 按服务器条目名筛选机密项可见列表：只保留可写入
// 服务器列表包含该条目名的机密项。空名字返回全部条目。
func FilterSecretsByServer(views []SecretView, serverName string) []SecretView {
	if serverName == "" {
		return views
	}
	out := make([]SecretView, 0, len(views))
	for _, view := range views {
		for _, name := range view.AllowedServers {
			if name == serverName {
				out = append(out, view)
				break
			}
		}
	}
	return out
}

// SecretViewsFromSnapshot 从快照生成机密项可见列表。客户端快照本身已经按
// profile 裁剪过，因此这里以快照内的全部服务器为范围；这个入口与数据库
// 读取共用同一段服务器范围判定。
func SecretViewsFromSnapshot(snapshot *Snapshot) []SecretView {
	rows := make([]secretDisplayRow, 0, len(snapshot.Secrets)+len(snapshot.SecretAllows))
	secretIDs := make(map[string]bool, len(snapshot.Secrets))
	for _, secret := range snapshot.Secrets {
		secretIDs[secret.ID] = true
		rows = append(rows, secretDisplayRow{
			secretID: secret.ID,
			view: SecretView{
				Name:           secret.Name,
				Note:           secret.Note,
				CreatedAt:      secret.CreatedAt,
				ValueUpdatedAt: secret.ValueUpdatedAt,
			},
		})
	}
	serverNames := make(map[string]string, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		serverNames[server.ID] = server.Name
	}
	for _, allow := range snapshot.SecretAllows {
		if !secretIDs[allow.SecretID] {
			// 防御：快照缺了被引用的机密项时跳过该行，不生成空名字条目。
			continue
		}
		name, exists := serverNames[allow.ServerID]
		if !exists {
			continue
		}
		rows = append(rows, secretDisplayRow{
			secretID: allow.SecretID, hasServer: true,
			serverID: allow.ServerID, serverName: name,
		})
	}
	return secretViewsForDisplayRows(rows, ScopeAllServers())
}

// ResolveServerInSnapshot resolves a server identifier inside a snapshot's
// server entries: an exact server-id match wins, then a name match; nil means
// no match. A client snapshot only carries granted entries, so out-of-scope
// and unknown identifiers are equally "not found" here.
func ResolveServerInSnapshot(snapshot *Snapshot, identifier string) *SnapshotServer {
	if snapshot == nil || identifier == "" {
		return nil
	}
	for i := range snapshot.Servers {
		if snapshot.Servers[i].ID == identifier {
			return &snapshot.Servers[i]
		}
	}
	for i := range snapshot.Servers {
		if snapshot.Servers[i].Name == identifier {
			return &snapshot.Servers[i]
		}
	}
	return nil
}

// secretAllowTarget 是一次授权改动的目标：机密项或服务器条目的内部标识符
// 与名字。
type secretAllowTarget struct {
	id   string
	name string
}

// secretAllowDeletion 是服务器条目删除时受影响的机密项：内部标识符与名字
// 供补写审计使用，before 是删除前的完整可写入列表。
type secretAllowDeletion struct {
	id     string
	name   string
	before string
}

// AllowSecrets 把若干服务器条目加入若干机密项的可写入列表。先校验全部名字，
// 任何一个不存在就一次列出全部错误并整体不生效；校验通过后在一个事务内完成
// 全部改动，并为每个发生变化的机密项写一条审计行。
func (s *Store) AllowSecrets(secretNames, serverNames []string) error {
	return s.changeSecretAllows(secretNames, serverNames, true)
}

// DisallowSecrets 把若干服务器条目移出若干机密项的可写入列表。校验、事务
// 与审计语义与 AllowSecrets 相同。
func (s *Store) DisallowSecrets(secretNames, serverNames []string) error {
	return s.changeSecretAllows(secretNames, serverNames, false)
}

// changeSecretAllows 是 allow / disallow 的共用主体。
func (s *Store) changeSecretAllows(secretNames, serverNames []string, allow bool) error {
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
	auditAt := s.nowTime()
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
		if err := writeSecretAuditTx(tx, auditAt, action, summary); err != nil {
			return err
		}
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
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

// secretAllowsBeforeServerDelete 在删除服务器条目前读出受影响的机密项列表，
// 供删除后按内部标识符补写审计。
func secretAllowsBeforeServerDelete(tx *sql.Tx, serverID string) ([]secretAllowDeletion, error) {
	rows, err := tx.Query(`
		SELECT DISTINCT sec.id, sec.name FROM secret_server_allow allow
		JOIN secrets sec ON sec.id = allow.secret_id
		WHERE allow.server_id=? ORDER BY sec.name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var affected []secretAllowDeletion
	for rows.Next() {
		var target secretAllowDeletion
		if err := rows.Scan(&target.id, &target.name); err != nil {
			return nil, err
		}
		affected = append(affected, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range affected {
		list, err := secretAllowServerNames(tx, affected[i].id)
		if err != nil {
			return nil, err
		}
		affected[i].before = strings.Join(list, ",")
	}
	return affected, nil
}

// writeServerDeleteSecretAudits 在删除服务器条目的同一事务里，为每个受影响的
// 机密项补写一条取消授权审计，摘要标明原因是条目被删除。
func writeServerDeleteSecretAudits(tx *sql.Tx, affected []secretAllowDeletion, at time.Time) error {
	sort.Slice(affected, func(i, j int) bool { return affected[i].name < affected[j].name })
	for _, secret := range affected {
		after, err := secretAllowServerNames(tx, secret.id)
		if err != nil {
			return err
		}
		if err := writeSecretAuditTx(tx, at, "secret-disallow", map[string]any{
			"name":   secret.name,
			"before": secret.before,
			"after":  strings.Join(after, ","),
			"reason": "服务器条目已删除",
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeSecretAuditTx 在机密项数据改动的同一事务里写一条 owner 审计行。摘要
// 只含调用方传入的白名单字段；任何路径都不得把值放进摘要。
func writeSecretAuditTx(tx *sql.Tx, at time.Time, action string, summary map[string]any) error {
	return writeAuditTx(tx, AuditRow{
		TS:      at,
		Action:  action,
		Command: MarshalAuditSummary(summary),
		Status:  "ok",
	})
}
