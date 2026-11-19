package webui

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	networkTypePhysical = "physical"
	networkTypeVirtual  = "virtual"

	locationCreatePath = "/ui/more/locations/new"
	locationRenamePath = "/ui/more/locations/rename"
	locationMovePath   = "/ui/more/locations/move"
	locationDeletePath = "/ui/more/locations/delete"
	networkCreatePath  = "/ui/more/networks/new"
	networkEditPath    = "/ui/more/networks/edit"
)

// locationNodeView 是地点树里的一个节点：机器与物理局域网只统计直接挂
// 在本节点的对象，下级机器数汇总全部后代，与存储层删除守卫的口径一致。
type locationNodeView struct {
	Site                   TopologySite
	Depth                  int
	MachineNames           []string
	PhysicalNetworkNames   []string
	DescendantMachineCount int
	Children               []*locationNodeView
	DeleteReason           string
	CanWrite               bool
}

// networkRowView 是网络列表里的一行展示数据。
type networkRowView struct {
	Network      TopologyNetwork
	MachineCount int
	CanWrite     bool
}

// locationOptionView 是表单里的一个地点选项；Move 场景会禁用自身与下级。
type locationOptionView struct {
	Site     TopologySite
	Depth    int
	Disabled bool
}

// locationsNetworksView 是地点与网络管理页的展示数据。
type locationsNetworksView struct {
	Tab              string
	CanWrite         bool
	Roots            []*locationNodeView
	PhysicalNetworks []networkRowView
	VirtualNetworks  []networkRowView
	SiteCount        int
	NetworkCount     int
	Notice           string
}

// locationFormView 是新建、改名、移动与删除地点的表单数据。
type locationFormView struct {
	Mode         string
	Site         *TopologySite
	Name         string
	Parent       string
	Options      []locationOptionView
	DeleteReason string
	Error        string
}

// networkFormView 是新建与编辑网络的表单数据。
type networkFormView struct {
	Mode          string
	Name          string
	Type          string
	Site          string
	NetworkRange  string
	BandwidthMbps string
	Metered       bool
	Options       []locationOptionView
	Error         string
}

// registerLocationsNetworksRoutes 注册地点与网络页。页面读取只要拓扑来
// 源；四个地点动作与两个网络动作只有提供拓扑写来源的角色才注册。
func (h *handler) registerLocationsNetworksRoutes() {
	if h.cfg.Topology == nil {
		return
	}
	h.mux.HandleFunc(locationsNetworksPath, h.requireSession(h.handleLocationsNetworksPage))
	if h.cfg.TopologyWriter == nil {
		return
	}
	h.mux.HandleFunc(locationCreatePath, h.requireSession(h.handleLocationCreate))
	h.mux.HandleFunc(locationRenamePath, h.requireSession(h.handleLocationRename))
	h.mux.HandleFunc(locationMovePath, h.requireSession(h.handleLocationMove))
	h.mux.HandleFunc(locationDeletePath, h.requireSession(h.handleLocationDelete))
	h.mux.HandleFunc(networkCreatePath, h.requireSession(h.handleNetworkCreate))
	h.mux.HandleFunc(networkEditPath, h.requireSession(h.handleNetworkEdit))
}

// handleLocationsNetworksPage 渲染地点树与网络两组列表。
func (h *handler) handleLocationsNetworksPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != locationsNetworksPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "地点与网络页只接受 GET 请求。")
		return
	}
	tab := "locations"
	if r.URL.Query().Get("tab") == "networks" {
		tab = "networks"
	}
	view, err := h.buildLocationsNetworksView(r, tab)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取地点与网络失败", "读取地点与网络失败，请稍后重试。")
		return
	}
	view.Notice = locationsNetworksNotice(r.URL.Query().Get("notice"))
	h.renderPage(w, r, http.StatusOK, "locations-networks.tmpl", pageData{
		Title: "地点与网络", LocationsNetworks: view,
	})
}

