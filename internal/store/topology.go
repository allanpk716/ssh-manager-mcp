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
	ErrSiteNotFound     = errors.New("地点不存在")
	ErrSiteNameExists   = errors.New("地点名字已存在")
	ErrNetworkNotFound  = errors.New("网络不存在")
	ErrNetworkNameTaken = errors.New("网络名字已存在")
)

// TopologyValidationError 表示调用方传入的拓扑内容不合法；错误文字可直接
// 展示给 owner。
type TopologyValidationError struct {
	Message string
}

func (e *TopologyValidationError) Error() string { return e.Message }

// SiteChanges 表示地点的一次部分更新。指针为空代表这个字段保持不变；
// 备注与上级传空字符串代表清空；上级用完整路径或全库唯一的裸名字指称。
type SiteChanges struct {
	Name   *string
	Note   *string
	Parent *string
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

// validateTopologyName 只拒绝去掉首尾空白后为空的名字，不限制 owner 使用
// 的字符；调用方负责先去掉首尾空白，名字唯一性另按小写形式检查。
func validateTopologyName(objectType, name string) error {
	if strings.TrimSpace(name) == "" {
		return &TopologyValidationError{Message: fmt.Sprintf("%s名字不能为空", objectType)}
	}
	return nil
}

// AddSite 登记一个根地点。地点行与审计行在同一个事务里提交。
func (s *Store) AddSite(name, note string) (*models.Site, error) {
	return s.AddSiteWithParent(name, note, "")
}

// AddSiteWithParent 登记一个地点并指定上级；parent 为空表示根地点。同
// 一上级下名字唯一，不同上级下允许同名。
func (s *Store) AddSiteWithParent(name, note, parent string) (*models.Site, error) {
	return s.AddSiteWithParentAndSource(name, note, parent, WriteSource{})
}

// AddSiteWithParentAndSource 与 AddSiteWithParent 相同，并把来源写入审
// 计行；Web UI 的地点管理页用它区分命令行与页面写入。
func (s *Store) AddSiteWithParentAndSource(name, note, parent string, source WriteSource) (*models.Site, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	name = strings.TrimSpace(name)
	parent = strings.TrimSpace(parent)
	if err := validateTopologyName("地点", name); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	parentID := ""
	if parent != "" {
		parentSite, resolveErr := resolveSiteRef(tx, parent)
		if resolveErr != nil {
			return nil, resolveErr
		}
		parentID = parentSite.ID
	}
	if err := ensureSiteNameAvailableTx(tx, name, parentID, ""); err != nil {
		return nil, err
	}
	site := &models.Site{
		ID:        newID(),
		ParentID:  parentID,
		Name:      name,
		Note:      strings.TrimSpace(note),
		CreatedAt: time.Unix(now(), 0),
		UpdatedAt: time.Unix(now(), 0),
	}
	if _, err := tx.Exec(
		`INSERT INTO sites(id,name,name_lower,parent_id,note,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		site.ID, site.Name, strings.ToLower(site.Name), nullableString(site.ParentID), site.Note,
		site.CreatedAt.Unix(), site.UpdatedAt.Unix(),
	); err != nil {
		return nil, err
	}
	parentPath := ""
	if site.ParentID != "" {
		var pathErr error
		if parentPath, pathErr = sitePathTx(tx, site.ParentID); pathErr != nil {
			return nil, pathErr
		}
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "site.add", map[string]any{
		"name":   site.Name,
		"parent": parentPath,
		"note":   site.Note,
	}, source); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return site, nil
}

// EditSite 按地点引用做部分更新。返回更新后的地点；数据行与审计行在同
// 一个事务里提交。
func (s *Store) EditSite(ref string, changes SiteChanges) (*models.Site, error) {
	return s.EditSiteWithSource(ref, changes, WriteSource{})
}

// EditSiteWithSource 与 EditSite 相同，并把来源写入审计行。
func (s *Store) EditSiteWithSource(ref string, changes SiteChanges, source WriteSource) (*models.Site, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	ref = strings.TrimSpace(ref)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	site, err := resolveSiteRef(tx, ref)
	if err != nil {
		return nil, err
	}
	originalName := site.Name
	parentChanged := false
	if changes.Parent != nil {
		newParent := strings.TrimSpace(*changes.Parent)
		if newParent == "" {
			site.ParentID = ""
			parentChanged = true
		} else {
			parentSite, resolveErr := resolveSiteRef(tx, newParent)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if err := ensureSiteParentAllowedTx(tx, site.ID, parentSite.ID); err != nil {
				return nil, err
			}
			site.ParentID = parentSite.ID
			parentChanged = true
		}
	}
	if changes.Name != nil {
		newName := strings.TrimSpace(*changes.Name)
		if err := validateTopologyName("地点", newName); err != nil {
			return nil, err
		}
		if err := ensureSiteNameAvailableTx(tx, newName, site.ParentID, site.ID); err != nil {
			return nil, err
		}
		site.Name = newName
	}
	if changes.Note != nil {
		site.Note = strings.TrimSpace(*changes.Note)
	}
	site.UpdatedAt = time.Unix(now(), 0)
	if _, err := tx.Exec(
		`UPDATE sites SET name=?,name_lower=?,parent_id=?,note=?,updated_at=? WHERE id=?`,
		site.Name, strings.ToLower(site.Name), nullableString(site.ParentID), site.Note,
		site.UpdatedAt.Unix(), site.ID,
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
	if parentChanged {
		parentPath, pathErr := sitePathTx(tx, site.ParentID)
		if pathErr != nil {
			return nil, pathErr
		}
		editChanges["parent"] = parentPath
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "site.edit", topologyEditSummary(originalName, editChanges), source); err != nil {
		return nil, err
	}
	return site, tx.Commit()
}

// AddNetwork 登记一个网络。类型与覆盖地点在同一个事务里校验，网络行与
// 审计行一起提交。
func (s *Store) AddNetwork(input models.Network) (*models.Network, error) {
	return s.AddNetworkWithSource(input, WriteSource{})
}

// AddNetworkWithSource 与 AddNetwork 相同，并把同一事务里的审计行标为指
// 定来源。
func (s *Store) AddNetworkWithSource(input models.Network, source WriteSource) (*models.Network, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	input.Name = strings.TrimSpace(input.Name)
	input.SiteName = strings.TrimSpace(input.SiteName)
	if _, err := models.ParseNetworkType(string(input.Type)); err != nil {
		return nil, err
	}
	if err := validateTopologyName("网络", input.Name); err != nil {
		return nil, err
	}
	if input.BandwidthMbps < 0 {
		return nil, &TopologyValidationError{Message: "标称带宽不能小于 0 Mbit/s"}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	site, err := networkSiteForTypeTx(tx, input.Type, input.SiteName)
	if err != nil {
		return nil, err
	}
	siteID, siteName := "", ""
	if site != nil {
		siteID, siteName = site.ID, site.Name
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
	auditSummary, err := networkAuditSummaryTx(tx, network)
	if err != nil {
		return nil, err
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "network.add", auditSummary, source); err != nil {
		return nil, err
	}
	return network, tx.Commit()
}

// EditNetwork 按网络名字做部分更新；类型不可修改。网络行与审计行在同一
// 个事务里提交。
func (s *Store) EditNetwork(name string, changes NetworkChanges) (*models.Network, error) {
	return s.EditNetworkWithSource(name, changes, WriteSource{})
}

// EditNetworkWithSource 与 EditNetwork 相同，并把同一事务里的审计行标为
// 指定来源。
func (s *Store) EditNetworkWithSource(name string, changes NetworkChanges, source WriteSource) (*models.Network, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	name = strings.TrimSpace(name)
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
		newName := strings.TrimSpace(*changes.Name)
		if err := validateTopologyName("网络", newName); err != nil {
			return nil, err
		}
		if err := ensureNetworkNameAvailableTx(tx, newName, network.ID); err != nil {
			return nil, err
		}
		network.Name = newName
		editChanges["name"] = network.Name
	}
	if changes.Site != nil {
		site, siteErr := networkSiteForTypeTx(tx, network.Type, strings.TrimSpace(*changes.Site))
		if siteErr != nil {
			return nil, siteErr
		}
		siteID, siteName := site.ID, site.Name
		network.SiteID, network.SiteName = siteID, siteName
		sitePath, pathErr := sitePathTx(tx, network.SiteID)
		if pathErr != nil {
			return nil, pathErr
		}
		editChanges["site"] = sitePath
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
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "network.edit", topologyEditSummary(name, editChanges), source); err != nil {
		return nil, err
	}
	return network, tx.Commit()
}

// DeleteNetwork 删除网络并级联删除它的网络成员。返回因此不再有任何网络
// 成员的服务器条目名字，供命令行逐条提示候选地址只剩兜底 Host。
func (s *Store) DeleteNetwork(name string) ([]string, error) {
	return s.DeleteNetworkWithSource(name, WriteSource{})
}

// DeleteNetworkWithSource 与 DeleteNetwork 相同，并把同一事务里的审计行
// 标为指定来源。
func (s *Store) DeleteNetworkWithSource(name string, source WriteSource) ([]string, error) {
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
	serverNames, err := serversWithoutOtherMembershipsTx(tx, network.ID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM network_memberships WHERE network_id=?`, network.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`DELETE FROM container_layout_coordinates WHERE entity_type=? AND entity_id=?`,
		LayoutEntityNetwork, network.ID,
	); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM networks WHERE id=?`, network.ID); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "network.rm", map[string]any{"name": network.Name}, source); err != nil {
		return nil, err
	}
	return serverNames, tx.Commit()
}

// serversWithoutOtherMembershipsTx 返回所属设备只在指定网络里有网络成员
// 的服务器条目名字；删除这个网络后它们只剩兜底 Host。
func serversWithoutOtherMembershipsTx(tx *sql.Tx, networkID string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT server.name
		FROM servers server
		WHERE server.device_id IN (
		        SELECT member.device_id FROM network_memberships member WHERE member.network_id=?)
		  AND NOT EXISTS (
		        SELECT 1 FROM network_memberships other
		        WHERE other.device_id=server.device_id AND other.network_id<>?)
		ORDER BY server.name,server.id`, networkID, networkID)
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

// ListNetworks 按筛选条件返回网络。结果先按地点名字、再按网络名字排序，
// 让地点列表与网络列表都保持稳定输出。地点筛选按「覆盖的地点及其下
// 级」匹配，与地点树的浏览习惯一致。
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
	var coveringSiteIDs map[string]bool
	if filter.Site != "" {
		site, err := resolveSiteRef(s.db, filter.Site)
		if err != nil {
			return nil, err
		}
		sites, err := loadSitesTx(s.db)
		if err != nil {
			return nil, err
		}
		coveringSiteIDs = make(map[string]bool)
		for _, id := range models.NewSiteTree(sites).SubtreeIDs(site.ID) {
			coveringSiteIDs[id] = true
		}
		conditions = append(conditions, "net.site_id IS NOT NULL")
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
		if coveringSiteIDs != nil && !coveringSiteIDs[network.SiteID] {
			continue
		}
		out = append(out, network)
	}
	return out, rows.Err()
}

