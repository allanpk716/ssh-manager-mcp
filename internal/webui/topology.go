package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 拓扑图页面与内部 JSON 接口的路径。JSON 接口只供拓扑图页面使用，不对外、
// 不承诺兼容；写接口只供拓扑图页面的拖动归类与网络成员编辑使用。
const (
	topologyPath    = "/ui/topology/"
	topologyAPIPath = "/ui/api/topology"

	topologyMembershipAddPath            = "/ui/api/topology/membership/add"
	topologyMembershipEditPath           = "/ui/api/topology/membership/edit"
	topologyMembershipRemovePath         = "/ui/api/topology/membership/remove"
	topologyMembershipMovePath           = "/ui/api/topology/membership/move"
	topologyMembershipRemovePhysicalPath = "/ui/api/topology/membership/remove-physical"
	topologyNetworkAddPath               = "/ui/api/topology/network/add"
	topologyProbeOverlayPath             = "/ui/api/topology/probe/overlay"
	topologyProbeStartPath               = "/ui/api/topology/probe/start"
	topologyProbeStatusPath              = "/ui/api/topology/probe/status"

	// topologyWriteMaxRequestBody 限制拓扑写请求的请求体；这里只有设备名、
	// 网络名与地址，不需要更大的内容。
	topologyWriteMaxRequestBody = 64 << 10
)

// TopologySite 是拓扑图里的一个站点，只携带只读呈现需要的字段。
type TopologySite struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
}

// TopologyNetwork 是拓扑图里的一个网络。Type 是 physical（物理局域网）
// 或 virtual（虚拟局域网）；物理局域网必须带所属站点。
type TopologyNetwork struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	SiteID        string `json:"site_id"`
	SiteName      string `json:"site_name"`
	Note          string `json:"note"`
	NetworkRange  string `json:"network_range"`
	BandwidthMbps int64  `json:"bandwidth_mbps"`
	Metered       bool   `json:"metered"`
}

// TopologyDeviceServer 是设备详情里的服务器条目摘要：名称、用户、端口与
// 主机密钥锚状态；不携带凭据与任何机密项。
type TopologyDeviceServer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	User   string `json:"user"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Pinned bool   `json:"pinned"`
}

// TopologySharedStorage 是设备作为提供方时的一处共享存储摘要。共享存储
// 详情页由共享存储规格提供，未合入前只显示名称。
type TopologySharedStorage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TopologyCandidate 是按当前起点排序后的一个候选 SSH 地址。Source 是
// physical（共同物理局域网）、virtual（共同虚拟局域网）或 fallback（服
// 务器条目兜底主机）。
type TopologyCandidate struct {
	Address     string `json:"address"`
	NetworkName string `json:"network_name,omitempty"`
	Source      string `json:"source"`
	ServerName  string `json:"server_name,omitempty"`
}

// TopologyDevice 是拓扑图上的一台设备。Kind 是 server（有关联服务器条
// 目）、client（关联客户端实例）或 unmanaged（不受管设备）。
type TopologyDevice struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Note           string                  `json:"note"`
	Kind           string                  `json:"kind"`
	ClientName     string                  `json:"client_name,omitempty"`
	Servers        []TopologyDeviceServer  `json:"servers"`
	SharedStorages []TopologySharedStorage `json:"shared_storages"`
	Candidates     []TopologyCandidate     `json:"candidates"`
	// CandidateNotice 非空时说明没有共同网络候选，只能看兜底主机；文案
	// 由数据来源生成，页面原样显示。
	CandidateNotice string `json:"candidate_notice,omitempty"`
}

// TopologyMembership 是一台设备在一个网络里的成员行；登记顺序沿用存储
// 里的全表单调递增序号。
type TopologyMembership struct {
	DeviceID          string `json:"device_id"`
	NetworkID         string `json:"network_id"`
	Address           string `json:"address"`
	RegistrationOrder int64  `json:"registration_order"`
}

// TopologyLayoutCoordinate 是既有布局坐标里的一行。容器坐标随布局保存票
// 落地；本结构只承载当前已经存在的设备坐标。
type TopologyLayoutCoordinate struct {
	ViewKey  string  `json:"view_key"`
	DeviceID string  `json:"device_id"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
}

