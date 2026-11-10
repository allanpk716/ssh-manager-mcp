package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
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
	ErrSecretNameInvalid               = errors.New("机密项名字不合法：只能使用字母、数字、下划线，长度 1 到 64，且首字符不能是数字")
	ErrSecretNameExists                = errors.New("机密项名字已存在（大小写不同的名字视为同一个）")
	ErrSecretNotFound                  = errors.New("机密项不存在")
	ErrSecretEnvironmentNameInvalid    = errors.New("机密项环境变量名不合法：格式要求为字母或下划线开头，后续为字母、数字或下划线")
	ErrSecretEnvironmentNameExists     = errors.New("机密项环境变量名已被其他机密项使用（大小写不同视为同一个）")
	ErrSecretEnvironmentNameReserved   = errors.New("机密项环境变量名在固定拒绝清单中")
	ErrSecretReferencedBySharedStorage = errors.New("机密项仍被共享存储引用")
	ErrSecretValueEmpty                = errors.New("机密项值不能为空")
	ErrSecretValueTooShort             = fmt.Errorf("机密项值太短：至少需要 %d 个字节；短值无法可靠参与后续的输出遮蔽", secretValueMinBytes)
	ErrSecretValueTooLarge             = fmt.Errorf("机密项值太大：最多 %d 个字节", secretValueMaxBytes)
)

// IsSecretValidationError 判断错误是否属于机密项输入校验失败。这类错误
// 的文本只描述规则本身，可以原样展示给操作者；其余错误可能带内部细节，
// 调用方必须改用通用文案。
func IsSecretValidationError(err error) bool {
	switch {
	case errors.Is(err, ErrSecretNameInvalid),
		errors.Is(err, ErrSecretNameExists),
		errors.Is(err, ErrSecretEnvironmentNameInvalid),
		errors.Is(err, ErrSecretEnvironmentNameExists),
		errors.Is(err, ErrSecretEnvironmentNameReserved),
		errors.Is(err, ErrSecretValueEmpty),
		errors.Is(err, ErrSecretValueTooShort),
		errors.Is(err, ErrSecretValueTooLarge):
		return true
	default:
		return false
	}
}

// SecretView 是机密项的对外可见形态：只有名字、备注、环境变量名、可写入
// 服务器条目名与两个时间。当前值、上一个值、值的哈希、长度与版本号都不
// 进入这个结构，让「读不回值」成为类型层面的约束，而不只是界面的约束。
type SecretView struct {
	ID              string
	Name            string
	Note            string
	EnvironmentName string
	AllowedServers  []string
	CreatedAt       int64
	ValueUpdatedAt  int64
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

// SecretMetadataEdit 表示一次机密项元数据修改。指针为空表示不改这个字段；
// 环境变量名指针指向空字符串表示清除环境变量名。
type SecretMetadataEdit struct {
	Note            *string
	EnvironmentName *string
}

// SecretValuesForMasking 读取全部机密项的当前值、上一个值与可写入服务器
// 条目标识符。任何一个值无法解密都返回错误，让调用方拒绝放行未遮蔽输出；
// 该读取路径只服务本进程内的输出遮蔽，调用方不得把返回值序列化到工具
// 结果、日志或审计。
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
		secretID, serverID := allow.SecretID, allow.ServerID
		if index, exists := indexByID[secretID]; exists {
			values[index].WritableServerIDs = append(values[index].WritableServerIDs, serverID)
		}
	}
	return values, nil
}

