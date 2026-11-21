package webui

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/networkrange"
)

// 服务器详情页一列卡片里的可编辑分区；值同时用于查询参数与表单隐藏
// 字段，决定底部弹层渲染哪一组字段。
const (
	serverDetailEditConnection  = "connection"
	serverDetailEditLocation    = "location"
	serverDetailEditAddress     = "address"
	serverDetailEditDescription = "description"
	serverDetailEditAccounts    = "accounts"
)

// serverDetailEditScopeFromQuery 返回要打开的编辑弹层；空串表示不打开。
func serverDetailEditScopeFromQuery(r *http.Request) string {
	switch r.URL.Query().Get("edit") {
	case serverDetailEditConnection, serverDetailEditLocation, serverDetailEditAddress,
		serverDetailEditDescription, serverDetailEditAccounts:
		return r.URL.Query().Get("edit")
	default:
		return ""
	}
}

// serverDetailEditScopeFromForm 返回表单声明的编辑分区；旧表单没有这个
// 字段时按连接分区处理，保持既有提交不失效。
func serverDetailEditScopeFromForm(r *http.Request) string {
	switch r.PostFormValue("edit_scope") {
	case serverDetailEditDescription, serverDetailEditAccounts:
		return r.PostFormValue("edit_scope")
	default:
		return serverDetailEditConnection
	}
}

// serverDetailPath 返回统一详情页地址；anchor 非空时追加对应卡片锚点。
func serverDetailPath(id string) string {
	return "/ui/servers/" + url.PathEscape(id)
}

// serverDetailCardAnchor 返回一个分区卡片的页面内锚点。
func serverDetailCardAnchor(scope string) string {
	return "#server-card-" + scope
}

// serverDetailEditPath 返回打开某个编辑弹层的详情页地址。
func serverDetailEditPath(id, scope string) string {
	return serverDetailPath(id) + "?edit=" + scope
}

// serverDetailAddressRow 是地址卡片里的一行：网络名、类型与地址。
type serverDetailAddressRow struct {
	NetworkName      string
	NetworkValue     string
	TypeLabel        string
	Address          string
	RangeSuggestions []serverDetailAddressSuggestion
}

// serverDetailAddressSuggestion 是按登记网段算出的一个候选网络；只用于
// 提示与编辑草稿确认，不代表网络成员已写入。
type serverDetailAddressSuggestion struct {
	NetworkName  string
	NetworkRange string
}

// serverDetailSiblingAccount 是同一台机器上的另一个登录账号；Note 复用
// 该条目的服务器元数据说明字段，ConnectionTest 是最近一次连接测试的持久
// 化结果。
type serverDetailSiblingAccount struct {
	ID             string
	Name           string
	User           string
	Note           string
	Revision       int64
	ConnectionTest *ServerConnectionTestResultView
}

// serverDetailDeviceOption 是改挂表单里的一台机器。
type serverDetailDeviceOption struct {
	Name         string
	LocationPath string
}

// serverDetailTopologyView 是地址与地点块的展示数据；客户端只读来源也会
// 填充，CanWrite 控制表单是否出现。
type serverDetailTopologyView struct {
	HasDevice    bool
	DeviceName   string
	LocationPath string
	// PreferredNetworkAddress 来自设备连接候选；列表卡片与详情页页头共用
	// 同一选取规则，只取物理或虚拟局域网候选，不把兜底主机当成网络成员地
	// 址。
	PreferredNetworkAddress string
	Addresses               []serverDetailAddressRow
	AvailableNetworks       []serverDetailDeviceOption
	Mismatches              []TopologyLocationMismatch
	SiblingAccounts         []serverDetailSiblingAccount
	MoveChoices             []serverDetailDeviceOption
	LocationOptions         locationOptionsView
	CanWrite                bool
	CanEditAddresses        bool
	LocationOpen            bool
	NewLocationHref         string
}

// serverDetailSoftwareView 是软件清单入口卡片的数据；摘要与清单明细都来
// 自软件清单设备页的同一份读取结果。
type serverDetailSoftwareView struct {
	Available  bool
	Reason     string
	DeviceName string
	Summary    SoftwareInventoryDeviceSummary
	Status     SoftwareInventoryStatus
}

// serverDetailSecretRow 是授权块里一个机密项的完整展示行。
type serverDetailSecretRow struct {
	ServerDetailSecretChoice
	LatestDeliveryPath string
	LatestDeliveryAt   string
}

// serverDetailProfileRow 是授权块里一个 profile 的完整展示行。
type serverDetailProfileRow struct {
	ServerDetailProfileChoice
	ClientNames []string
}

