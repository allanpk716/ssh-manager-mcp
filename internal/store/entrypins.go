package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
)

// migrateEntryPins 把旧的「主机:端口」锚迁移为按服务器条目存放（票 #58）。
// 每行旧锚复制给所有兜底 Host 与端口都相同的条目；没有任何条目指向的孤立锚
// 丢弃，并写一条迁移审计列出被丢弃的地址与主机密钥指纹。迁移后清空旧表，
// 旧表不再作为事实源；因此重复打开时没有旧行可处理，天然幂等。
func migrateEntryPins(db *sql.DB) error {
	var oldRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM host_keys`).Scan(&oldRows); err != nil {
		return err
	}
	if oldRows == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrateEntryPinsTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateEntryPinsTx 是迁移主体，运行在调用方的事务里：打开 vault 与导入
// 快照两个入口共用同一套规则。
func migrateEntryPinsTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT host_port, key_blob, created_at, pin_format, pin_source, pin_device FROM host_keys ORDER BY host_port`)
	if err != nil {
		return err
	}
	var copied []string
	var dropped []string
	for rows.Next() {
		var hostPort string
		var blob []byte
		var createdAt int64
		var format, source, device string
		if err := rows.Scan(&hostPort, &blob, &createdAt, &format, &source, &device); err != nil {
			rows.Close()
			return err
		}
		matched, err := entryIDsAtAddress(tx, hostPort)
		if err != nil {
			rows.Close()
			return err
		}
		if len(matched) == 0 {
			dropped = append(dropped, fmt.Sprintf("%s fp=%s", hostPort, PinFingerprint(blob, format)))
			continue
		}
		for _, id := range matched {
			res, err := tx.Exec(
				`INSERT INTO entry_host_keys (server_id, key_blob, created_at, pin_format, pin_source, pin_device, handshake_addr)
				 VALUES (?,?,?,?,?,?,?)
				 ON CONFLICT(server_id) DO NOTHING`,
				id, blob, createdAt, format, source, device, hostPort)
			if err != nil {
				rows.Close()
				return err
			}
			inserted, err := res.RowsAffected()
			if err != nil {
				rows.Close()
				return err
			}
			if inserted > 0 {
				copied = append(copied, id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.Exec(`DELETE FROM host_keys`); err != nil {
		return err
	}
	if len(copied) > 0 || len(dropped) > 0 {
		command := fmt.Sprintf("copied=%d entries: %s dropped=%d: %s",
			len(copied), strings.Join(copied, ","), len(dropped), strings.Join(dropped, ", "))
		if err := writeAuditTx(tx, AuditRow{
			TS:      time.Now(),
			Action:  "pin-migrate",
			Command: command,
			Status:  "ok",
		}); err != nil {
			return err
		}
	}
	return nil
}

// entryIDsAtAddress 返回兜底地址（host:port）上的全部条目标识符，按标识符排序。
func entryIDsAtAddress(q *sql.Tx, hostPort string) ([]string, error) {
	rows, err := q.Query(`SELECT id FROM servers WHERE host || ':' || port = ? ORDER BY id`, hostPort)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PinFingerprint 把一把锚折算成可比较的密钥身份，并用于审计与展示。指纹
// 格式的锚本身就是指纹串；密钥字节格式的锚解析后计算 SHA256 指纹。无法
// 解析的密钥字节（正常写入路径不会产生）退化为原始字节的十六进制写法。
func PinFingerprint(blob []byte, format string) string {
	if format == PinFormatFingerprint {
		return string(blob)
	}
	if pub, err := ssh.ParsePublicKey(blob); err == nil {
		return ssh.FingerprintSHA256(pub)
	}
	return fmt.Sprintf("raw:%x", blob)
}

// loadEntryPinMeta 按条目标识符读取锚及其来源信息；没有锚时返回 (nil, nil)。
func loadEntryPinMeta(q dbtx, serverID string) (*PinMeta, error) {
	var m PinMeta
	err := q.QueryRow(
		`SELECT server_id, key_blob, pin_format, pin_source, pin_device, handshake_addr, created_at
		 FROM entry_host_keys WHERE server_id=?`, serverID,
	).Scan(&m.ServerID, &m.Blob, &m.Format, &m.Source, &m.Device, &m.HandshakeAddr, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetEntryPin 返回服务器条目当前的主机密钥锚；条目尚无锚时返回 (nil, nil)。
func (s *Store) GetEntryPin(srv *models.Server) (*Pin, error) {
	m, err := loadEntryPinMeta(s.db, srv.ID)
	if err != nil || m == nil {
		return nil, err
	}
	pin := m.Pin
	return &pin, nil
}

// GetEntryPinMeta 按条目标识符返回锚及来源信息；条目尚无锚时返回 (nil, nil)。
func (s *Store) GetEntryPinMeta(serverID string) (*PinMeta, error) {
	return loadEntryPinMeta(s.db, serverID)
}

// ListEntryPins 列出全部按条目存放的锚（含来源信息），按条目标识符排序。
func (s *Store) ListEntryPins() ([]PinMeta, error) {
	rows, err := s.db.Query(
		`SELECT server_id, key_blob, pin_format, pin_source, pin_device, handshake_addr, created_at
		 FROM entry_host_keys ORDER BY server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PinMeta
	for rows.Next() {
		var m PinMeta
		if err := rows.Scan(&m.ServerID, &m.Blob, &m.Format, &m.Source, &m.Device, &m.HandshakeAddr, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveEntryPin 记录一次首次连接即信任（单机直连或权威端本地连接）学到的
// 主机密钥。按条目整行覆盖：同一条目重开首次信任窗口时，格式、来源、转发
// 设备与握手地址一起重置，避免旧来源信息留在新锚上。
func (s *Store) SaveEntryPin(srv *models.Server, marshaledKey []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(
		`INSERT INTO entry_host_keys (server_id, key_blob, created_at, pin_format, pin_source, pin_device, handshake_addr)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(server_id) DO UPDATE SET
		   key_blob=excluded.key_blob, created_at=excluded.created_at,
		   pin_format='blob', pin_source='tofu', pin_device='', handshake_addr=excluded.handshake_addr`,
		srv.ID, marshaledKey, now(), PinFormatBlob, PinSourceTofu, "", EntryFallbackAddr(srv))
	return err
}

// InsertForwardedEntryPin 落一条设备转发的锚（权威端 /pin-hostkey 的存储侧）。
// 仅可新增：返回值只表示本次调用是否插入了新行；条目已有锚时绝不覆盖、
// 不写审计。锚与审计行同一事务提交，审计失败时锚一并回滚。
func (s *Store) InsertForwardedEntryPin(srv *models.Server, handshakeAddr string, blob []byte, device string, audit AuditRow) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO entry_host_keys (server_id, key_blob, created_at, pin_format, pin_source, pin_device, handshake_addr)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(server_id) DO NOTHING`,
		srv.ID, blob, now(), PinFormatBlob, PinSourceForward, device, handshakeAddr)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		existing, err := loadEntryPinMeta(tx, srv.ID)
		if err != nil {
			return false, err
		}
		if existing == nil {
			return false, fmt.Errorf("entry pin for %s vanished inside the insert transaction", srv.ID)
		}
		return false, nil
	}
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ApplyForwardedEntryPin 在客户端只读缓存里记录权威端已接受的转发锚。这是
// 只读缓存唯一允许写锚的入口；已有一致的锚时成功返回，不同则报错让调用方
// 重拉快照后重试。不写本地审计：权威端的转发审计行就是这把锚的历史。
func (s *Store) ApplyForwardedEntryPin(srv *models.Server, handshakeAddr string, marshaledKey []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO entry_host_keys (server_id, key_blob, created_at, pin_format, pin_source, pin_device, handshake_addr)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(server_id) DO NOTHING`,
		srv.ID, marshaledKey, now(), PinFormatBlob, PinSourceForward, s.forwardDevice, handshakeAddr)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		existing, err := loadEntryPinMeta(tx, srv.ID)
		if err != nil {
			return err
		}
		if existing == nil {
			return fmt.Errorf("apply forwarded host key for entry %s: anchor is missing after the insert was skipped (pull and retry)", srv.ID)
		}
		if !existing.Matches(marshaledKey) {
			return fmt.Errorf("apply forwarded host key for entry %s: existing pin differs from the presented key (pull and retry)", srv.ID)
		}
		return nil
	}
	return tx.Commit()
}

// UpsertManualEntryPin 落一条 owner 手工锚（servers pin-hostkey 的
// --fingerprint / --from-keyscan）。已有锚且未加 --force 时拒绝且不写审计；
// --force 是唯一覆盖入口，覆盖与审计同一事务提交。
func (s *Store) UpsertManualEntryPin(srv *models.Server, handshakeAddr string, blob []byte, format string, force bool, auditFor func(old *PinMeta) AuditRow) (*PinMeta, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if format != PinFormatBlob && format != PinFormatFingerprint {
		return nil, fmt.Errorf("pin format must be %q or %q, got %q", PinFormatBlob, PinFormatFingerprint, format)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	old, err := loadEntryPinMeta(tx, srv.ID)
	if err != nil {
		return nil, err
	}
	if old != nil && !force {
		return old, ErrPinExists
	}
	if _, err := tx.Exec(
		`INSERT INTO entry_host_keys (server_id, key_blob, created_at, pin_format, pin_source, pin_device, handshake_addr)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(server_id) DO UPDATE SET
		   key_blob=excluded.key_blob, created_at=excluded.created_at,
		   pin_format=excluded.pin_format, pin_source=excluded.pin_source,
		   pin_device=excluded.pin_device, handshake_addr=excluded.handshake_addr`,
		srv.ID, blob, now(), format, PinSourceManual, "", handshakeAddr); err != nil {
		return nil, err
	}
	if err := writeAuditTx(tx, auditFor(old)); err != nil {
		return nil, err
	}
	return old, tx.Commit()
}

// ClearEntryPin 删除服务器条目当前的主机密钥锚并写审计，一次事务完成。条目
// 本来就没有锚时是幂等成功，不写审计。
func (s *Store) ClearEntryPin(serverID string, auditFor func(old PinMeta) AuditRow) (*PinMeta, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	old, err := loadEntryPinMeta(tx, serverID)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, nil
	}
	if _, err := tx.Exec(`DELETE FROM entry_host_keys WHERE server_id=?`, serverID); err != nil {
		return nil, err
	}
	if err := writeAuditTx(tx, auditFor(*old)); err != nil {
		return nil, err
	}
	return old, tx.Commit()
}

// deleteEntryPinWithAudit 在删除服务器条目的同一事务里删除它的锚并写审计
// （票 #58：删除条目不再留下孤立锚）。条目本来就没有锚时不写审计。
func deleteEntryPinWithAudit(tx *sql.Tx, srv *models.Server) error {
	old, err := loadEntryPinMeta(tx, srv.ID)
	if err != nil {
		return err
	}
	if old == nil {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM entry_host_keys WHERE server_id=?`, srv.ID); err != nil {
		return err
	}
	return writeAuditTx(tx, AuditRow{
		TS:       time.Now(),
		ServerID: srv.ID,
		Action:   "pin-delete",
		Command: fmt.Sprintf("server=%s host=%s:%d fp=%s source=%s via=server-delete",
			srv.Name, srv.Host, srv.Port, PinFingerprint(old.Blob, old.Format), old.Source),
		Status: "ok",
	})
}

// EntryFallbackAddr 渲染条目的兜底 SSH 地址「Host:端口」。本票阶段每个条目
// 只有这一个候选地址；多地址按序选址由网络拓扑票接入。
func EntryFallbackAddr(srv *models.Server) string {
	return fallbackAddr(srv.Host, srv.Port)
}

// fallbackAddr 是条目兜底地址的唯一拼接入口，模型与快照行共用。
func fallbackAddr(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// EntryVirtualLanAddresses 返回条目的虚拟局域网候选地址。本票阶段候选地址
// 数据源恒为空；网络拓扑票（#67）接入数据后，首次锚定与锚定转发才会只接受
// 虚拟局域网握手地址。
func EntryVirtualLanAddresses(srv *models.Server) []string {
	return nil
}