// TopologyLocalDevice 是运行时解析的本机设备。拓扑数据本身不设「权威端所
// 在设备」标记；Known 与 Reason 只来自本机状态文件的解析结果。
type TopologyLocalDevice struct {
	Known      bool   `json:"known"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// TopologyData 是拓扑图页面一次读取到的全部只读数据。
type TopologyData struct {
	Sites             []TopologySite             `json:"sites"`
	Networks          []TopologyNetwork          `json:"networks"`
	Devices           []TopologyDevice           `json:"devices"`
	Memberships       []TopologyMembership       `json:"memberships"`
	LayoutCoordinates []TopologyLayoutCoordinate `json:"layout_coordinates"`
	LocalDevice       TopologyLocalDevice        `json:"local_device"`
}

// TopologySource 是 Web UI 读取拓扑数据的唯一入口。instance 是客户端缓存
// 实例名，空串表示默认实例；单机与权威端来源忽略它。
type TopologySource interface {
	Topology(instance string) (*TopologyData, error)
}

// TopologyNameNotFoundError 表示写请求里的设备、网络或站点不存在；接口
// 按 404 返回，其余校验错误按 400 返回。
type TopologyNameNotFoundError struct {
	Message string
}

func (e *TopologyNameNotFoundError) Error() string { return e.Message }

// TopologyWriter 是 Web UI 修改网络拓扑成员与新建物理局域网的唯一入口。
// 单机与权威端由拓扑来源适配层实现；客户端不提供，写路由也不注册。每个
// 方法返回可直接显示的中文提示，错误提示与命令行等价面一致。
type TopologyWriter interface {
	AddNetworkMembership(device, network, address string) (string, error)
	EditNetworkMembership(device, network, address string) (string, error)
	RemoveNetworkMembership(device, network string) (string, error)
	MoveNetworkMembership(device, fromNetwork, toNetwork, address string) (string, error)
	RemovePhysicalNetworkMemberships(device string) (string, error)
	AddPhysicalNetwork(name, site string) (string, error)
}

// registerTopologyRoutes 注册拓扑图页面与只读数据接口。没有拓扑来源的构
// 造（例如初始化向导）不注册任何路由，导航也不显示入口。
func (h *handler) registerTopologyRoutes() {
	if h.cfg.Topology == nil {
		return
	}
	h.mux.HandleFunc(topologyPath, h.requireSession(h.handleTopologyPage))
	h.mux.HandleFunc(topologyAPIPath, h.requireSession(h.handleTopologyData))
	if h.cfg.TopologyProbe != nil {
		h.mux.HandleFunc(topologyProbeOverlayPath, h.requireSession(h.handleTopologyProbeOverlay))
		h.mux.HandleFunc(topologyProbeStartPath, h.requireSession(h.handleTopologyProbeStart))
		h.mux.HandleFunc(topologyProbeStatusPath, h.requireSession(h.handleTopologyProbeStatus))
	}
	if h.cfg.TopologyWriter == nil {
		return
	}
	h.mux.HandleFunc(topologyMembershipAddPath, h.requireSession(h.handleTopologyMembershipAdd))
	h.mux.HandleFunc(topologyMembershipEditPath, h.requireSession(h.handleTopologyMembershipEdit))
	h.mux.HandleFunc(topologyMembershipRemovePath, h.requireSession(h.handleTopologyMembershipRemove))
	h.mux.HandleFunc(topologyMembershipMovePath, h.requireSession(h.handleTopologyMembershipMove))
	h.mux.HandleFunc(topologyMembershipRemovePhysicalPath, h.requireSession(h.handleTopologyMembershipRemovePhysical))
	h.mux.HandleFunc(topologyNetworkAddPath, h.requireSession(h.handleTopologyNetworkAdd))
}

// handleTopologyPage 渲染网络拓扑页面。页面数据由浏览器脚本从只读接口读
// 取；模板不内嵌脚本与样式。
func (h *handler) handleTopologyPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "网络拓扑页只接受 GET 请求。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "topology.tmpl", pageData{Title: "网络拓扑", TopologyPage: true})
}

// handleTopologyData 返回拓扑图使用的只读 JSON。接口要求会话；非 GET 请求
// 在进入这里之前已经过来源与防跨站请求伪造校验。
func (h *handler) handleTopologyData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "拓扑数据接口只接受 GET 请求。")
		return
	}
	data, err := h.cfg.Topology.Topology(h.requestClientInstance(r))
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusInternalServerError, "读取网络拓扑失败，请稍后重试。")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	_ = encoder.Encode(data)
}

// writeTopologyJSONError 给内部接口写 JSON 形式的中文错误。
func (h *handler) writeTopologyJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// topologyWriteRequest 是写接口的请求体；字段全部必填，由各处理器按自己
// 的路径裁剪需要哪些。
type topologyWriteRequest struct {
	Device      string `json:"device"`
	Network     string `json:"network"`
	FromNetwork string `json:"from_network"`
	ToNetwork   string `json:"to_network"`
	Address     string `json:"address"`
	Name        string `json:"name"`
	Site        string `json:"site"`
}

// decodeTopologyWriteRequest 解析写请求体并去掉各字段前后的空白。
func decodeTopologyWriteRequest(r *http.Request) (*topologyWriteRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, topologyWriteMaxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("读取请求内容失败：%w", err)
	}
	if len(body) > topologyWriteMaxRequestBody {
		return nil, errors.New("请求内容过大")
	}
	var request topologyWriteRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, errors.New("请求内容不是合法的 JSON 对象。")
	}
	request.Device = strings.TrimSpace(request.Device)
	request.Network = strings.TrimSpace(request.Network)
	request.FromNetwork = strings.TrimSpace(request.FromNetwork)
	request.ToNetwork = strings.TrimSpace(request.ToNetwork)
	request.Address = strings.TrimSpace(request.Address)
	request.Name = strings.TrimSpace(request.Name)
	request.Site = strings.TrimSpace(request.Site)
	return &request, nil
}

// writeTopologyResult 执行一次写操作并把结果换成 JSON；名称不存在按 404，
// 其余校验错误按 400，成功返回写来源给出的中文提示。
func (h *handler) writeTopologyResult(w http.ResponseWriter, run func() (string, error)) {
	message, err := run()
	if err != nil {
		var notFound *TopologyNameNotFoundError
		switch {
		case errors.As(err, &notFound):
			h.writeTopologyJSONError(w, http.StatusNotFound, notFound.Message)
		default:
			h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// handleTopologyMembershipAdd 把设备加入网络并登记地址。
func (h *handler) handleTopologyMembershipAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "网络成员新增接口只接受 POST 请求。")
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Device == "" || request.Network == "" || request.Address == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "设备、网络与地址都不能为空。")
		return
	}
	h.writeTopologyResult(w, func() (string, error) {
		return h.cfg.TopologyWriter.AddNetworkMembership(request.Device, request.Network, request.Address)
	})
}

// handleTopologyMembershipEdit 修改设备在一个网络里的地址。
func (h *handler) handleTopologyMembershipEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "网络成员地址修改接口只接受 POST 请求。")
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Device == "" || request.Network == "" || request.Address == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "设备、网络与地址都不能为空。")
		return
	}
	h.writeTopologyResult(w, func() (string, error) {
		return h.cfg.TopologyWriter.EditNetworkMembership(request.Device, request.Network, request.Address)
	})
}

// handleTopologyMembershipRemove 把设备移出一个网络。
func (h *handler) handleTopologyMembershipRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "网络成员退出接口只接受 POST 请求。")
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Device == "" || request.Network == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "设备与网络都不能为空。")
		return
	}
	h.writeTopologyResult(w, func() (string, error) {
		return h.cfg.TopologyWriter.RemoveNetworkMembership(request.Device, request.Network)
	})
}

// handleTopologyMembershipMove 把设备的网络成员从一个网络移动到另一个网
// 络并登记新地址；「同时加入」走新增接口，不经过这里。
func (h *handler) handleTopologyMembershipMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "网络成员移动接口只接受 POST 请求。")
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Device == "" || request.FromNetwork == "" || request.ToNetwork == "" || request.Address == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "设备、原网络、目标网络与地址都不能为空。")
		return
	}
	h.writeTopologyResult(w, func() (string, error) {
		return h.cfg.TopologyWriter.MoveNetworkMembership(request.Device, request.FromNetwork, request.ToNetwork, request.Address)
	})
}

// handleTopologyMembershipRemovePhysical 删除设备的全部物理局域网成员；
// 虚拟局域网成员保留。
func (h *handler) handleTopologyMembershipRemovePhysical(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "物理局域网成员删除接口只接受 POST 请求。")
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Device == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "设备不能为空。")
		return
	}
	h.writeTopologyResult(w, func() (string, error) {
		return h.cfg.TopologyWriter.RemovePhysicalNetworkMemberships(request.Device)
	})
}

// handleTopologyNetworkAdd 在拖动归类流程里新建一个物理局域网；只需要网
// 络名与所属站点，其余网络属性仍走命令行。
func (h *handler) handleTopologyNetworkAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "物理局域网新建接口只接受 POST 请求。")
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Name == "" || request.Site == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "网络名与所属站点都不能为空。")
		return
	}
	h.writeTopologyResult(w, func() (string, error) {
		return h.cfg.TopologyWriter.AddPhysicalNetwork(request.Name, request.Site)
	})
}
