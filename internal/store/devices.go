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

// ServerDeviceMove 描述一次服务器条目关联或移动的结果，供命令行输出。
type ServerDeviceMove struct {
	Server     *models.Server
	FromDevice *models.Device
	ToDevice   *models.Device
	AnchorCopy *ServerAnchorCopy
}

// AddServerDeviceResult 描述 servers add 连同设备归属一起落库后的结果。
type AddServerDeviceResult struct {
	ServerID      string
	Device        *models.Device
	CreatedDevice bool
	AnchorCopy    *ServerAnchorCopy
}

// ServerAnchorCopy 描述设备内兄弟条目锚复制的判定结果。
type ServerAnchorCopy struct {
	Copied         bool
	FromServerName string
	AnchorConflict bool
}

// AddDevice 登记一台设备。客户端实例按名字解析成设备码标识符；设备行与
// 审计行在同一个事务里提交。
func (s *Store) AddDevice(name, note, client string) (*models.Device, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	name = strings.TrimSpace(name)
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
	if err := insertDeviceTx(tx, device); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.add", deviceAuditSummary(device)); err != nil {
		return nil, err
	}
	return device, tx.Commit()
}

// insertDeviceTx 写入一台设备；自动生成与手工登记共用这一份列映射。
func insertDeviceTx(tx *sql.Tx, device *models.Device) error {
	_, err := tx.Exec(
		`INSERT INTO devices(id,name,name_lower,note,client_token_id,client_name,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		device.ID, device.Name, strings.ToLower(device.Name), device.Note,
		nullableString(device.ClientTokenID), device.ClientName,
		device.CreatedAt.Unix(), device.UpdatedAt.Unix(),
	)
	return err
}

// EditDevice 按设备名字做部分更新。客户端实例关联既能换成另一个设备码，
// 也能通过指向空字符串的 Client 字段解除。数据行与审计行同事务提交。
func (s *Store) EditDevice(name string, changes DeviceChanges) (*models.Device, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	name = strings.TrimSpace(name)
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
		newName := strings.TrimSpace(*changes.Name)
		if err := validateTopologyName("设备", newName); err != nil {
			return nil, err
		}
		if err := ensureDeviceNameAvailableTx(tx, newName, device.ID); err != nil {
			return nil, err
		}
		device.Name = newName
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

// MoveServerToDevice 把服务器条目关联到目标设备；已有归属时就是移动。
// 条目、目标设备与 owner 审计在同一个事务里提交。
func (s *Store) MoveServerToDevice(serverIdentifier, deviceName string) (*ServerDeviceMove, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	srv, err := serverByIdentifierTx(tx, strings.TrimSpace(serverIdentifier))
	if err != nil {
		return nil, err
	}
	target, err := deviceByNameTx(tx, deviceName)
	if err != nil {
		return nil, err
	}
	var from *models.Device
	if srv.DeviceID != "" {
		from, err = deviceByIDTx(tx, srv.DeviceID)
		if err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE servers SET device_id=? WHERE id=?`, nullableString(target.ID), srv.ID); err != nil {
		return nil, err
	}
	srv.DeviceID = target.ID
	anchorCopy, err := s.copySiblingEntryPinTx(tx, srv)
	if err != nil {
		return nil, err
	}
	summary := map[string]any{"server": srv.Name, "to": target.Name}
	if from != nil {
		summary["from"] = from.Name
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "device.move-server", summary); err != nil {
		return nil, err
	}
	return &ServerDeviceMove{
		Server: srv, FromDevice: from, ToDevice: target, AnchorCopy: anchorCopy,
	}, tx.Commit()
}

// AddServerWithCredentialsToDevice 新增服务器条目并处理它的设备归属：目标
// 设备为空时自动生成同名设备（设备名冲突加数字后缀）；给出目标设备时关联
// 已有设备。条目、凭据、设备与审计在同一个事务里提交。
func (s *Store) AddServerWithCredentialsToDevice(
	srv *models.Server, cred, sudo *models.Credential, deviceName string,
) (*AddServerDeviceResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if err := validateServerText(srv); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	added, err := s.addServerWithCredentialsToDeviceTx(tx, srv, cred, sudo, deviceName)
	if err != nil {
		return nil, err
	}
	return added, tx.Commit()
}