// SecretValuesForCommandMasking 只供带环境变量注入计划的执行工具使用。这
// 类工具按规格必须在一个变量读取失败时继续执行并返回未注入警告，因此这里
// 跳过无法解密的值，让其他可用值继续参与输出遮蔽。其他调用方必须使用
// SecretValuesForMasking 的失败即报错行为。
func (s *Store) SecretValuesForCommandMasking() ([]SecretValuesForMasking, error) {
	rows, err := s.db.Query(`
		SELECT id, name, value_blob, COALESCE(previous_value_blob,'')
		FROM secrets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indexByID := make(map[string]int)
	values := make([]SecretValuesForMasking, 0)
	for rows.Next() {
		var id, name string
		var currentBlob, previousBlob []byte
		if err := rows.Scan(&id, &name, &currentBlob, &previousBlob); err != nil {
			return nil, err
		}
		current, openErr := open(s.masterKey, currentBlob)
		if openErr != nil {
			continue
		}
		var previous []byte
		if len(previousBlob) > 0 {
			if previous, openErr = open(s.masterKey, previousBlob); openErr != nil {
				previous = nil
			}
		}
		indexByID[id] = len(values)
		values = append(values, SecretValuesForMasking{
			Name: name, Current: current, Previous: previous,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	allows, err := s.db.Query(`SELECT secret_id, server_id FROM secret_server_allow ORDER BY secret_id, server_id`)
	if err != nil {
		return nil, err
	}
	defer allows.Close()
	for allows.Next() {
		var secretID, serverID string
		if err := allows.Scan(&secretID, &serverID); err != nil {
			return nil, err
		}
		if index, exists := indexByID[secretID]; exists {
			values[index].WritableServerIDs = append(values[index].WritableServerIDs, serverID)
		}
	}
	return values, allows.Err()
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

// secretEnvironmentExactDenyList 是环境变量名的固定精确拒绝清单。这些名字
// 会改变目标机命令环境的基础语义，因此任何机密项都不能使用。
var secretEnvironmentExactDenyList = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {}, "PWD": {},
	"OLDPWD": {}, "IFS": {}, "ENV": {}, "BASH_ENV": {}, "CDPATH": {}, "PS1": {},
	"PS2": {}, "PS3": {}, "PS4": {}, "PROMPT_COMMAND": {}, "TERM": {}, "LANG": {},
	"TZ": {}, "TMPDIR": {}, "MAIL": {}, "HOSTNAME": {}, "PATHEXT": {}, "COMSPEC": {},
	"SYSTEMROOT": {}, "SYSTEMDRIVE": {}, "WINDIR": {}, "USERPROFILE": {}, "USERNAME": {},
	"USERDOMAIN": {}, "APPDATA": {}, "LOCALAPPDATA": {}, "PROGRAMDATA": {},
	"PROGRAMFILES": {}, "TEMP": {}, "TMP": {}, "HOMEDRIVE": {}, "HOMEPATH": {},
	"PSMODULEPATH": {}, "PROMPT": {}, "PROCESSOR_ARCHITECTURE": {},
}

// secretEnvironmentDenyPrefixes 是环境变量名的固定前缀拒绝清单。相关名字会
// 影响动态链接、SSH、提权或 shell 函数等安全边界。
var secretEnvironmentDenyPrefixes = []string{
	"LD_", "DYLD_", "SSH_", "SSHMGR_", "SUDO_", "BASH_FUNC_", "LC_",
}

// ValidateSecretEnvironmentName 校验机密项环境变量名。空值表示清除，由专管
// 方法单独接受；非空值必须符合格式规则且不在固定拒绝清单中。
func ValidateSecretEnvironmentName(name string) error {
	if name == "" {
		return nil
	}
	if !models.EnvironmentVariableNameFormatIsValid(name) {
		return fmt.Errorf("%w：%s", ErrSecretEnvironmentNameInvalid, name)
	}
	lower := strings.ToLower(name)
	if _, exists := secretEnvironmentExactDenyList[strings.ToUpper(lower)]; exists {
		return fmt.Errorf("%w：精确名 %s", ErrSecretEnvironmentNameReserved, name)
	}
	for _, prefix := range secretEnvironmentDenyPrefixes {
		if strings.HasPrefix(lower, strings.ToLower(prefix)) {
			return fmt.Errorf("%w：拒绝前缀 %s，变量名 %s", ErrSecretEnvironmentNameReserved, prefix, name)
		}
	}
	return nil
}

// validateSecretValue 按原始字节校验值。空值、短于 8 个字节、超过 64 KiB
// 都拒绝；末尾换行计入长度。
func validateSecretValue(value []byte) error {
	switch n := len(value); {
	case n == 0:
		return ErrSecretValueEmpty
	case n < secretValueMinBytes:
		return ErrSecretValueTooShort
	case n > secretValueMaxBytes:
		return ErrSecretValueTooLarge
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
func (s *Store) AddSecret(name, note string, value []byte, source WriteSource) error {
	return s.AddSecretWithEnvironmentName(name, note, value, "", source)
}

// AddSecretWithEnvironmentName 新增一个带环境变量名的机密项。名字、值与
// 环境变量名全部校验通过后才落库，避免“机密项已新增但变量名失败”的半成品。
func (s *Store) AddSecretWithEnvironmentName(name, note string, value []byte, environmentName string, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	if err := ValidateSecretEnvironmentName(environmentName); err != nil {
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
	if environmentName != "" {
		var existingSecret string
		err = tx.QueryRow(
			`SELECT name FROM secrets WHERE env_name_lower=?`,
			strings.ToLower(environmentName),
		).Scan(&existingSecret)
		if err == nil {
			return secretEnvironmentNameExistsError(existingSecret, environmentName)
		}
		if err != sql.ErrNoRows {
			return err
		}
	}
	ts := s.nowTime()
	id := newID()
	if _, err := tx.Exec(
		`INSERT INTO secrets (id,name,name_lower,note,env_name,env_name_lower,value_blob,previous_value_blob,value_version,created_at,value_updated_at)
		 VALUES (?,?,?,?,?,?,?,?,1,?,?)`,
		id, name, strings.ToLower(name), note, environmentName, strings.ToLower(environmentName), blob, nil, ts.Unix(), ts.Unix()); err != nil {
		return err
	}
	if err := writeSecretAuditTx(tx, ts, "secret-add", map[string]any{"name": name}, source); err != nil {
		return err
	}
	if environmentName != "" {
		if err := writeSecretAuditTx(tx, ts, "secret-env", map[string]any{
			"name": name, "before": "", "after": environmentName,
		}, source); err != nil {
			return err
		}
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
}

// SetSecretEnvironmentName 设置或清除一个机密项的环境变量名。空值表示清
// 除；改名前会重新检查格式、拒绝清单与全库唯一性。
func (s *Store) SetSecretEnvironmentName(secretName, environmentName string, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := ValidateSecretEnvironmentName(environmentName); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var secretID, before string
	err = tx.QueryRow(`SELECT id, COALESCE(env_name, '') FROM secrets WHERE name=?`, secretName).Scan(&secretID, &before)
	if err == sql.ErrNoRows {
		return ErrSecretNotFound
	}
	if err != nil {
		return err
	}
	desiredLower := strings.ToLower(environmentName)
	if desiredLower != "" {
		var existingSecret string
		err = tx.QueryRow(
			`SELECT name FROM secrets WHERE env_name_lower=? AND id<>?`,
			desiredLower, secretID,
		).Scan(&existingSecret)
		if err == nil {
			return secretEnvironmentNameExistsError(existingSecret, environmentName)
		}
		if err != sql.ErrNoRows {
			return err
		}
	}
	if environmentName == before {
		return tx.Rollback()
	}
	if _, err := tx.Exec(
		`UPDATE secrets SET env_name=?, env_name_lower=? WHERE id=?`,
		environmentName, desiredLower, secretID,
	); err != nil {
		return err
	}
	if err := writeSecretAuditTx(tx, s.nowTime(), "secret-env", map[string]any{
		"name": secretName, "before": before, "after": environmentName,
	}, source); err != nil {
		return err
	}
	return s.commitSecretChange(tx)
}

// EditSecretMetadata 在同一个事务里修改备注与环境变量名。任一字段失败时
// 两者都保持原值；审计行为与单独修改保持一致，不记录备注原文或变量值。
func (s *Store) EditSecretMetadata(secretName string, edit SecretMetadataEdit, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if edit.Note == nil && edit.EnvironmentName == nil {
		return nil
	}
	if edit.EnvironmentName != nil {
		if err := ValidateSecretEnvironmentName(*edit.EnvironmentName); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var secretID, beforeNote, beforeEnvironmentName string
	err = tx.QueryRow(
		`SELECT id, COALESCE(note,''), COALESCE(env_name,'') FROM secrets WHERE name=?`,
		secretName,
	).Scan(&secretID, &beforeNote, &beforeEnvironmentName)
	if err == sql.ErrNoRows {
		return ErrSecretNotFound
	}
	if err != nil {
		return err
	}

	desiredNote := beforeNote
	if edit.Note != nil {
		desiredNote = *edit.Note
	}
	desiredEnvironmentName := beforeEnvironmentName
	if edit.EnvironmentName != nil {
		desiredEnvironmentName = *edit.EnvironmentName
	}
	if desiredEnvironmentName != "" {
		desiredLower := strings.ToLower(desiredEnvironmentName)
		var existingSecret string
		err = tx.QueryRow(
			`SELECT name FROM secrets WHERE env_name_lower=? AND id<>?`,
			desiredLower, secretID,
		).Scan(&existingSecret)
		if err == nil {
			return secretEnvironmentNameExistsError(existingSecret, desiredEnvironmentName)
		}
		if err != sql.ErrNoRows {
			return err
		}
	}

	var result sql.Result
	switch {
	case edit.Note != nil && edit.EnvironmentName != nil:
		result, err = tx.Exec(
			`UPDATE secrets SET note=?, env_name=?, env_name_lower=? WHERE id=?`,
			desiredNote, desiredEnvironmentName, strings.ToLower(desiredEnvironmentName), secretID,
		)
	case edit.Note != nil:
		result, err = tx.Exec(`UPDATE secrets SET note=? WHERE id=?`, desiredNote, secretID)
	default:
		result, err = tx.Exec(
			`UPDATE secrets SET env_name=?, env_name_lower=? WHERE id=?`,
			desiredEnvironmentName, strings.ToLower(desiredEnvironmentName), secretID,
		)
	}
	if err != nil {
		return err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if rows == 0 {
		return ErrSecretNotFound
	}

	auditTime := s.nowTime()
	if edit.Note != nil {
		if err := writeSecretAuditTx(tx, auditTime, "secret-edit", map[string]any{
			"name": secretName, "change": "备注已更改",
		}, source); err != nil {
			return err
		}
	}
	environmentNameChanged := edit.EnvironmentName != nil && desiredEnvironmentName != beforeEnvironmentName
	if environmentNameChanged {
		if err := writeSecretAuditTx(tx, auditTime, "secret-env", map[string]any{
			"name": secretName, "before": beforeEnvironmentName, "after": desiredEnvironmentName,
		}, source); err != nil {
			return err
		}
		return s.commitSecretChange(tx)
	}
	return tx.Commit()
}

// secretEnvironmentNameExistsError 生成环境变量名重名报错，指明已占用它的
// 机密项，便于 owner 定位冲突对象。
func secretEnvironmentNameExistsError(existingSecret, environmentName string) error {
	return fmt.Errorf("%w：%s 使用 %s", ErrSecretEnvironmentNameExists, existingSecret, environmentName)
}

// SetSecretValue 覆盖机密项的值：旧值移入上一个值字段，值版本号加一，值的
// 最后更改时间更新；备注与可写入服务器列表不变。数据与审计行在同一事务
// 提交，审计摘要只写「值已更改」。
func (s *Store) SetSecretValue(name string, value []byte, source WriteSource) error {
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
	if err := writeSecretAuditTx(tx, ts, "secret-set", map[string]any{"name": name, "change": "值已更改"}, source); err != nil {
		return err
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
}

// EditSecretNote 只改备注。数据与审计行在同一事务提交，审计不记录备注原文。
func (s *Store) EditSecretNote(name, note string, source WriteSource) error {
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
	if err := writeSecretAuditTx(tx, s.nowTime(), "secret-edit", map[string]any{"name": name, "change": "备注已更改"}, source); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSecret 直接删除机密项行；关联的可写入服务器行由外键级联删除。数据
// 与审计行在同一事务提交。
func (s *Store) DeleteSecret(name string, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	shareNames, err := sharedStorageNamesForSecretTx(tx, name)
	if err != nil {
		return err
	}
	if len(shareNames) > 0 {
		return fmt.Errorf("%w：%s；请先删除这些共享存储，或把它们的挂载密码改成其他机密项",
			ErrSecretReferencedBySharedStorage, strings.Join(shareNames, "、"))
	}
	res, err := tx.Exec(`DELETE FROM secrets WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrSecretNotFound
	}
	if err := writeSecretAuditTx(tx, s.nowTime(), "secret-rm", map[string]any{"name": name}, source); err != nil {
		return err
	}
	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
}