func locationsNetworksNotice(kind string) string {
	switch kind {
	case "location-created":
		return "已新建地点。"
	case "location-saved":
		return "已保存地点。"
	case "location-deleted":
		return "已删除地点。"
	case "network-saved":
		return "已保存网络。"
	default:
		return ""
	}
}

// buildLocationsNetworksView 把拓扑数据整理成地点树与两个网络分组。
func (h *handler) buildLocationsNetworksView(r *http.Request, tab string) (*locationsNetworksView, error) {
	data, err := h.cfg.Topology.Topology(h.requestClientInstance(r), "")
	if err != nil {
		return nil, err
	}
	view := &locationsNetworksView{
		Tab: tab, CanWrite: h.cfg.TopologyWriter != nil,
		SiteCount: len(data.Sites), NetworkCount: len(data.Networks),
	}
	children := map[string][]TopologySite{}
	for _, site := range data.Sites {
		children[site.ParentID] = append(children[site.ParentID], site)
	}
	for _, list := range children {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	machinesBySite := map[string][]string{}
	for _, device := range data.Devices {
		if device.SiteID != "" {
			machinesBySite[device.SiteID] = append(machinesBySite[device.SiteID], device.Name)
		}
	}
	physicalBySite := map[string][]string{}
	for _, network := range data.Networks {
		if network.Type == networkTypePhysical && network.SiteID != "" {
			physicalBySite[network.SiteID] = append(physicalBySite[network.SiteID], network.Name)
		}
	}
	var build func(site TopologySite, depth int) *locationNodeView
	build = func(site TopologySite, depth int) *locationNodeView {
		node := &locationNodeView{
			Site: site, Depth: depth, CanWrite: view.CanWrite,
			MachineNames: machinesBySite[site.ID], PhysicalNetworkNames: physicalBySite[site.ID],
		}
		for _, child := range children[site.ID] {
			childNode := build(child, depth+1)
			node.DescendantMachineCount += childNode.MachineCount() + childNode.DescendantMachineCount
			node.Children = append(node.Children, childNode)
		}
		node.DeleteReason = locationDeleteReason(node, children[site.ID])
		return node
	}
	for _, site := range children[""] {
		view.Roots = append(view.Roots, build(site, 0))
	}
	memberCount := map[string]int{}
	for _, membership := range data.Memberships {
		memberCount[membership.NetworkID]++
	}
	for _, network := range data.Networks {
		row := networkRowView{Network: network, MachineCount: memberCount[network.ID], CanWrite: view.CanWrite}
		if network.Type == networkTypeVirtual {
			view.VirtualNetworks = append(view.VirtualNetworks, row)
		} else {
			view.PhysicalNetworks = append(view.PhysicalNetworks, row)
		}
	}
	return view, nil
}

// MachineCount 返回直接登记在本地点的机器数量。
func (n *locationNodeView) MachineCount() int { return len(n.MachineNames) }

// DepthClass 返回按层级缩进的样式类；超过 12 级时使用最后一档。
func (n *locationNodeView) DepthClass() string { return depthClass(n.Depth) }

// DepthClass 返回表单地点选项的缩进样式类。
func (o locationOptionView) DepthClass() string { return depthClass(o.Depth) }

func depthClass(depth int) string {
	if depth > 12 {
		depth = 12
	}
	return fmt.Sprintf("depth-%d", depth)
}

// locationDeleteReason 与存储层删除守卫同一顺序：先下级地点，再直接机
// 器，最后物理局域网；返回空串表示可以删除。
func locationDeleteReason(node *locationNodeView, children []TopologySite) string {
	var blockers []string
	if len(children) > 0 {
		names := make([]string, 0, len(children))
		for _, child := range children {
			names = append(names, child.Name)
		}
		blockers = append(blockers, fmt.Sprintf("还有下级地点：%s；请先删除或改上级", strings.Join(names, "、")))
	}
	if len(node.MachineNames) > 0 {
		blockers = append(blockers, fmt.Sprintf(
			"还有机器登记在这个地点：%s；请先改机器地点", strings.Join(node.MachineNames, "、")))
	}
	if len(node.PhysicalNetworkNames) > 0 {
		blockers = append(blockers, fmt.Sprintf(
			"还有物理局域网覆盖这个地点：%s；请先删除或改覆盖地点", strings.Join(node.PhysicalNetworkNames, "、")))
	}
	if len(blockers) == 0 {
		return ""
	}
	return fmt.Sprintf("地点 %s 不能删除：%s", node.Site.Name, strings.Join(blockers, "；"))
}

// topologyData 读取一次完整拓扑数据；地点与网络页的读与写共用。
func (h *handler) topologyData(r *http.Request) (*TopologyData, error) {
	return h.cfg.Topology.Topology(h.requestClientInstance(r), "")
}

// locationTreeOptions 返回表单里的树形地点选项；includeRoot 控制是否提供
// “无上级”，disabledPaths 是不可选的完整路径集合。
func locationTreeOptions(data *TopologyData, includeRoot bool, disabledPaths map[string]bool) []locationOptionView {
	options := make([]locationOptionView, 0, len(data.Sites)+1)
	if includeRoot {
		options = append(options, locationOptionView{
			Site: TopologySite{ID: "", Name: "无上级（顶级地点）", LocationPath: ""},
		})
	}
	children := map[string][]TopologySite{}
	for _, site := range data.Sites {
		children[site.ParentID] = append(children[site.ParentID], site)
	}
	for _, list := range children {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	var add func(sites []TopologySite, depth int)
	add = func(sites []TopologySite, depth int) {
		for _, site := range sites {
			options = append(options, locationOptionView{
				Site: site, Depth: depth, Disabled: disabledPaths[site.LocationPath],
			})
			add(children[site.ID], depth+1)
		}
	}
	add(children[""], 0)
	return options
}

// findSiteByPath 按完整路径查找地点；路径是页面与存储层共同使用的地点
// 引用，多级树下同名节点不会混淆。
func findSiteByPath(data *TopologyData, path string) *TopologySite {
	for _, site := range data.Sites {
		if site.LocationPath == path {
			found := site
			return &found
		}
	}
	return nil
}

// renderLocationForm 渲染一个地点表单；错误沿用表单状态码参数。
func (h *handler) renderLocationForm(w http.ResponseWriter, r *http.Request, status int, view *locationFormView, title string) {
	h.renderPage(w, r, status, "location-form.tmpl", pageData{
		Title: title, LocationForm: view,
	})
}

// handleLocationCreate 渲染与处理新建地点表单。
func (h *handler) handleLocationCreate(w http.ResponseWriter, r *http.Request) {
	data, err := h.topologyData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取地点失败", "读取地点失败，请稍后重试。")
		return
	}
	view := &locationFormView{Mode: "create", Options: locationTreeOptions(data, true, nil)}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		view.Parent = strings.TrimSpace(r.URL.Query().Get("parent"))
		h.renderLocationForm(w, r, http.StatusOK, view, "新建地点")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新建地点只接受 GET 与 POST 请求。")
		return
	}
	view.Name = strings.TrimSpace(r.FormValue("name"))
	view.Parent = strings.TrimSpace(r.FormValue("parent"))
	if view.Name == "" {
		view.Error = "请填写地点名称。"
		h.renderLocationForm(w, r, http.StatusOK, view, "新建地点")
		return
	}
	message, err := h.cfg.TopologyWriter.AddLocation(
		view.Name, view.Parent, "", TopologyWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		view.Error = topologyWriteErrorText(err)
		h.renderLocationForm(w, r, http.StatusOK, view, "新建地点")
		return
	}
	h.redirectLocationsNetworksWithNotice(w, r, "location-created", message)
}