// addServerWithCredentialsToDeviceTx 是新增服务器条目的唯一存储层主体：
// 凭据、所属设备、条目与锚复制共用一个事务。deviceName 为空时自动生成
// 同名设备（重名追加数字后缀），非空时关联已有设备。
func (s *Store) addServerWithCredentialsToDeviceTx(
	tx *sql.Tx, srv *models.Server, cred, sudo *models.Credential, deviceName string,
) (*AddServerDeviceResult, error) {
	srv.CredentialID, srv.AuthMethod = "", ""
	if cred != nil {
		cid, err := insertCredentialTx(tx, s.masterKey, cred)
		if err != nil {
			return nil, err
		}
		srv.CredentialID, srv.AuthMethod = cid, cred.Type.AuthMethodForServer()
	}
	if sudo != nil {
		sid, err := insertCredentialTx(tx, s.masterKey, sudo)
		if err != nil {
			return nil, err
		}
		srv.SudoCredentialID = sid
	}

	created := false
	device := &models.Device{Name: strings.TrimSpace(deviceName)}
	var err error
	if device.Name == "" {
		device.Name, err = availableDeviceNameTx(tx, srv.Name)
		if err != nil {
			return nil, err
		}
		created = true
	} else {
		device, err = deviceByNameTx(tx, device.Name)
		if err != nil {
			return nil, err
		}
	}
	if created {
		device.ID = newID()
		device.CreatedAt, device.UpdatedAt = time.Unix(now(), 0), time.Unix(now(), 0)
		if err := insertDeviceTx(tx, device); err != nil {
			return nil, err
		}
		if err := writeTopologyAuditTx(tx, s.nowTime(), "device.add", deviceAuditSummary(device)); err != nil {
			return nil, err
		}
	}
	srv.DeviceID = device.ID
	id, err := insertServerTx(tx, srv)
	if err != nil {
		return nil, err
	}
	srv.ID = id
	anchorCopy, err := s.copySiblingEntryPinTx(tx, srv)
	if err != nil {
		return nil, err
	}
	return &AddServerDeviceResult{
		ServerID: id, Device: device, CreatedDevice: created, AnchorCopy: anchorCopy,
	}, nil
}

