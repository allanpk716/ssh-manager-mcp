package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// 共享存储操作的稳定错误；报错文本直接面向 owner，不包含挂载密码。
var (
	ErrSharedStorageNotFound     = errors.New("共享存储不存在")
	ErrSharedStorageNameExists   = errors.New("共享名已存在（大小写不同的共享名视为同一个）")
	ErrSharedStorageDriveExists  = errors.New("盘符已存在")
	ErrSharedStorageDriveInvalid = errors.New("盘符不合法：只接受 D 到 Z 的单个字母（大小写均可），登记后存为大写")
)

// SharedStorageValidationError 表示共享存储登记输入校验失败。错误文字只
// 描述规则本身，可以直接展示给操作者。
type SharedStorageValidationError struct {
	Err error
}

func (e *SharedStorageValidationError) Error() string { return e.Err.Error() }
func (e *SharedStorageValidationError) Unwrap() error { return e.Err }

// IsSharedStorageValidationError 判断错误是否属于共享存储登记输入校验失
// 败；其余错误可能带内部细节，调用方必须改用通用文案。
func IsSharedStorageValidationError(err error) bool {
	var validation *SharedStorageValidationError
	if errors.As(err, &validation) {
		return true
	}
	return errors.Is(err, ErrSharedStorageNameExists) ||
		errors.Is(err, ErrSharedStorageDriveExists) ||
		errors.Is(err, ErrSharedStorageDriveInvalid)
}

func sharedStorageValidation(err error) error {
	return &SharedStorageValidationError{Err: err}
}

// SharedStorageInput 是一次共享存储登记的完整输入。目录约定保持给定顺序。
type SharedStorageInput struct {
	Name           string
	Device         string
	User           string
	PasswordSecret string
	Drive          string
	Directories    []models.SharedStorageDirectory
	Note           string
}

// SharedStorageFilter 是共享存储列表的筛选条件；空字符串表示不筛选。
type SharedStorageFilter struct {
	Device string
	Server string
}

// SharedStorageChanges 表示共享存储的一次部分更新。空指针表示保持不变；
// Directories 指向空列表表示清空目录约定；ChangeNote 不给时清空旧说明。
type SharedStorageChanges struct {
	Name           *string
	Device         *string
	User           *string
	PasswordSecret *string
	Drive          *string
	Directories    *[]models.SharedStorageDirectory
	Note           *string
	ChangeNote     *string
}

// ValidateSharedStorageDrive 校验并归一化 Windows 固定盘符。
func ValidateSharedStorageDrive(drive string) (string, error) {
	value := strings.ToUpper(strings.TrimSpace(drive))
	if len(value) != 1 || value < "D" || value > "Z" {
		return "", ErrSharedStorageDriveInvalid
	}
	return value, nil
}

// validateSharedStorageDirectories 校验目录约定：用途与共享内部子路径都
// 必须非空，用途不能重复。这里不猜测路径分隔符规则，子路径原样保存。
func validateSharedStorageDirectories(directories []models.SharedStorageDirectory) error {
	seen := make(map[string]bool, len(directories))
	for _, directory := range directories {
		purpose := strings.TrimSpace(directory.Purpose)
		path := strings.TrimSpace(directory.Path)
		if purpose == "" || path == "" {
			return errors.New("目录约定的用途与子路径都不能为空，格式为 用途=子路径")
		}
		if seen[purpose] {
			return fmt.Errorf("目录约定的用途不能重复：%s", purpose)
		}
		seen[purpose] = true
	}
	return nil
}

// normalizeSharedStorageDirectories 去掉目录约定两端的空白并保持顺序。
func normalizeSharedStorageDirectories(directories []models.SharedStorageDirectory) []models.SharedStorageDirectory {
	out := make([]models.SharedStorageDirectory, 0, len(directories))
	for _, directory := range directories {
		out = append(out, models.SharedStorageDirectory{
			Purpose: strings.TrimSpace(directory.Purpose),
			Path:    strings.TrimSpace(directory.Path),
		})
	}
	return out
}

// ParseSharedStorageDirectoryLines 把「用途=子路径」形式的目录约定输入解析
// 成目录结构。命令行与 Web UI 共用这一份解析，解析后的进一步校验仍由
// 新增与修改入口完成。
func ParseSharedStorageDirectoryLines(values []string) ([]models.SharedStorageDirectory, error) {
	out := make([]models.SharedStorageDirectory, 0, len(values))
	for _, value := range values {
		purpose, path, found := strings.Cut(value, "=")
		if !found {
			return nil, &SharedStorageValidationError{
				Err: fmt.Errorf("目录约定 %q 缺少等号，格式为 用途=子路径", value),
			}
		}
		out = append(out, models.SharedStorageDirectory{Purpose: purpose, Path: path})
	}
	return out, nil
}

