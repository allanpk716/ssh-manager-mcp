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

// NetworkMembershipWriteResult 是一次网络成员写入结果；Warning 非空表示写
// 入成功但拓扑里有需要 owner 注意的问题。
type NetworkMembershipWriteResult struct {
	Membership *models.NetworkMembership
	Warning    string
}

// AddNetworkMembership 把设备加入网络并登记地址。地址按去首尾空白后的小
// 写形式保证同一网络内唯一；成员行与审计行同事务提交。
func (s *Store) AddNetworkMembership(deviceName, networkName, address string) (*models.NetworkMembership, error) {
	write, err := s.addNetworkMembership(deviceName, networkName, address, WriteSource{})
	if write == nil {
		return nil, err
	}
	return write.Membership, err
}

// AddNetworkMembershipWithSource 与 AddNetworkMembership 相同，并把同一事务
// 里的审计行标为指定来源。
func (s *Store) AddNetworkMembershipWithSource(
	deviceName, networkName, address string, source WriteSource,
) (*models.NetworkMembership, error) {
	write, err := s.addNetworkMembership(deviceName, networkName, address, source)
	if write == nil {
		return nil, err
	}
	return write.Membership, err
}

// AddNetworkMembershipWithWarning 与 AddNetworkMembership 相同，但把写入成
// 功后仍需展示的警告返回给调用方。
func (s *Store) AddNetworkMembershipWithWarning(
	deviceName, networkName, address string,
) (*NetworkMembershipWriteResult, error) {
	return s.addNetworkMembership(deviceName, networkName, address, WriteSource{})
}

// AddNetworkMembershipWithSourceAndWarning 与
// AddNetworkMembershipWithWarning 相同，并把审计行标为指定来源。
func (s *Store) AddNetworkMembershipWithSourceAndWarning(
	deviceName, networkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	return s.addNetworkMembership(deviceName, networkName, address, source)
}

func (s *Store) addNetworkMembership(
	deviceName, networkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	membership, err := s.addNetworkMembershipTx(tx, deviceName, networkName, address, source)
	if err != nil {
		return nil, err
	}
	return membership, tx.Commit()
}

// addNetworkMembershipTx 在调用方事务里登记一个网络成员；成员行与审计行
// 随外层事务一起提交或回滚。
func (s *Store) addNetworkMembershipTx(
	tx *sql.Tx, deviceName, networkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, &TopologyValidationError{Message: "网络成员地址不能为空"}
	}
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
		return nil, &TopologyValidationError{Message: fmt.Sprintf(
			"网络 %s 的地址 %s 已被设备 %s 占用", network.Name, address, existingDevice)}
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
		return nil, &TopologyValidationError{Message: fmt.Sprintf(
			"同一设备 %s 在网络 %s 里已有网络成员；请用 devices member edit 修改地址", device.Name, network.Name)}
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
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "device.member-add", membershipAuditSummary(membership), source); err != nil {
		return nil, err
	}
	warning, err := physicalVirtualAddressConflictWarningTx(tx, membership)
	if err != nil {
		return nil, err
	}
	return &NetworkMembershipWriteResult{Membership: membership, Warning: warning}, nil
}