// serverDetailPageView 是详情页除服务器条目本体外的页面组装数据；
// EditDrawer 非空时页面末尾渲染对应分区的底部编辑弹层。
type serverDetailPageView struct {
	EditDrawer    string
	Edit          *serverEditView
	AccountDrafts map[string]string
	AddressDrafts []serverDetailAddressRow
	SecretRows    []serverDetailSecretRow
	ProfileRows   []serverDetailProfileRow
	ShareRows     []ServerDetailSharedStorageChoice
	Topology      *serverDetailTopologyView
	// PreferredNetworkAddress 是条目主机为空时页头显示的网络成员地址；列
	// 表卡片与详情页页头共用同一选取规则，取连接候选里的第一个物理或虚拟
	// 局域网地址。
	PreferredNetworkAddress string
	Software                *serverDetailSoftwareView
	Move                    *serverDetailMoveDraft
	AdvancedOpen            bool
}

// serverDetailMoveDraft 保存改挂确认框里 owner 已选择的目标机器与确认状态。
type serverDetailMoveDraft struct {
	Device     string
	Understood bool
}

// DrawerTitle 返回当前编辑弹层的标题。
func (v serverDetailPageView) DrawerTitle() string {
	switch v.EditDrawer {
	case serverDetailEditLocation:
		return "编辑地点"
	case serverDetailEditAddress:
		return "编辑地址"
	case serverDetailEditDescription:
		return "编辑说明"
	case serverDetailEditAccounts:
		return "编辑其他账号说明"
	default:
		return "编辑连接"
	}
}

// DrawerCaption 返回当前编辑弹层的说明文字。
func (v serverDetailPageView) DrawerCaption() string {
	switch v.EditDrawer {
	case serverDetailEditLocation:
		return "地点登记在机器上，同一台机器的全部服务器条目共用。"
	case serverDetailEditAddress:
		return "地址登记在机器上，同一台机器的全部服务器条目共用。"
	case serverDetailEditDescription:
		return "这些内容只写给人看，用于补充这台服务器的背景。"
	case serverDetailEditAccounts:
		return "只编辑显示用的说明；账号的登录信息在各自详情页修改。"
	default:
		return "只修改这个条目自己的登录信息。"
	}
}

// handleServerDetail 渲染统一详情页。一列卡片的数据全部组装；编辑弹层由
// edit 查询参数选择。旧分段地址与旧编辑参数回到统一详情页。
func (h *handler) handleServerDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if target, ok := serverDetailLegacyRedirect(id, r.URL.Query()); ok {
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
	}
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
		return
	}
	if taskID := strings.TrimSpace(r.URL.Query().Get("task")); taskID != "" {
		detail.ConnectionTest = h.serverConnectionTaskView(detail, taskID)
	}
	noticeKind := ""
	if serverDetailSavedNoticeKind("saved-"+r.URL.Query().Get("saved")) != "" {
		noticeKind = "success"
	}
	noticeParam := r.URL.Query().Get("notice")
	if noticeParam == "" {
		noticeParam = "saved-" + r.URL.Query().Get("saved")
	}
	notice := serverDetailNoticeText(noticeParam)
	if noticeParam == "server-moved" {
		notice = serverDetailMoveNoticeText(detail.DeviceName, r.URL.Query().Get("anchor-copy"))
		noticeKind = "success"
		if notice == "" {
			noticeKind = ""
		}
	}
	switch r.URL.Query().Get("hostkey") {
	case "updated":
		notice, noticeKind = "已覆盖主机密钥锚。", "success"
	case "cleared":
		notice, noticeKind = "已删除主机密钥锚。", "success"
	}
	options := serverDetailRenderOptions{status: http.StatusOK, notice: notice, noticeKind: noticeKind}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if r.URL.Query().Get("move") == "confirm" {
			options.move = &serverDetailMoveDraft{}
		} else if r.URL.Query().Get("advanced") == "open" {
			options.advancedOpen = true
		}
	}
	h.renderServerDetailPageView(w, r, detail, options)
}

// renderServerDetailPage 组装并渲染详情页；notice 与 noticeKind 为空时不显
// 示提示条。
func (h *handler) renderServerDetailPage(
	w http.ResponseWriter, r *http.Request, status int, detail *ServerDetailView,
	notice, noticeKind string,
) {
	h.renderServerDetailPageView(w, r, detail, serverDetailRenderOptions{
		status: status, notice: notice, noticeKind: noticeKind,
	})
}

// serverDetailRenderOptions 描述一次详情页渲染的附加状态。
type serverDetailRenderOptions struct {
	status        int
	notice        string
	noticeKind    string
	drawer        string
	edit          *serverEditView
	accountDrafts map[string]string
	addressDrafts []serverDetailAddressRow
	move          *serverDetailMoveDraft
	advancedOpen  bool
}