// handleLocationRename 渲染与处理地点改名表单。
func (h *handler) handleLocationRename(w http.ResponseWriter, r *http.Request) {
	data, err := h.topologyData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取地点失败", "读取地点失败，请稍后重试。")
		return
	}
	site := findSiteByPath(data, strings.TrimSpace(formOrQuery(r, "site")))
	if site == nil {
		h.renderError(w, r, http.StatusNotFound, "地点不存在", "请求的地点不存在。")
		return
	}
	view := &locationFormView{Mode: "rename", Site: site, Name: site.Name}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		h.renderLocationForm(w, r, http.StatusOK, view, "地点改名")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "地点改名只接受 GET 与 POST 请求。")
		return
	}
	view.Name = strings.TrimSpace(r.FormValue("name"))
	if view.Name == "" {
		view.Error = "请填写地点名称。"
		h.renderLocationForm(w, r, http.StatusOK, view, "地点改名")
		return
	}
	name := view.Name
	message, err := h.cfg.TopologyWriter.EditLocation(site.LocationPath,
		TopologyLocationChanges{Name: &name}, TopologyWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		view.Error = topologyWriteErrorText(err)
		h.renderLocationForm(w, r, http.StatusOK, view, "地点改名")
		return
	}
	h.redirectLocationsNetworksWithNotice(w, r, "location-saved", message)
}