// AddSharedStorage 登记一处 SMB 共享存储。提供方设备、挂载密码机密项、
// 共享名与盘符全部校验通过后才落库；数据行与审计行在同一个事务提交。
func (s *Store) AddSharedStorage(input SharedStorageInput, source WriteSource) (*models.SharedStorage, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	name := strings.TrimSpace(input.Name)
	if err := validateTopologyName("共享", name); err != nil {
		return nil, sharedStorageValidation(err)
	}
	user := strings.TrimSpace(input.User)
	if user == "" {
		return nil, &SharedStorageValidationError{Err: errors.New("挂载账号用户名不能为空")}
	}
	drive, err := ValidateSharedStorageDrive(input.Drive)
	if err != nil {
		return nil, sharedStorageValidation(err)
	}
	directories := normalizeSharedStorageDirectories(input.Directories)
	if err := validateSharedStorageDirectories(directories); err != nil {
		return nil, sharedStorageValidation(err)
	}
	directoriesJSON, err := json.Marshal(directories)
	if err != nil {
		return nil, err
	}
	note := strings.TrimSpace(input.Note)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	device, err := sharedStorageDeviceTx(tx, input.Device)
	if err != nil {
		return nil, err
	}
	secret, err := sharedStorageSecretTx(tx, input.PasswordSecret)
	if err != nil {
		return nil, sharedStorageValidation(err)
	}
	if err := ensureSharedStorageNameAvailableTx(tx, name, ""); err != nil {
		return nil, sharedStorageValidation(err)
	}
	if err := ensureSharedStorageDriveAvailableTx(tx, drive, ""); err != nil {
		return nil, sharedStorageValidation(err)
	}

	share := &models.SharedStorage{
		ID: newID(), Name: name, DeviceID: device.ID, DeviceName: device.Name,
		MountUser: user, PasswordSecretID: secret.ID, PasswordSecretName: secret.Name,
		Drive: drive, Directories: directories, Note: note, Version: 1,
	}
	createdAt := s.nowTime()
	share.CreatedAt, share.UpdatedAt = createdAt, createdAt
	if _, err := tx.Exec(`
		INSERT INTO shared_storages(
			id,name,name_lower,device_id,mount_user,password_secret_id,drive,note,directories,version,change_note,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,1,'',?,?)`,
		share.ID, share.Name, strings.ToLower(share.Name), share.DeviceID, share.MountUser,
		share.PasswordSecretID, share.Drive, share.Note, string(directoriesJSON),
		share.CreatedAt.Unix(), share.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	if err := writeSharedStorageAuditTx(tx, createdAt, "share-add", map[string]any{
		"name": share.Name, "device": share.DeviceName, "user": share.MountUser,
		"password-secret": share.PasswordSecretName, "drive": share.Drive,
		"directories": directoryAuditValue(share.Directories), "note": share.Note,
	}, source); err != nil {
		return nil, err
	}
	loaded, err := s.sharedStorageByIDTx(tx, share.ID)
	if err != nil {
		return nil, err
	}
	return loaded, tx.Commit()
}

// GetSharedStorage 按共享名查询登记；共享名比较不区分大小写。
func (s *Store) GetSharedStorage(name string) (*models.SharedStorage, error) {
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM shared_storages WHERE name_lower=?`, strings.ToLower(strings.TrimSpace(name)),
	).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSharedStorageNotFound
		}
		return nil, err
	}
	return s.sharedStorageByNameTx(s.db, name)
}

// ListSharedStorages 返回共享存储，可按提供方设备或可挂载服务器筛选。
func (s *Store) ListSharedStorages(filter SharedStorageFilter) ([]*models.SharedStorage, error) {
	query := `
		SELECT share.id
		FROM shared_storages share
		LEFT JOIN devices device ON device.id=share.device_id`
	conditions := make([]string, 0, 2)
	arguments := make([]any, 0, 2)
	if filter.Device != "" {
		conditions = append(conditions, "lower(device.name)=lower(?)")
		arguments = append(arguments, strings.TrimSpace(filter.Device))
	}
	if filter.Server != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM secret_server_allow allow
			JOIN servers server ON server.id=allow.server_id
			WHERE allow.secret_id=share.password_secret_id
			  AND (server.name=? OR server.id=?))`)
		server := strings.TrimSpace(filter.Server)
		arguments = append(arguments, server, server)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY share.name,share.id"
	ids, err := sharedStorageIDsByQuery(s.db, query, arguments...)
	if err != nil {
		return nil, err
	}
	return s.sharedStoragesByIDs(s.db, ids)
}

