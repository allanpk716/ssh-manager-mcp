package webui

import (
	"net/http"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/shareaddress"
)

// ShareDesktopDeviceOption 是桌面手动添加引导里的一台可选拓扑设备。
type ShareDesktopDeviceOption struct {
	ID   string
	Name string
}

// ShareDesktopDirectoryPath 是按 Windows 写法展开的一条目录约定访问路径。
type ShareDesktopDirectoryPath struct {
	Purpose string
	Path    string
}

// ShareDesktopGuideView 是共享存储桌面手动添加引导的页面数据。这里只读
// 拓扑与共享登记，不读取机密项值，也没有任何密码入口。
type ShareDesktopGuideView struct {
	Detail                *ShareRow
	DeviceOptions         []ShareDesktopDeviceOption
	SelectedDeviceID      string
	DesktopNotInTopology  bool
	Plan                  shareaddress.DesktopAddressPlan
	WindowsDirectoryPaths []ShareDesktopDirectoryPath
	Error                 string
}

func (h *handler) handleShareDesktopGuide(w http.ResponseWriter, r *http.Request, identifier string) {
	if h.cfg.Role == RoleClient || h.cfg.Topology == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "桌面手动添加引导只接受 GET 请求。")
		return
	}
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	topology, err := h.cfg.Topology.Topology(h.requestClientInstance(r), TopologyProbeOriginSelf)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取网络拓扑失败", "读取网络拓扑失败，请稍后重试。")
		return
	}

	view := ShareDesktopGuideView{Detail: detail}
	deviceIDsByName := make(map[string]string, len(topology.Devices))
	for _, device := range topology.Devices {
		deviceIDsByName[device.Name] = device.ID
	}
	networksByID := make(map[string]TopologyNetwork, len(topology.Networks))
	for _, network := range topology.Networks {
		networksByID[network.ID] = network
	}
	membershipsByDevice := make(map[string][]*models.NetworkMembership)
	for _, membership := range topology.Memberships {
		model := &models.NetworkMembership{
			DeviceID: membership.DeviceID, NetworkID: membership.NetworkID,
			Address: membership.Address, RegistrationOrder: membership.RegistrationOrder,
		}
		if network, ok := networksByID[membership.NetworkID]; ok {
			model.NetworkName = network.Name
			model.NetworkType = models.NetworkType(network.Type)
		}
		membershipsByDevice[membership.DeviceID] = append(membershipsByDevice[membership.DeviceID], model)
	}
	for _, device := range topology.Devices {
		view.DeviceOptions = append(view.DeviceOptions, ShareDesktopDeviceOption{ID: device.ID, Name: device.Name})
	}

	view.SelectedDeviceID = strings.TrimSpace(r.URL.Query().Get("device"))
	view.DesktopNotInTopology = view.SelectedDeviceID == "none"
	var desktopMemberships []*models.NetworkMembership
	if view.SelectedDeviceID != "" && !view.DesktopNotInTopology {
		found := false
		for _, device := range topology.Devices {
			if device.ID == view.SelectedDeviceID {
				found = true
				break
			}
		}
		if !found {
			view.Error = "选择的拓扑设备不存在，请重新选择。"
			h.renderShareDesktopGuide(w, r, http.StatusBadRequest, &view)
			return
		}
		desktopMemberships = membershipsByDevice[view.SelectedDeviceID]
	}

	providerDeviceID, providerFound := deviceIDsByName[detail.DeviceName]
	providerMemberships := membershipsByDevice[providerDeviceID]
	switch {
	case view.SelectedDeviceID == "":
		view.Plan.Reason = "请先选择这台电脑对应的拓扑设备；如果不能确定，可选择「这台电脑不在拓扑里」。"
	case !providerFound:
		view.Plan.Reason = "共享存储提供方设备不在网络拓扑里，无法推算地址。"
	default:
		view.Plan = shareaddress.PlanDesktopAddresses(
			providerMemberships, desktopMemberships, !view.DesktopNotInTopology,
		)
	}
	if view.Plan.RecommendedAddress != nil {
		view.WindowsDirectoryPaths = windowsDirectoryPaths(
			view.Plan.RecommendedAddress.Address, detail.Name, detail.Directories,
		)
	}
	h.renderShareDesktopGuide(w, r, http.StatusOK, &view)
}

func windowsDirectoryPaths(
	address, shareName string, directories []ShareDirectoryView,
) []ShareDesktopDirectoryPath {
	paths := make([]ShareDesktopDirectoryPath, 0, len(directories))
	for _, directory := range directories {
		subPath := strings.ReplaceAll(directory.Path, "/", `\`)
		paths = append(paths, ShareDesktopDirectoryPath{
			Purpose: directory.Purpose,
			Path:    `\\` + address + `\` + shareName + `\` + subPath,
		})
	}
	return paths
}

func (h *handler) renderShareDesktopGuide(
	w http.ResponseWriter, r *http.Request, status int, view *ShareDesktopGuideView,
) {
	h.renderPage(w, r, status, "share-desktop.tmpl", pageData{
		Title: "桌面手动添加 " + view.Detail.Name, ShareDesktopGuide: view,
	})
}
