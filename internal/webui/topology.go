package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// 拓扑图页面与内部 JSON 接口的路径。JSON 接口只供拓扑图页面使用，不对外、
// 不承诺兼容；写接口只供拓扑图页面的布局保存、拖动归类与网络成员编辑使
// 用，并且只给可写角色注册。
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

	topologyLayoutPath      = "/ui/api/topology/layout"
	topologyLayoutResetPath = "/ui/api/topology/layout/reset"

	// topologyLayoutMaxBytes 限制一次布局保存的请求体大小；三种视图的坐标
	// 数量以拓扑实体数量为上限，远小于这个值。
	topologyLayoutMaxBytes = 1 << 20

	topologyViewPhysical = store.LayoutViewPhysical
	topologyViewVirtual  = store.LayoutViewVirtual
	topologyViewOverlay  = store.LayoutViewOverlay

	// TopologyProbeOriginSelf 表示以本机设备作为探测或候选地址起点。
	TopologyProbeOriginSelf = "self"
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

// TopologyLayoutCoordinate 是布局坐标里的一台设备坐标。
type TopologyLayoutCoordinate struct {
	ViewKey  string  `json:"view_key"`
	DeviceID string  `json:"device_id"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
}

// TopologyContainerLayoutCoordinate 是布局坐标里的一个站点区域或网络分组
// 框坐标。EntityType 是 site 或 network。
type TopologyContainerLayoutCoordinate struct {
	ViewKey    string  `json:"view_key"`
	EntityType string  `json:"entity_type"`
	EntityID   string  `json:"entity_id"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
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
	Sites                      []TopologySite                      `json:"sites"`
	Networks                   []TopologyNetwork                   `json:"networks"`
	Devices                    []TopologyDevice                    `json:"devices"`
	Memberships                []TopologyMembership                `json:"memberships"`
	LayoutCoordinates          []TopologyLayoutCoordinate          `json:"layout_coordinates"`
	ContainerLayoutCoordinates []TopologyContainerLayoutCoordinate `json:"container_layout_coordinates"`
	LocalDevice                TopologyLocalDevice                 `json:"local_device"`
}

// TopologySource 是 Web UI 读取拓扑数据的唯一入口。instance 是客户端缓存
// 实例名，空串表示默认实例；单机与权威端来源忽略它。
type TopologySource interface {
	Topology(instance, from string) (*TopologyData, error)
}

// TopologyNameNotFoundError 表示写请求里的设备、网络或站点不存在；接口
// 按 404 返回，其余校验错误按 400 返回。
type TopologyNameNotFoundError struct {
	Message string
}

func (e *TopologyNameNotFoundError) Error() string { return e.Message }

// TopologyInvalidInputError 表示用户输入或请求内容不合法；接口按 400 返回
// 其中的中文提示。
type TopologyInvalidInputError struct {
	Message string
}

func (e *TopologyInvalidInputError) Error() string { return e.Message }

// TopologyWriteSource 描述一次拓扑图写请求的来源地址，适配层把它写进审计行。
type TopologyWriteSource struct {
	RemoteAddress string
}

// ErrTopologyLayoutInvalid 表示布局请求引用了不存在的实体或内容非法；处理
// 器把它换算成面向浏览器的客户端错误。
var ErrTopologyLayoutInvalid = errors.New("布局请求内容无效")