// sharedStorageIDsByQuery 执行只返回共享存储编号的查询。列表查询与设备
// 删除预览共用它，避免两处各自维护结果集循环。
func sharedStorageIDsByQuery(db dbtx, query string, arguments ...any) ([]string, error) {
	rows, err := db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// sharedStoragesByIDs 按编号逐个读取完整共享存储，保持查询给出的顺序。
func (s *Store) sharedStoragesByIDs(db dbtx, ids []string) ([]*models.SharedStorage, error) {
	out := make([]*models.SharedStorage, 0, len(ids))
	for _, id := range ids {
		share, err := s.sharedStorageByIDTx(db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, share)
	}
	return out, nil
}

// EditSharedStorage 按共享名做部分更新。每次成功修改都让共享存储版本号
// 加一；带 ChangeNote 就写入新说明，不带就清空旧说明。数据行与审计行在
// 同一个事务提交。
func (s *Store) EditSharedStorage(name string, changes SharedStorageChanges, source WriteSource) (*models.SharedStorage, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if changes.Name == nil && changes.Device == nil && changes.User == nil &&
		changes.PasswordSecret == nil && changes.Drive == nil && changes.Directories == nil &&
		changes.Note == nil && changes.ChangeNote == nil {
		return nil, errors.New("共享存储修改至少要给出一个要修改的字段")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	share, err := s.sharedStorageByNameTx(tx, name)
	if err != nil {
		return nil, err
	}
	editedFields := make(map[string]any)
	if changes.Name != nil {
		newName := strings.TrimSpace(*changes.Name)
		if err := validateTopologyName("共享", newName); err != nil {
			return nil, sharedStorageValidation(err)
		}
		if err := ensureSharedStorageNameAvailableTx(tx, newName, share.ID); err != nil {
			return nil, sharedStorageValidation(err)
		}
		share.Name = newName
		editedFields["name"] = share.Name
	}
	if changes.Device != nil {
		device, err := sharedStorageDeviceTx(tx, *changes.Device)
		if err != nil {
			return nil, err
		}
		share.DeviceID, share.DeviceName = device.ID, device.Name
		editedFields["device"] = share.DeviceName
	}
	if changes.User != nil {
		user := strings.TrimSpace(*changes.User)
		if user == "" {
			return nil, &SharedStorageValidationError{Err: errors.New("挂载账号用户名不能为空")}
		}
		share.MountUser = user
		editedFields["user"] = share.MountUser
	}
	if changes.PasswordSecret != nil {
		secret, err := sharedStorageSecretTx(tx, *changes.PasswordSecret)
		if err != nil {
			return nil, err
		}
		share.PasswordSecretID, share.PasswordSecretName = secret.ID, secret.Name
		editedFields["password-secret"] = share.PasswordSecretName
	}
	if changes.Drive != nil {
		drive, err := ValidateSharedStorageDrive(*changes.Drive)
		if err != nil {
			return nil, sharedStorageValidation(err)
		}
		if err := ensureSharedStorageDriveAvailableTx(tx, drive, share.ID); err != nil {
			return nil, sharedStorageValidation(err)
		}
		share.Drive = drive
		editedFields["drive"] = share.Drive
	}
	if changes.Directories != nil {
		directories := normalizeSharedStorageDirectories(*changes.Directories)
		if err := validateSharedStorageDirectories(directories); err != nil {
			return nil, sharedStorageValidation(err)
		}
		share.Directories = directories
		editedFields["directories"] = directoryAuditValue(share.Directories)
	}
	if changes.Note != nil {
		share.Note = strings.TrimSpace(*changes.Note)
		editedFields["note"] = share.Note
	}
	changeNote := ""
	if changes.ChangeNote != nil {
		changeNote = strings.TrimSpace(*changes.ChangeNote)
		editedFields["change-note"] = changeNote
	}
	share.ChangeNote = changeNote
	directoriesJSON, err := json.Marshal(share.Directories)
	if err != nil {
		return nil, err
	}
	changedAt := s.nowTime()
	share.UpdatedAt = changedAt
	if _, err := tx.Exec(`
		UPDATE shared_storages SET
			name=?,name_lower=?,device_id=?,mount_user=?,password_secret_id=?,drive=?,
			note=?,directories=?,version=version+1,change_note=?,updated_at=?
		WHERE id=?`,
		share.Name, strings.ToLower(share.Name), share.DeviceID, share.MountUser,
		share.PasswordSecretID, share.Drive, share.Note, string(directoriesJSON),
		share.ChangeNote, share.UpdatedAt.Unix(), share.ID,
	); err != nil {
		return nil, err
	}
	if err := writeSharedStorageAuditTx(tx, changedAt, "share-edit", map[string]any{
		"name": share.Name, "changes": editedFields,
	}, source); err != nil {
		return nil, err
	}
	loaded, err := s.sharedStorageByIDTx(tx, share.ID)
	if err != nil {
		return nil, err
	}
	return loaded, tx.Commit()
}

// DeleteSharedStorage 只删除共享存储登记，不触达任何服务器。返回删除前的
// 登记供命令行列出可挂载范围；数据行与审计行在同一个事务提交。
func (s *Store) DeleteSharedStorage(name string, source WriteSource) (*models.SharedStorage, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	share, err := s.sharedStorageByNameTx(tx, name)
	if err != nil {
		return nil, err
	}
	deletedAt := s.nowTime()
	if _, err := tx.Exec(`DELETE FROM shared_storages WHERE id=?`, share.ID); err != nil {
		return nil, err
	}
	if err := writeSharedStorageAuditTx(tx, deletedAt, "share-rm", map[string]any{
		"name": share.Name, "device": share.DeviceName,
	}, source); err != nil {
		return nil, err
	}
	return share, tx.Commit()
}

// sharedStorageByIDTx 读取一条共享存储，并补齐候选访问地址、目录约定与
// 可挂载服务器。db 可以是普通连接或写事务。客户端缓存允许设备行缺失，
// 此时使用快照带来的提供方设备名。
func (s *Store) sharedStorageByIDTx(db dbtx, id string) (*models.SharedStorage, error) {
	share := &models.SharedStorage{}
	var directoriesJSON string
	var createdAt, updatedAt int64
	var deviceName sql.NullString
	err := db.QueryRow(`
		SELECT share.id,share.name,share.device_id,device.name,share.mount_user,
		       share.password_secret_id,secret.name,share.drive,share.note,share.directories,
		       share.version,share.change_note,share.created_at,share.updated_at
		FROM shared_storages share
		LEFT JOIN devices device ON device.id=share.device_id
		JOIN secrets secret ON secret.id=share.password_secret_id
		WHERE share.id=?`, id,
	).Scan(&share.ID, &share.Name, &share.DeviceID, &deviceName, &share.MountUser,
		&share.PasswordSecretID, &share.PasswordSecretName, &share.Drive, &share.Note,
		&directoriesJSON, &share.Version, &share.ChangeNote, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSharedStorageNotFound
	}
	if err != nil {
		return nil, err
	}
	share.DeviceName = deviceName.String
	if share.DeviceName == "" {
		share.DeviceName = s.sharedStorageDeviceNames[share.ID]
	}
	share.CreatedAt, share.UpdatedAt = time.Unix(createdAt, 0), time.Unix(updatedAt, 0)
	directories, err := decodeSharedStorageDirectories(directoriesJSON)
	if err != nil {
		return nil, fmt.Errorf("读取共享存储 %s 的目录约定失败: %w", share.Name, err)
	}
	share.Directories = directories
	if err := loadSharedStorageDetails(db, share); err != nil {
		return nil, err
	}
	return share, nil
}

func (s *Store) sharedStorageByNameTx(db dbtx, name string) (*models.SharedStorage, error) {
	var id string
	if err := db.QueryRow(
		`SELECT id FROM shared_storages WHERE name_lower=?`, strings.ToLower(strings.TrimSpace(name)),
	).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSharedStorageNotFound
		}
		return nil, err
	}
	return s.sharedStorageByIDTx(db, id)
}