// renderServerDetailPageView 渲染详情页；drawer 非空时打开对应编辑弹层，
// edit 携带连接弹层的校验提示与最新值。
func (h *handler) renderServerDetailPageView(
	w http.ResponseWriter, r *http.Request, detail *ServerDetailView,
	options serverDetailRenderOptions,
) {
	drawer := options.drawer
	if drawer == "" && r.Method == http.MethodGet || drawer == "" && r.Method == http.MethodHead {
		drawer = serverDetailEditScopeFromQuery(r)
	}
	edit := options.edit
	if drawer == serverDetailEditConnection && edit == nil && h.cfg.ServerWriter != nil &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) {
		edit = h.serverEditView(r, detail, "")
	}
	view, err := h.serverDetailPageWith(r, detail, drawer, edit, options.accountDrafts)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if options.addressDrafts != nil {
		view.AddressDrafts = options.addressDrafts
	}
	if !h.caps.CanWrite {
		view.EditDrawer = ""
	}
	view.Move = options.move
	view.AdvancedOpen = options.advancedOpen
	if options.edit != nil && options.edit.Notice != "" && options.notice == "" {
		options.notice = options.edit.Notice
	}
	data := pageData{
		Title: detail.Name, ServerDetail: detail,
		ServerDetailNotice: options.notice, ServerDetailNoticeKind: options.noticeKind,
		ServerDetailPage: view, ServerEdit: view.Edit,
	}
	h.renderPage(w, r, options.status, "servers-detail.tmpl", data)
}

// serverDetailLegacyRedirect 把旧分段地址与旧编辑参数换算成统一详情页地
// 址；没有旧参数时返回 false。
func serverDetailLegacyRedirect(id string, query url.Values) (string, bool) {
	scope := ""
	if query.Get("edit") == "1" {
		scope = serverDetailEditConnection
	}
	if query.Get("location") == "1" {
		scope = serverDetailEditLocation
	}
	if !query.Has("tab") && scope == "" {
		return "", false
	}
	target := serverDetailPath(id)
	if scope != "" {
		target = serverDetailEditPath(id, scope)
	}
	return target, true
}

// serverDetailNoticeText 把详情页动作的查询参数提示码换成文案。
func serverDetailNoticeText(kind string) string {
	switch kind {
	case "location-saved":
		return "已保存机器地点。"
	case "saved-address":
		return "已保存地址。"
	case "server-moved":
		return "已改挂到另一台机器。"
	case "saved-connection":
		return "已保存连接信息。"
	case "saved-description":
		return "已保存说明。"
	case "saved-accounts":
		return "已保存账号说明。"
	default:
		return ""
	}
}

// serverDetailSavedNoticeKind 把保存成功的提示码换成分区值；空串表示不是
// 保存成功提示。
func serverDetailSavedNoticeKind(kind string) string {
	switch kind {
	case "saved-connection":
		return serverDetailEditConnection
	case "saved-description":
		return serverDetailEditDescription
	case "saved-accounts":
		return serverDetailEditAccounts
	case "saved-address":
		return serverDetailEditAddress
	default:
		return ""
	}
}

// serverDetailPage 组装一列卡片的附加数据。拓扑读取失败会让整页失败；软
// 件清单没有数据时只显示原因，不阻塞详情页。
func (h *handler) serverDetailPage(r *http.Request, detail *ServerDetailView) (*serverDetailPageView, error) {
	drawer := serverDetailEditScopeFromQuery(r)
	var edit *serverEditView
	if drawer == serverDetailEditConnection && h.cfg.ServerWriter != nil {
		edit = h.serverEditView(r, detail, "")
	}
	return h.serverDetailPageWith(r, detail, drawer, edit, nil)
}

// serverDetailPageWith 按指定弹层与编辑数据组装页面；校验失败重新渲染时
// 由调用方传入最新值与保留的账号说明草稿。
func (h *handler) serverDetailPageWith(
	r *http.Request, detail *ServerDetailView, drawer string,
	edit *serverEditView, accountDrafts map[string]string,
) (*serverDetailPageView, error) {
	view := &serverDetailPageView{
		EditDrawer: drawer, Edit: edit, AccountDrafts: accountDrafts,
	}
	if h.cfg.Topology != nil {
		data, err := h.cfg.Topology.Topology(h.requestClientInstance(r), TopologyProbeOriginSelf)
		if err != nil {
			return nil, err
		}
		view.Topology = h.serverDetailTopologyView(detail, data, r)
		view.PreferredNetworkAddress = view.Topology.PreferredNetworkAddress
		if drawer == serverDetailEditLocation && view.Topology.CanWrite && view.Topology.HasDevice {
			view.Topology.LocationOpen = true
		}
		if drawer == serverDetailEditAddress && view.Topology.CanEditAddresses {
			view.AddressDrafts = append([]serverDetailAddressRow(nil), view.Topology.Addresses...)
			if len(view.AddressDrafts) == 0 {
				view.AddressDrafts = []serverDetailAddressRow{{NetworkValue: ServerAddressNetworkOther}}
			}
		}
	}
	view.Software = h.serverDetailSoftwareView(r, detail)
	view.SecretRows = serverDetailSecretRows(detail)
	view.ProfileRows = h.serverDetailProfileRows(r, detail)
	view.ShareRows = detail.SharedStorageChoices
	return view, nil
}