// handleLocationMove 渲染与处理移动地点表单；自身与下级不可成为新上级。
func (h *handler) handleLocationMove(w http.ResponseWriter, r *http.Request) {
	data, err := h.topologyData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取地点失败", "读取地点失败，请稍后重试。")
		return
	}
	site := findSiteByPath(data, strings.TrimSpace(formOrQuery(r, "site")))
	if site == nil {
		h.renderError(w, r, http.StatusNotFound, "地点不存在", "请求的地点不存在。")
		return
	}
	disabled := map[string]bool{}
	for _, candidate := range data.Sites {
		if candidate.ID == site.ID {
			disabled[candidate.LocationPath] = true
		}
	}
	// 先按标识符建父子关系，再沿上级链标记被移动节点的全部祖先以下节点。
	children := map[string][]TopologySite{}
	for _, candidate := range data.Sites {
		children[candidate.ParentID] = append(children[candidate.ParentID], candidate)
	}
	var markDescendants func(parentID string)
	markDescendants = func(parentID string) {
		for _, child := range children[parentID] {
			disabled[child.LocationPath] = true
			markDescendants(child.ID)
		}
	}
	markDescendants(site.ID)
	view := &locationFormView{
		Mode: "move", Site: site, Parent: site.ParentID,
		Options: locationTreeOptions(data, true, disabled),
	}
	// 选中值用完整路径；树选项里已经按路径生成值。
	for _, option := range view.Options {
		if option.Site.ID == site.ParentID {
			view.Parent = option.Site.LocationPath
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		h.renderLocationForm(w, r, http.StatusOK, view, "移动地点")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "移动地点只接受 GET 与 POST 请求。")
		return
	}
	view.Parent = strings.TrimSpace(r.FormValue("parent"))
	if disabled[view.Parent] {
		view.Error = "不能把地点移动到它自己或它的下级。"
		h.renderLocationForm(w, r, http.StatusOK, view, "移动地点")
		return
	}
	parent := view.Parent
	message, err := h.cfg.TopologyWriter.EditLocation(site.LocationPath,
		TopologyLocationChanges{Parent: &parent}, TopologyWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		view.Error = topologyWriteErrorText(err)
		h.renderLocationForm(w, r, http.StatusOK, view, "移动地点")
		return
	}
	h.redirectLocationsNetworksWithNotice(w, r, "location-saved", message)
}

// handleLocationDelete 渲染删除确认；被存储层守卫拦住的地点只显示原因。
func (h *handler) handleLocationDelete(w http.ResponseWriter, r *http.Request) {
	data, err := h.topologyData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取地点失败", "读取地点失败，请稍后重试。")
		return
	}
	site := findSiteByPath(data, strings.TrimSpace(formOrQuery(r, "site")))
	if site == nil {
		h.renderError(w, r, http.StatusNotFound, "地点不存在", "请求的地点不存在。")
		return
	}
	view := &locationFormView{Mode: "delete", Site: site}
	view.DeleteReason, view.Error = h.locationDeleteReason(data, site)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status := http.StatusOK
		if view.DeleteReason != "" {
			status = http.StatusBadRequest
		}
		h.renderLocationForm(w, r, status, view, "删除地点")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "删除地点只接受 GET 与 POST 请求。")
		return
	}
	message, err := h.cfg.TopologyWriter.DeleteLocation(
		site.LocationPath, TopologyWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		view.Error = topologyWriteErrorText(err)
		h.renderLocationForm(w, r, http.StatusBadRequest, view, "删除地点")
		return
	}
	h.redirectLocationsNetworksWithNotice(w, r, "location-deleted", message)
}

