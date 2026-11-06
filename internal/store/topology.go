package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// 拓扑操作的稳定错误；报错文本直接面向 owner，自带完整原因。
var (
	ErrSiteNotFound     = errors.New("站点不存在")
	ErrSiteNameExists   = errors.New("站点名字已存在")
	ErrNetworkNotFound  = errors.New("网络不存在")
	ErrNetworkNameTaken = errors.New("网络名字已存在")
)

// SiteChanges 表示站点的一次部分更新。指针为空代表这个字段保持不变；
// 备注传空字符串代表清空备注。
type SiteChanges struct {
	Name *string
	Note *string
}

// NetworkChanges 表示网络的一次部分更新。网络类型没有指针字段：类型登
// 记后不可修改；其余字段按指针是否为空判断。
type NetworkChanges struct {
	Name          *string
	Site          *string
	Note          *string
	NetworkRange  *string
	BandwidthMbps *int64
	Metered       *bool
}

// NetworkFilter 是网络列表的筛选条件；空字符串表示不筛选。
type NetworkFilter struct {
	Site string
	Type models.NetworkType
}

// validateTopologyName 只拒绝空名字与首尾空白名字，不限制 owner 使用的
// 字符；名字唯一性另按小写形式检查。
func validateTopologyName(objectType, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s名字不能为空", objectType)
	}
	return nil
}