func loadSharedStorageDetails(db dbtx, share *models.SharedStorage) error {
	addresses, err := sharedStorageCandidateAddresses(db, share.DeviceID)
	if err != nil {
		return err
	}
	share.CandidateAddresses = addresses
	servers, err := sharedStorageMountableServers(db, share)
	if err != nil {
		return err
	}
	share.MountableServers = servers
	return nil
}

func sharedStorageCandidateAddresses(db dbtx, deviceID string) ([]string, error) {
	rows, err := db.Query(`
		SELECT member.address
		FROM network_memberships member
		JOIN networks network ON network.id=member.network_id
		WHERE member.device_id=?
		ORDER BY network.name,member.address`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	addresses := make([]string, 0)
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	return addresses, rows.Err()
}

func sharedStorageMountableServers(db dbtx, share *models.SharedStorage) ([]models.SharedStorageServer, error) {
	rows, err := db.Query(`
		SELECT server.id,server.name,server.shell_type,COALESCE(device.name,'')
		FROM secret_server_allow allow
		JOIN servers server ON server.id=allow.server_id
		LEFT JOIN devices device ON device.id=server.device_id
		WHERE allow.secret_id=?
		ORDER BY server.name,server.id`, share.PasswordSecretID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	servers := make([]models.SharedStorageServer, 0)
	for rows.Next() {
		var server models.SharedStorageServer
		var shellType string
		if err := rows.Scan(&server.ID, &server.Name, &shellType, &server.DeviceName); err != nil {
			return nil, err
		}
		server.ShellType = models.NormalizeShellType(shellType)
		server.MountPoint = SharedStorageMountPoint(share.Drive, share.Name, server.ShellType)
		servers = append(servers, server)
	}
	return servers, rows.Err()
}

// SharedStorageMountPoint 按服务器系统类型推算挂载点约定。这里只做登记
// 与查看，不做任何现场检测。
func SharedStorageMountPoint(drive, shareName string, shellType models.ShellType) string {
	if shellType.IsWindows() {
		return drive + ":"
	}
	return "/mnt/sshmgr/" + shareName
}

// decodeSharedStorageDirectories 解码存储层保存的目录约定数组，并把缺失
// 数组统一成空数组。
func decodeSharedStorageDirectories(directoriesJSON string) ([]models.SharedStorageDirectory, error) {
	var directories []models.SharedStorageDirectory
	if err := json.Unmarshal([]byte(directoriesJSON), &directories); err != nil {
		return nil, err
	}
	if directories == nil {
		return []models.SharedStorageDirectory{}, nil
	}
	return directories, nil
}

func sharedStorageDeviceTx(tx *sql.Tx, name string) (*models.Device, error) {
	device, err := deviceByNameTx(tx, name)
	if errors.Is(err, ErrDeviceNotFound) {
		return nil, &SharedStorageValidationError{
			Err: fmt.Errorf("提供方设备 %q 不存在：请先执行 sshmgr devices add 登记设备", name),
		}
	}
	return device, err
}

func sharedStorageSecretTx(tx *sql.Tx, name string) (struct{ ID, Name string }, error) {
	var out struct{ ID, Name string }
	err := tx.QueryRow(
		`SELECT id,name FROM secrets WHERE name_lower=?`, strings.ToLower(strings.TrimSpace(name)),
	).Scan(&out.ID, &out.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return out, &SharedStorageValidationError{
			Err: fmt.Errorf("挂载密码机密项 %q 不存在：请先执行 sshmgr secrets add 登记机密项", name),
		}
	}
	return out, err
}

func ensureSharedStorageNameAvailableTx(tx *sql.Tx, name, excludeID string) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM shared_storages WHERE name_lower=? AND id<>?`,
		strings.ToLower(name), excludeID,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w：%s", ErrSharedStorageNameExists, name)
	}
	return nil
}

func ensureSharedStorageDriveAvailableTx(tx *sql.Tx, drive, excludeID string) error {
	var existingName string
	err := tx.QueryRow(
		`SELECT name FROM shared_storages WHERE drive=? AND id<>?`, drive, excludeID,
	).Scan(&existingName)
	if err == nil {
		return fmt.Errorf("%w：%s 已被共享存储 %s 使用", ErrSharedStorageDriveExists, drive, existingName)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func writeSharedStorageAuditTx(tx *sql.Tx, at time.Time, action string, summary map[string]any, source WriteSource) error {
	return writeAuditTx(tx, AuditRow{
		TS: at, Action: action, Command: MarshalAuditSummary(summary), Status: "ok",
		Source: source.kind, SourceIP: source.address,
	})
}

func directoryAuditValue(directories []models.SharedStorageDirectory) []map[string]string {
	out := make([]map[string]string, 0, len(directories))
	for _, directory := range directories {
		out = append(out, map[string]string{"purpose": directory.Purpose, "path": directory.Path})
	}
	return out
}
