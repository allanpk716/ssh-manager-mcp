package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrProbeTaskRunning 表示已有一个画布触发的连通探测任务在进行中；接口层
// 按冲突返回，页面据此阻止重复触发。
var ErrProbeTaskRunning = errors.New("已有连通探测任务正在进行，请等它完成后再触发。")

// TopologyProbeOrigin 是一次探测叠加的起点解析结果。From 是 self 或服务器
// 编号；Known 为假时 Notice 会说明原因，页面继续可用。
type TopologyProbeOrigin struct {
	From       string `json:"from"`
	Known      bool   `json:"known"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	ServerID   string `json:"server_id,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}

// TopologyProbeLine 是详情面板里一个共同网络地址的探测结论。Path 是为虚拟
// 局域网「经转发节点」标记预留的可选路径字段；首版恒为空，虚拟局域网成功
// 结果使用统一样式。
type TopologyProbeLine struct {
	NetworkID            string `json:"network_id"`
	NetworkName          string `json:"network_name"`
	NetworkType          string `json:"network_type"`
	Address              string `json:"address"`
	Port                 int    `json:"port"`
	Observed             bool   `json:"observed"`
	OK                   bool   `json:"ok"`
	Category             string `json:"category,omitempty"`
	CategoryLabel        string `json:"category_label,omitempty"`
	DurationMilliseconds int64  `json:"duration_ms"`
	ObservedAt           string `json:"observed_at,omitempty"`
	Path                 string `json:"path,omitempty"`
}

// TopologyProbeDevice 是一台设备在当前起点视角的探测汇总。Status 是 ok
// （至少一个地址成功）、fail（有观察但全部不通）、unprobed（有共同网络
// 但没有本地观察）或 no_common_network（起点与目标没有共同网络）。
type TopologyProbeDevice struct {
	DeviceID            string              `json:"device_id"`
	Status              string              `json:"status"`
	FastestMilliseconds int64               `json:"fastest_ms"`
	Inconsistent        bool                `json:"inconsistent"`
	Note                string              `json:"note,omitempty"`
	Lines               []TopologyProbeLine `json:"lines"`
}

// TopologyProbeOverlay 是拓扑图页面按所选起点读取到的本地探测结果叠加。
// 结果只来自发起实例本地的探测结果文件，不回传权威端。
type TopologyProbeOverlay struct {
	Origin  TopologyProbeOrigin   `json:"origin"`
	Notice  string                `json:"notice,omitempty"`
	Devices []TopologyProbeDevice `json:"devices"`
}

// TopologyProbeTaskStatus 是一次画布触发探测的任务状态。State 是 running、
// done 或 error。
type TopologyProbeTaskStatus struct {
	ID      string   `json:"task_id"`
	State   string   `json:"state"`
	From    string   `json:"from"`
	Targets []string `json:"targets"`
	Message string   `json:"message,omitempty"`
}

// TopologyProbe 是拓扑图页面按需触发连通探测与读取本地探测结果叠加的唯一
// 入口。三种角色都提供；客户端适配层会拒绝服务器起点。触发与读取复用仓
// 库既有的连通探测服务与本地探测结果文件，不另写一套。
type TopologyProbe interface {
	// Overlay 返回指定起点的本地探测结果叠加；起点无法解析时通过 Notice
	// 给出可读提示，而不是中断页面。
	Overlay(instance, from string) (*TopologyProbeOverlay, error)
	// StartProbe 启动一次后台连通探测并返回任务标识；已有任务在进行中时返
	// 回 ErrProbeTaskRunning。
	StartProbe(instance, from string, targets []string) (string, error)
	// ProbeStatus 返回任务状态；任务不存在时返回 false。
	ProbeStatus(taskID string) (*TopologyProbeTaskStatus, bool)
}

// topologyProbeStartRequest 是启动探测接口的请求体。
type topologyProbeStartRequest struct {
	From    string   `json:"from"`
	Targets []string `json:"targets"`
}

// handleTopologyProbeOverlay 返回当前起点的本地探测结果叠加。GET 请求在进
// 入这里之前已经过会话校验。
func (h *handler) handleTopologyProbeOverlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "探测结果叠加接口只接受 GET 请求。")
		return
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	if from == "" {
		from = "self"
	}
	if h.cfg.Role == RoleClient && from != "self" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "客户端只能以本机为起点查看探测结果。")
		return
	}
	overlay, err := h.cfg.TopologyProbe.Overlay(h.requestClientInstance(r), from)
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusInternalServerError, "读取探测结果失败，请稍后重试。")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(overlay)
}

// handleTopologyProbeStart 启动一次探测任务。POST 请求在进入这里之前已经过
// 会话、来源与防跨站请求伪造校验。
func (h *handler) handleTopologyProbeStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "探测启动接口只接受 POST 请求。")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, topologyWriteMaxRequestBody+1))
	if err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "读取请求内容失败。")
		return
	}
	if len(body) > topologyWriteMaxRequestBody {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "请求内容过大")
		return
	}
	var request topologyProbeStartRequest
	if err := json.Unmarshal(body, &request); err != nil {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "请求内容不是合法的 JSON 对象。")
		return
	}
	from := strings.TrimSpace(request.From)
	if from == "" {
		from = "self"
	}
	targets := make([]string, 0, len(request.Targets))
	for _, target := range request.Targets {
		target = strings.TrimSpace(target)
		if target != "" {
			targets = append(targets, target)
		}
	}
	if h.cfg.Role == RoleClient && from != "self" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "客户端只能以本机为起点触发探测。")
		return
	}
	if len(targets) == 0 {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "探测目标不能为空；请至少选择一个目标。")
		return
	}
	taskID, err := h.cfg.TopologyProbe.StartProbe(h.requestClientInstance(r), from, targets)
	switch {
	case errors.Is(err, ErrProbeTaskRunning):
		h.writeTopologyJSONError(w, http.StatusConflict, err.Error())
	case err != nil:
		h.writeTopologyJSONError(w, http.StatusBadRequest, err.Error())
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"task_id": taskID})
	}
}

// handleTopologyProbeStatus 轮询探测任务状态。
func (h *handler) handleTopologyProbeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.writeTopologyJSONError(w, http.StatusMethodNotAllowed, "探测状态接口只接受 GET 请求。")
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskID == "" {
		h.writeTopologyJSONError(w, http.StatusBadRequest, "探测任务标识不能为空。")
		return
	}
	status, ok := h.cfg.TopologyProbe.ProbeStatus(taskID)
	if !ok {
		h.writeTopologyJSONError(w, http.StatusNotFound, "探测任务不存在或已过期。")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(status)
}
