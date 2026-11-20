package webui

import (
	"fmt"
	"net/http"
)

const (
	morePath              = "/ui/more/"
	locationsNetworksPath = "/ui/more/locations-networks/"
)

// moreView 是“更多”菜单页的展示数据：每组入口的右侧摘要在这里一次算
// 好，模板只负责排版；读取单项摘要失败时显示固定提示，不影响其余入口。
type moreView struct {
	SoftwareInventorySummary string
	LocationsNetworksSummary string
	TopologySummary          string
	BackupSummary            string
}

// registerMoreRoutes 注册“更多”菜单页与地点网络管理页。客户端只注册只读
// 入口；地点与网络的写路由在提供拓扑写来源时才注册。
func (h *handler) registerMoreRoutes() {
	h.mux.HandleFunc(morePath, h.requireSession(h.handleMorePage))
	h.registerLocationsNetworksRoutes()
}

// handleMorePage 渲染“更多”菜单。
func (h *handler) handleMorePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != morePath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "“更多”菜单只接受 GET 请求。")
		return
	}
	view, err := h.buildMoreView(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取菜单数据失败", "读取菜单数据失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "more.tmpl", pageData{Title: "更多", More: view})
}

// buildMoreView 汇总各菜单行的右侧摘要；单项来源失败只影响本行摘要。
func (h *handler) buildMoreView(r *http.Request) (*moreView, error) {
	view := &moreView{}
	if h.cfg.SoftwareInventory != nil && h.caps.ShowSoftwareInventoryArea {
		devices, err := h.cfg.SoftwareInventory.SoftwareInventoryDevices(SoftwareInventoryFilter{})
		switch {
		case err != nil:
			view.SoftwareInventorySummary = "读取失败"
		default:
			managed := 0
			for _, device := range devices {
				if device.Status.Managed {
					managed++
				}
			}
			view.SoftwareInventorySummary = fmt.Sprintf("%d 台设备 · %d 台受管", len(devices), managed)
		}
	}
	if h.cfg.Topology != nil && h.caps.ShowTopologyArea {
		data, err := h.cfg.Topology.Topology(h.requestClientInstance(r), "")
		switch {
		case err != nil:
			view.LocationsNetworksSummary = "读取失败"
		default:
			view.LocationsNetworksSummary = fmt.Sprintf("%d 个地点 · %d 个网络", len(data.Sites), len(data.Networks))
		}
		if h.caps.CanEditTopology {
			view.TopologySummary = "电脑可编辑"
		} else {
			view.TopologySummary = "仅查看"
		}
	}
	if h.cfg.Ops != nil && h.caps.CanManageBackups {
		status, err := h.cfg.Ops.BackupStatus()
		switch {
		case err != nil:
			view.BackupSummary = "读取失败"
		case status.HasLatest:
			view.BackupSummary = "最近备份：" + status.LatestTime.Format("2006-01-02 15:04")
		default:
			view.BackupSummary = "尚无备份"
		}
	}
	return view, nil
}
