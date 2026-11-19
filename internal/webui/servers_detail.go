package webui

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 服务器详情页的四个分段；值同时用于查询参数与模板判断。
const (
	serverDetailTabConnection    = "connection"
	serverDetailTabAddresses     = "addresses"
	serverDetailTabAuthorization = "authorization"
	serverDetailTabSoftware      = "software"
)

// serverDetailTabFromQuery 返回要显示的分段；未知值回落到连接块。
func serverDetailTabFromQuery(r *http.Request) string {
	switch r.URL.Query().Get("tab") {
	case serverDetailTabAddresses:
		return serverDetailTabAddresses
	case serverDetailTabAuthorization:
		return serverDetailTabAuthorization
	case serverDetailTabSoftware:
		return serverDetailTabSoftware
	default:
		return serverDetailTabConnection
	}
}

// serverDetailTabPath 返回一个分段的详情页地址。
func serverDetailTabPath(id, tab string) string {
	return "/ui/servers/" + url.PathEscape(id) + "?tab=" + tab
}

// serverDetailAddressRow 是地址卡片里的一行：网络名、类型与地址。
type serverDetailAddressRow struct {
	NetworkName string
	TypeLabel   string
	Address     string
}

// locationOptionsView 是共用地点树单选片段的渲染数据。
type locationOptionsView struct {
	Options   []locationOptionView
	InputName string
	Selected  string
}

// locationOptions 组装共用地点树单选片段。
func locationOptions(options []locationOptionView, name, selected string) locationOptionsView {
	return locationOptionsView{Options: options, InputName: name, Selected: selected}
}

// serverDetailSiblingAccount 是同一台机器上的另一个登录账号。
type serverDetailSiblingAccount struct {
	Name string
	User string
}

// serverDetailDeviceOption 是改挂表单里的一台机器。
type serverDetailDeviceOption struct {
	Name         string
	LocationPath string
}

// serverDetailTopologyView 是地址与地点块的展示数据；客户端只读来源也会
// 填充，CanWrite 控制表单是否出现。
type serverDetailTopologyView struct {
	HasDevice         bool
	DeviceName        string
	LocationPath      string
	Addresses         []serverDetailAddressRow
	AvailableNetworks []serverDetailDeviceOption
	Mismatches        []TopologyLocationMismatch
	SiblingAccounts   []serverDetailSiblingAccount
	MoveChoices       []serverDetailDeviceOption
	LocationOptions   []locationOptionView
	CanWrite          bool
	LocationOpen      bool
	FallbackAddress   string
	NewLocationHref   string
}

// serverDetailSoftwareView 是软件块的数据；实测与声明两份清单在服务端分
// 好，模板只负责展示。
type serverDetailSoftwareView struct {
	Available  bool
	Reason     string
	DeviceName string
	Status     SoftwareInventoryStatus
	Measured   []SoftwareInventoryGroup
	Declared   []SoftwareInventoryGroup
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

// serverDetailPageView 是详情页除服务器条目本体外的页面组装数据。
type serverDetailPageView struct {
	Tab          string
	EditOpen     bool
	Edit         *serverEditView
	SecretRows   []serverDetailSecretRow
	ProfileRows  []serverDetailProfileRow
	ShareRows    []ServerDetailSharedStorageChoice
	Topology     *serverDetailTopologyView
	Software     *serverDetailSoftwareView
	ReadonlyRows bool
}

// handleServerDetail 渲染服务器详情页。四个分段由查询参数选择，地址与地
// 点、软件块的数据在这里按需组装；组装失败按服务器错误处理。
func (h *handler) handleServerDetail(w http.ResponseWriter, r *http.Request, id string) {
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if detail == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不存在", "请求的服务器不存在。")
		return
	}
	notice, noticeKind := serverDetailNoticeText(r.URL.Query().Get("notice")), ""
	switch r.URL.Query().Get("hostkey") {
	case "updated":
		notice, noticeKind = "已覆盖主机密钥锚。", "success"
	case "cleared":
		notice, noticeKind = "已删除主机密钥锚。", "success"
	}
	h.renderServerDetailPage(w, r, http.StatusOK, detail, notice, noticeKind)
}

