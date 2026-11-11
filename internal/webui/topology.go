package webui

import (
	"encoding/json"
	"net/http"
)

// 拓扑图页面与内部 JSON 接口的路径。JSON 接口只供拓扑图页面使用，不对外、
// 不承诺兼容；本票只有读取接口，写入接口随布局保存与归类票另行注册。
const (
	topologyPath    = "/ui/topology/"
	topologyAPIPath = "/ui/api/topology"
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

// registerTopologyRoutes 注册拓扑图页面与只读数据接口。没有拓扑来源的构
// 造（例如初始化向导）不注册任何路由，导航也不显示入口。
func (h *handler) registerTopologyRoutes() {
	if h.cfg.Topology == nil {
		return
	}
	h.mux.HandleFunc(topologyPath, h.requireSession(h.handleTopologyPage))
	h.mux.HandleFunc(topologyAPIPath, h.requireSession(h.handleTopologyData))
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
