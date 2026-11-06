package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// 设备操作的稳定错误；报错文本直接面向 owner，自带完整原因。
var (
	ErrDeviceNotFound   = errors.New("设备不存在")
	ErrDeviceNameExists = errors.New("设备名字已存在")
	ErrClientNotFound   = errors.New("客户端实例不存在")
)

// deviceQueryer 抽象数据库连接与事务都能执行的按行查询。
type deviceQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// DeviceChanges 表示设备的一次部分更新。Client 为空指针代表保持不变；
// 指向空字符串代表解除客户端实例关联。
type DeviceChanges struct {
	Name   *string
	Note   *string
	Client *string
}

// DeviceFilter 是设备列表的筛选条件；空字符串与 false 表示不筛选。
type DeviceFilter struct {
	Site       string
	Network    string
	Unassigned bool
}

// AddDevice 登记一台设备。客户端实例按名字解析成设备码标识符；设备行与
// 审计行在同一个事务里提交。
func (s *Store) AddDevice(name, note, client string) (*models.Device, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if err := validateTopologyName("设备", name); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := ensureDeviceNameAvailableTx(tx, name, ""); err != nil {
		return nil, err
	}
	device := &models.Device{
		ID:   newID(),
		Name: name,
		Note: strings.TrimSpace(note),
	}
	if client != "" {
		device.ClientTokenID, device.ClientName, err = activeClientTx(tx, client)
		if err != nil {
			return nil, err
		}
		if err := ensureClientAvailableTx(tx, device.ClientTokenID, device.ID); err != nil {
			return nil, err
		}
	}
	device.CreatedAt, device.UpdatedAt = time.Unix(now(), 0), time.Unix(now(), 0)
	if _, err := tx.Exec(
		`INSERT INTO devices(id,name,name_lower,note,client_token_id,client_name,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		device.ID, device.Name, strings.ToLower(device.Name), device.Note,
		nullableString(device.ClientTokenID), device.ClientName,
		device.CreatedAt.Unix(), device.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.add", deviceAuditSummary(device)); err != nil {
		return nil, err
	}
	return device, tx.Commit()
}

// EditDevice 按设备名字做部分更新。客户端实例关联既能换成另一个设备码，
// 也能通过指向空字符串的 Client 字段解除。数据行与审计行同事务提交。
func (s *Store) EditDevice(name string, changes DeviceChanges) (*models.Device, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, name)
	if err != nil {
		return nil, err
	}
	editChanges := make(map[string]any)
	if changes.Name != nil {
		if err := validateTopologyName("设备", *changes.Name); err != nil {
			return nil, err
		}
		if err := ensureDeviceNameAvailableTx(tx, *changes.Name, device.ID); err != nil {
			return nil, err
		}
		device.Name = *changes.Name
		editChanges["name"] = device.Name
	}
	if changes.Note != nil {
		device.Note = strings.TrimSpace(*changes.Note)
		editChanges["note"] = device.Note
	}
	if changes.Client != nil {
		clientName := strings.TrimSpace(*changes.Client)
		if clientName == "" {
			device.ClientTokenID, device.ClientName = "", ""
		} else {
			device.ClientTokenID, device.ClientName, err = activeClientTx(tx, clientName)
			if err != nil {
				return nil, err
			}
		}
		if err := ensureClientAvailableTx(tx, device.ClientTokenID, device.ID); err != nil {
			return nil, err
		}
		editChanges["client"] = device.ClientName
	}
	device.UpdatedAt = time.Unix(now(), 0)
	if _, err := tx.Exec(
		`UPDATE devices SET name=?,name_lower=?,note=?,client_token_id=?,client_name=?,updated_at=? WHERE id=?`,
		device.Name, strings.ToLower(device.Name), device.Note,
		nullableString(device.ClientTokenID), device.ClientName, device.UpdatedAt.Unix(), device.ID,
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.edit", topologyEditSummary(name, editChanges)); err != nil {
		return nil, err
	}
	return device, tx.Commit()
}

// GetDevice 按名字返回设备，供详情命令使用。
func (s *Store) GetDevice(name string) (*models.Device, error) {
	return deviceByNameTx(s.db, name)
}

// DeleteDevice 删除还没有关联服务器条目的设备，并在同一个事务里删除它的
// 全部网络成员。客户端实例关联保存在设备行上，设备删除后即解除。
func (s *Store) DeleteDevice(name string) (*models.Device, []*models.NetworkMembership, error) {
	if s.readOnly {
		return nil, nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, name)
	if err != nil {
		return nil, nil, err
	}
	memberships, err := networkMembershipsForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(`DELETE FROM network_memberships WHERE device_id=?`, device.ID); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(`DELETE FROM devices WHERE id=?`, device.ID); err != nil {
		return nil, nil, err
	}
	memberSummaries := make([]map[string]any, 0, len(memberships))
	for _, membership := range memberships {
		memberSummaries = append(memberSummaries, membershipAuditSummary(membership))
	}
	summary := map[string]any{
		"name": device.Name, "client": device.ClientName, "members": memberSummaries,
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.rm", summary); err != nil {
		return nil, nil, err
	}
	return device, memberships, tx.Commit()
}

// ListDevices 按筛选条件返回设备。站点筛选由设备所在的物理局域网推出；
// 未归类筛选只返回没有任何网络成员的设备。
func (s *Store) ListDevices(filter DeviceFilter) ([]*models.Device, error) {
	query := `SELECT id,name,note,COALESCE(client_token_id,''),client_name,created_at,updated_at FROM devices`
	var conditions []string
	var arguments []any
	if filter.Unassigned {
		conditions = append(conditions, `NOT EXISTS (
			SELECT 1 FROM network_memberships member WHERE member.device_id=devices.id)`)
	}
	if filter.Site != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM network_memberships member
			JOIN networks network ON network.id=member.network_id
			JOIN sites site ON site.id=network.site_id
			WHERE member.device_id=devices.id AND network.type=? AND site.name=?)`)
		arguments = append(arguments, string(models.NetworkPhysical), filter.Site)
	}
	if filter.Network != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM network_memberships member
			JOIN networks network ON network.id=member.network_id
			WHERE member.device_id=devices.id AND network.name=?)`)
		arguments = append(arguments, filter.Network)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY name,id"
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Device, 0)
	byID := make(map[string]*models.Device)
	for rows.Next() {
		var device models.Device
		var created, updated int64
		if err := rows.Scan(&device.ID, &device.Name, &device.Note, &device.ClientTokenID,
			&device.ClientName, &created, &updated); err != nil {
			return nil, err
		}
		device.CreatedAt, device.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		device.Memberships = make([]*models.NetworkMembership, 0)
		byID[device.ID] = &device
		out = append(out, &device)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	memberRows, err := s.db.Query(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		ORDER BY network.name,member.address`)
	if err != nil {
		return nil, err
	}
	defer memberRows.Close()
	for memberRows.Next() {
		membership, err := scanNetworkMembership(memberRows)
		if err != nil {
			return nil, err
		}
		if device := byID[membership.DeviceID]; device != nil {
			device.Memberships = append(device.Memberships, membership)
		}
	}
	return out, memberRows.Err()
}

