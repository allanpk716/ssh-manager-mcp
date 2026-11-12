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

// ErrServerDeletePreviewChanged 表示 owner 确认的删除预览摘要与执行时重新组
// 装的预览不一致。删除必须整体中止，由 owner 查看新预览后重新确认。
var ErrServerDeletePreviewChanged = errors.New("服务器删除预览已变化；未删除任何数据，请重新查看新的预览")

// ServerDeleteCredential 是删除预览里的一份凭据影响：删除还是保留、类型与
// 其他服务器条目的引用数。
type ServerDeleteCredential struct {
	ID string
	// Kind 是这份凭据在服务器条目里的位置：login（登录凭据）或 sudo。
	Kind string
	Type string
	// Deleted 表示没有其他条目引用，会随删除一起清理。
	Deleted bool
	// References 是删除后仍引用这份凭据的其他服务器条目数量。
	References int
}

// DeliveryRecordImpact 是删除预览里的一条机密项下发记录：哪个服务
// 器条目的哪个路径曾由 sshmgr 填值。引用的机密项即使已删除也按原名字列
// 出，供 owner 核对随条目一起消失的线索。
type DeliveryRecordImpact struct {
	ServerName  string
	Path        string
	FileSHA256  string
	SecretNames []string
	WrittenAt   time.Time
}

// ServerDeletePreview 是服务器条目删除影响清单，只覆盖当前级联删除会动
// 到的数据，包含会随条目一起消失的下发记录。
type ServerDeletePreview struct {
	Server       *models.Server
	Device       *models.Device
	HasDevice    bool
	Anchors      []PinMeta
	ProfileNames []string
	SecretNames  []string
	Credentials  []ServerDeleteCredential
	// DeliveryRecords 是会随条目一起删除的机密项下发记录。
	DeliveryRecords []DeliveryRecordImpact
	digest          string
}

// Digest 返回可比对的预览摘要。摘要覆盖预览全部内容与服务器条目修订号：
// 确认与执行之间任何一组影响或条目本身发生变化，旧摘要都会被拒绝。
func (p *ServerDeletePreview) Digest() string { return p.digest }

// ServerDeletePreview 按内部标识符组装删除预览。预览只读数据库，不改变任
// 何行，也不写审计。
func (s *Store) ServerDeletePreview(id string) (*ServerDeletePreview, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return serverDeletePreviewTx(tx, id)
}

// DeleteServerCascadingConfirmed 带已确认的预览摘要执行级联删除：先在删除
// 事务里重新组装预览并比对摘要，不一致就整体中止；一致时沿用现有级联删除
// 主体（锚、机密项可写入关联与独占凭据一起清理）。
func (s *Store) DeleteServerCascadingConfirmed(id, confirmedDigest string) (*ServerDeletePreview, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	preview, err := serverDeletePreviewTx(tx, id)
	if err != nil {
		return nil, err
	}
	if preview == nil {
		return nil, nil // 已不存在：与既有删除语义一致的幂等空操作
	}
	if preview.digest != confirmedDigest {
		return preview, ErrServerDeletePreviewChanged
	}
	deleted, err := s.deleteServerCascadingTx(tx, id, s.nowTime())
	if err != nil {
		return nil, err
	}
	if deleted == nil {
		return nil, fmt.Errorf("服务器删除预览与数据库状态不一致：条目 %s 已不存在", preview.Server.Name)
	}
	return preview, tx.Commit()
}

// serverDeletePreviewTx 在一个数据库事务里读取全部级联影响，保证预览与删
// 除主体使用同一套查询。条目不存在时返回 (nil, nil)。
func serverDeletePreviewTx(tx *sql.Tx, id string) (*ServerDeletePreview, error) {
	srv, err := getServerTx(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	preview := &ServerDeletePreview{Server: srv}
	if srv.DeviceID != "" {
		device, err := deviceByIDTx(tx, srv.DeviceID)
		if err != nil && !errors.Is(err, ErrDeviceNotFound) {
			return nil, err
		}
		if device != nil && err == nil {
			preview.Device = device
			preview.HasDevice = true
		}
	}
	anchor, err := loadEntryPinMeta(tx, srv.ID)
	if err != nil {
		return nil, err
	}
	if anchor != nil {
		preview.Anchors = []PinMeta{*anchor}
	}
	profiles, err := serverDeleteProfileNamesTx(tx, srv.ID)
	if err != nil {
		return nil, err
	}
	preview.ProfileNames = profiles
	secrets, err := secretAllowsBeforeServerDelete(tx, srv.ID)
	if err != nil {
		return nil, err
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].name < secrets[j].name })
	for _, secret := range secrets {
		preview.SecretNames = append(preview.SecretNames, secret.name)
	}
	impacts, err := serverDeleteCredentialImpactsTx(tx, srv)
	if err != nil {
		return nil, err
	}
	preview.Credentials = impacts
	deliveries, err := deleteDeliveryRecordsTx(tx, false, srv.ID)
	if err != nil {
		return nil, err
	}
	preview.DeliveryRecords = deliveries
	preview.digest = previewDigest(preview)
	return preview, nil
}