// serverDetailTopologyView 从拓扑数据组装地址与地点块。
func (h *handler) serverDetailTopologyView(
	detail *ServerDetailView, data *TopologyData, r *http.Request,
) *serverDetailTopologyView {
	view := &serverDetailTopologyView{
		HasDevice: detail.HasDevice, DeviceName: detail.DeviceName,
		CanWrite:         h.cfg.TopologyWriter != nil,
		CanEditAddresses: h.caps.CanWrite,
	}
	if !view.CanWrite {
		view.LocationOpen = false
	}
	networkByID := map[string]TopologyNetwork{}
	for _, network := range data.Networks {
		networkByID[network.ID] = network
	}
	var device *TopologyDevice
	for index := range data.Devices {
		if data.Devices[index].Name == detail.DeviceName {
			device = &data.Devices[index]
			break
		}
	}
	if device == nil {
		return view
	}
	view.PreferredNetworkAddress = preferredNetworkAddress(device.Candidates)
	view.LocationPath = device.LocationPath
	suggestionNetworks := make([]networkrange.Network, 0, len(data.Networks))
	for _, network := range data.Networks {
		suggestionNetworks = append(suggestionNetworks, networkrange.Network{
			Name: network.Name, Range: network.NetworkRange, SiteID: network.SiteID,
			Physical: network.Type != networkTypeVirtual,
		})
	}
	suggestionSites := make([]*models.Site, 0, len(data.Sites))
	for _, site := range data.Sites {
		suggestionSites = append(suggestionSites, &models.Site{ID: site.ID, ParentID: site.ParentID})
	}
	rangeSuggestions := func(address, currentNetwork string) []serverDetailAddressSuggestion {
		matches := networkrange.Suggestions(networkrange.Input{
			Address: address, CurrentNetwork: currentNetwork, DeviceSiteID: device.SiteID,
			Networks: suggestionNetworks, Sites: suggestionSites,
		})
		suggestions := make([]serverDetailAddressSuggestion, 0, len(matches))
		for _, match := range matches {
			suggestions = append(suggestions, serverDetailAddressSuggestion{
				NetworkName: match.NetworkName, NetworkRange: match.NetworkRange,
			})
		}
		return suggestions
	}
	for _, membership := range data.Memberships {
		if membership.DeviceID != device.ID {
			continue
		}
		network := networkByID[membership.NetworkID]
		view.Addresses = append(view.Addresses, serverDetailAddressRow{
			NetworkName: network.Name, NetworkValue: network.Name,
			TypeLabel: networkTypeLabel(network.Type), Address: membership.Address,
			RangeSuggestions: rangeSuggestions(membership.Address, network.Name),
		})
	}
	if detail.Host != "" {
		view.Addresses = append(view.Addresses, serverDetailAddressRow{
			NetworkName: "其他（公网或未登记网络）", NetworkValue: ServerAddressNetworkOther,
			TypeLabel: "其他地址", Address: detail.Host,
			RangeSuggestions: rangeSuggestions(detail.Host, ServerAddressNetworkOther),
		})
	}
	for _, network := range data.Networks {
		view.AvailableNetworks = append(view.AvailableNetworks, serverDetailDeviceOption{
			Name: network.Name, LocationPath: network.LocationPath,
		})
	}
	view.Mismatches = device.LocationMismatches
	for _, server := range device.Servers {
		if server.ID == detail.ID || server.Name == detail.Name {
			continue
		}
		row := serverDetailSiblingAccount{ID: server.ID, Name: server.Name, User: server.User}
		if sibling, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), server.ID); err == nil && sibling != nil {
			row.Note = sibling.Description
			row.Revision = sibling.Revision
			row.ConnectionTest = sibling.ConnectionTest
		}
		view.SiblingAccounts = append(view.SiblingAccounts, row)
	}
	for _, candidate := range data.Devices {
		if candidate.Name == device.Name {
			continue
		}
		view.MoveChoices = append(view.MoveChoices, serverDetailDeviceOption{
			Name: candidate.Name, LocationPath: candidate.LocationPath,
		})
	}
	if view.CanWrite {
		options := []locationOptionView{{
			Site: TopologySite{ID: "", Name: "不登记地点", LocationPath: ""},
		}}
		options = append(options, locationTreeOptions(data, false, nil)...)
		view.LocationOptions = locationOptionsView{
			Options: options, InputName: "site", Selected: device.LocationPath,
		}
		view.NewLocationHref = locationCreatePath + "?" + url.Values{
			"parent":    {device.LocationPath},
			"return_to": {serverDetailEditPath(detail.ID, serverDetailEditLocation)},
		}.Encode()
	}
	return view
}