// renderServerDetailPage 组装并渲染详情页；notice 与 noticeKind 为空时不显
// 示提示条。
func (h *handler) renderServerDetailPage(
	w http.ResponseWriter, r *http.Request, status int, detail *ServerDetailView,
	notice, noticeKind string,
) {
	h.renderServerDetailPageView(w, r, status, detail, notice, noticeKind, nil)
}

// renderServerDetailPageView 渲染详情页；edit 非空时在连接块内联展开编辑
// 表单并显示它的提示。
func (h *handler) renderServerDetailPageView(
	w http.ResponseWriter, r *http.Request, status int, detail *ServerDetailView,
	notice, noticeKind string, edit *serverEditView,
) {
	view, err := h.serverDetailPage(r, detail)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器详情失败", "读取服务器详情失败，请稍后重试。")
		return
	}
	if edit != nil {
		view.EditOpen = true
		view.Edit = edit
		if edit.Notice != "" && notice == "" {
			notice = edit.Notice
		}
	}
	data := pageData{
		Title: detail.Name, ServerDetail: detail,
		ServerDetailNotice: notice, ServerDetailNoticeKind: noticeKind,
		ServerDetailPage: view, ServerEdit: view.Edit,
	}
	h.renderPage(w, r, status, "servers-detail.tmpl", data)
}

// serverDetailNoticeText 把详情页动作的查询参数提示码换成文案。
func serverDetailNoticeText(kind string) string {
	switch kind {
	case "location-saved":
		return "已保存机器地点。"
	case "address-saved":
		return "已保存地址。"
	case "address-removed":
		return "已移除地址。"
	case "server-moved":
		return "已改挂到另一台机器。"
	default:
		return ""
	}
}