// TopologyWriter 是 Web UI 修改网络拓扑的唯一入口：布局保存与重置、网络
// 成员编辑与新建物理局域网。单机与权威端由拓扑来源适配层实现；客户端不
// 提供，写路由也不注册。网络成员方法返回可直接显示的中文提示，错误提示
// 与命令行等价面一致。
type TopologyWriter interface {
	AddNetworkMembership(device, network, address string, source TopologyWriteSource) (string, error)
	EditNetworkMembership(device, network, address string, source TopologyWriteSource) (string, error)
	RemoveNetworkMembership(device, network string, source TopologyWriteSource) (string, error)
	MoveNetworkMembership(device, fromNetwork, toNetwork, address string, source TopologyWriteSource) (string, error)
	RemovePhysicalNetworkMemberships(device string, source TopologyWriteSource) (string, error)
	AddPhysicalNetwork(name, site string, source TopologyWriteSource) (string, error)
	// SaveTopologyLayout 保存一个视图的完整布局并写一条来源为 Web UI 的审计。
	SaveTopologyLayout(save TopologyLayoutSave, source TopologyWriteSource) error
	// ResetTopologyLayout 清空一个视图保存的全部坐标并写一条来源为 Web UI
	// 的审计。
	ResetTopologyLayout(view string, source TopologyWriteSource) error
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
	h.mux.HandleFunc(topologyLayoutPath, h.requireSession(h.handleTopologyLayoutSave))
	h.mux.HandleFunc(topologyLayoutResetPath, h.requireSession(h.handleTopologyLayoutReset))
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
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	if from == "" {
		from = TopologyProbeOriginSelf
	}
	if h.cfg.Role == RoleClient && from != TopologyProbeOriginSelf {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "客户端只能以本机为起点查看网络拓扑。")
		return
	}
	data, err := h.cfg.Topology.Topology(h.requestClientInstance(r), from)
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

// topologyWriteRequest 是网络成员与网络写接口共用的一组可选字段；各处理
// 器只校验并使用自己路径需要的字段。
type topologyWriteRequest struct {
	Device      string `json:"device"`
	Network     string `json:"network"`
	FromNetwork string `json:"from_network"`
	ToNetwork   string `json:"to_network"`
	Address     string `json:"address"`
	Name        string `json:"name"`
	Site        string `json:"site"`
}

var (
	errTopologyBodyRead       = errors.New("读取请求内容失败。")
	errTopologyBodyInvalid    = errors.New("请求内容不是合法的 JSON 对象。")
	errTopologyBodyTooLarge   = errors.New("请求内容过大")
	errTopologyLayoutInvalid  = errors.New("请求体不是合法的布局 JSON")
	errTopologyLayoutTooLarge = errors.New("布局请求内容过大")
)

// decodeTopologyJSONBody 读取并解析受长度限制的拓扑 JSON 请求体。
func decodeTopologyJSONBody(r *http.Request, maxBytes int, payload any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxBytes)+1))
	if err != nil {
		return errTopologyBodyRead
	}
	if len(body) > maxBytes {
		return errTopologyBodyTooLarge
	}
	if err := json.Unmarshal(body, payload); err != nil {
		return errTopologyBodyInvalid
	}
	return nil
}