// networkTypeLabel 把网络类型代号换成页面文案。
func networkTypeLabel(networkType string) string {
	if networkType == networkTypeVirtual {
		return "虚拟局域网"
	}
	return "物理局域网"
}

// serverDetailSoftwareView 组装软件清单入口卡片；摘要直接沿用设备软件清
// 单页的数据，不在详情页另算一套状态。
func (h *handler) serverDetailSoftwareView(r *http.Request, detail *ServerDetailView) *serverDetailSoftwareView {
	view := &serverDetailSoftwareView{}
	if h.cfg.SoftwareInventory == nil {
		view.Reason = "软件清单在权威端查看。"
		return view
	}
	if !detail.HasDevice || detail.DeviceName == "" {
		view.Reason = "未关联机器，暂无软件清单。"
		return view
	}
	device, err := h.cfg.SoftwareInventory.SoftwareInventoryDevice(detail.DeviceName)
	if err != nil || device == nil {
		view.Reason = "这台机器还没有软件清单。"
		return view
	}
	view.Available = true
	view.DeviceName = device.Name
	view.Summary = device.Summary
	view.Status = device.Status
	return view
}

// serverDetailSecretRows 把机密项选择与最近写入记录拼成展示行；客户端没
// 有选择数据时按已授权的只读清单展示。
func serverDetailSecretRows(detail *ServerDetailView) []serverDetailSecretRow {
	choices := detail.SecretChoices
	if len(choices) == 0 {
		for _, secret := range detail.Secrets {
			choices = append(choices, ServerDetailSecretChoice{
				ServerDetailSecret: secret, Checked: true,
			})
		}
	}
	rows := make([]serverDetailSecretRow, 0, len(choices))
	latestBySecret := latestDeliveryBySecret(detail.DeliveryRecords)
	for _, choice := range choices {
		row := serverDetailSecretRow{ServerDetailSecretChoice: choice}
		if latest, ok := latestBySecret[choice.Name]; ok {
			row.LatestDeliveryPath = latest.Path
			row.LatestDeliveryAt = latest.WrittenAt.Format("2006-01-02")
		}
		rows = append(rows, row)
	}
	return rows
}

// latestDeliveryBySecret 返回每个机密项最近一次写到这台服务器的文件。
func latestDeliveryBySecret(records []SecretDeliveryRecordRow) map[string]SecretDeliveryRecordRow {
	latest := map[string]SecretDeliveryRecordRow{}
	for _, record := range records {
		for _, secret := range record.Secrets {
			previous, ok := latest[secret.Name]
			if !ok || record.WrittenAt.After(previous.WrittenAt) {
				latest[secret.Name] = record
			}
		}
	}
	return latest
}

// serverDetailProfileRows 把 profile 选择与绑定客户端拼成展示行。
func (h *handler) serverDetailProfileRows(r *http.Request, detail *ServerDetailView) []serverDetailProfileRow {
	rows := make([]serverDetailProfileRow, 0, len(detail.ProfileChoices))
	var clients []ClientSummary
	if h.cfg.Access != nil {
		if list, err := h.cfg.Access.ListClients(); err == nil {
			clients = list
		}
	}
	for _, choice := range detail.ProfileChoices {
		row := serverDetailProfileRow{ServerDetailProfileChoice: choice}
		for _, client := range clients {
			if client.Revoked || client.ProfileID != choice.ID {
				continue
			}
			row.ClientNames = append(row.ClientNames, client.Name)
		}
		rows = append(rows, row)
	}
	return rows
}

// serverDetailWriteErrorText 把拓扑写错误换成可显示文案；返回 false 表示
// 应按服务器错误处理。
func serverDetailWriteErrorText(err error) (string, bool) {
	var missing *TopologyNameNotFoundError
	if errors.As(err, &missing) {
		return missing.Message, true
	}
	var invalid *TopologyInvalidInputError
	if errors.As(err, &invalid) {
		return invalid.Message, true
	}
	return "", false
}