// AllVirtualNetworkAddresses 返回拓扑里所有设备在虚拟局域网中的成员地址。
// 监听地址推导只拿这一组地址与本机网卡地址求交集，不按网卡名或网段猜测。
func (s *Store) AllVirtualNetworkAddresses() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT member.address
		FROM network_memberships member
		JOIN networks network ON network.id=member.network_id
		WHERE network.type=?
		ORDER BY member.address`, string(models.NetworkVirtual))
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
		out = append(out, address)
	}
	return out, rows.Err()
}

// EditNetworkMembership 修改设备在指定网络里的地址。数据行与审计行在同一
// 个事务里提交。
func (s *Store) EditNetworkMembership(deviceName, networkName, address string) (*models.NetworkMembership, error) {
	write, err := s.editNetworkMembership(deviceName, networkName, address, WriteSource{})
	if write == nil {
		return nil, err
	}
	return write.Membership, err
}

// EditNetworkMembershipWithSource 与 EditNetworkMembership 相同，并把同一
// 事务里的审计行标为指定来源。
func (s *Store) EditNetworkMembershipWithSource(
	deviceName, networkName, address string, source WriteSource,
) (*models.NetworkMembership, error) {
	write, err := s.editNetworkMembership(deviceName, networkName, address, source)
	if write == nil {
		return nil, err
	}
	return write.Membership, err
}

// EditNetworkMembershipWithWarning 与 EditNetworkMembership 相同，但把写入
// 成功后仍需展示的警告返回给调用方。
func (s *Store) EditNetworkMembershipWithWarning(
	deviceName, networkName, address string,
) (*NetworkMembershipWriteResult, error) {
	return s.editNetworkMembership(deviceName, networkName, address, WriteSource{})
}

// EditNetworkMembershipWithSourceAndWarning 与
// EditNetworkMembershipWithWarning 相同，并把审计行标为指定来源。
func (s *Store) EditNetworkMembershipWithSourceAndWarning(
	deviceName, networkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	return s.editNetworkMembership(deviceName, networkName, address, source)
}

func (s *Store) editNetworkMembership(
	deviceName, networkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, &TopologyValidationError{Message: "网络成员地址不能为空"}
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
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "device.member-edit", membershipAuditSummary(membership), source); err != nil {
		return nil, err
	}
	warning, err := physicalVirtualAddressConflictWarningTx(tx, membership)
	if err != nil {
		return nil, err
	}
	return &NetworkMembershipWriteResult{Membership: membership, Warning: warning}, tx.Commit()
}

// MoveNetworkMembership 把设备在 fromNetworkName 里的成员移动到
// toNetworkName 并登记新地址。两条数据行与两条审计行（沿用 member-rm 与
// member-add 动作）在同一个事务里提交，目标地址冲突或目标网络已有成员时
// 不改变任何数据。命令行等价面是先执行 devices member rm 再执行
// devices member add。
func (s *Store) MoveNetworkMembership(deviceName, fromNetworkName, toNetworkName, address string) (*models.NetworkMembership, error) {
	write, err := s.moveNetworkMembership(deviceName, fromNetworkName, toNetworkName, address, WriteSource{})
	if write == nil {
		return nil, err
	}
	return write.Membership, err
}

// MoveNetworkMembershipWithSource 与 MoveNetworkMembership 相同，并把同一
// 事务里的两条审计行都标为指定来源。
func (s *Store) MoveNetworkMembershipWithSource(
	deviceName, fromNetworkName, toNetworkName, address string, source WriteSource,
) (*models.NetworkMembership, error) {
	write, err := s.moveNetworkMembership(deviceName, fromNetworkName, toNetworkName, address, source)
	if write == nil {
		return nil, err
	}
	return write.Membership, err
}

// MoveNetworkMembershipWithWarning 与 MoveNetworkMembership 相同，但把写入
// 成功后仍需展示的警告返回给调用方。
func (s *Store) MoveNetworkMembershipWithWarning(
	deviceName, fromNetworkName, toNetworkName, address string,
) (*NetworkMembershipWriteResult, error) {
	return s.moveNetworkMembership(deviceName, fromNetworkName, toNetworkName, address, WriteSource{})
}

// MoveNetworkMembershipWithSourceAndWarning 与
// MoveNetworkMembershipWithWarning 相同，并把两条审计行都标为指定来源。
func (s *Store) MoveNetworkMembershipWithSourceAndWarning(
	deviceName, fromNetworkName, toNetworkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	return s.moveNetworkMembership(deviceName, fromNetworkName, toNetworkName, address, source)
}

func (s *Store) moveNetworkMembership(
	deviceName, fromNetworkName, toNetworkName, address string, source WriteSource,
) (*NetworkMembershipWriteResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, &TopologyValidationError{Message: "网络成员地址不能为空"}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	membership, err := networkMembershipTx(tx, deviceName, fromNetworkName)
	if err != nil {
		return nil, err
	}
	target, err := networkByNameTx(tx, toNetworkName)
	if err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM network_memberships WHERE device_id=? AND network_id=?`,
		membership.DeviceID, target.ID,
	).Scan(&count); err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, &TopologyValidationError{Message: fmt.Sprintf(
			"同一设备 %s 在网络 %s 里已有网络成员；请用 devices member edit 修改地址", membership.DeviceName, target.Name)}
	}
	if target.Type != membership.NetworkType {
		return nil, &TopologyValidationError{Message: fmt.Sprintf(
			"不能把网络成员从类型 %s 的网络移动到类型 %s 的网络", membership.NetworkType, target.Type)}
	}
	moved := &models.NetworkMembership{
		DeviceID: membership.DeviceID, DeviceName: membership.DeviceName,
		NetworkID: target.ID, NetworkName: target.Name,
		NetworkType: target.Type, SiteName: target.SiteName, Address: address,
	}
	if err := ensureMembershipAddressAvailableTx(tx, moved); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`DELETE FROM network_memberships WHERE device_id=? AND network_id=?`,
		membership.DeviceID, membership.NetworkID,
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "device.member-rm", membershipAuditSummary(membership), source); err != nil {
		return nil, err
	}
	var registrationOrder int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(registration_order),0)+1 FROM network_memberships`,
	).Scan(&registrationOrder); err != nil {
		return nil, err
	}
	moved.RegistrationOrder = registrationOrder
	moved.CreatedAt, moved.UpdatedAt = time.Unix(now(), 0), time.Unix(now(), 0)
	if _, err := tx.Exec(
		`INSERT INTO network_memberships(device_id,network_id,address,address_lower,registration_order,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		moved.DeviceID, moved.NetworkID, moved.Address, strings.ToLower(moved.Address),
		moved.RegistrationOrder, moved.CreatedAt.Unix(), moved.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "device.member-add", membershipAuditSummary(moved), source); err != nil {
		return nil, err
	}
	warning, err := physicalVirtualAddressConflictWarningTx(tx, moved)
	if err != nil {
		return nil, err
	}
	return &NetworkMembershipWriteResult{Membership: moved, Warning: warning}, tx.Commit()
}