// deleteDeliveryRecordsTx 列出会随服务器条目或设备一起删除的下发记录。
// byDevice 为真时按设备筛选，并先按服务器条目名排序；否则按服务器条目筛
// 选并直接按路径排序。每条记录引用的机密项名字按字母顺序聚合。
func deleteDeliveryRecordsTx(tx *sql.Tx, byDevice bool, id string) ([]DeliveryRecordImpact, error) {
	predicate := "record.server_id=?"
	orderBy := "record.path,record.id"
	if byDevice {
		predicate = "server.device_id=?"
		orderBy = "server.name,record.path,record.id"
	}
	rows, err := tx.Query(`
		SELECT server.name,record.path,record.file_sha256,record.written_at,record.id
		FROM secret_delivery_records record
		JOIN servers server ON server.id=record.server_id
		WHERE `+predicate+` ORDER BY `+orderBy, id)
	if err != nil {
		return nil, err
	}
	records := make([]DeliveryRecordImpact, 0)
	ids := make([]string, 0)
	for rows.Next() {
		var record DeliveryRecordImpact
		var recordID string
		var writtenAt int64
		if err := rows.Scan(&record.ServerName, &record.Path, &record.FileSHA256, &writtenAt, &recordID); err != nil {
			rows.Close()
			return nil, err
		}
		record.WrittenAt = time.Unix(writtenAt, 0)
		records = append(records, record)
		ids = append(ids, recordID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	err = attachSecretDeliveryReferences(tx, records, ids, func(index int, refs []secretDeliveryReference) {
		names := make([]string, 0, len(refs))
		for _, ref := range refs {
			names = append(names, ref.SecretName)
		}
		records[index].SecretNames = names
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func serverDeleteProfileNamesTx(tx *sql.Tx, serverID string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT profile.name FROM profile_servers binding
		JOIN profiles profile ON profile.id=binding.profile_id
		WHERE binding.server_id=? ORDER BY profile.name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func serverDeleteCredentialImpactsTx(tx *sql.Tx, srv *models.Server) ([]ServerDeleteCredential, error) {
	var impacts []ServerDeleteCredential
	for _, item := range []struct{ id, kind string }{
		{srv.CredentialID, "login"},
		{srv.SudoCredentialID, "sudo"},
	} {
		if item.id == "" {
			continue
		}
		duplicate := false
		for _, existing := range impacts {
			if existing.ID == item.id {
				duplicate = true
			}
		}
		if duplicate {
			continue
		}
		kind, _, found, err := credentialSummaryTx(tx, item.id)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		refs, err := credentialReferencedElseBy(tx, item.id, srv.ID)
		if err != nil {
			return nil, err
		}
		impacts = append(impacts, ServerDeleteCredential{
			ID: item.id, Kind: item.kind, Type: string(kind),
			Deleted: refs == 0, References: refs,
		})
	}
	return impacts, nil
}

// credentialSummaryTx 是事务内的凭据非敏感摘要查询。
func credentialSummaryTx(tx *sql.Tx, id string) (kind string, hasPassphrase, found bool, err error) {
	var typ string
	var passRaw []byte
	err = tx.QueryRow(`SELECT type, passphrase_blob FROM credentials WHERE id=?`, id).Scan(&typ, &passRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return typ, passRaw != nil, true, nil
}

// previewDigest 把预览内容规范成稳定文本后取 SHA-256。只依赖内容本身，不
// 依赖结构体字段顺序或指针地址。
func previewDigest(p *ServerDeletePreview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server=%s|name=%s|revision=%d|device=", p.Server.ID, p.Server.Name, p.Server.Revision)
	if p.HasDevice && p.Device != nil {
		fmt.Fprintf(&b, "%s(%s)", p.Device.ID, p.Device.Name)
	}
	fmt.Fprint(&b, "|anchors=")
	for _, anchor := range p.Anchors {
		fmt.Fprintf(&b, "%s:%s;", anchor.Source, PinFingerprint(anchor.Blob, anchor.Format))
	}
	fmt.Fprintf(&b, "|profiles=%s|secrets=%s", strings.Join(p.ProfileNames, ","), strings.Join(p.SecretNames, ","))
	fmt.Fprint(&b, "|credentials=")
	for _, cred := range p.Credentials {
		fmt.Fprintf(&b, "%s:%s:%s:%t:%d;", cred.ID, cred.Kind, cred.Type, cred.Deleted, cred.References)
	}
	fmt.Fprint(&b, "|deliveries=")
	for _, record := range p.DeliveryRecords {
		fmt.Fprintf(&b, "%s:%s:%s:%s:%d;", record.ServerName, record.Path,
			record.FileSHA256, strings.Join(record.SecretNames, ","), record.WrittenAt.Unix())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