// AddSite 登记一个站点。站点行与审计行在同一个事务里提交。
func (s *Store) AddSite(name, note string) (*models.Site, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if err := validateTopologyName("站点", name); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := ensureSiteNameAvailableTx(tx, name, ""); err != nil {
		return nil, err
	}
	site := &models.Site{
		ID:        newID(),
		Name:      name,
		Note:      strings.TrimSpace(note),
		CreatedAt: time.Unix(now(), 0),
		UpdatedAt: time.Unix(now(), 0),
	}
	if _, err := tx.Exec(
		`INSERT INTO sites(id,name,name_lower,note,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		site.ID, site.Name, strings.ToLower(site.Name), site.Note, site.CreatedAt.Unix(), site.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "site.add", map[string]any{
		"name": site.Name,
		"note": site.Note,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return site, nil
}

// EditSite 按站点名字做部分更新。返回更新后的站点；数据行与审计行在
// 同一个事务里提交。
func (s *Store) EditSite(name string, changes SiteChanges) (*models.Site, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	site, err := siteByNameTx(tx, name)
	if err != nil {
		return nil, err
	}
	if changes.Name != nil {
		if err := validateTopologyName("站点", *changes.Name); err != nil {
			return nil, err
		}
		if err := ensureSiteNameAvailableTx(tx, *changes.Name, site.ID); err != nil {
			return nil, err
		}
		site.Name = *changes.Name
	}
	if changes.Note != nil {
		site.Note = strings.TrimSpace(*changes.Note)
	}
	site.UpdatedAt = time.Unix(now(), 0)
	if _, err := tx.Exec(
		`UPDATE sites SET name=?,name_lower=?,note=?,updated_at=? WHERE id=?`,
		site.Name, strings.ToLower(site.Name), site.Note, site.UpdatedAt.Unix(), site.ID,
	); err != nil {
		return nil, err
	}
	editChanges := make(map[string]any)
	if changes.Name != nil {
		editChanges["name"] = site.Name
	}
	if changes.Note != nil {
		editChanges["note"] = site.Note
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "site.edit", topologyEditSummary(name, editChanges)); err != nil {
		return nil, err
	}
	return site, tx.Commit()
}

// AddNetwork 登记一个网络。类型与站点归属在同一个事务里校验，网络行与
// 审计行一起提交。
func (s *Store) AddNetwork(input models.Network) (*models.Network, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if _, err := models.ParseNetworkType(string(input.Type)); err != nil {
		return nil, err
	}
	if err := validateTopologyName("网络", input.Name); err != nil {
		return nil, err
	}
	if input.BandwidthMbps < 0 {
		return nil, errors.New("标称带宽不能小于 0 Mbit/s")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	siteID, siteName, err := networkSiteForTypeTx(tx, input.Type, input.SiteName)
	if err != nil {
		return nil, err
	}
	if err := ensureNetworkNameAvailableTx(tx, input.Name, ""); err != nil {
		return nil, err
	}
	ts := now()
	network := &models.Network{
		ID:            newID(),
		Name:          input.Name,
		Type:          input.Type,
		SiteID:        siteID,
		SiteName:      siteName,
		Note:          strings.TrimSpace(input.Note),
		NetworkRange:  strings.TrimSpace(input.NetworkRange),
		BandwidthMbps: input.BandwidthMbps,
		Metered:       input.Metered,
		CreatedAt:     time.Unix(ts, 0),
		UpdatedAt:     time.Unix(ts, 0),
	}
	if _, err := tx.Exec(`
		INSERT INTO networks(id,name,name_lower,type,site_id,note,cidr,bandwidth_mbps,metered,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		network.ID, network.Name, strings.ToLower(network.Name), string(network.Type), nullableString(network.SiteID),
		network.Note, network.NetworkRange, network.BandwidthMbps, boolInt(network.Metered),
		network.CreatedAt.Unix(), network.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "network.add", networkAuditSummary(network)); err != nil {
		return nil, err
	}
	return network, tx.Commit()
}

// EditNetwork 按网络名字做部分更新；类型不可修改。网络行与审计行在同一
// 个事务里提交。
func (s *Store) EditNetwork(name string, changes NetworkChanges) (*models.Network, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	network, err := networkByNameTx(tx, name)
	if err != nil {
		return nil, err
	}
	editChanges := make(map[string]any)
	if changes.Name != nil {
		if err := validateTopologyName("网络", *changes.Name); err != nil {
			return nil, err
		}
		if err := ensureNetworkNameAvailableTx(tx, *changes.Name, network.ID); err != nil {
			return nil, err
		}
		network.Name = *changes.Name
		editChanges["name"] = network.Name
	}
	if changes.Site != nil {
		siteID, siteName, err := networkSiteForTypeTx(tx, network.Type, *changes.Site)
		if err != nil {
			return nil, err
		}
		network.SiteID, network.SiteName = siteID, siteName
		editChanges["site"] = siteName
	}
	if changes.Note != nil {
		network.Note = strings.TrimSpace(*changes.Note)
		editChanges["note"] = network.Note
	}
	if changes.NetworkRange != nil {
		network.NetworkRange = strings.TrimSpace(*changes.NetworkRange)
		editChanges["cidr"] = network.NetworkRange
	}
	if changes.BandwidthMbps != nil {
		if *changes.BandwidthMbps < 0 {
			return nil, errors.New("标称带宽不能小于 0 Mbit/s")
		}
		network.BandwidthMbps = *changes.BandwidthMbps
		editChanges["bandwidth-mbps"] = network.BandwidthMbps
	}
	if changes.Metered != nil {
		network.Metered = *changes.Metered
		editChanges["metered"] = network.Metered
	}
	network.UpdatedAt = time.Unix(now(), 0)
	if _, err := tx.Exec(`
		UPDATE networks SET name=?,name_lower=?,site_id=?,note=?,cidr=?,bandwidth_mbps=?,metered=?,updated_at=?
		WHERE id=?`,
		network.Name, strings.ToLower(network.Name), nullableString(network.SiteID), network.Note,
		network.NetworkRange, network.BandwidthMbps, boolInt(network.Metered), network.UpdatedAt.Unix(), network.ID,
	); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "network.edit", topologyEditSummary(name, editChanges)); err != nil {
		return nil, err
	}
	return network, tx.Commit()
}