// RemoveNetworkMembership 删除设备在指定网络里的成员。数据行与审计行在
// 同一个事务里提交。
func (s *Store) RemoveNetworkMembership(deviceName, networkName string) (*models.NetworkMembership, error) {
	return s.RemoveNetworkMembershipWithSource(deviceName, networkName, WriteSource{})
}

// RemoveNetworkMembershipWithSource 与 RemoveNetworkMembership 相同，并把
// 同一事务里的审计行标为指定来源。
func (s *Store) RemoveNetworkMembershipWithSource(
	deviceName, networkName string, source WriteSource,
) (*models.NetworkMembership, error) {
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
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "device.member-rm", membershipAuditSummary(membership), source); err != nil {
		return nil, err
	}
	return membership, tx.Commit()
}

// RemovePhysicalNetworkMemberships 删除设备的全部物理局域网成员，虚拟局
// 域网成员保留。全部数据行与审计行（沿用 member-rm 动作）在同一个事务
// 里提交；返回被删除的成员，供调用方给出与命令行一致的提示。
func (s *Store) RemovePhysicalNetworkMemberships(deviceName string) ([]*models.NetworkMembership, error) {
	return s.RemovePhysicalNetworkMembershipsWithSource(deviceName, WriteSource{})
}

// RemovePhysicalNetworkMembershipsWithSource 与
// RemovePhysicalNetworkMemberships 相同，并把同一事务里的全部审计行标为
// 指定来源。
func (s *Store) RemovePhysicalNetworkMembershipsWithSource(
	deviceName string, source WriteSource,
) ([]*models.NetworkMembership, error) {
	if s.readOnly {
		return nil, ErrReadOnly
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
	rows, err := tx.Query(`
		SELECT member.device_id,device.name,member.network_id,network.name,network.type,
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
		FROM network_memberships member
		JOIN devices device ON device.id=member.device_id
		JOIN networks network ON network.id=member.network_id
		LEFT JOIN sites site ON site.id=network.site_id
		WHERE member.device_id=? AND network.type=?
		ORDER BY member.registration_order`, device.ID, string(models.NetworkPhysical))
	if err != nil {
		return nil, err
	}
	removed := make([]*models.NetworkMembership, 0)
	for rows.Next() {
		membership, err := scanNetworkMembership(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		removed = append(removed, membership)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	auditAt := s.nowTime()
	for _, membership := range removed {
		if _, err := tx.Exec(
			`DELETE FROM network_memberships WHERE device_id=? AND network_id=?`,
			membership.DeviceID, membership.NetworkID,
		); err != nil {
			return nil, err
		}
		if err := writeTopologyAuditSourceTx(tx, auditAt, "device.member-rm", membershipAuditSummary(membership), source); err != nil {
			return nil, err
		}
	}
	return removed, tx.Commit()
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

// physicalVirtualAddressConflictWarningTx 在一次网络成员写入完成后检查同一
// 设备的其他成员：若另一类网络里有等价地址，返回不阻止写入的警告。地址
// 等价判断复用统一的 SSH 主机规范化函数。
func physicalVirtualAddressConflictWarningTx(
	tx *sql.Tx, membership *models.NetworkMembership,
) (string, error) {
	if membership == nil ||
		(membership.NetworkType != models.NetworkPhysical && membership.NetworkType != models.NetworkVirtual) {
		return "", nil
	}
	memberships, err := networkMembershipsForDeviceTx(tx, membership.DeviceID)
	if err != nil {
		return "", err
	}
	addressKey := NormalizeSSHHost(membership.Address)
	for _, other := range memberships {
		if other == nil || other.NetworkType == membership.NetworkType ||
			(other.NetworkType != models.NetworkPhysical && other.NetworkType != models.NetworkVirtual) ||
			NormalizeSSHHost(other.Address) != addressKey {
			continue
		}
		physicalNetwork, virtualNetwork := membership.NetworkName, other.NetworkName
		if membership.NetworkType == models.NetworkVirtual {
			physicalNetwork, virtualNetwork = other.NetworkName, membership.NetworkName
		}
		return fmt.Sprintf(
			"警告：地址 %s 同时登记在物理局域网 %s 与虚拟局域网 %s。这样无法证明首次连接实际走的是虚拟局域网，这台设备的服务器条目将无法自动首次锚定。请把其中一个成员改成所在网络自己的地址；确需先连接时，可用带外锚定：sshmgr servers pin-hostkey <服务器条目名> --fingerprint SHA256:...。",
			membership.Address, physicalNetwork, virtualNetwork,
		), nil
	}
	return "", nil
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
	return &TopologyValidationError{Message: fmt.Sprintf(
		"网络 %s 的地址 %s 已被设备 %s 占用", membership.NetworkName, membership.Address, existingDevice)}
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