// serverDetailWriteRedirect 保存成功后回到统一详情页的对应卡片锚点。
func serverDetailWriteRedirect(w http.ResponseWriter, r *http.Request, id, notice, anchor string) {
	target := serverDetailPath(id) + "?notice=" + notice
	if anchor != "" {
		target += anchor
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// serverDetailMoveRedirect 回到详情页高级操作，只携带主机密钥指纹复制结
// 论；目标机器名由详情页读取当前存储数据得到，不经过跳转参数。
func serverDetailMoveRedirect(w http.ResponseWriter, r *http.Request, id string, result TopologyServerMoveResult) {
	outcome := result.AnchorCopyOutcome
	switch outcome {
	case TopologyAnchorCopyCopied, TopologyAnchorCopyNone, TopologyAnchorCopyConflict:
	default:
		outcome = TopologyAnchorCopyNone
	}
	query := url.Values{
		"notice":      {"server-moved"},
		"anchor-copy": {outcome},
	}
	http.Redirect(w, r, serverDetailPath(id)+"?"+query.Encode()+"#server-advanced-actions", http.StatusSeeOther)
}

// serverDetailMoveNoticeText 生成改挂成功后的完整提示。机器名必须来自当前
// 存储数据；未知指纹结论返回空串，页面不显示成功提示。
func serverDetailMoveNoticeText(device, anchorOutcome string) string {
	if device == "" {
		return ""
	}
	notice := "已改挂到目标机器 " + device + "。"
	suffix := ""
	validOutcome := false
	switch anchorOutcome {
	case TopologyAnchorCopyCopied:
		suffix, validOutcome = "主机密钥指纹已复制。", true
	case TopologyAnchorCopyConflict:
		suffix, validOutcome = "同机账号的主机密钥指纹不一致，未复制。", true
	case TopologyAnchorCopyNone:
		suffix, validOutcome = "无可复制的主机密钥指纹。", true
	}
	if !validOutcome {
		return ""
	}
	return notice + suffix
}

// handleServerDetailLocation 处理机器地点保存表单。
func (h *handler) handleServerDetailLocation(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "保存机器地点只接受 POST 请求。")
		return
	}
	detail := h.serverDetailForWrite(w, r, id)
	if detail == nil {
		return
	}
	if !detail.HasDevice || detail.DeviceName == "" {
		h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "这台服务器条目还没有关联机器，不能单独改地点。")
		return
	}
	site := strings.TrimSpace(r.PostFormValue("site"))
	if _, err := h.cfg.TopologyWriter.EditDeviceSite(
		detail.DeviceName, site, TopologyWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		h.renderServerDetailDrawerError(
			w, r, detail, serverDetailWriteStatus(err), serverDetailWriteMessage(err), serverDetailEditLocation,
		)
		return
	}
	serverDetailWriteRedirect(w, r, detail.ID, "location-saved", serverDetailCardAnchor(serverDetailEditLocation))
}

// handleServerDetailAddressList 处理整份地址列表保存。校验失败、写入失败
// 与修订号冲突都保持弹层打开并回显整份草稿。
func (h *handler) handleServerDetailAddressList(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "保存地址列表只接受 POST 请求。")
		return
	}
	detail := h.serverDetailForWrite(w, r, id)
	if detail == nil {
		return
	}
	form, drafts := parseServerAddressListForm(r, detail.Revision)
	if !detail.HasDevice || detail.DeviceName == "" {
		h.renderServerDetailAddressError(w, r, detail, http.StatusBadRequest,
			"这台服务器条目还没有关联机器，请先改挂到机器或到新增流程补网络位置。", drafts)
		return
	}
	_, conflict, err := h.cfg.ServerWriter.EditServerAddresses(
		detail.ID, form, ServerWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		var formErr *ServerFormError
		if errors.As(err, &formErr) {
			h.renderServerDetailAddressError(w, r, detail, http.StatusBadRequest, formErr.Message, drafts)
			return
		}
		h.renderServerDetailAddressError(w, r, detail, http.StatusInternalServerError,
			"保存地址失败，请稍后重试。", drafts)
		return
	}
	if conflict != nil {
		h.renderServerDetailAddressError(w, r, conflict.Current, http.StatusConflict,
			"服务器条目已被其他入口修改，以下为最新值；请核对后重新提交。", drafts)
		return
	}
	http.Redirect(
		w, r, serverDetailPath(detail.ID)+"?saved=address"+serverDetailCardAnchor(serverDetailEditAddress),
		http.StatusSeeOther,
	)
}