// DeleteNetwork 删除网络。当前版本还没有网络成员；后续成员级联会在同一
// 个事务里扩展。网络行与审计行同一个事务提交。
func (s *Store) DeleteNetwork(name string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	network, err := networkByNameTx(tx, name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM networks WHERE id=?`, network.ID); err != nil {
		return err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "network.rm", map[string]any{"name": network.Name}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListNetworks 按筛选条件返回网络。结果先按站点名字、再按网络名字排序，
// 让站点列表与网络列表都保持稳定输出。
func (s *Store) ListNetworks(filter NetworkFilter) ([]*models.Network, error) {
	if filter.Type != "" {
		if _, err := models.ParseNetworkType(string(filter.Type)); err != nil {
			return nil, err
		}
	}
	query := `
		SELECT net.id,net.name,net.type,COALESCE(net.site_id,''),COALESCE(site.name,''),net.note,net.cidr,
		       net.bandwidth_mbps,net.metered,net.created_at,net.updated_at
		FROM networks net LEFT JOIN sites site ON site.id=net.site_id`
	var conditions []string
	var arguments []any
	if filter.Site != "" {
		conditions = append(conditions, "site.name=?")
		arguments = append(arguments, filter.Site)
	}
	if filter.Type != "" {
		conditions = append(conditions, "net.type=?")
		arguments = append(arguments, string(filter.Type))
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY site.name,net.name,net.id"
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Network, 0)
	for rows.Next() {
		network, err := scanNetwork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, network)
	}
	return out, rows.Err()
}

// DeleteSite 删除没有物理局域网引用的站点。仍有网络时列出网络名字并拒绝。
func (s *Store) DeleteSite(name string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	site, err := siteByNameTx(tx, name)
	if err != nil {
		return err
	}
	networkNames, err := networkNamesForSiteTx(tx, site.ID)
	if err != nil {
		return err
	}
	if len(networkNames) > 0 {
		return fmt.Errorf("站点 %s 下还有物理局域网：%s；请先删除或改归属这些网络", site.Name, strings.Join(networkNames, "、"))
	}
	if _, err := tx.Exec(`DELETE FROM sites WHERE id=?`, site.ID); err != nil {
		return err
	}
	if err := writeTopologyAuditTx(tx, s.nowTime(), "site.rm", map[string]any{"name": site.Name}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSites 返回全部站点，按名字排序。
func (s *Store) ListSites() ([]*models.Site, error) {
	rows, err := s.db.Query(`SELECT id,name,note,created_at,updated_at FROM sites ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Site, 0)
	for rows.Next() {
		var site models.Site
		var created, updated int64
		if err := rows.Scan(&site.ID, &site.Name, &site.Note, &created, &updated); err != nil {
			return nil, err
		}
		site.CreatedAt, site.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, &site)
	}
	return out, rows.Err()
}

// ensureSiteNameAvailableTx 检查站点名字在小写比较下的唯一性。excludeID
// 为空时检查全表，编辑时传当前站点标识符排除自己。
func ensureSiteNameAvailableTx(tx *sql.Tx, name, excludeID string) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sites WHERE name_lower=? AND id<>?`, strings.ToLower(name), excludeID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w：%s", ErrSiteNameExists, name)
	}
	return nil
}

// siteByNameTx 按名字取站点；不存在时返回 ErrSiteNotFound。
func siteByNameTx(tx *sql.Tx, name string) (*models.Site, error) {
	var site models.Site
	var created, updated int64
	err := tx.QueryRow(`SELECT id,name,note,created_at,updated_at FROM sites WHERE name=?`, name).
		Scan(&site.ID, &site.Name, &site.Note, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：%s", ErrSiteNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	site.CreatedAt, site.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &site, nil
}

// networkSiteForTypeTx 校验网络类型与站点归属的组合，并返回站点标识符与
// 名字。物理局域网必须有站点；虚拟局域网必须没有站点。
func networkSiteForTypeTx(tx *sql.Tx, networkType models.NetworkType, siteName string) (string, string, error) {
	switch networkType {
	case models.NetworkPhysical:
		if strings.TrimSpace(siteName) == "" {
			return "", "", errors.New("物理局域网必须归属一个站点；请用 --site 指定")
		}
		site, err := siteByNameTx(tx, siteName)
		if err != nil {
			return "", "", err
		}
		return site.ID, site.Name, nil
	case models.NetworkVirtual:
		if strings.TrimSpace(siteName) != "" {
			return "", "", errors.New("虚拟局域网不属于站点；请去掉 --site")
		}
		return "", "", nil
	default:
		return "", "", fmt.Errorf("未知网络类型 %q；只接受 physical（物理局域网）或 virtual（虚拟局域网）", string(networkType))
	}
}

// ensureNetworkNameAvailableTx 检查网络名字在小写比较下的唯一性。
func ensureNetworkNameAvailableTx(tx *sql.Tx, name, excludeID string) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM networks WHERE name_lower=? AND id<>?`, strings.ToLower(name), excludeID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w：%s", ErrNetworkNameTaken, name)
	}
	return nil
}