// locationDeleteReason 直接按拓扑数据计算删除守卫原因，供列表与删除页
// 展示；真正删除仍由存储层最终判定。
func (h *handler) locationDeleteReason(data *TopologyData, site *TopologySite) (string, string) {
	var childNames, machineNames, networkNames []string
	for _, candidate := range data.Sites {
		if candidate.ParentID == site.ID {
			childNames = append(childNames, candidate.Name)
		}
	}
	for _, device := range data.Devices {
		if device.SiteID == site.ID {
			machineNames = append(machineNames, device.Name)
		}
	}
	for _, network := range data.Networks {
		if network.Type == networkTypePhysical && network.SiteID == site.ID {
			networkNames = append(networkNames, network.Name)
		}
	}
	var blockers []string
	if len(childNames) > 0 {
		blockers = append(blockers, fmt.Sprintf("还有下级地点：%s；请先删除或改上级", strings.Join(childNames, "、")))
	}
	if len(machineNames) > 0 {
		blockers = append(blockers, fmt.Sprintf("还有机器登记在这个地点：%s；请先改机器地点", strings.Join(machineNames, "、")))
	}
	if len(networkNames) > 0 {
		blockers = append(blockers, fmt.Sprintf("还有物理局域网覆盖这个地点：%s；请先删除或改覆盖地点", strings.Join(networkNames, "、")))
	}
	if len(blockers) == 0 {
		return "", ""
	}
	return fmt.Sprintf("地点 %s 不能删除：%s", site.Name, strings.Join(blockers, "；")), ""
}

// handleNetworkCreate 渲染与处理新建网络表单。
func (h *handler) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	data, err := h.topologyData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取地点失败", "读取地点失败，请稍后重试。")
		return
	}
	view := &networkFormView{Mode: "create", Options: locationTreeOptions(data, false, nil)}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		view.Type = networkTypePhysical
		if r.URL.Query().Get("type") == networkTypeVirtual {
			view.Type = networkTypeVirtual
		}
		h.renderNetworkForm(w, r, http.StatusOK, view, "新建网络")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新建网络只接受 GET 与 POST 请求。")
		return
	}
	view.readForm(r)
	if message := view.validate(); message != "" {
		view.Error = message
		h.renderNetworkForm(w, r, http.StatusOK, view, "新建网络")
		return
	}
	message, err := h.cfg.TopologyWriter.AddManagedNetwork(view.input(), TopologyWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		view.Error = topologyWriteErrorText(err)
		h.renderNetworkForm(w, r, http.StatusOK, view, "新建网络")
		return
	}
	h.redirectLocationsNetworksWithNotice(w, r, "network-saved", message)
}

