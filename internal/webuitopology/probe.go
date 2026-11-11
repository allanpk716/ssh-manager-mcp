package webuitopology

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// probeTaskRetention 是已完成任务的状态保留数量；页面轮询只需要最近的任
// 务，过旧记录直接丢弃。
const probeTaskRetention = 32

// ProbeRunnerDeps 描述探测入口的依赖。Topology 与 OpenStore 由角色适配层注
// 入；ResultsFor 与 NewService 留空时使用生产实现，测试注入替身。
type ProbeRunnerDeps struct {
	// Topology 提供与拓扑图页面一致的只读数据，叠加据此判定共同网络。
	Topology webui.TopologySource
	// OpenStore 为一次探测打开存储；单机与权威端打开 vault，客户端打开带
	// 旁车审计的临时快照库。
	OpenStore func(instance string) (*store.Store, func(), error)
	// ResultsFor 返回发起实例本地的探测结果文件；空值表示没有可写位置。
	ResultsFor func(instance string) *proberesults.FileProbeResults
	// NewService 构造连通探测服务；空值使用生产服务。目标数量、并发与总
	// 时限沿用服务的既有上限。
	NewService func(st *store.Store, instance string, results *proberesults.FileProbeResults) *probe.Service
	// AllowRemoteOrigin 表示能否选择受管服务器作为起点；客户端为假。
	AllowRemoteOrigin bool
}

// probeRunner 把拓扑图页面的探测请求接到既有连通探测服务上，并在进程内
// 维护「启动任务、轮询状态」的任务表。
type probeRunner struct {
	deps    ProbeRunnerDeps
	mu      sync.Mutex
	tasks   map[string]*webui.TopologyProbeTaskStatus
	running string
}

// NewProbeRunner 返回拓扑图页面使用的探测入口。
func NewProbeRunner(deps ProbeRunnerDeps) webui.TopologyProbe {
	if deps.ResultsFor == nil {
		deps.ResultsFor = func(instance string) *proberesults.FileProbeResults {
			results, ok, err := probeobserve.ResultsFor(instance)
			if err != nil || !ok {
				return nil
			}
			return results
		}
	}
	if deps.NewService == nil {
		deps.NewService = func(st *store.Store, instance string, results *proberesults.FileProbeResults) *probe.Service {
			return &probe.Service{Store: st, Instance: instance, Results: results}
		}
	}
	return &probeRunner{deps: deps, tasks: map[string]*webui.TopologyProbeTaskStatus{}}
}

// ProbeFromStoreOpener 返回按请求打开权威 vault 的探测入口，供单机适配层
// 使用；拓扑数据与探测使用同一个打开函数。
func ProbeFromStoreOpener(open func() (*store.Store, func(), error), topology webui.TopologySource) webui.TopologyProbe {
	return NewProbeRunner(ProbeRunnerDeps{
		Topology: topology,
		OpenStore: func(string) (*store.Store, func(), error) {
			return open()
		},
		AllowRemoteOrigin: true,
	})
}

// ProbeFromStore 返回绑定长期存储句柄的探测入口，供权威端 serve 进程使
// 用。
func ProbeFromStore(st *store.Store) webui.TopologyProbe {
	return NewProbeRunner(ProbeRunnerDeps{
		Topology: FromStore(st),
		OpenStore: func(string) (*store.Store, func(), error) {
			return st, func() {}, nil
		},
		AllowRemoteOrigin: true,
	})
}

