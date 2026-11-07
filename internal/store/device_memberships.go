package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// ErrNetworkMembershipNotFound 表示设备在这个网络里还没有网络成员。
var ErrNetworkMembershipNotFound = errors.New("设备在这个网络里没有网络成员")

// AddNetworkMembership 把设备加入网络并登记地址。地址按去首尾空白后的小
// 写形式保证同一网络内唯一；成员行与审计行同事务提交。
func (s *Store) AddNetworkMembership(deviceName, networkName, address string) (*models.NetworkMembership, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, errors.New("网络成员地址不能为空")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, deviceName)
	if err != nil {
		return nil, err
	}
	network, err := networkByNameTx(tx, networkName)
	if err != nil {
		return nil, err
	}
	var existingDevice string
	err = tx.QueryRow(
		`SELECT device.name FROM network_memberships member
		 JOIN devices device ON device.id=member.device_id
		 WHERE member.network_id=? AND member.address_lower=?`,
		network.ID, strings.ToLower(address),
	).Scan(&existingDevice)
	if err == nil {
		return nil, fmt.Errorf("网络 %s 的地址 %s 已被设备 %s 占用", network.Name, address, existingDevice)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM network_memberships WHERE device_id=? AND network_id=?`,
		device.ID, network.ID,
	).Scan(&count); err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, fmt.Errorf("同一设备 %s 在网络 %s 里已有网络成员；请用 devices member edit 修改地址", device.Name, network.Name)
	}
	// 登记序号按全表单调递增：同一台设备的新成员保持新增顺序，删除成员
	// 后也不回收已经用过的序号。
	var registrationOrder int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(registration_order),0)+1 FROM network_memberships`,
	).Scan(&registrationOrder); err != nil {
		return nil, err
	}
	membership := &models.NetworkMembership{
		DeviceID: device.ID, DeviceName: device.Name,
		NetworkID: network.ID, NetworkName: network.Name,
		NetworkType: network.Type, SiteName: network.SiteName, Address: address,
		RegistrationOrder: registrationOrder,
	}
	membership.CreatedAt, membership.UpdatedAt = time.Unix(now(), 0), time.Unix(now(), 0)
	if _, err := tx.Exec(
		`INSERT INTO network_memberships(device_id,network_id,address,address_lower,registration_order,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		membership.DeviceID, membership.NetworkID, membership.Address, strings.ToLower(membership.Address),
		membership.RegistrationOrder, membership.CreatedAt.Unix(), membership.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.member-add", membershipAuditSummary(membership)); err != nil {
		return nil, err
	}
	return membership, tx.Commit()
}

// EditNetworkMembership 修改设备在指定网络里的地址。数据行与审计行在同一
// 个事务里提交。
func (s *Store) EditNetworkMembership(deviceName, networkName, address string) (*models.NetworkMembership, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, errors.New("网络成员地址不能为空")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	membership, err := networkMembershipTx(tx, deviceName, networkName)
	if err != nil {
		return nil, err
	}
	membership.Address = address
	if err := ensureMembershipAddressAvailableTx(tx, membership); err != nil {
		return nil, err
	}
	membership.UpdatedAt = time.Unix(now(), 0)
	if _, err := tx.Exec(
		`UPDATE network_memberships SET address=?,address_lower=?,updated_at=? WHERE device_id=? AND network_id=?`,
		membership.Address, strings.ToLower(membership.Address), membership.UpdatedAt.Unix(),
		membership.DeviceID, membership.NetworkID,
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.member-edit", membershipAuditSummary(membership)); err != nil {
		return nil, err
	}
	return membership, tx.Commit()
}

// RemoveNetworkMembership 删除设备在指定网络里的成员。数据行与审计行在
// 同一个事务里提交。
func (s *Store) RemoveNetworkMembership(deviceName, networkName string) (*models.NetworkMembership, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	membership, err := networkMembershipTx(tx, deviceName, networkName)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`DELETE FROM network_memberships WHERE device_id=? AND network_id=?`,
		membership.DeviceID, membership.NetworkID,
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.member-rm", membershipAuditSummary(membership)); err != nil {
		return nil, err
	}
	return membership, tx.Commit()
}

// ListNetworkMemberships 返回一台设备的全部网络成员，按网络名字排序。
func (s *Store) ListNetworkMemberships(deviceName string) ([]*models.NetworkMembership, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		WHERE member.device_id=?
		ORDER BY network.name,member.address`, device.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.NetworkMembership, 0)
	for rows.Next() {
		membership, err := scanNetworkMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, membership)
	}
	return out, rows.Err()
}