func parseServerAddressListForm(r *http.Request, fallbackRevision int64) (ServerAddressListForm, []serverDetailAddressRow) {
	revision := int64(0)
	if parsed, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("revision")), 10, 64); err == nil {
		revision = parsed
	} else if strings.TrimSpace(r.PostFormValue("revision")) == "" {
		revision = fallbackRevision
	}
	networks := r.PostForm["address_network"]
	addresses := r.PostForm["address"]
	rows := make([]ServerAddressRow, 0, len(networks))
	drafts := make([]serverDetailAddressRow, 0, len(networks))
	for index, network := range networks {
		address := ""
		if index < len(addresses) {
			address = strings.TrimSpace(addresses[index])
		}
		network = strings.TrimSpace(network)
		if network == "" && address == "" {
			continue
		}
		rows = append(rows, ServerAddressRow{Network: network, Address: address})
		drafts = append(drafts, serverDetailAddressRow{NetworkValue: network, Address: address})
	}
	return ServerAddressListForm{Rows: rows, ExpectedRevision: revision}, drafts
}

func (h *handler) renderServerDetailAddressError(
	w http.ResponseWriter, r *http.Request, detail *ServerDetailView, status int,
	notice string, drafts []serverDetailAddressRow,
) {
	h.renderServerDetailPageView(w, r, detail, serverDetailRenderOptions{
		status: status, notice: notice, drawer: serverDetailEditAddress, addressDrafts: drafts,
	})
}

// handleServerDetailMove 处理改挂到另一台机器表单，等价于命令行
// devices move-server。
func (h *handler) handleServerDetailMove(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "改挂服务器只接受 POST 请求。")
		return
	}
	detail := h.serverDetailForWrite(w, r, id)
	if detail == nil {
		return
	}
	device := strings.TrimSpace(r.PostFormValue("device"))
	if device == "" {
		h.renderServerDetailMoveError(w, r, detail, http.StatusBadRequest, "请选择目标机器。", device, false)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		h.renderServerDetailMoveError(w, r, detail, http.StatusBadRequest, "请先勾选「我已了解」。", device, false)
		return
	}
	result, err := h.cfg.TopologyWriter.MoveServerToDevice(
		detail.ID, device, TopologyWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderServerDetailMoveError(
			w, r, detail, serverDetailWriteStatus(err), serverDetailWriteMessage(err), device, true,
		)
		return
	}
	serverDetailMoveRedirect(w, r, detail.ID, result)
}

// renderServerDetailMoveError 带改挂确认状态重新渲染详情页，保留 owner 已
// 选目标机器与勾选状态。
func (h *handler) renderServerDetailMoveError(
	w http.ResponseWriter, r *http.Request, detail *ServerDetailView,
	status int, notice, device string, understood bool,
) {
	h.renderServerDetailPageView(w, r, detail, serverDetailRenderOptions{
		status: status, notice: notice,
		move: &serverDetailMoveDraft{Device: device, Understood: understood},
	})
}

// handleServerDetailTest 启动一次连接测试。局部请求把连接卡片的结果区换
// 成进行中状态并继续轮询；普通请求回到详情页，不再进入独立结果页。
func (h *handler) handleServerDetailTest(w http.ResponseWriter, r *http.Request, id string) {
	taskID := strings.TrimSpace(r.URL.Query().Get("task"))
	if r.Method == http.MethodGet {
		if taskID == "" || !isFragmentRequest(r) {
			h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "读取连接测试状态只接受局部刷新请求。")
			return
		}
		detail := h.serverDetailForWrite(w, r, id)
		if detail == nil {
			return
		}
		view := h.serverConnectionTaskView(detail, taskID)
		if view != nil && view.State == ServerConnectionTestRunning {
			// 进行中内容每秒都相同；返回 204 让 htmx 不改写无障碍动态提
			// 醒区域，只有完成态变化时才替换一次结果。
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if view == nil {
			view = detail.ConnectionTest
		}
		h.renderConnectionTestFragment(w, r, view, true)
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "测试连接只接受 POST 请求。")
		return
	}
	detail := h.serverDetailForWrite(w, r, id)
	if detail == nil {
		return
	}
	if h.cfg.ServerConnectionTester == nil {
		h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "当前部署没有提供连接测试来源。")
		return
	}
	probeShell := strings.TrimSpace(detail.Shell) == ""
	taskID, err := h.cfg.ServerConnectionTester.StartServerTest(
		detail.ID, probeShell, ServerWriteSource{RemoteAddress: remoteIP(r)}, ServerConnectionAnchor{},
	)
	if err != nil || taskID == "" {
		if isFragmentRequest(r) {
			h.renderConnectionTestFragment(w, r, &ServerConnectionTestResultView{
				State: ServerConnectionTestFailed, FailureLabel: "连接测试未启动，请稍后重试。",
			}, false)
			return
		}
		h.renderServerDetailWriteError(w, r, detail, http.StatusInternalServerError, "连接测试未启动，请稍后重试。")
		return
	}
	if isFragmentRequest(r) {
		h.renderConnectionTestFragment(w, r, h.runningConnectionTestView(detail.ID, taskID), false)
		return
	}
	target := url.Values{"tab": {"connection"}, "task": {taskID}}
	http.Redirect(w, r, "/ui/servers/"+url.PathEscape(detail.ID)+"?"+target.Encode(), http.StatusSeeOther)
}

