package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// UploadContentDefaultLimitBytes 是 upload_content 未设置环境变量时使用的
// 解码后内容上限。下发记录模板原文默认使用同一个上限；调用方解析出不同的
// 生效上限时，必须在登记请求里显式传入。
const UploadContentDefaultLimitBytes int64 = 8 << 20 // 8 MiB

// SecretDeliverySecret 是登记请求里一个机密项的名字与当时写入的值版本号。
type SecretDeliverySecret struct {
	Name         string
	ValueVersion int64
}

// SecretDeliveryTemplateTooLargeError 表示带占位符的模板原文超过调用方
// 传入的 upload_content 生效上限。
type SecretDeliveryTemplateTooLargeError struct {
	Size  int64
	Limit int64
}

// ErrSecretDeliveryInvalidPath 表示登记路径不是目标操作系统上的绝对路径。
var ErrSecretDeliveryInvalidPath = errors.New("delivery path must be absolute")

func (e *SecretDeliveryTemplateTooLargeError) Error() string {
	return fmt.Sprintf("template (%d bytes) exceeds the upload content limit (%d bytes)", e.Size, e.Limit)
}

// SecretDeliveryRecordInput 是下发记录登记口的完整输入。调用方保证模板
// 原文不携带解密后的机密项值；授权范围使用发起客户端绑定或本机会话所属
// 的 profile 编号。
type SecretDeliveryRecordInput struct {
	ServerID               string
	Path                   string
	Template               []byte
	FileSHA256             string
	Secrets                []SecretDeliverySecret
	WrittenAt              time.Time
	InitiatingDeviceName   string
	InitiatingInstanceName string
	AuthorizationProfileID string
	// TemplateLimitBytes 是调用方解析出的 upload_content 生效上限；空值或
	// 非正数表示使用默认上限。存储层不读取环境变量，避免同一进程内两处
	// 解析结果不一致。
	TemplateLimitBytes int64
}

// SecretDeliverySecretRecord 是查询结果里的机密项引用。SecretID 只用于后
// 续判断机密项是否仍存在；机密项删除后记录仍按原名字返回。
type SecretDeliverySecretRecord struct {
	SecretID     string
	Name         string
	ValueVersion int64
	Deleted      bool
}

// SecretDeliveryRecord 是权威 vault 保留的最新一条下发记录。
type SecretDeliveryRecord struct {
	ServerID               string
	ServerName             string
	Path                   string
	Template               []byte
	FileSHA256             string
	Secrets                []SecretDeliverySecretRecord
	WrittenAt              time.Time
	InitiatingDeviceName   string
	InitiatingInstanceName string
	RecordedAt             time.Time
}

// SecretDeliveryRecordFilter 支持按机密项名字、按服务器条目或两者同时查询。
type SecretDeliveryRecordFilter struct {
	SecretName string
	ServerID   string
}

// SecretDeliveryRecordResult 返回登记结果与本次是否替换了同路径旧记录。
type SecretDeliveryRecordResult struct {
	Replaced bool
	Record   SecretDeliveryRecord
}

// SecretDeliveryRefusalReason 是授权校验拒绝的稳定机器可读原因，供客户端
// 呈现层区分失败类别。
type SecretDeliveryRefusalReason string

const (
	SecretDeliveryRefusalServerNotInProfile SecretDeliveryRefusalReason = "server_not_in_profile"
	SecretDeliveryRefusalSecretNotWritable  SecretDeliveryRefusalReason = "secret_not_writable"
	SecretDeliveryRefusalDeviceCodeNotBound SecretDeliveryRefusalReason = "device_code_not_bound"
)

// SecretDeliveryRefusalError 表示登记请求本身已解析成功，但目标服务器不在
// 发起授权范围内，或不在某个引用机密项的可写入服务器列表内。
type SecretDeliveryRefusalError struct {
	Reason     SecretDeliveryRefusalReason
	ServerID   string
	ServerName string
	SecretName string
	Path       string
}

func (e *SecretDeliveryRefusalError) Error() string {
	switch e.Reason {
	case SecretDeliveryRefusalServerNotInProfile:
		return fmt.Sprintf("server %q is not granted to the initiating profile", e.ServerName)
	case SecretDeliveryRefusalSecretNotWritable:
		return fmt.Sprintf("secret %q is not writable on server %q", e.SecretName, e.ServerName)
	case SecretDeliveryRefusalDeviceCodeNotBound:
		return "device code is absent, revoked, or not bound to a profile"
	default:
		return string(e.Reason)
	}
}