// writeTopologyWriteJSONRequestError 把成员、网络与探测写请求的解析错误映
// 射为状态码；请求超过限制时统一返回 413。
func (h *handler) writeTopologyWriteJSONRequestError(w http.ResponseWriter, err error) {
	if errors.Is(err, errTopologyBodyTooLarge) {
		h.writeTopologyJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
}

// decodeTopologyWriteRequest 解析写请求体并去掉各字段前后的空白。
func decodeTopologyWriteRequest(r *http.Request) (*topologyWriteRequest, error) {
	var request topologyWriteRequest
	if err := decodeTopologyJSONBody(r, topologyWriteMaxRequestBody, &request); err != nil {
		return nil, err
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
// 用户输入错误按 400，其余错误按 500 并写日志。
func (h *handler) writeTopologyResult(w http.ResponseWriter, r *http.Request, run func() (string, error)) {
	message, err := run()
	if err != nil {
		var notFound *TopologyNameNotFoundError
		var invalid *TopologyInvalidInputError
		switch {
		case errors.As(err, &notFound):
			h.writeTopologyJSONError(w, http.StatusNotFound, notFound.Message)
		case errors.As(err, &invalid):
			h.writeTopologyJSONError(w, http.StatusBadRequest, invalid.Message)
		default:
			h.logError(r, "保存网络拓扑失败", err)
			h.writeTopologyJSONError(w, http.StatusInternalServerError, "保存网络拓扑失败，请稍后重试。")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// topologyWriteOperation 描述一个网络成员或网络写接口的差异部分；方法、
// 请求体解析与错误形态由公共处理器统一处理。
type topologyWriteOperation struct {
	methodNotAllowed string
	missingFields    string
	hasFields        func(request *topologyWriteRequest) bool
	run              func(request *topologyWriteRequest, source TopologyWriteSource) (string, error)
}

// handleTopologyWriteOperation 是六个成员与网络写接口的公共骨架。
func (h *handler) handleTopologyWriteOperation(
	w http.ResponseWriter, r *http.Request, operation topologyWriteOperation,
) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, operation.methodNotAllowed)
		return
	}
	request, err := decodeTopologyWriteRequest(r)
	if err != nil {
		h.writeTopologyWriteJSONRequestError(w, err)
		return
	}
	if !operation.hasFields(request) {
		h.writeTopologyJSONError(w, http.StatusBadRequest, operation.missingFields)
		return
	}
	source := TopologyWriteSource{RemoteAddress: remoteIP(r)}
	h.writeTopologyResult(w, r, func() (string, error) { return operation.run(request, source) })
}

// handleTopologyMembershipAdd 把设备加入网络并登记地址。
func (h *handler) handleTopologyMembershipAdd(w http.ResponseWriter, r *http.Request) {
	h.handleTopologyWriteOperation(w, r, topologyWriteOperation{
		methodNotAllowed: "网络成员新增接口只接受 POST 请求。",
		missingFields:    "设备、网络与地址都不能为空。",
		hasFields: func(request *topologyWriteRequest) bool {
			return request.Device != "" && request.Network != "" && request.Address != ""
		},
		run: func(request *topologyWriteRequest, source TopologyWriteSource) (string, error) {
			return h.cfg.TopologyWriter.AddNetworkMembership(request.Device, request.Network, request.Address, source)
		},
	})
}

// handleTopologyMembershipEdit 修改设备在一个网络里的地址。
func (h *handler) handleTopologyMembershipEdit(w http.ResponseWriter, r *http.Request) {
	h.handleTopologyWriteOperation(w, r, topologyWriteOperation{
		methodNotAllowed: "网络成员地址修改接口只接受 POST 请求。",
		missingFields:    "设备、网络与地址都不能为空。",
		hasFields: func(request *topologyWriteRequest) bool {
			return request.Device != "" && request.Network != "" && request.Address != ""
		},
		run: func(request *topologyWriteRequest, source TopologyWriteSource) (string, error) {
			return h.cfg.TopologyWriter.EditNetworkMembership(request.Device, request.Network, request.Address, source)
		},
	})
}

// handleTopologyMembershipRemove 把设备移出一个网络。
func (h *handler) handleTopologyMembershipRemove(w http.ResponseWriter, r *http.Request) {
	h.handleTopologyWriteOperation(w, r, topologyWriteOperation{
		methodNotAllowed: "网络成员退出接口只接受 POST 请求。",
		missingFields:    "设备与网络都不能为空。",
		hasFields: func(request *topologyWriteRequest) bool {
			return request.Device != "" && request.Network != ""
		},
		run: func(request *topologyWriteRequest, source TopologyWriteSource) (string, error) {
			return h.cfg.TopologyWriter.RemoveNetworkMembership(request.Device, request.Network, source)
		},
	})
}

// handleTopologyMembershipMove 把设备的网络成员从一个网络移动到另一个网
// 络并登记新地址；「同时加入」走新增接口，不经过这里。
func (h *handler) handleTopologyMembershipMove(w http.ResponseWriter, r *http.Request) {
	h.handleTopologyWriteOperation(w, r, topologyWriteOperation{
		methodNotAllowed: "网络成员移动接口只接受 POST 请求。",
		missingFields:    "设备、原网络、目标网络与地址都不能为空。",
		hasFields: func(request *topologyWriteRequest) bool {
			return request.Device != "" && request.FromNetwork != "" && request.ToNetwork != "" && request.Address != ""
		},
		run: func(request *topologyWriteRequest, source TopologyWriteSource) (string, error) {
			return h.cfg.TopologyWriter.MoveNetworkMembership(
				request.Device, request.FromNetwork, request.ToNetwork, request.Address, source)
		},
	})
}

// handleTopologyMembershipRemovePhysical 删除设备的全部物理局域网成员；
// 虚拟局域网成员保留。
func (h *handler) handleTopologyMembershipRemovePhysical(w http.ResponseWriter, r *http.Request) {
	h.handleTopologyWriteOperation(w, r, topologyWriteOperation{
		methodNotAllowed: "物理局域网成员删除接口只接受 POST 请求。",
		missingFields:    "设备不能为空。",
		hasFields:        func(request *topologyWriteRequest) bool { return request.Device != "" },
		run: func(request *topologyWriteRequest, source TopologyWriteSource) (string, error) {
			return h.cfg.TopologyWriter.RemovePhysicalNetworkMemberships(request.Device, source)
		},
	})
}

// handleTopologyNetworkAdd 在拖动归类流程里新建一个物理局域网；只需要网
// 络名与所属站点，其余网络属性仍走命令行。
func (h *handler) handleTopologyNetworkAdd(w http.ResponseWriter, r *http.Request) {
	h.handleTopologyWriteOperation(w, r, topologyWriteOperation{
		methodNotAllowed: "物理局域网新建接口只接受 POST 请求。",
		missingFields:    "网络名与所属站点都不能为空。",
		hasFields: func(request *topologyWriteRequest) bool {
			return request.Name != "" && request.Site != ""
		},
		run: func(request *topologyWriteRequest, source TopologyWriteSource) (string, error) {
			return h.cfg.TopologyWriter.AddPhysicalNetwork(request.Name, request.Site, source)
		},
	})
}

// TopologyLayoutSave 是一次布局保存请求的载荷：一个视图的全部设备坐标与
// 站点区域或网络分组框坐标，保存时整体替换该视图。
type TopologyLayoutSave struct {
	ViewKey    string                              `json:"view"`
	Devices    []TopologyLayoutCoordinate          `json:"devices,omitempty"`
	Containers []TopologyContainerLayoutCoordinate `json:"containers,omitempty"`
}

// topologyLayoutResetRequest 是布局重置请求的载荷。
type topologyLayoutResetRequest struct {
	ViewKey string `json:"view"`
}

// validTopologyView 判断视图键是否属于拓扑图页面的三种视图。
func validTopologyView(view string) bool {
	return view == topologyViewPhysical || view == topologyViewVirtual || view == topologyViewOverlay
}

// decodeTopologyLayoutBody 解析并做处理器层校验。引用是否存在由写来源在存
// 储事务里判断。
func decodeTopologyLayoutBody(r *http.Request, payload any) error {
	if err := decodeTopologyJSONBody(r, topologyLayoutMaxBytes, payload); err != nil {
		switch {
		case errors.Is(err, errTopologyBodyTooLarge):
			return errTopologyLayoutTooLarge
		case errors.Is(err, errTopologyBodyRead), errors.Is(err, errTopologyBodyInvalid):
			return errTopologyLayoutInvalid
		default:
			return err
		}
	}
	return nil
}

// topologyLayoutSaveRequest 是布局保存请求的线上形态。横纵坐标用指针区
// 分「没给」与「值为 0」，两者都是非法请求与合法坐标的区别。
type topologyLayoutSaveRequest struct {
	ViewKey    string                           `json:"view"`
	Devices    []topologyLayoutDeviceRequest    `json:"devices"`
	Containers []topologyLayoutContainerRequest `json:"containers"`
}

type topologyLayoutDeviceRequest struct {
	DeviceID string   `json:"device_id"`
	X        *float64 `json:"x"`
	Y        *float64 `json:"y"`
}

type topologyLayoutContainerRequest struct {
	EntityType string   `json:"entity_type"`
	EntityID   string   `json:"entity_id"`
	X          *float64 `json:"x"`
	Y          *float64 `json:"y"`
}

// handleTopologyLayoutSave 处理一次布局保存。防跨站请求伪造校验在进入处理
// 器之前已经完成；这里只接受 JSON 请求体。
func (h *handler) handleTopologyLayoutSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "布局保存接口只接受 POST 请求。")
		return
	}
	var request topologyLayoutSaveRequest
	if err := decodeTopologyLayoutBody(r, &request); err != nil {
		h.writeTopologyLayoutJSONRequestError(w, err)
		return
	}
	if !validTopologyView(request.ViewKey) {
		h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局视图只能是物理视图、虚拟视图或叠加视图。")
		return
	}
	save := TopologyLayoutSave{
		ViewKey:    request.ViewKey,
		Devices:    make([]TopologyLayoutCoordinate, 0, len(request.Devices)),
		Containers: make([]TopologyContainerLayoutCoordinate, 0, len(request.Containers)),
	}
	for _, coordinate := range request.Devices {
		if strings.TrimSpace(coordinate.DeviceID) == "" {
			h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局里的设备坐标缺少设备标识。")
			return
		}
		if coordinate.X == nil || coordinate.Y == nil {
			h.writeTopologyJSONError(w, http.StatusBadRequest, "布局里的设备坐标必须同时给出横坐标与纵坐标。")
			return
		}
		save.Devices = append(save.Devices, TopologyLayoutCoordinate{
			DeviceID: coordinate.DeviceID, X: *coordinate.X, Y: *coordinate.Y,
		})
	}
	for _, coordinate := range request.Containers {
		if coordinate.EntityType != store.LayoutEntitySite && coordinate.EntityType != store.LayoutEntityNetwork {
			h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局里的站点区域或网络分组框坐标只能是站点或网络。")
			return
		}
		if strings.TrimSpace(coordinate.EntityID) == "" {
			h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局里的站点区域或网络分组框坐标缺少站点或网络标识。")
			return
		}
		if coordinate.X == nil || coordinate.Y == nil {
			h.writeTopologyJSONError(w, http.StatusBadRequest, "布局里的站点区域或网络分组框坐标必须同时给出横坐标与纵坐标。")
			return
		}
		save.Containers = append(save.Containers, TopologyContainerLayoutCoordinate{
			EntityType: coordinate.EntityType, EntityID: coordinate.EntityID,
			X: *coordinate.X, Y: *coordinate.Y,
		})
	}
	if err := h.cfg.TopologyWriter.SaveTopologyLayout(save, TopologyWriteSource{
		RemoteAddress: remoteIP(r),
	}); err != nil {
		if errors.Is(err, ErrTopologyLayoutInvalid) {
			h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局引用的站点、网络或设备不存在，请刷新页面后重试。")
			return
		}
		h.writeTopologyJSONError(w, http.StatusInternalServerError, "保存布局失败，请稍后重试。")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTopologyLayoutReset 处理一次布局重置。
func (h *handler) handleTopologyLayoutReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "布局重置接口只接受 POST 请求。")
		return
	}
	var request topologyLayoutResetRequest
	if err := decodeTopologyLayoutBody(r, &request); err != nil {
		h.writeTopologyLayoutJSONRequestError(w, err)
		return
	}
	if !validTopologyView(request.ViewKey) {
		h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局视图只能是物理视图、虚拟视图或叠加视图。")
		return
	}
	if err := h.cfg.TopologyWriter.ResetTopologyLayout(request.ViewKey, TopologyWriteSource{
		RemoteAddress: remoteIP(r),
	}); err != nil {
		if errors.Is(err, ErrTopologyLayoutInvalid) {
			h.writeTopologyJSONError(w, http.StatusUnprocessableEntity, "布局重置请求内容无效，请刷新页面后重试。")
			return
		}
		h.writeTopologyJSONError(w, http.StatusInternalServerError, "重置布局失败，请稍后重试。")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeTopologyLayoutJSONRequestError 把请求体解析错误换算成对应的客户端错
// 误状态码。
func (h *handler) writeTopologyLayoutJSONRequestError(w http.ResponseWriter, err error) {
	if errors.Is(err, errTopologyLayoutTooLarge) {
		h.writeTopologyJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	h.writeTopologyWriteJSONRequestError(w, err)
}