// sharedStorageNamesForSecretTx 列出仍把某个机密项当挂载密码使用的共享
// 存储名字。删除机密项前先做这项检查，把数据库外键错误转换成 owner 可以
// 直接处理的说明。
func sharedStorageNamesForSecretTx(tx *sql.Tx, secretName string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT share.name
		FROM shared_storages share
		JOIN secrets secret ON secret.id=share.password_secret_id
		WHERE secret.name=? ORDER BY share.name,share.id`, secretName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ListSecretsInScope 列出服务器范围内的机密项可见列表。指定集合时只返回
// 交集非空的机密项；全部服务器范围保留空列表，供 owner 在本机排查还没有
// 配置可写入服务器的机密项。
func (s *Store) ListSecretsInScope(scope SecretServerScope) ([]SecretView, error) {
	rows, err := s.db.Query(`
		SELECT sec.id, sec.name, sec.note, sec.env_name, sec.created_at, sec.value_updated_at,
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
		if err := rows.Scan(&secretID, &row.view.Name, &row.view.Note, &row.view.EnvironmentName, &row.view.CreatedAt, &row.view.ValueUpdatedAt,
			&serverID, &serverName); err != nil {
			return nil, err
		}
		row.secretID = secretID
		row.view.ID = secretID
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
				ID:              secret.ID,
				Name:            secret.Name,
				Note:            secret.Note,
				EnvironmentName: secret.EnvironmentName,
				CreatedAt:       secret.CreatedAt,
				ValueUpdatedAt:  secret.ValueUpdatedAt,
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
func (s *Store) AllowSecrets(secretNames, serverNames []string, source WriteSource) error {
	return s.ChangeSecretAllowsForNames(secretNames, serverNames, true, source)
}

// DisallowSecrets 把若干服务器条目移出若干机密项的可写入列表。校验、事务
// 与审计语义与 AllowSecrets 相同。
func (s *Store) DisallowSecrets(secretNames, serverNames []string, source WriteSource) error {
	return s.ChangeSecretAllowsForNames(secretNames, serverNames, false, source)
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
		}, WriteSource{}); err != nil {
			return err
		}
	}
	return nil
}

// writeSecretAuditTx 在机密项数据改动的同一事务里写一条 owner 审计行。摘要
// 只含调用方传入的白名单字段；任何路径都不得把值放进摘要。
func writeSecretAuditTx(tx *sql.Tx, at time.Time, action string, summary map[string]any, source WriteSource) error {
	return writeAuditTx(tx, AuditRow{
		TS:       at,
		Action:   action,
		Command:  MarshalAuditSummary(summary),
		Status:   "ok",
		Source:   source.kind,
		SourceIP: source.address,
	})
}
