package store

import (
	"database/sql"
	"fmt"
	"net"
	"strconv"
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

// PinFingerprint 渲染一把锚的主机密钥指纹，供审计与展示使用。指纹格式的锚
// 本身就是指纹串；密钥字节格式的锚解析后计算 SHA256 指纹。无法解析的密钥
// 字节（正常写入路径不会产生）退化为占位文本。
func PinFingerprint(blob []byte, format string) string {
	return pinKeyIdentity(blob, format, "unparseable")
}

// pinKeyIdentity 是锚身份的唯一判定逻辑，展示与旧格式行推导共用；无法解析
// 的密钥字节由调用方指定占位表示。
func pinKeyIdentity(blob []byte, format, invalid string) string {
	if format == PinFormatFingerprint {
		return string(blob)
	}
	if pub, err := ssh.ParsePublicKey(blob); err == nil {
		return ssh.FingerprintSHA256(pub)
	}
	return invalid
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
// 主机密钥。
func (s *Store) SaveEntryPin(srv *models.Server, marshaledKey []byte) error {
	return s.SaveEntryPinAtHandshake(srv, EntryFallbackAddr(srv), marshaledKey)
}

// SaveEntryPinAtHandshake 记录实际握手时学到的主机密钥。统一拨号回调把
// SSH 返回的握手地址传进来，避免多地址条目固定记录兜底地址。首次锚定只
// 新增：并发竞争时已有一致锚则成功返回，不同锚则报锚不一致。
func (s *Store) SaveEntryPinAtHandshake(srv *models.Server, handshakeAddr string, marshaledKey []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	inserted, err := insertEntryPinWithAuditTx(tx, srv.ID, handshakeAddr, marshaledKey,
		PinFormatBlob, PinSourceTofu, "", AuditRow{
			TS: time.Now(), ServerID: srv.ID, Action: "pin-tofu",
			Command: fmt.Sprintf("entry=%s(%s) handshake=%s fp=%s source=%s",
				srv.Name, srv.ID, handshakeAddr, PinFingerprint(marshaledKey, PinFormatBlob), PinSourceTofu),
			Status: "ok",
		})
	if err != nil {
		return err
	}
	if inserted {
		return tx.Commit()
	}
	existing, err := loadEntryPinMeta(tx, srv.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("entry pin for %s vanished inside the first-trust transaction", srv.ID)
	}
	if !existing.Matches(marshaledKey) {
		return fmt.Errorf("%w: presented %s", ErrEntryPinMismatch,
			PinFingerprint(marshaledKey, PinFormatBlob))
	}
	return nil
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
	inserted, err := insertEntryPinWithAuditTx(tx, srv.ID, handshakeAddr, blob,
		PinFormatBlob, PinSourceForward, device, audit)
	if err != nil || !inserted {
		return inserted, err
	}
	return true, tx.Commit()
}

// insertEntryPinWithAuditTx 是“只新增不覆盖”的锚写入公共入口：插入成功才
// 在同一事务写锚审计；条目已有锚时不写审计。设备转发与设备内锚复制共用。
func insertEntryPinWithAuditTx(
	tx *sql.Tx, serverID, handshakeAddr string, blob []byte, format, source, device string, audit AuditRow,
) (bool, error) {
	res, err := tx.Exec(
		`INSERT INTO entry_host_keys (server_id, key_blob, created_at, pin_format, pin_source, pin_device, handshake_addr)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(server_id) DO NOTHING`,
		serverID, blob, now(), format, source, device, handshakeAddr)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		existing, err := loadEntryPinMeta(tx, serverID)
		if err != nil {
			return false, err
		}
		if existing == nil {
			return false, fmt.Errorf("entry pin for %s vanished inside the insert transaction", serverID)
		}
		return false, nil
	}
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, nil
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

// EntryFallbackAddr 渲染条目的兜底 SSH 地址「Host:端口」。它始终是统一
// 拨号入口的最后一个候选地址。
func EntryFallbackAddr(srv *models.Server) string {
	return fallbackAddr(srv.Host, srv.Port)
}

// fallbackAddr 是条目兜底地址的唯一拼接入口，模型与快照行共用。
func fallbackAddr(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// NormalizeSSHHost renders one host name in the canonical form shared by
// topology addresses and complete SSH addresses. IP addresses use their
// standard text; domain names are case-insensitive.
func NormalizeSSHHost(address string) string {
	address = strings.TrimSpace(address)
	host := address
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(host)
}

// NormalizeSSHAddress renders a complete SSH address in one canonical form.
func NormalizeSSHAddress(address string, port int) string {
	host, portText := address, strconv.Itoa(port)
	if parsedHost, parsedPort, err := net.SplitHostPort(address); err == nil {
		host, portText = parsedHost, parsedPort
	}
	return net.JoinHostPort(NormalizeSSHHost(host), portText)
}

// ContainsSSHAddress reports whether one complete SSH address is in a
// candidate list, comparing both sides after canonical normalization.
func ContainsSSHAddress(addresses []string, address string, port int) bool {
	want := NormalizeSSHAddress(address, port)
	for _, candidate := range addresses {
		if NormalizeSSHAddress(candidate, port) == want {
			return true
		}
	}
	return false
}

// EntryVirtualLanAddresses 返回条目所属设备在虚拟局域网里的候选 SSH 地址。
// 单机 vault 与客户端缓存使用同一个存储查询，因此快照热更新后的下一次
// 询问就能看到当前网络成员。
func (s *Store) EntryVirtualLanAddresses(srv *models.Server) ([]string, error) {
	if s == nil || srv == nil || srv.DeviceID == "" {
		return nil, nil
	}
	return s.networkMembershipAddresses(`
		SELECT member.address
		FROM network_memberships member
		JOIN networks network ON network.id=member.network_id
		WHERE member.device_id=? AND network.type=?
		ORDER BY member.registration_order,network.name,member.address`,
		srv.Port, srv.DeviceID, models.NetworkVirtual)
}

// EntryCandidateAddresses 返回条目所属设备的全部网络成员地址，加上条目的
// 兜底地址。锚定转发用它校验客户端报告的握手地址确实可能是本条目的地址。
func (s *Store) EntryCandidateAddresses(srv *models.Server) ([]string, error) {
	if s == nil || srv == nil {
		return nil, nil
	}
	out, err := s.networkMembershipAddresses(`
		SELECT member.address
		FROM network_memberships member
		WHERE member.device_id=?
		ORDER BY member.registration_order,member.address`, srv.Port, srv.DeviceID)
	if err != nil {
		return nil, err
	}
	return append(out, NormalizeSSHAddress(srv.Host, srv.Port)), nil
}

func (s *Store) networkMembershipAddresses(query string, port int, arguments ...any) ([]string, error) {
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		out = append(out, NormalizeSSHAddress(address, port))
	}
	return out, rows.Err()
}