// handleNetworkEdit 渲染与处理网络编辑表单；网络类型登记后不可修改。
func (h *handler) handleNetworkEdit(w http.ResponseWriter, r *http.Request) {
	data, err := h.topologyData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取网络失败", "读取网络失败，请稍后重试。")
		return
	}
	var network *TopologyNetwork
	identifier := strings.TrimSpace(formOrQuery(r, "original"))
	if identifier == "" {
		identifier = strings.TrimSpace(formOrQuery(r, "name"))
	}
	for _, candidate := range data.Networks {
		if candidate.Name == identifier {
			found := candidate
			network = &found
			break
		}
	}
	if network == nil {
		h.renderError(w, r, http.StatusNotFound, "网络不存在", "请求的网络不存在。")
		return
	}
	view := &networkFormView{
		Mode: "edit", Type: network.Type, Options: locationTreeOptions(data, false, nil),
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		view.Name = network.Name
		view.Site = network.LocationPath
		view.NetworkRange = network.NetworkRange
		view.BandwidthMbps = strconv.FormatInt(network.BandwidthMbps, 10)
		view.Metered = network.Metered
		h.renderNetworkForm(w, r, http.StatusOK, view, "编辑网络")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "编辑网络只接受 GET 与 POST 请求。")
		return
	}
	view.readForm(r)
	view.Type = network.Type
	if message := view.validate(); message != "" {
		view.Error = message
		h.renderNetworkForm(w, r, http.StatusOK, view, "编辑网络")
		return
	}
	name := view.Name
	networkRange := view.NetworkRange
	bandwidth := view.bandwidth()
	metered := view.Metered
	changes := TopologyNetworkChanges{
		Name: &name, NetworkRange: &networkRange,
		BandwidthMbps: &bandwidth, Metered: &metered,
	}
	if network.Type == networkTypePhysical {
		site := view.Site
		changes.Site = &site
	}
	message, err := h.cfg.TopologyWriter.EditManagedNetwork(
		network.Name, changes, TopologyWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		view.Error = topologyWriteErrorText(err)
		h.renderNetworkForm(w, r, http.StatusOK, view, "编辑网络")
		return
	}
	h.redirectLocationsNetworksWithNotice(w, r, "network-saved", message)
}

func (h *handler) renderNetworkForm(w http.ResponseWriter, r *http.Request, status int, view *networkFormView, title string) {
	h.renderPage(w, r, status, "network-form.tmpl", pageData{Title: title, NetworkForm: view})
}

func (v *networkFormView) readForm(r *http.Request) {
	v.Name = strings.TrimSpace(r.FormValue("name"))
	v.Type = strings.TrimSpace(r.FormValue("type"))
	v.Site = strings.TrimSpace(r.FormValue("site"))
	v.NetworkRange = strings.TrimSpace(r.FormValue("range"))
	v.BandwidthMbps = strings.TrimSpace(r.FormValue("bandwidth_mbps"))
	v.Metered = r.FormValue("metered") == "1"
}

func (v *networkFormView) bandwidth() int64 {
	value, _ := strconv.ParseInt(v.BandwidthMbps, 10, 64)
	return value
}

func (v *networkFormView) validate() string {
	switch {
	case v.Name == "":
		return "请填写网络名称。"
	case v.Type != networkTypePhysical && v.Type != networkTypeVirtual:
		return "请选择网络类型。"
	case v.Type == networkTypePhysical && v.Site == "":
		return "物理局域网必须选择覆盖的地点。"
	case v.Type == networkTypeVirtual && v.Site != "":
		return "虚拟局域网不覆盖地点，请不要选择覆盖地点。"
	}
	if v.BandwidthMbps != "" {
		value, err := strconv.ParseInt(v.BandwidthMbps, 10, 64)
		if err != nil || value < 0 {
			return "标称带宽需填非负整数（单位 Mbps）。"
		}
	}
	return ""
}

func (v *networkFormView) input() TopologyNetworkInput {
	return TopologyNetworkInput{
		Name: v.Name, Type: v.Type, Site: v.Site, NetworkRange: v.NetworkRange,
		BandwidthMbps: v.bandwidth(), Metered: v.Metered,
	}
}

// topologyWriteErrorText 把写入口错误换成可直接显示的中文提示。
func topologyWriteErrorText(err error) string {
	var notFound *TopologyNameNotFoundError
	var invalid *TopologyInvalidInputError
	switch {
	case errors.As(err, &notFound):
		return notFound.Message
	case errors.As(err, &invalid):
		return invalid.Message
	default:
		return "保存失败，请稍后重试。"
	}
}

func formOrQuery(r *http.Request, key string) string {
	if r.Method == http.MethodPost {
		return r.FormValue(key)
	}
	return r.URL.Query().Get(key)
}

func (h *handler) redirectLocationsNetworksWithNotice(w http.ResponseWriter, r *http.Request, kind, message string) {
	_ = message // 成功提示由列表页按 notice 类别显示，写入口详细路径不进网址。
	http.Redirect(w, r, locationsNetworksPath+"?notice="+kind, http.StatusSeeOther)
}
