package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// siteQueryer 抽象数据库连接与事务都能执行的地点查询。地点引用解析需要
// 读全表建树，连接与事务共用同一份实现。
type siteQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// loadSitesTx 读取全部地点并带出上级标识符。
func loadSitesTx(q siteQueryer) ([]*models.Site, error) {
	rows, err := q.Query(`
		SELECT id,COALESCE(parent_id,''),name,note,created_at,updated_at
		FROM sites
		ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Site, 0)
	for rows.Next() {
		var site models.Site
		var created, updated int64
		if err := rows.Scan(&site.ID, &site.ParentID, &site.Name, &site.Note, &created, &updated); err != nil {
			return nil, err
		}
		site.CreatedAt, site.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, &site)
	}
	return out, rows.Err()
}

// resolveSiteRef 把命令行或接口里的地点引用解析成唯一地点。引用可以是
// 完整路径「公司 / 一厂区 / 3 号楼」，也可以是全库唯一的裸名字；裸名字
// 对应多个地点时列出候选完整路径并拒绝，绝不猜测。
func resolveSiteRef(q siteQueryer, ref string) (*models.Site, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("%w：地点引用不能为空", ErrSiteNotFound)
	}
	segments, err := splitSitePath(ref)
	if err != nil {
		return nil, err
	}
	sites, err := loadSitesTx(q)
	if err != nil {
		return nil, err
	}
	tree := models.NewSiteTree(sites)
	if len(segments) == 1 {
		var matches []*models.Site
		for _, site := range sites {
			if strings.EqualFold(site.Name, segments[0]) {
				matches = append(matches, site)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%w：%s", ErrSiteNotFound, ref)
		}
		if len(matches) > 1 {
			return nil, siteRefAmbiguousError(segments[0], matches, tree)
		}
		return matches[0], nil
	}
	target := strings.ToLower(strings.Join(segments, "/"))
	var matches []*models.Site
	for _, site := range sites {
		path := tree.Path(site.ID)
		if len(path) == 0 {
			continue
		}
		if strings.ToLower(strings.Join(path, "/")) == target {
			matches = append(matches, site)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w：%s", ErrSiteNotFound, ref)
	}
	if len(matches) > 1 {
		return nil, siteRefAmbiguousError(ref, matches, tree)
	}
	return matches[0], nil
}

func siteRefAmbiguousError(ref string, matches []*models.Site, tree *models.SiteTree) error {
	paths := make([]string, 0, len(matches))
	for _, site := range matches {
		paths = append(paths, tree.PathString(site.ID))
	}
	return &TopologyValidationError{Message: fmt.Sprintf(
		"地点 %s 对应多个地点，请用完整路径指称：%s", ref, strings.Join(paths, "、"),
	)}
}

// splitSitePath 按斜杠拆分地点路径并去掉每级首尾空白；任何一级为空都拒
// 绝，避免「公司 // 3 号楼」被静默解释成别的地点。
func splitSitePath(ref string) ([]string, error) {
	rawSegments := strings.Split(ref, "/")
	segments := make([]string, 0, len(rawSegments))
	for _, raw := range rawSegments {
		segment := strings.TrimSpace(raw)
		if segment == "" {
			return nil, &TopologyValidationError{Message: fmt.Sprintf(
				"地点路径 %s 包含空的层级；请写成「公司 / 一厂区 / 3 号楼」", ref,
			)}
		}
		segments = append(segments, segment)
	}
	return segments, nil
}

// sitePathTx 返回一个地点的完整路径，供审计摘要与提示使用。
func sitePathTx(q siteQueryer, siteID string) (string, error) {
	if siteID == "" {
		return "", nil
	}
	sites, err := loadSitesTx(q)
	if err != nil {
		return "", err
	}
	return models.NewSiteTree(sites).PathString(siteID), nil
}

// ensureSiteParentAllowedTx 校验新的上级不会形成地点循环：上级不能是地
// 点自己，也不能是它任意一级下级。
func ensureSiteParentAllowedTx(q siteQueryer, siteID, parentID string) error {
	// 把 A 的上级改成 B 时，只有 B 在 A 的下级树里（A 是 B 的上级）才会
	// 成环；B 只是 A 的现有上级不构成问题。
	if parentID == "" || (parentID != siteID && !siteIsAncestorOf(q, siteID, parentID)) {
		return nil
	}
	path, err := sitePathTx(q, siteID)
	if err != nil {
		return err
	}
	return &TopologyValidationError{Message: fmt.Sprintf(
		"地点 %s 的上级不能是它自己或它的下级；这会形成地点循环", path,
	)}
}

// siteIsAncestorOf 判断 candidateID 是否在 siteID 的上级链上。它用已访
// 问集合防御损坏数据里的环。
func siteIsAncestorOf(q siteQueryer, candidateID, siteID string) bool {
	sites, err := loadSitesTx(q)
	if err != nil {
		return false
	}
	byID := make(map[string]*models.Site, len(sites))
	for _, site := range sites {
		byID[site.ID] = site
	}
	current := siteID
	seen := make(map[string]bool)
	for current != "" && !seen[current] {
		if current == candidateID {
			return true
		}
		seen[current] = true
		site := byID[current]
		if site == nil {
			return false
		}
		current = site.ParentID
	}
	return false
}

// namesReferencingSiteTx 返回引用某个地点的对象名字，按名字排序；地点删
// 除守卫用它把阻挡删除的对象列给 owner 看。
func namesReferencingSiteTx(q siteQueryer, query string, siteID string) ([]string, error) {
	rows, err := q.Query(query, siteID)
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
