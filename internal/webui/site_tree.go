package webui

import "sort"

// eachSiteTreeNode 按名称顺序深度优先遍历地点树。已访问集合用于防御损
// 坏数据里的地点循环，避免页面渲染陷入递归不返回。
func eachSiteTreeNode(sites []TopologySite, visit func(site TopologySite, depth int)) {
	children := map[string][]TopologySite{}
	for _, site := range sites {
		children[site.ParentID] = append(children[site.ParentID], site)
	}
	for _, list := range children {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	seen := map[string]bool{}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, site := range children[parent] {
			if site.ID == "" || seen[site.ID] || site.ParentID == site.ID {
				continue
			}
			seen[site.ID] = true
			visit(site, depth)
			walk(site.ID, depth+1)
		}
	}
	walk("", 0)
}
