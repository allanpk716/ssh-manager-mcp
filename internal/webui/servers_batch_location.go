package webui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// serverBatchLocationPath 是服务器列表批量设置地点的后果确认页与提交处
// 理器；GET 渲染确认页，POST 在一次事务里写完全部所涉设备。
const serverBatchLocationPath = "/ui/servers/batch/location"

// serverBatchLocationOpenParam 是确认页「上一步」带回列表的参数；列表脚
// 本看到它就重新打开地点选择弹层。
const serverBatchLocationOpenParam = "batch_location"

// ServerBatchLocationDeviceView 是后果确认页里一台将被修改地点的机器。
type ServerBatchLocationDeviceView struct {
	Name             string
	CurrentLocation  string
	OtherServersText string
}

// ServerBatchLocationUnassociatedView 是一个没有关联机器的选中服务器条
// 目；它不参与写入，也不阻塞其他条目提交。
type ServerBatchLocationUnassociatedView struct {
	ID   string
	Name string
}

// ServerBatchLocationView 是批量设置地点后果确认页的展示数据。Error 非空
// 时页面只显示错误与返回链接，不再渲染确认表单。
type ServerBatchLocationView struct {
	AffectedCount int
	Target        string
	SiteValue     string
	Servers       []string
	Devices       []ServerBatchLocationDeviceView
	Unassociated  []ServerBatchLocationUnassociatedView
	ReturnTo      string
	BackHref      string
	Error         string
	// siteRef 是交给拓扑写入口的地点引用；选择「未登记地点」时为空字符
	// 串，表示清空所涉设备的地点。
	siteRef string
}

// handleServerBatchLocation 分发批量设置地点的确认页与提交。
func (h *handler) handleServerBatchLocation(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Topology == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		view, message, status := h.serverBatchLocationView(r, r.URL.Query())
		h.renderServerBatchLocation(w, r, status, view, message)
	case http.MethodPost:
		h.handleServerBatchLocationPost(w, r)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "批量设置地点只接受 GET 或 POST 请求。")
	}
}

// serverBatchLocationView 按选中的服务器条目与目标地点组装后果确认数
// 据。返回的提示非空时表示请求内容不合法或读取失败，此时不应写入。
func (h *handler) serverBatchLocationView(
	r *http.Request, values url.Values,
) (*ServerBatchLocationView, string, int) {
	view := &ServerBatchLocationView{
		SiteValue: strings.TrimSpace(values.Get("site")),
		ReturnTo:  safeReturnPath(values.Get("return_to"), serversPrefix, serversPrefix),
		Servers:   dedupeServerBatchSelection(values["server"]),
	}
	view.BackHref = serverBatchLocationBackHref(view.ReturnTo)
	if len(view.Servers) == 0 {
		return view, "请先选择要设置地点的服务器。", http.StatusBadRequest
	}
	if view.SiteValue == "" {
		return view, "请先选择目标地点。", http.StatusBadRequest
	}
	rows, err := h.cfg.Servers.ServerList(h.requestClientInstance(r), false)
	if err != nil {
		h.logError(r, "批量设置地点读取服务器列表", err)
		return view, "读取服务器列表失败，请稍后重试。", http.StatusInternalServerError
	}
	rowsByID := make(map[string]ServerListEntry, len(rows))
	for _, row := range rows {
		rowsByID[row.ID] = row
	}
	topology, err := h.cfg.Topology.Topology(h.requestClientInstance(r), "")
	if err != nil {
		h.logError(r, "批量设置地点读取拓扑", err)
		return view, "读取地点数据失败，请稍后重试。", http.StatusInternalServerError
	}
	if view.SiteValue == serverListUnregisteredSite {
		view.Target = "未登记地点"
	} else {
		found := false
		for _, site := range topology.Sites {
			if site.LocationPath == view.SiteValue {
				found = true
				break
			}
		}
		if !found {
			return view, "目标地点不存在，请返回上一步重选。", http.StatusBadRequest
		}
		view.Target = view.SiteValue
		view.siteRef = view.SiteValue
	}
	deviceByServer := serverDeviceIndex(topology)
	selected := make(map[string]bool, len(view.Servers))
	for _, id := range view.Servers {
		if _, ok := rowsByID[id]; !ok {
			return view, "选中的服务器条目不存在，请刷新列表后重试。", http.StatusBadRequest
		}
		selected[id] = true
	}
	seenDevices := map[string]bool{}
	for _, id := range view.Servers {
		row, ok := rowsByID[id]
		if !ok {
			return view, "选中的服务器条目不存在，请刷新列表后重试。", http.StatusBadRequest
		}
		device, hasDevice := deviceByServer[id]
		if !hasDevice {
			view.Unassociated = append(view.Unassociated, ServerBatchLocationUnassociatedView{ID: id, Name: row.Name})
			continue
		}
		view.AffectedCount++
		if seenDevices[device.ID] {
			continue
		}
		seenDevices[device.ID] = true
		current := device.LocationPath
		if current == "" {
			current = "未登记地点"
		}
		var others []string
		for _, server := range device.Servers {
			if server.ID != "" && !selected[server.ID] {
				others = append(others, server.Name)
			}
		}
		view.Devices = append(view.Devices, ServerBatchLocationDeviceView{
			Name: device.Name, CurrentLocation: current,
			OtherServersText: strings.Join(others, "、"),
		})
	}
	if view.AffectedCount == 0 {
		return view, "选中的服务器条目都没有关联机器，无法设置地点。", http.StatusBadRequest
	}
	return view, "", http.StatusOK
}