// ListNetworkMembershipsByRegistration 返回一台设备的全部网络成员，按登记
// 序号排序，供后续连接选址按 owner 的登记顺序尝试地址。
func (s *Store) ListNetworkMembershipsByRegistration(deviceName string) ([]*models.NetworkMembership, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		WHERE member.device_id=?
		ORDER BY member.registration_order,network.name,member.address`, device.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.NetworkMembership, 0)
	for rows.Next() {
		membership, err := scanNetworkMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, membership)
	}
	return out, rows.Err()
}

// ListNetworkMembershipsForNetwork 返回一个网络里的全部网络成员，按设备
// 名字排序，供删除网络前的预览使用。
func (s *Store) ListNetworkMembershipsForNetwork(networkName string) ([]*models.NetworkMembership, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	network, err := networkByNameTx(tx, networkName)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		WHERE member.network_id=?
		ORDER BY device.name,member.address`, network.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.NetworkMembership, 0)
	for rows.Next() {
		membership, err := scanNetworkMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, membership)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// networkMembershipsForDeviceTx 返回一台设备在事务里的全部网络成员。
func networkMembershipsForDeviceTx(tx *sql.Tx, deviceID string) ([]*models.NetworkMembership, error) {
	rows, err := tx.Query(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		WHERE member.device_id=?
		ORDER BY network.name,member.address`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.NetworkMembership, 0)
	for rows.Next() {
		membership, err := scanNetworkMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, membership)
	}
	return out, rows.Err()
}

// networkMembershipTx 取设备在指定网络里的成员。
func networkMembershipTx(tx *sql.Tx, deviceName, networkName string) (*models.NetworkMembership, error) {
	device, err := deviceByNameTx(tx, deviceName)
	if err != nil {
		return nil, err
	}
	network, err := networkByNameTx(tx, networkName)
	if err != nil {
		return nil, err
	}
	row := tx.QueryRow(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		WHERE member.device_id=? AND member.network_id=?`, device.ID, network.ID)
	membership, err := scanNetworkMembership(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：设备 %s，网络 %s", ErrNetworkMembershipNotFound, device.Name, network.Name)
	}
	return membership, err
}

// ensureMembershipAddressAvailableTx 检查同网络地址唯一性，冲突时报出占用设备。
func ensureMembershipAddressAvailableTx(tx *sql.Tx, membership *models.NetworkMembership) error {
	var existingDevice string
	err := tx.QueryRow(
		`SELECT device.name FROM network_memberships member
		 JOIN devices device ON device.id=member.device_id
		 WHERE member.network_id=? AND member.address_lower=? AND member.device_id<>?`,
		membership.NetworkID, strings.ToLower(membership.Address), membership.DeviceID,
	).Scan(&existingDevice)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("网络 %s 的地址 %s 已被设备 %s 占用", membership.NetworkName, membership.Address, existingDevice)
}

// scanNetworkMembership 把数据库行转换成网络成员对象。
func scanNetworkMembership(sc scanner) (*models.NetworkMembership, error) {
	var (
		membership  models.NetworkMembership
		networkType string
		order       int64
		created     int64
		updated     int64
	)
	if err := sc.Scan(&membership.DeviceID, &membership.DeviceName, &membership.NetworkID,
		&membership.NetworkName, &networkType, &membership.SiteName, &membership.Address,
		&order, &created, &updated); err != nil {
		return nil, err
	}
	membership.NetworkType = models.NetworkType(networkType)
	membership.RegistrationOrder = order
	membership.CreatedAt, membership.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &membership, nil
}

// membershipAuditSummary 只写网络成员字段白名单。
func membershipAuditSummary(membership *models.NetworkMembership) map[string]any {
	return map[string]any{
		"device": membership.DeviceName, "network": membership.NetworkName, "address": membership.Address,
	}
}