// serverDetailPage 组装四个分段的附加数据。拓扑读取失败会让整页失败；软
// 件清单没有数据时只显示原因，不阻塞详情页。
func (h *handler) serverDetailPage(r *http.Request, detail *ServerDetailView) (*serverDetailPageView, error) {
	view := &serverDetailPageView{
		Tab:          serverDetailTabFromQuery(r),
		EditOpen:     r.URL.Query().Get("edit") == "1",
		ReadonlyRows: h.cfg.ServerWriter == nil,
	}
	if view.EditOpen && h.cfg.ServerWriter != nil {
		view.Edit = h.serverEditView(r, detail, "")
	}
	if h.cfg.Topology != nil {
		data, err := h.cfg.Topology.Topology(h.requestClientInstance(r), TopologyProbeOriginSelf)
		if err != nil {
			return nil, err
		}
		view.Topology = h.serverDetailTopologyView(detail, data, r)
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
		CanWrite:        h.cfg.TopologyWriter != nil,
		FallbackAddress: detail.Host,
	}
	if !view.CanWrite {
		view.LocationOpen = false
	} else {
		view.LocationOpen = r.URL.Query().Get("location") == "1"
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
	view.LocationPath = device.LocationPath
	for _, membership := range data.Memberships {
		if membership.DeviceID != device.ID {
			continue
		}
		network := networkByID[membership.NetworkID]
		view.Addresses = append(view.Addresses, serverDetailAddressRow{
			NetworkName: network.Name, TypeLabel: networkTypeLabel(network.Type),
			Address: membership.Address,
		})
	}
	usedNetworks := map[string]bool{}
	for _, membership := range data.Memberships {
		if membership.DeviceID == device.ID {
			usedNetworks[membership.NetworkID] = true
		}
	}
	for _, network := range data.Networks {
		if !usedNetworks[network.ID] {
			view.AvailableNetworks = append(view.AvailableNetworks, serverDetailDeviceOption{
				Name: network.Name, LocationPath: network.LocationPath,
			})
		}
		if network.Type == networkTypeVirtual {
			continue
		}
	}
	view.Mismatches = device.LocationMismatches
	for _, server := range device.Servers {
		if server.ID == detail.ID || server.Name == detail.Name {
			continue
		}
		view.SiblingAccounts = append(view.SiblingAccounts, serverDetailSiblingAccount{
			Name: server.Name, User: server.User,
		})
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
		view.LocationOptions = append(view.LocationOptions, locationOptionView{
			Site: TopologySite{ID: "", Name: "不登记地点", LocationPath: ""},
		})
		view.LocationOptions = append(view.LocationOptions, locationTreeOptions(data, false, nil)...)
		view.NewLocationHref = locationCreatePath + "?" + url.Values{
			"parent":    {device.LocationPath},
			"return_to": {serverDetailTabPath(detail.ID, serverDetailTabAddresses) + "&location=1"},
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

// serverDetailSoftwareView 组装软件块；实测与声明按条目来源拆成两份清单。
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
	view.Status = device.Status
	for _, group := range device.Groups {
		measured := group
		declared := group
		measured.Items = nil
		declared.Items = nil
		for _, item := range group.Items {
			if item.Source == "声明" {
				declared.Items = append(declared.Items, item)
			} else {
				measured.Items = append(measured.Items, item)
			}
		}
		if len(measured.Items) > 0 {
			view.Measured = append(view.Measured, measured)
		}
		if len(declared.Items) > 0 {
			view.Declared = append(view.Declared, declared)
		}
	}
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

// serverDetailWriteRedirect 保存成功后回到地址与地点块。
func serverDetailWriteRedirect(w http.ResponseWriter, r *http.Request, id, notice string) {
	http.Redirect(w, r, serverDetailTabPath(id, serverDetailTabAddresses)+"&notice="+notice, http.StatusSeeOther)
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
		h.renderServerDetailWriteError(w, r, detail, serverDetailWriteStatus(err), serverDetailWriteMessage(err))
		return
	}
	serverDetailWriteRedirect(w, r, detail.ID, "location-saved")
}

// handleServerDetailAddressWrite 处理地址的新增、编辑与移除。
func (h *handler) handleServerDetailAddressWrite(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "保存地址只接受 POST 请求。")
		return
	}
	detail := h.serverDetailForWrite(w, r, id)
	if detail == nil {
		return
	}
	if !detail.HasDevice || detail.DeviceName == "" {
		h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "这台服务器条目还没有关联机器，请先改挂到机器或到新增流程补网络位置。")
		return
	}
	device := detail.DeviceName
	network := strings.TrimSpace(r.PostFormValue("network"))
	address := strings.TrimSpace(r.PostFormValue("address"))
	source := TopologyWriteSource{RemoteAddress: remoteIP(r)}
	var err error
	switch action {
	case "add":
		if network == "" || address == "" {
			h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "请选择网络并填写地址。")
			return
		}
		_, err = h.cfg.TopologyWriter.AddNetworkMembership(device, network, address, source)
	case "edit":
		if network == "" || address == "" {
			h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "请选择网络并填写地址。")
			return
		}
		_, err = h.cfg.TopologyWriter.EditNetworkMembership(device, network, address, source)
	default:
		if network == "" {
			h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "请选择要移除的网络。")
			return
		}
		_, err = h.cfg.TopologyWriter.RemoveNetworkMembership(device, network, source)
	}
	if err != nil {
		h.renderServerDetailWriteError(w, r, detail, serverDetailWriteStatus(err), serverDetailWriteMessage(err))
		return
	}
	notice := "address-saved"
	if action == "remove" {
		notice = "address-removed"
	}
	serverDetailWriteRedirect(w, r, detail.ID, notice)
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
		h.renderServerDetailWriteError(w, r, detail, http.StatusBadRequest, "请选择目标机器。")
		return
	}
	_, err := h.cfg.TopologyWriter.MoveServerToDevice(
		detail.ID, device, TopologyWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderServerDetailWriteError(w, r, detail, serverDetailWriteStatus(err), serverDetailWriteMessage(err))
		return
	}
	serverDetailWriteRedirect(w, r, detail.ID, "server-moved")
}

// handleServerDetailTest 启动一次连接测试并进入既有结果页；结果页会显示
// 实际使用的网络指称与真实握手地址。
func (h *handler) handleServerDetailTest(w http.ResponseWriter, r *http.Request, id string) {
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
		h.renderServerDetailWriteError(w, r, detail, http.StatusInternalServerError, "连接测试未启动，请稍后重试。")
		return
	}
	target := url.Values{"server": {detail.ID}, "task": {taskID}}
	http.Redirect(w, r, serverAddResultPath+"?"+target.Encode(), http.StatusSeeOther)
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
