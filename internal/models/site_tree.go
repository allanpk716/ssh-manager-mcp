package models

import "strings"

// SitePathSeparator 是地点完整路径各级之间的分隔符，例如
// 「公司 / 一厂区 / 3 号楼」。命令行与查询输出的完整路径都使用它。
const SitePathSeparator = " / "

// SiteTree 把一组地点按上级关系组织成树，并提供完整路径与覆盖判断。
// 读写两侧共用它，保证「公司 / 一厂区 / 3 号楼」在命令行、Web UI 数据
// 源与 MCP 查询里拼接一致。数据损坏出现环时按已见过的地点截断，不让
// 查询无限循环。
type SiteTree struct {
	byID     map[string]*Site
	children map[string][]*Site
}

// NewSiteTree 按地点切片建树；切片本身不会被修改。
func NewSiteTree(sites []*Site) *SiteTree {
	tree := &SiteTree{
		byID:     make(map[string]*Site, len(sites)),
		children: make(map[string][]*Site, len(sites)),
	}
	for _, site := range sites {
		if site == nil || site.ID == "" {
			continue
		}
		tree.byID[site.ID] = site
	}
	for _, site := range sites {
		if site == nil || site.ID == "" {
			continue
		}
		if site.ParentID == site.ID {
			continue
		}
		tree.children[site.ParentID] = append(tree.children[site.ParentID], site)
	}
	return tree
}

// Path 返回从根到该地点的名字切片。地点不存在时返回空切片；上级链断
// 裂时从最高可达一级开始返回。
func (t *SiteTree) Path(siteID string) []string {
	if t == nil || siteID == "" {
		return nil
	}
	var names []string
	seen := make(map[string]bool)
	current := siteID
	for current != "" && !seen[current] {
		site, ok := t.byID[current]
		if !ok {
			break
		}
		names = append(names, site.Name)
		seen[current] = true
		current = site.ParentID
	}
	for left, right := 0, len(names)-1; left < right; left, right = left+1, right-1 {
		names[left], names[right] = names[right], names[left]
	}
	return names
}

// PathString 返回以「 / 」连接的完整地点路径；地点不存在或没有登记时
// 返回空字符串。
func (t *SiteTree) PathString(siteID string) string {
	return strings.Join(t.Path(siteID), SitePathSeparator)
}

// Covers 判断 ancestorID 覆盖 descendantID：两者相同，或 descendantID
// 在 ancestorID 的下级树里。任一地点不存在时返回两者是否相等。
func (t *SiteTree) Covers(ancestorID, descendantID string) bool {
	if ancestorID == "" || descendantID == "" {
		return false
	}
	seen := make(map[string]bool)
	current := descendantID
	for current != "" && !seen[current] {
		if current == ancestorID {
			return true
		}
		seen[current] = true
		site, ok := t.byID[current]
		if !ok {
			return false
		}
		current = site.ParentID
	}
	return false
}

// SubtreeIDs 返回 locationID 及其全部下级地点的标识符，供按地点树筛选
// 使用。地点不存在时返回空切片。
func (t *SiteTree) SubtreeIDs(locationID string) []string {
	if t == nil {
		return nil
	}
	if _, ok := t.byID[locationID]; !ok {
		return nil
	}
	ids := []string{locationID}
	queue := []string{locationID}
	seen := map[string]bool{locationID: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range t.children[current] {
			if child == nil || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			ids = append(ids, child.ID)
			queue = append(queue, child.ID)
		}
	}
	return ids
}