// networkByNameTx 按名字取网络，并带出站点名字供命令行输出。
func networkByNameTx(tx *sql.Tx, name string) (*models.Network, error) {
	row := tx.QueryRow(`
		SELECT net.id,net.name,net.type,COALESCE(net.site_id,''),COALESCE(site.name,''),net.note,net.cidr,
		       net.bandwidth_mbps,net.metered,net.created_at,net.updated_at
		FROM networks net LEFT JOIN sites site ON site.id=net.site_id
		WHERE net.name=?`, name)
	network, err := scanNetwork(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：%s", ErrNetworkNotFound, name)
	}
	return network, err
}

// scanNetwork 把数据库行转换成网络对象。
func scanNetwork(sc scanner) (*models.Network, error) {
	var (
		network     models.Network
		networkType string
		metered     int
		createdAt   int64
		updatedAt   int64
	)
	if err := sc.Scan(&network.ID, &network.Name, &networkType, &network.SiteID, &network.SiteName,
		&network.Note, &network.NetworkRange, &network.BandwidthMbps, &metered, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	network.Type = models.NetworkType(networkType)
	network.Metered = metered != 0
	network.CreatedAt, network.UpdatedAt = time.Unix(createdAt, 0), time.Unix(updatedAt, 0)
	return &network, nil
}

// networkNamesForSiteTx 返回引用某个站点的网络名字，按名字排序。站点
// 删除守卫用它把挡住删除的对象列给 owner 看。
func networkNamesForSiteTx(tx *sql.Tx, siteID string) ([]string, error) {
	rows, err := tx.Query(`SELECT name FROM networks WHERE site_id=? ORDER BY name`, siteID)
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

// writeTopologyAuditTx 在拓扑写事务里写一条 owner 审计行。
func writeTopologyAuditTx(tx *sql.Tx, at time.Time, action string, summary map[string]any) error {
	return writeAuditTx(tx, AuditRow{
		TS:      at,
		Action:  action,
		Command: MarshalAuditSummary(summary),
		Status:  "ok",
	})
}

// topologyEditSummary 生成「对象名 + 实际传入字段的新值」的审计摘要。
func topologyEditSummary(objectName string, changes map[string]any) map[string]any {
	return map[string]any{"name": objectName, "changes": changes}
}

// networkAuditSummary 只放网络字段的白名单值，不写内部标识符与时间。
func networkAuditSummary(network *models.Network) map[string]any {
	return map[string]any{
		"name":           network.Name,
		"type":           string(network.Type),
		"site":           network.SiteName,
		"note":           network.Note,
		"cidr":           network.NetworkRange,
		"bandwidth-mbps": network.BandwidthMbps,
		"metered":        network.Metered,
	}
}

// boolInt 把布尔值转换成 SQLite 的整数存储形态。
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