func (h *handler) serverConnectionTaskView(detail *ServerDetailView, taskID string) *ServerConnectionTestResultView {
	if h.cfg.ServerConnectionTester == nil {
		return detail.ConnectionTest
	}
	status, ok := h.cfg.ServerConnectionTester.ServerTestStatus(taskID)
	if !ok || status == nil || status.ServerID != detail.ID {
		return detail.ConnectionTest
	}
	if status.State == ServerConnectionTestRunning {
		return h.runningConnectionTestView(detail.ID, taskID)
	}
	if status.Result != nil {
		return status.Result
	}
	return detail.ConnectionTest
}

func (h *handler) runningConnectionTestView(serverID, taskID string) *ServerConnectionTestResultView {
	poll := url.Values{"task": {taskID}}
	return &ServerConnectionTestResultView{
		State:    ServerConnectionTestRunning,
		PollPath: "/ui/servers/" + url.PathEscape(serverID) + "/test?" + poll.Encode(),
	}
}

func (h *handler) renderConnectionTestFragment(
	w http.ResponseWriter, r *http.Request, view *ServerConnectionTestResultView, updateHeader bool,
) {
	persistedResult := view != nil && view.Persisted && !view.CompletedAt.IsZero() &&
		view.State != ServerConnectionTestRunning
	h.renderPage(w, r, http.StatusOK, "server-connection-test-result.tmpl", pageData{
		ServerConnectionTest:             view,
		ServerConnectionTestUpdateHeader: updateHeader && persistedResult,
	})
}

// serverDetailForWrite 读取写操作目标的服务器详情，找不到或读失败时已写
// 响应并返回 nil。
func (h *handler) serverDetailForWrite(w http.ResponseWriter, r *http.Request, id string) *ServerDetailView {
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return nil
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
		return nil
	}
	return detail
}

func serverDetailWriteStatus(err error) int {
	var missing *TopologyNameNotFoundError
	if errors.As(err, &missing) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func serverDetailWriteMessage(err error) string {
	if message, ok := serverDetailWriteErrorText(err); ok {
		return message
	}
	return "保存失败，请稍后重试。"
}

func (h *handler) renderServerDetailWriteError(
	w http.ResponseWriter, r *http.Request, detail *ServerDetailView, status int, notice string,
) {
	h.renderServerDetailPage(w, r, status, detail, notice, "")
}

// renderServerDetailDrawerError 带提示重新渲染详情页并保持指定编辑弹层打
// 开，让 owner 在同一层里看到失败原因。
func (h *handler) renderServerDetailDrawerError(
	w http.ResponseWriter, r *http.Request, detail *ServerDetailView, status int,
	notice, drawer string,
) {
	h.renderServerDetailPageView(w, r, detail, serverDetailRenderOptions{
		status: status, notice: notice, drawer: drawer,
	})
}

// mergeServerEditUpload 读取编辑表单上传的私钥文件并合入表单值；选择文件
// 后等价于「改用私钥」并粘贴文件内容。
func (h *handler) mergeServerEditUpload(w http.ResponseWriter, r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		return true
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "表单格式不正确", "编辑表单读取失败，请重试。")
		return false
	}
	if r.MultipartForm == nil {
		return true
	}
	file, _, err := r.FormFile("key_file")
	if err != nil {
		return true
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil || len(strings.TrimSpace(string(content))) == 0 {
		return true
	}
	values := r.MultipartForm.Value
	values["key"] = []string{string(content)}
	if len(values["credential_action"]) == 0 || values["credential_action"][0] == "" ||
		values["credential_action"][0] == "keep" {
		values["credential_action"] = []string{"key"}
	}
	r.PostForm = values
	r.Form = values
	return true
}

// NoteFirstLine 返回备注首行，供授权块的名称下方说明使用。
func (s ServerDetailSecret) NoteFirstLine() string {
	if line, _, found := strings.Cut(s.Note, "\n"); found {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(s.Note)
}

// NoteFirstLine 返回共享存储备注首行，供授权块的名称下方说明使用。
func (s ServerDetailSharedStorage) NoteFirstLine() string {
	if line, _, found := strings.Cut(s.Note, "\n"); found {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(s.Note)
}