// copySiblingEntryPinTx 在同一个事务里判断并复制同设备、同端口兄弟条目
// 的锚。目标条目已有锚、没有可复制锚、或多个兄弟锚互不一致时不覆盖。
func (s *Store) copySiblingEntryPinTx(tx *sql.Tx, srv *models.Server) (*ServerAnchorCopy, error) {
	existing, err := loadEntryPinMeta(tx, srv.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &ServerAnchorCopy{}, nil
	}
	rows, err := tx.Query(`
		SELECT pin.server_id,server.name,pin.key_blob,pin.pin_format,pin.pin_source,
		       pin.pin_device,pin.handshake_addr,pin.created_at
		FROM servers server
		JOIN entry_host_keys pin ON pin.server_id=server.id
		WHERE server.device_id=? AND server.port=? AND server.id<>?
		ORDER BY server.name,server.id`, srv.DeviceID, srv.Port, srv.ID)
	if err != nil {
		return nil, err
	}
	type siblingPin struct {
		name string
		pin  PinMeta
	}
	var siblings []siblingPin
	identities := make(map[string]struct{})
	for rows.Next() {
		var item siblingPin
		if err := rows.Scan(&item.pin.ServerID, &item.name, &item.pin.Blob, &item.pin.Format,
			&item.pin.Source, &item.pin.Device, &item.pin.HandshakeAddr, &item.pin.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		siblings = append(siblings, item)
		invalidIdentity := fmt.Sprintf("raw:%x", item.pin.Blob)
		identities[pinKeyIdentity(item.pin.Blob, item.pin.Format, invalidIdentity)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(siblings) == 0 {
		return &ServerAnchorCopy{}, nil
	}
	if len(identities) > 1 {
		return &ServerAnchorCopy{AnchorConflict: true}, nil
	}

	source := siblings[0]
	handshakeAddr := source.pin.HandshakeAddr
	if handshakeAddr == "" {
		handshakeAddr = EntryFallbackAddr(srv)
	}
	audit := AuditRow{
		TS:       s.nowTime(),
		ServerID: srv.ID,
		Action:   "pin-copy",
		Command: MarshalAuditSummary(map[string]any{
			"entry":   srv.Name,
			"host":    EntryFallbackAddr(srv),
			"主机密钥指纹":  PinFingerprint(source.pin.Blob, source.pin.Format),
			"来源服务器条目": source.name,
		}),
		Status: "ok",
	}
	inserted, err := insertEntryPinWithAuditTx(tx, srv.ID, handshakeAddr,
		source.pin.Blob, source.pin.Format, source.pin.Source, source.pin.Device, audit)
	if err != nil {
		return nil, err
	}
	return &ServerAnchorCopy{Copied: inserted, FromServerName: source.name}, nil
}

// ListServersForDevice 返回一台设备关联的服务器条目摘要，按条目名字排序。
func (s *Store) ListServersForDevice(deviceName string) ([]*models.DeviceServer, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	return deviceServersForDeviceTx(s.db, device.ID)
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
				WHERE member.device_id=devices.id AND network.type=? AND lower(site.name)=lower(?))`)
		arguments = append(arguments, string(models.NetworkPhysical), strings.TrimSpace(filter.Site))
	}
	if filter.Network != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM network_memberships member
			JOIN networks network ON network.id=member.network_id
				WHERE member.device_id=devices.id AND lower(network.name)=lower(?))`)
		arguments = append(arguments, strings.TrimSpace(filter.Network))
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
		       COALESCE(site.name,''),member.address,member.registration_order,member.created_at,member.updated_at
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
	name = strings.TrimSpace(name)
	device, err := scanDevice(q.QueryRow(`
		SELECT id,name,note,COALESCE(client_token_id,''),client_name,created_at,updated_at
		FROM devices WHERE name_lower=?`, strings.ToLower(name),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：%s", ErrDeviceNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	return device, nil
}

// deviceByIDTx 按内部标识符取设备，供服务器条目读取原设备时使用。
func deviceByIDTx(q deviceQueryer, id string) (*models.Device, error) {
	device, err := scanDevice(q.QueryRow(`
		SELECT id,name,note,COALESCE(client_token_id,''),client_name,created_at,updated_at
		FROM devices WHERE id=?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：%s", ErrDeviceNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return device, nil
}

// scanDevice 把设备行扫描成对象；按名字与按内部标识符查询共用。
func scanDevice(sc scanner) (*models.Device, error) {
	var device models.Device
	var created, updated int64
	if err := sc.Scan(&device.ID, &device.Name, &device.Note, &device.ClientTokenID,
		&device.ClientName, &created, &updated); err != nil {
		return nil, err
	}
	device.CreatedAt, device.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &device, nil
}

// serverByIdentifierTx 先按内部标识符、再按名字解析服务器条目。
func serverByIdentifierTx(tx *sql.Tx, identifier string) (*models.Server, error) {
	srv, err := getServerTx(tx, identifier)
	if err == nil {
		return srv, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	row := tx.QueryRow(`
		SELECT id,name,host,port,user,COALESCE(device_id,''),auth_method,credential_id,sudo_credential_id,shell_type,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at,revision
		FROM servers WHERE name=?`, identifier)
	srv, err = scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("服务器条目不存在：%s", identifier)
	}
	return srv, err
}

// ensureDeviceNameAvailableTx 检查设备名字在小写比较下的唯一性。
func ensureDeviceNameAvailableTx(tx *sql.Tx, name, excludeID string) error {
	var count int
	name = strings.TrimSpace(name)
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

// availableDeviceNameTx 返回可用的设备名字；与服务器条目同名冲突时从
// 「-2」开始追加数字后缀。
func availableDeviceNameTx(tx *sql.Tx, base string) (string, error) {
	base = strings.TrimSpace(base)
	if err := validateTopologyName("设备", base); err != nil {
		return "", err
	}
	for number := 1; ; number++ {
		name := base
		if number > 1 {
			name = fmt.Sprintf("%s-%d", base, number)
		}
		err := ensureDeviceNameAvailableTx(tx, name, "")
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, ErrDeviceNameExists) {
			return "", err
		}
	}
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