// handleServerBatchLocationPost 处理确认提交：按设备去重后交给拓扑写入
// 口一次写入；失败时回到确认页显示原因，不出现成功跳转。
func (h *handler) handleServerBatchLocationPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "请求内容无法解析", "批量设置地点的请求内容无法解析，请返回上一步重试。")
		return
	}
	view, message, status := h.serverBatchLocationView(r, r.PostForm)
	if message != "" {
		h.renderServerBatchLocation(w, r, status, view, message)
		return
	}
	devices := make([]string, 0, len(view.Devices))
	for _, device := range view.Devices {
		devices = append(devices, device.Name)
	}
	err := h.cfg.TopologyWriter.EditDeviceSites(
		devices, view.siteRef, TopologyWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		var missing *TopologyNameNotFoundError
		if errors.As(err, &missing) {
			h.renderServerBatchLocation(w, r, http.StatusNotFound, view, missing.Message)
			return
		}
		var invalid *TopologyInvalidInputError
		if errors.As(err, &invalid) {
			h.renderServerBatchLocation(w, r, http.StatusBadRequest, view, invalid.Message)
			return
		}
		h.logError(r, "批量设置地点", err)
		h.renderServerBatchLocation(w, r, http.StatusInternalServerError, view, "批量设置地点失败，全部地点保持不变，请稍后重试。")
		return
	}
	http.Redirect(w, r, view.ReturnTo, http.StatusSeeOther)
}

func (h *handler) renderServerBatchLocation(
	w http.ResponseWriter, r *http.Request, status int, view *ServerBatchLocationView, message string,
) {
	view.Error = message
	h.renderPage(w, r, status, "servers_batch_location.tmpl", pageData{
		Title: "确认批量设置地点", ServerBatchLocation: view,
	})
}

// dedupeServerBatchSelection 去掉空白值与重复的服务器条目标识符，保持传
// 入顺序；设置地点与测试连接两个批量入口共用同一份选中清单规则。
func dedupeServerBatchSelection(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// serverBatchLocationBackHref 给「上一步」链接追加重新打开地点弹层的参
// 数，同时保留原列表的筛选条件。
func serverBatchLocationBackHref(returnTo string) string {
	separator := "?"
	if strings.Contains(returnTo, "?") {
		separator = "&"
	}
	return returnTo + separator + serverBatchLocationOpenParam + "=1"
}