// DeleteSite 删除一个地点。仍有下级地点、机器或物理局域网指向它时列出具
// 体对象并拒绝。
func (s *Store) DeleteSite(ref string) error {
	return s.DeleteSiteWithSource(ref, WriteSource{})
}

// DeleteSiteWithSource 与 DeleteSite 相同，并把来源写入审计行。
func (s *Store) DeleteSiteWithSource(ref string, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	ref = strings.TrimSpace(ref)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	site, err := resolveSiteRef(tx, ref)
	if err != nil {
		return err
	}
	var blockers []string
	childNames, err := namesReferencingSiteTx(tx,
		`SELECT name FROM sites WHERE parent_id=? ORDER BY name,id`, site.ID)
	if err != nil {
		return err
	}
	if len(childNames) > 0 {
		blockers = append(blockers, fmt.Sprintf(
			"还有下级地点：%s；请先删除或改上级", strings.Join(childNames, "、")))
	}
	deviceNames, err := namesReferencingSiteTx(tx,
		`SELECT name FROM devices WHERE site_id=? ORDER BY name,id`, site.ID)
	if err != nil {
		return err
	}
	if len(deviceNames) > 0 {
		blockers = append(blockers, fmt.Sprintf(
			"还有机器登记在这个地点：%s；请先改机器地点", strings.Join(deviceNames, "、")))
	}
	networkNames, err := networkNamesForSiteTx(tx, site.ID)
	if err != nil {
		return err
	}
	if len(networkNames) > 0 {
		blockers = append(blockers, fmt.Sprintf(
			"还有物理局域网覆盖这个地点：%s；请先删除或改覆盖地点", strings.Join(networkNames, "、")))
	}
	if len(blockers) > 0 {
		return &TopologyValidationError{Message: fmt.Sprintf(
			"地点 %s 不能删除：%s", site.Name, strings.Join(blockers, "；"))}
	}
	sitePath, err := sitePathTx(tx, site.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM container_layout_coordinates WHERE entity_type=? AND entity_id=?`,
		LayoutEntitySite, site.ID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sites WHERE id=?`, site.ID); err != nil {
		return err
	}
	if err := writeTopologyAuditSourceTx(tx, s.nowTime(), "site.rm", map[string]any{"name": sitePath}, source); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSites 返回全部地点（带上级标识符），按名字排序。
func (s *Store) ListSites() ([]*models.Site, error) {
	return loadSitesTx(s.db)
}

// ensureSiteNameAvailableTx 检查地点名字在同一上级下（忽略大小写）唯一。
// excludeID 为空时检查新增，编辑时传当前地点标识符排除自己。
func ensureSiteNameAvailableTx(tx *sql.Tx, name, parentID, excludeID string) error {
	var count int
	name = strings.TrimSpace(name)
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sites WHERE COALESCE(parent_id,'')=? AND name_lower=? AND id<>?`,
		parentID, strings.ToLower(name), excludeID,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w：%s", ErrSiteNameExists, name)
	}
	return nil
}

// networkSiteForTypeTx 校验网络类型与覆盖地点的组合并返回地点。物理局
// 域网必须登记覆盖一个地点（可以是任意一级）；虚拟局域网不覆盖地点。
func networkSiteForTypeTx(tx *sql.Tx, networkType models.NetworkType, siteRef string) (*models.Site, error) {
	switch networkType {
	case models.NetworkPhysical:
		if strings.TrimSpace(siteRef) == "" {
			return nil, &TopologyValidationError{Message: "物理局域网必须登记覆盖的地点；请用 --site 指定"}
		}
		site, err := resolveSiteRef(tx, siteRef)
		if err != nil {
			return nil, err
		}
		return site, nil
	case models.NetworkVirtual:
		if strings.TrimSpace(siteRef) != "" {
			return nil, &TopologyValidationError{Message: "虚拟局域网不覆盖地点；请去掉 --site"}
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("未知网络类型 %q；只接受 physical（物理局域网）或 virtual（虚拟局域网）", string(networkType))
	}
}

// ensureNetworkNameAvailableTx 检查网络名字在小写比较下的唯一性。
func ensureNetworkNameAvailableTx(tx *sql.Tx, name, excludeID string) error {
	var count int
	name = strings.TrimSpace(name)
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
	name = strings.TrimSpace(name)
	row := tx.QueryRow(`
		SELECT net.id,net.name,net.type,COALESCE(net.site_id,''),COALESCE(site.name,''),net.note,net.cidr,
		       net.bandwidth_mbps,net.metered,net.created_at,net.updated_at
		FROM networks net LEFT JOIN sites site ON site.id=net.site_id
		WHERE net.name_lower=?`, strings.ToLower(name))
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
	return writeTopologyAuditSourceTx(tx, at, action, summary, WriteSource{})
}

func writeTopologyAuditSourceTx(
	tx *sql.Tx, at time.Time, action string, summary map[string]any, source WriteSource,
) error {
	row := OwnerAuditRowAt(at, action, summary)
	row.Source, row.SourceIP = source.kind, source.address
	return writeAuditTx(tx, row)
}

// topologyEditSummary 生成「对象名 + 实际传入字段的新值」的审计摘要。
func topologyEditSummary(objectName string, changes map[string]any) map[string]any {
	return map[string]any{"name": objectName, "changes": changes}
}

// networkAuditSummaryTx 只放网络字段的白名单值，不写内部标识符与时间。
// 覆盖地点写完整路径，多级树下同名叶级地点也能从审计里区分。
func networkAuditSummaryTx(tx *sql.Tx, network *models.Network) (map[string]any, error) {
	sitePath, err := sitePathTx(tx, network.SiteID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name":           network.Name,
		"type":           string(network.Type),
		"site":           sitePath,
		"note":           network.Note,
		"cidr":           network.NetworkRange,
		"bandwidth-mbps": network.BandwidthMbps,
		"metered":        network.Metered,
	}, nil
}

// boolInt 把布尔值转换成 SQLite 的整数存储形态。
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