// RecordSecretDelivery 在单个事务里校验授权、替换最新记录并写审计行。
func (s *Store) RecordSecretDelivery(input SecretDeliveryRecordInput, source WriteSource) (SecretDeliveryRecordResult, error) {
	if s.readOnly {
		return SecretDeliveryRecordResult{}, ErrReadOnly
	}
	if err := validateSecretDeliveryInput(input, source); err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	defer tx.Rollback()

	var serverName, shellType string
	err = tx.QueryRow(
		`SELECT name,shell_type FROM servers WHERE id=?`, input.ServerID,
	).Scan(&serverName, &shellType)
	if errors.Is(err, sql.ErrNoRows) {
		return SecretDeliveryRecordResult{}, fmt.Errorf("server %q not found", input.ServerID)
	}
	if err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	targetIsWindows := models.NormalizeShellType(shellType).IsWindows()
	path, pathKey := normalizeSecretDeliveryPath(input.Path, targetIsWindows)
	if !secretDeliveryPathIsAbsolute(path, targetIsWindows) {
		return SecretDeliveryRecordResult{}, ErrSecretDeliveryInvalidPath
	}
	authorizationProfileID := input.AuthorizationProfileID
	if source.kind == AuditSourceForward {
		var initiatingInstanceName string
		err = tx.QueryRow(`
			SELECT COALESCE(cache_tokens.profile_id,''),cache_tokens.name,
			       COALESCE(devices.name,cache_tokens.name)
			FROM cache_tokens
			LEFT JOIN devices ON devices.client_token_id=cache_tokens.id
			WHERE cache_tokens.id=? AND cache_tokens.status='active'`,
			source.deviceCode,
		).Scan(&authorizationProfileID, &initiatingInstanceName, &input.InitiatingDeviceName)
		if errors.Is(err, sql.ErrNoRows) {
			return s.refuseSecretDeliveryTx(tx, input, serverName, path, "", SecretDeliveryRefusalDeviceCodeNotBound, source)
		}
		if err != nil {
			return SecretDeliveryRecordResult{}, err
		}
		if authorizationProfileID == "" {
			return s.refuseSecretDeliveryTx(tx, input, serverName, path, "", SecretDeliveryRefusalDeviceCodeNotBound, source)
		}
		input.InitiatingInstanceName = initiatingInstanceName
	}

	var profileGranted int
	err = tx.QueryRow(
		`SELECT 1 FROM profile_servers WHERE profile_id=? AND server_id=?`,
		authorizationProfileID, input.ServerID,
	).Scan(&profileGranted)
	if errors.Is(err, sql.ErrNoRows) {
		return s.refuseSecretDeliveryTx(tx, input, serverName, path, "", SecretDeliveryRefusalServerNotInProfile, source)
	}
	if err != nil {
		return SecretDeliveryRecordResult{}, err
	}

	refs := make([]SecretDeliverySecretRecord, 0, len(input.Secrets))
	for _, secret := range input.Secrets {
		var secretID, canonicalName string
		err = tx.QueryRow(
			`SELECT id,name FROM secrets WHERE name_lower=?`, strings.ToLower(secret.Name),
		).Scan(&secretID, &canonicalName)
		if errors.Is(err, sql.ErrNoRows) {
			return s.refuseSecretDeliveryTx(tx, input, serverName, path, secret.Name, SecretDeliveryRefusalSecretNotWritable, source)
		}
		if err != nil {
			return SecretDeliveryRecordResult{}, err
		}
		var allowed int
		err = tx.QueryRow(
			`SELECT 1 FROM secret_server_allow WHERE secret_id=? AND server_id=?`,
			secretID, input.ServerID,
		).Scan(&allowed)
		if errors.Is(err, sql.ErrNoRows) {
			return s.refuseSecretDeliveryTx(tx, input, serverName, path, secret.Name, SecretDeliveryRefusalSecretNotWritable, source)
		}
		if err != nil {
			return SecretDeliveryRecordResult{}, err
		}
		refs = append(refs, SecretDeliverySecretRecord{
			SecretID: secretID, Name: canonicalName, ValueVersion: secret.ValueVersion,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	var existingID string
	var oldPath string
	var oldTemplate []byte
	var oldDigest string
	var oldWrittenAt, oldRecordedAt int64
	var oldDevice, oldInstance string
	err = tx.QueryRow(
		`SELECT id,path,template,file_sha256,written_at,initiating_device_name,
		        initiating_instance_name,recorded_at
		 FROM secret_delivery_records WHERE server_id=? AND path_key=?`,
		input.ServerID, pathKey,
	).Scan(&existingID, &oldPath, &oldTemplate, &oldDigest, &oldWrittenAt,
		&oldDevice, &oldInstance, &oldRecordedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SecretDeliveryRecordResult{}, err
	}
	var oldRefs []SecretDeliverySecretRecord
	if existingID != "" {
		if oldRefs, err = secretDeliverySecretsTx(tx, existingID); err != nil {
			return SecretDeliveryRecordResult{}, err
		}
	}
	replaced := existingID != ""
	recordID := existingID
	recordedAt := s.nowTime()
	if recordID == "" {
		recordID = newID()
		if _, err = tx.Exec(`
			INSERT INTO secret_delivery_records(
				id,server_id,path,path_key,template,file_sha256,written_at,
				initiating_device_name,initiating_instance_name,recorded_at
			) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			recordID, input.ServerID, path, pathKey, input.Template, input.FileSHA256,
			input.WrittenAt.Unix(), input.InitiatingDeviceName, input.InitiatingInstanceName,
			recordedAt.Unix(),
		); err != nil {
			return SecretDeliveryRecordResult{}, err
		}
	} else {
		if _, err = tx.Exec(`
			UPDATE secret_delivery_records
			SET path=?,template=?,file_sha256=?,written_at=?,
			    initiating_device_name=?,initiating_instance_name=?,recorded_at=?
			WHERE id=?`,
			path, input.Template, input.FileSHA256, input.WrittenAt.Unix(),
			input.InitiatingDeviceName, input.InitiatingInstanceName, recordedAt.Unix(), recordID,
		); err != nil {
			return SecretDeliveryRecordResult{}, err
		}
		if _, err = tx.Exec(`DELETE FROM secret_delivery_record_secrets WHERE record_id=?`, recordID); err != nil {
			return SecretDeliveryRecordResult{}, err
		}
	}
	for _, ref := range refs {
		if _, err = tx.Exec(`
			INSERT INTO secret_delivery_record_secrets(record_id,secret_id,secret_name,value_version)
			VALUES(?,?,?,?)`, recordID, ref.SecretID, ref.Name, ref.ValueVersion,
		); err != nil {
			return SecretDeliveryRecordResult{}, err
		}
	}
	record := SecretDeliveryRecord{
		ServerID: input.ServerID, ServerName: serverName, Path: path,
		Template: append([]byte(nil), input.Template...), FileSHA256: input.FileSHA256,
		Secrets: refs, WrittenAt: input.WrittenAt,
		InitiatingDeviceName:   input.InitiatingDeviceName,
		InitiatingInstanceName: input.InitiatingInstanceName,
		RecordedAt:             recordedAt,
	}
	action := "secret-delivery-record"
	summary := map[string]any{
		"server": serverName, "path": path,
		"secrets": secretDeliveryJoinedNames(refs, func(ref SecretDeliverySecretRecord) string {
			return ref.Name
		}),
		"device": input.InitiatingDeviceName, "instance": input.InitiatingInstanceName,
	}
	if source.kind == AuditSourceForward {
		summary["device_code"] = source.deviceCode
	}
	if replaced {
		action = "secret-delivery-replace"
		summary["previous_path"] = oldPath
		summary["previous_sha256"] = oldDigest
		summary["previous_template"] = string(oldTemplate)
		summary["previous_secrets"] = secretDeliveryAuditReferences(oldRefs)
		summary["previous_written_at"] = oldWrittenAt
		summary["previous_device"] = oldDevice
		summary["previous_instance"] = oldInstance
		summary["previous_recorded_at"] = oldRecordedAt
	}
	if err := writeSecretAuditTx(tx, s.nowTime(), action, summary, source); err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	return SecretDeliveryRecordResult{Replaced: replaced, Record: record}, nil
}

func (s *Store) refuseSecretDeliveryTx(
	tx *sql.Tx,
	input SecretDeliveryRecordInput,
	serverName, path, secretName string,
	reason SecretDeliveryRefusalReason,
	source WriteSource,
) (SecretDeliveryRecordResult, error) {
	summary := map[string]any{
		"reason": string(reason), "server": serverName, "path": path,
		"secrets": secretDeliveryJoinedNames(input.Secrets, func(secret SecretDeliverySecret) string {
			return secret.Name
		}),
		"device": input.InitiatingDeviceName, "instance": input.InitiatingInstanceName,
	}
	if secretName != "" {
		summary["refused_secret"] = secretName
	}
	if source.kind == AuditSourceForward {
		summary["device_code"] = source.deviceCode
	}
	if err := writeAuditTx(tx, AuditRow{
		TS: s.nowTime(), Action: "secret-delivery-refused",
		Command: MarshalAuditSummary(summary), Status: "error",
		Source: source.kind, SourceIP: source.address,
	}); err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SecretDeliveryRecordResult{}, err
	}
	return SecretDeliveryRecordResult{}, &SecretDeliveryRefusalError{
		Reason: reason, ServerID: input.ServerID, ServerName: serverName,
		SecretName: secretName, Path: path,
	}
}

func validateSecretDeliveryInput(input SecretDeliveryRecordInput, source WriteSource) error {
	if input.ServerID == "" {
		return errors.New("server id is required")
	}
	if input.Path == "" {
		return errors.New("delivery path is required")
	}
	if len(input.Secrets) == 0 {
		return errors.New("delivery record must reference at least one secret")
	}
	if source.kind != AuditSourceForward && input.AuthorizationProfileID == "" {
		return errors.New("authorization profile is required")
	}
	if source.kind != AuditSourceForward &&
		(input.InitiatingDeviceName == "" || input.InitiatingInstanceName == "") {
		return errors.New("initiating device name and instance name are required")
	}
	seen := make(map[string]bool, len(input.Secrets))
	for _, secret := range input.Secrets {
		if secret.Name == "" {
			return errors.New("secret name is required")
		}
		key := strings.ToLower(secret.Name)
		if seen[key] {
			return fmt.Errorf("duplicate secret reference %q", secret.Name)
		}
		seen[key] = true
		if secret.ValueVersion <= 0 {
			return fmt.Errorf("secret %q must carry a positive value version", secret.Name)
		}
	}
	if input.WrittenAt.IsZero() {
		return errors.New("written-at time is required")
	}
	limit := input.TemplateLimitBytes
	if limit <= 0 {
		limit = UploadContentDefaultLimitBytes
	}
	if int64(len(input.Template)) > limit {
		return &SecretDeliveryTemplateTooLargeError{
			Size: int64(len(input.Template)), Limit: limit,
		}
	}
	if _, err := hex.DecodeString(input.FileSHA256); err != nil || len(input.FileSHA256) != sha256.Size*2 {
		return errors.New("file hash must be a hexadecimal SHA-256 digest")
	}
	return nil
}

func secretDeliveryJoinedNames[T any](items []T, name func(T) string) string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, name(item))
	}
	return strings.Join(names, ",")
}

func normalizeSecretDeliveryPath(path string, windows bool) (string, string) {
	if !windows {
		return path, path
	}
	normalized := strings.ReplaceAll(path, `\`, `/`)
	return normalized, strings.ToLower(normalized)
}

func secretDeliveryPathIsAbsolute(path string, windows bool) bool {
	if !windows {
		return strings.HasPrefix(path, "/")
	}
	return len(path) >= 3 &&
		((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && path[2] == '/'
}

func secretDeliveryAuditReferences(refs []SecretDeliverySecretRecord) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, fmt.Sprintf("%s@%d", ref.Name, ref.ValueVersion))
	}
	return strings.Join(parts, ",")
}

func secretDeliverySecretsTx(tx *sql.Tx, recordID string) ([]SecretDeliverySecretRecord, error) {
	rows, err := tx.Query(`
		SELECT secret_id,secret_name,value_version
		FROM secret_delivery_record_secrets WHERE record_id=? ORDER BY secret_name`,
		recordID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []SecretDeliverySecretRecord
	for rows.Next() {
		var ref SecretDeliverySecretRecord
		if err := rows.Scan(&ref.SecretID, &ref.Name, &ref.ValueVersion); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// SecretDeliveryRecords 按过滤器读取权威 vault 的最新下发记录。
func (s *Store) SecretDeliveryRecords(filter SecretDeliveryRecordFilter) ([]SecretDeliveryRecord, error) {
	rows, err := s.db.Query(`
		SELECT record.id,record.server_id,server.name,record.path,record.template,
		       record.file_sha256,record.written_at,record.initiating_device_name,
		       record.initiating_instance_name,record.recorded_at
		FROM secret_delivery_records record
		JOIN servers server ON server.id=record.server_id
		WHERE (?='' OR record.server_id=?)
		  AND (?='' OR EXISTS (
		      SELECT 1 FROM secret_delivery_record_secrets ref
		      WHERE ref.record_id=record.id AND lower(ref.secret_name)=lower(?)
		  ))
		ORDER BY server.name,record.path`, filter.ServerID, filter.ServerID, filter.SecretName, filter.SecretName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]SecretDeliveryRecord, 0)
	ids := make([]string, 0)
	for rows.Next() {
		var record SecretDeliveryRecord
		var recordID string
		var writtenAt, recordedAt int64
		if err := rows.Scan(
			&recordID, &record.ServerID, &record.ServerName, &record.Path, &record.Template,
			&record.FileSHA256, &writtenAt, &record.InitiatingDeviceName,
			&record.InitiatingInstanceName, &recordedAt,
		); err != nil {
			return nil, err
		}
		record.WrittenAt = time.Unix(writtenAt, 0)
		record.RecordedAt = time.Unix(recordedAt, 0)
		records = append(records, record)
		ids = append(ids, recordID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	err = attachSecretDeliveryReferences(s.db, records, ids, func(index int, refs []secretDeliveryReference) {
		records[index].Secrets = make([]SecretDeliverySecretRecord, 0, len(refs))
		for _, ref := range refs {
			records[index].Secrets = append(records[index].Secrets, SecretDeliverySecretRecord{
				SecretID: ref.SecretID, Name: ref.SecretName, ValueVersion: ref.ValueVersion,
				Deleted: !ref.Exists,
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// secretDeliveryReference 是关联表里一条下发记录机密项引用；Exists 供展示
// 层把已删除的机密项标出来。
type secretDeliveryReference struct {
	RecordID     string
	SecretID     string
	SecretName   string
	ValueVersion int64
	Exists       bool
}

// attachSecretDeliveryReferences 查出一批记录引用的机密项并挂回调用方给出
// 的记录切片。ids 与 records 按下标一一对应；assign 在公共查询和挂回循环
// 里是唯一适配不同记录形状的位置。
func attachSecretDeliveryReferences[T any](
	db dbtx, records []T, ids []string, assign func(index int, refs []secretDeliveryReference),
) error {
	if len(records) == 0 && len(ids) == 0 {
		return nil
	}
	if len(records) != len(ids) {
		return fmt.Errorf("delivery records and ids are misaligned: %d != %d", len(records), len(ids))
	}
	rows, err := db.Query(`
		SELECT ref.record_id,ref.secret_id,ref.secret_name,ref.value_version,
		       EXISTS(SELECT 1 FROM secrets secret WHERE secret.id=ref.secret_id)
		FROM secret_delivery_record_secrets ref
		WHERE ref.record_id IN (`+strings.Repeat("?,", len(ids)-1)+`?)
		ORDER BY ref.record_id,ref.secret_name`, stringIDsToAny(ids)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byRecord := make(map[string][]secretDeliveryReference, len(records))
	for rows.Next() {
		var recordID string
		var ref secretDeliveryReference
		if err := rows.Scan(
			&recordID, &ref.SecretID, &ref.SecretName, &ref.ValueVersion, &ref.Exists,
		); err != nil {
			return err
		}
		byRecord[recordID] = append(byRecord[recordID], ref)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range records {
		assign(index, byRecord[ids[index]])
	}
	return nil
}

func stringIDsToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