// deviceByNameTx 按名字取设备；名字比较不区分大小写。
func deviceByNameTx(q deviceQueryer, name string) (*models.Device, error) {
	var device models.Device
	var created, updated int64
	err := q.QueryRow(`
		SELECT id,name,note,COALESCE(client_token_id,''),client_name,created_at,updated_at
		FROM devices WHERE name_lower=?`, strings.ToLower(name),
	).Scan(&device.ID, &device.Name, &device.Note, &device.ClientTokenID, &device.ClientName, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：%s", ErrDeviceNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	device.CreatedAt, device.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &device, nil
}

// ensureDeviceNameAvailableTx 检查设备名字在小写比较下的唯一性。
func ensureDeviceNameAvailableTx(tx *sql.Tx, name, excludeID string) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM devices WHERE name_lower=? AND id<>?`, strings.ToLower(name), excludeID,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w：%s", ErrDeviceNameExists, name)
	}
	return nil
}

// activeClientTx 按设备码名字找仍可使用的客户端实例。
func activeClientTx(tx *sql.Tx, name string) (string, string, error) {
	var id, storedName, status string
	err := tx.QueryRow(
		`SELECT id,name,status FROM cache_tokens WHERE lower(name)=lower(?)`, strings.TrimSpace(name),
	).Scan(&id, &storedName, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w：%s", ErrClientNotFound, name)
	}
	if err != nil {
		return "", "", err
	}
	if models.CacheTokenStatus(status) != models.CacheTokenActive {
		return "", "", fmt.Errorf("客户端实例 %s 已吊销，不能关联设备", storedName)
	}
	return id, storedName, nil
}

// ensureClientAvailableTx 保证一个设备码只关联一台设备。
func ensureClientAvailableTx(tx *sql.Tx, clientID, deviceID string) error {
	if clientID == "" {
		return nil
	}
	var otherName string
	err := tx.QueryRow(
		`SELECT name FROM devices WHERE client_token_id=? AND id<>?`, clientID, deviceID,
	).Scan(&otherName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("客户端实例 %s 已关联设备 %s；一个客户端实例只能关联一台设备",
		clientNameTx(tx, clientID), otherName)
}

// clientNameTx 读取设备码名字，用于一对一冲突报错。
func clientNameTx(tx *sql.Tx, clientID string) string {
	var name string
	if err := tx.QueryRow(`SELECT name FROM cache_tokens WHERE id=?`, clientID).Scan(&name); err != nil {
		return clientID
	}
	return name
}

// deviceAuditSummary 只写设备字段白名单，不写内部标识符。
func deviceAuditSummary(device *models.Device) map[string]any {
	return map[string]any{
		"name":   device.Name,
		"note":   device.Note,
		"client": device.ClientName,
	}
}