// Overlay 读取当前起点的本地探测结果并按拓扑登记整理成叠加数据。起点解
// 析失败时返回带可读提示的空叠加，页面不中断。
func (r *probeRunner) Overlay(instance, from string) (*webui.TopologyProbeOverlay, error) {
	if from == "" {
		from = webui.TopologyProbeOriginSelf
	}
	if !r.deps.AllowRemoteOrigin && from != webui.TopologyProbeOriginSelf {
		return nil, &webui.TopologyInvalidInputError{Message: "客户端只能以本机为起点查看探测结果。"}
	}
	data, err := r.deps.Topology.Topology(instance, from)
	if err != nil {
		return nil, err
	}
	origin, notice := probeOriginView(data, from)
	overlay := &webui.TopologyProbeOverlay{
		Origin:  origin,
		Notice:  notice,
		Devices: make([]webui.TopologyProbeDevice, 0, len(data.Devices)),
	}
	if !origin.Known {
		return overlay, nil
	}
	var observations []proberesults.Observation
	if results := r.deps.ResultsFor(instance); results != nil {
		observations, err = results.List(origin.DeviceID, "")
		if err != nil {
			return nil, err
		}
	}
	networks := make(map[string]webui.TopologyNetwork, len(data.Networks))
	for _, network := range data.Networks {
		networks[network.ID] = network
	}
	memberships := make(map[string][]*models.NetworkMembership)
	networkIDByName := make(map[string]string, len(data.Networks))
	for index := range data.Memberships {
		membership := data.Memberships[index]
		network, ok := networks[membership.NetworkID]
		if !ok {
			continue
		}
		networkIDByName[network.Name] = network.ID
		model := &models.NetworkMembership{
			DeviceID: membership.DeviceID, NetworkID: membership.NetworkID,
			NetworkType: models.NetworkType(network.Type), Address: membership.Address,
			NetworkName: network.Name, RegistrationOrder: membership.RegistrationOrder,
		}
		memberships[membership.DeviceID] = append(memberships[membership.DeviceID], model)
	}
	originMemberships := memberships[origin.DeviceID]
	observationsByTarget := make(map[string][]proberesults.Observation, len(observations))
	for _, observation := range observations {
		observationsByTarget[observation.TargetDeviceID] = append(observationsByTarget[observation.TargetDeviceID], observation)
	}
	for _, device := range data.Devices {
		if device.ID == origin.DeviceID {
			continue
		}
		overlay.Devices = append(overlay.Devices, probeDeviceSummary(
			device, networkIDByName, originMemberships, memberships[device.ID], observationsByTarget[device.ID]))
	}
	return overlay, nil
}

// StartProbe 启动一次后台连通探测；进行中重复触发返回 ErrProbeTaskRunning。
func (r *probeRunner) StartProbe(instance, from string, targets []string) (string, error) {
	if from == "" {
		from = webui.TopologyProbeOriginSelf
	}
	if !r.deps.AllowRemoteOrigin && from != webui.TopologyProbeOriginSelf {
		return "", &webui.TopologyInvalidInputError{Message: "客户端只能以本机为起点触发探测。"}
	}
	if len(targets) == 0 {
		return "", &webui.TopologyInvalidInputError{Message: "探测目标不能为空。"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != "" {
		return "", webui.ErrProbeTaskRunning
	}
	id, err := randomProbeTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.TopologyProbeTaskStatus{
		ID: id, State: "running", From: from,
		Targets: append([]string(nil), targets...),
	}
	r.tasks[id] = task
	r.running = id
	go r.runProbe(instance, task)
	return id, nil
}

// ProbeStatus 返回任务状态。
func (r *probeRunner) ProbeStatus(taskID string) (*webui.TopologyProbeTaskStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, false
	}
	status := *task
	status.Targets = append([]string(nil), task.Targets...)
	return &status, true
}

// runProbe 在后台执行一次探测并写审计；无论成败都写一条来源为 Web UI 的
// 审计行，与命令行和 MCP 工具的口径一致。
func (r *probeRunner) runProbe(instance string, task *webui.TopologyProbeTaskStatus) {
	finish := func(state, message string) {
		r.mu.Lock()
		task.State = state
		task.Message = message
		if r.running == task.ID {
			r.running = ""
		}
		r.pruneTasksLocked()
		r.mu.Unlock()
	}
	st, closeStore, err := r.deps.OpenStore(instance)
	if err != nil {
		finish("error", "打开拓扑数据失败："+err.Error())
		return
	}
	defer closeStore()
	results := r.deps.ResultsFor(instance)
	service := r.deps.NewService(st, instance, results)
	start := time.Now()
	report, probeErr := service.Probe(context.Background(), probe.Request{From: task.From, Targets: task.Targets})
	summary, originServerID := probe.AuditSummary(task.From, report, probeErr)
	status := "error"
	if probeErr == nil {
		status = "ok"
	}
	_ = st.WriteAudit(store.AuditRow{
		TS: start, ServerID: originServerID, Action: "probe-connectivity", Status: status,
		Source: store.AuditSourceWebUI, Command: store.MarshalAuditSummary(summary),
	})
	switch {
	case probeErr != nil:
		finish("error", probeErr.Error())
	case report.OriginError != "":
		finish("error", "无法连接起点服务器："+report.OriginError)
	default:
		finish("done", probeDoneMessage(report))
	}
}

// probeOriginView 从拓扑数据解析探测起点；解析失败返回可读提示。
func probeOriginView(data *webui.TopologyData, from string) (webui.TopologyProbeOrigin, string) {
	if from == webui.TopologyProbeOriginSelf {
		origin := webui.TopologyProbeOrigin{From: from}
		if data.LocalDevice.Known {
			origin.Known = true
			origin.DeviceID = data.LocalDevice.DeviceID
			origin.DeviceName = data.LocalDevice.DeviceName
			return origin, ""
		}
		return origin, unknownProbeOriginNotice(data.LocalDevice.Reason)
	}
	for _, device := range data.Devices {
		for _, server := range device.Servers {
			if server.ID == from {
				return webui.TopologyProbeOrigin{
					From: from, Known: true, DeviceID: device.ID, DeviceName: device.Name,
					ServerID: server.ID, ServerName: server.Name,
				}, ""
			}
		}
	}
	return webui.TopologyProbeOrigin{From: from}, fmt.Sprintf(
		"找不到起点服务器 %s，或它还未关联拓扑设备；请核对后再选择。", from)
}

func unknownProbeOriginNotice(reason string) string {
	switch reason {
	case "local-device-not-set":
		return "本机设备尚未登记，无法叠加探测结果；请 owner 补登记本机设备。"
	case "local-device-deleted":
		return "本机设备已从拓扑删除，无法叠加探测结果；请 owner 重新登记本机设备。"
	case "device-missing-from-snapshot":
		return "本机设备不在当前快照里，无法叠加探测结果；请先同步客户端缓存。"
	case "client-device-not-recorded":
		return "本机设备还没有随快照记录，无法叠加探测结果；请先同步客户端缓存。"
	default:
		return "本机设备无法识别，无法叠加探测结果。"
	}
}

// probeDeviceSummary 把一台目标设备与本地观察合成为叠加汇总与详情行。共
// 同网络判定复用统一拨号入口的规划函数，保证与真实探测尝试的地址一致。
func probeDeviceSummary(
	device webui.TopologyDevice, networkIDByName map[string]string,
	originMemberships, targetMemberships []*models.NetworkMembership,
	observations []proberesults.Observation,
) webui.TopologyProbeDevice {
	summary := webui.TopologyProbeDevice{DeviceID: device.ID, Lines: []webui.TopologyProbeLine{}}
	plan := sshbroker.PlanCommonNetworkAddresses(targetMemberships, originMemberships)
	if len(plan.Candidates) == 0 {
		summary.Status = "no_common_network"
		summary.Note = "无共同网络，未探测"
		return summary
	}
	defaultPort := 22
	if len(device.Servers) > 0 {
		defaultPort = device.Servers[0].Port
	}
	var commonNetworkFailure string
	anyObserved := false
	anyOK := false
	for _, candidate := range plan.Candidates {
		var lines []webui.TopologyProbeLine
		networkObserved := false
		networkOK := false
		for _, observation := range observations {
			if observation.Network != candidate.NetworkName ||
				store.NormalizeSSHHost(observation.Address) != store.NormalizeSSHHost(candidate.Address) {
				continue
			}
			networkObserved = true
			if observation.OK {
				networkOK = true
				anyOK = true
				if observation.Duration >= 0 && (summary.FastestMilliseconds == 0 ||
					observation.Duration.Milliseconds() < summary.FastestMilliseconds) {
					summary.FastestMilliseconds = observation.Duration.Milliseconds()
				}
			}
			lines = append(lines, probeLineView(networkIDByName, candidate, observation))
		}
		if len(lines) == 0 {
			lines = append(lines, webui.TopologyProbeLine{
				NetworkID: networkIDByName[candidate.NetworkName], NetworkName: candidate.NetworkName,
				NetworkType: string(candidate.Source), Address: candidate.Address, Port: defaultPort,
			})
		} else {
			anyObserved = true
		}
		if (candidate.Source == sshbroker.DialAddressPhysical ||
			candidate.Source == sshbroker.DialAddressVirtual) &&
			networkObserved && !networkOK && commonNetworkFailure == "" {
			commonNetworkFailure = candidate.NetworkName
			for _, observation := range observations {
				if observation.Network == candidate.NetworkName && !observation.OK && failedProbeCategory(observation.Category) {
					summary.Note = fmt.Sprintf("登记与探测不一致：%s（%s）",
						candidate.NetworkName, models.ProbeErrorCategoryLabel(observation.Category))
					summary.Inconsistent = true
					break
				}
			}
		}
		summary.Lines = append(summary.Lines, lines...)
	}
	switch {
	case anyOK:
		summary.Status = "ok"
	case anyObserved:
		summary.Status = "fail"
	default:
		summary.Status = "unprobed"
	}
	return summary
}

// failedProbeCategory 判断一个失败分类能否作为「登记与探测不一致」的证据；
// 探测未完成、缺少工具与没有结果行说明「没有测到连接」，不算不一致。
func failedProbeCategory(category models.ProbeErrorCategory) bool {
	switch category {
	case models.ProbeErrorIncomplete, models.ProbeErrorMissingTool, models.ProbeErrorNoResult, models.ProbeErrorNone:
		return false
	default:
		return true
	}
}

func probeLineView(networkIDByName map[string]string, candidate sshbroker.DialAddress, observation proberesults.Observation) webui.TopologyProbeLine {
	category := ""
	label := ""
	if !observation.OK {
		category = string(observation.Category)
		label = models.ProbeErrorCategoryLabel(observation.Category)
	}
	return webui.TopologyProbeLine{
		NetworkID: networkIDByName[candidate.NetworkName], NetworkName: candidate.NetworkName,
		NetworkType: string(candidate.Source), Address: candidate.Address, Port: observation.Port,
		Observed: true, OK: observation.OK, Category: category, CategoryLabel: label,
		DurationMilliseconds: observation.Duration.Milliseconds(),
		ObservedAt:           observation.ObservedAt.UTC().Format(time.RFC3339),
	}
}

// probeDoneMessage 汇总一次成功执行的探测结论，供页面状态区显示。
func probeDoneMessage(report *probe.Report) string {
	reachable, failed, skipped := 0, 0, 0
	for _, target := range report.Targets {
		anyOK := false
		for _, line := range target.Lines {
			if line.OK {
				anyOK = true
				break
			}
		}
		switch {
		case target.NoCommonNetwork || target.UnprobedNote != "":
			skipped++
		case anyOK:
			reachable++
		default:
			failed++
		}
	}
	parts := []string{strconv.Itoa(reachable) + " 台可达", strconv.Itoa(failed) + " 台不通"}
	if skipped > 0 {
		parts = append(parts, strconv.Itoa(skipped)+" 台未探测")
	}
	return "探测完成：" + strings.Join(parts, "，") + "。"
}

func (r *probeRunner) pruneTasksLocked() {
	if len(r.tasks) <= probeTaskRetention {
		return
	}
	for id, task := range r.tasks {
		if task.State != "running" {
			delete(r.tasks, id)
			if len(r.tasks) <= probeTaskRetention {
				return
			}
		}
	}
}

func randomProbeTaskID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成探测任务标识失败：%w", err)
	}
	return hex.EncodeToString(buffer), nil
}
