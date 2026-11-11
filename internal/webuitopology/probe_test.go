package webuitopology

// 拓扑图探测入口的测试：叠加数据只看公开返回结构；触发探测沿用连通探测
// 服务的可替换注入点，断言本地探测结果文件与审计行，不看内部任务结构。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// probeFixture 在 seedVault 的拓扑上补一个本地探测结果文件，并返回可注入
// 拨号替身的探测入口。
type probeFixture struct {
	store   *store.Store
	results *proberesults.FileProbeResults
	runner  webui.TopologyProbe
	dial    *blockingDial
}

type blockingDial struct {
	mu       sync.Mutex
	outcomes map[string]probeOutcome
	release  chan struct{}
}

type probeOutcome struct {
	err      error
	duration time.Duration
}

func (b *blockingDial) dial(_ context.Context, address string, port int, _ time.Duration) (time.Duration, error) {
	if b.release != nil {
		<-b.release
	}
	b.mu.Lock()
	outcome := b.outcomes[fmt.Sprintf("%s:%d", address, port)]
	b.mu.Unlock()
	return outcome.duration, outcome.err
}

func (b *blockingDial) set(address string, port int, err error, duration time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.outcomes == nil {
		b.outcomes = map[string]probeOutcome{}
	}
	b.outcomes[fmt.Sprintf("%s:%d", address, port)] = probeOutcome{err: err, duration: duration}
}

func newProbeFixture(t *testing.T) *probeFixture {
	t.Helper()
	st, _, _, _ := seedVault(t)
	results := proberesults.NewFileProbeResults(filepath.Join(t.TempDir(), "probe-results.json"))
	dial := &blockingDial{}
	runner := NewProbeRunner(ProbeRunnerDeps{
		Topology:          FromStore(st),
		OpenStore:         func(string) (*store.Store, func(), error) { return st, func() {}, nil },
		ResultsFor:        func(string) *proberesults.FileProbeResults { return results },
		AllowRemoteOrigin: true,
		NewService: func(opened *store.Store, _ string, file *proberesults.FileProbeResults) *probe.Service {
			return &probe.Service{Store: opened, Results: file, LocalDial: dial.dial}
		},
	})
	return &probeFixture{store: st, results: results, runner: runner, dial: dial}
}

func (f *probeFixture) record(t *testing.T, observation proberesults.Observation) {
	t.Helper()
	if err := f.results.Record(observation); err != nil {
		t.Fatalf("写探测结果失败：%v", err)
	}
}

func probeDeviceByID(t *testing.T, overlay *webui.TopologyProbeOverlay, deviceID string) webui.TopologyProbeDevice {
	t.Helper()
	for _, device := range overlay.Devices {
		if device.DeviceID == deviceID {
			return device
		}
	}
	t.Fatalf("叠加数据里找不到设备 %s：%+v", deviceID, overlay.Devices)
	return webui.TopologyProbeDevice{}
}

// TestProbeOverlaySummarizesLocalObservations: 本机起点的叠加按设备汇总最快
// 成功结果，详情逐网络逐地址显示通否、错误短语、耗时与观察时间；owner 界
// 面照常显示网络名与地址。
func TestProbeOverlaySummarizesLocalObservations(t *testing.T) {
	fixture := newProbeFixture(t)
	workstation, err := fixture.store.GetDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	nas, err := fixture.store.GetDevice("NAS")
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().Add(-20 * time.Second).UTC()
	fixture.record(t, proberesults.Observation{
		OriginDeviceID: workstation.ID, TargetDeviceID: nas.ID,
		Network: "家A局域网", Address: "192.168.1.30", Port: 22,
		OK: true, Duration: 3 * time.Millisecond, ObservedAt: observedAt, Source: proberesults.SourceProbe,
	})
	fixture.record(t, proberesults.Observation{
		OriginDeviceID: workstation.ID, TargetDeviceID: nas.ID,
		Network: "虚拟网", Address: "100.64.0.30", Port: 22,
		Category: models.ProbeErrorTimeout, Duration: 1500 * time.Millisecond,
		ObservedAt: observedAt, Source: proberesults.SourceProbe,
	})

	overlay, err := fixture.runner.Overlay("", "self")
	if err != nil {
		t.Fatal(err)
	}
	if !overlay.Origin.Known || overlay.Origin.DeviceID != workstation.ID || overlay.Origin.DeviceName != "工作站" {
		t.Fatalf("本机起点解析错误：%+v", overlay.Origin)
	}
	nasSummary := probeDeviceByID(t, overlay, nas.ID)
	if nasSummary.Status != "ok" || nasSummary.FastestMilliseconds != 3 {
		t.Fatalf("NAS 汇总应为最快成功 3 毫秒：%+v", nasSummary)
	}
	if len(nasSummary.Lines) != 2 {
		t.Fatalf("NAS 应逐网络逐地址两行：%+v", nasSummary.Lines)
	}
	first := nasSummary.Lines[0]
	if first.NetworkName != "家A局域网" || first.NetworkType != "physical" || first.Address != "192.168.1.30" ||
		!first.OK || !first.Observed || first.DurationMilliseconds != 3 || first.ObservedAt == "" {
		t.Fatalf("家A局域网探测行错误：%+v", first)
	}
	second := nasSummary.Lines[1]
	if second.NetworkName != "虚拟网" || second.NetworkType != "virtual" || !second.Observed || second.OK ||
		second.CategoryLabel != "连接超时" || second.DurationMilliseconds != 1500 {
		t.Fatalf("虚拟网探测行错误：%+v", second)
	}

	laptop, err := fixture.store.GetDevice("笔记本")
	if err != nil {
		t.Fatal(err)
	}
	laptopSummary := probeDeviceByID(t, overlay, laptop.ID)
	if laptopSummary.Status != "unprobed" || len(laptopSummary.Lines) == 0 || laptopSummary.Lines[0].Observed {
		t.Fatalf("共同网络但没有观察的设备应显示未探测：%+v", laptopSummary)
	}

	cloud, err := fixture.store.GetDevice("云主机")
	if err != nil {
		t.Fatal(err)
	}
	cloudSummary := probeDeviceByID(t, overlay, cloud.ID)
	if cloudSummary.Status != "no_common_network" || cloudSummary.Note != "无共同网络，未探测" {
		t.Fatalf("无共同网络设备汇总错误：%+v", cloudSummary)
	}
}

// TestProbeOverlayMarksRegistrationMismatch: 登记了共同物理局域网但探测不通
// 时，设备与详情给出「登记与探测不一致」提示；虚拟局域网成功仍算可达。
func TestProbeOverlayMarksRegistrationMismatch(t *testing.T) {
	fixture := newProbeFixture(t)
	workstation, err := fixture.store.GetDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	nas, err := fixture.store.GetDevice("NAS")
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().UTC()
	fixture.record(t, proberesults.Observation{
		OriginDeviceID: workstation.ID, TargetDeviceID: nas.ID,
		Network: "家A局域网", Address: "192.168.1.30", Port: 22,
		Category: models.ProbeErrorConnectionRefused, Duration: time.Millisecond,
		ObservedAt: observedAt, Source: proberesults.SourceProbe,
	})
	fixture.record(t, proberesults.Observation{
		OriginDeviceID: workstation.ID, TargetDeviceID: nas.ID,
		Network: "虚拟网", Address: "100.64.0.30", Port: 22,
		OK: true, Duration: 8 * time.Millisecond, ObservedAt: observedAt, Source: proberesults.SourceProbe,
	})

	overlay, err := fixture.runner.Overlay("", "self")
	if err != nil {
		t.Fatal(err)
	}
	nasSummary := probeDeviceByID(t, overlay, nas.ID)
	if nasSummary.Status != "ok" || !nasSummary.Inconsistent {
		t.Fatalf("物理局域网不通但虚拟局域网可达时应标记不一致：%+v", nasSummary)
	}
	if nasSummary.Note != "登记与探测不一致：家A局域网（连接被拒绝）" {
		t.Fatalf("不一致提示错误：%q", nasSummary.Note)
	}
}

func TestProbeOverlayMarksVirtualNetworkRegistrationMismatch(t *testing.T) {
	fixture := newProbeFixture(t)
	workstation, err := fixture.store.GetDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	nas, err := fixture.store.GetDevice("NAS")
	if err != nil {
		t.Fatal(err)
	}
	fixture.record(t, proberesults.Observation{
		OriginDeviceID: workstation.ID, TargetDeviceID: nas.ID,
		Network: "虚拟网", Address: "100.64.0.30", Port: 22,
		Category: models.ProbeErrorTimeout, Duration: time.Millisecond,
		ObservedAt: time.Now().UTC(), Source: proberesults.SourceProbe,
	})

	overlay, err := fixture.runner.Overlay("", "self")
	if err != nil {
		t.Fatal(err)
	}
	nasSummary := probeDeviceByID(t, overlay, nas.ID)
	if nasSummary.Status != "fail" || !nasSummary.Inconsistent {
		t.Fatalf("共同虚拟局域网探测不通也应标记登记与探测不一致：%+v", nasSummary)
	}
	if nasSummary.Note != "登记与探测不一致：虚拟网（连接超时）" {
		t.Fatalf("虚拟局域网不一致提示错误：%q", nasSummary.Note)
	}
}

// TestProbeOverlayRemoteOriginAndReadableNotices: 经服务器起点按该服务器的
// 设备视角叠加；本机设备未登记或起点服务器找不到时返回可读提示。
func TestProbeOverlayRemoteOriginAndReadableNotices(t *testing.T) {
	fixture := newProbeFixture(t)
	servers, err := fixture.store.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	dbServerID := ""
	for _, server := range servers {
		if server.Name == "db-1" {
			dbServerID = server.ID
		}
	}
	if dbServerID == "" {
		t.Fatal("种子库里找不到 db-1 服务器条目")
	}
	overlay, err := fixture.runner.Overlay("", dbServerID)
	if err != nil {
		t.Fatal(err)
	}
	if !overlay.Origin.Known || overlay.Origin.ServerID != dbServerID || overlay.Origin.DeviceName != "机房机器" {
		t.Fatalf("服务器起点解析错误：%+v", overlay.Origin)
	}
	cloud, err := fixture.store.GetDevice("云主机")
	if err != nil {
		t.Fatal(err)
	}
	if probeDeviceByID(t, overlay, cloud.ID).Status != "no_common_network" {
		t.Fatalf("经服务器起点也应判定无共同网络：%+v", overlay)
	}

	missing, err := fixture.runner.Overlay("", "no-such-server")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Origin.Known || !strings.Contains(missing.Notice, "no-such-server") {
		t.Fatalf("起点服务器找不到时应给可读提示：%+v", missing)
	}

	unknownLocal := NewProbeRunner(ProbeRunnerDeps{
		Topology: stubTopologySource{data: &webui.TopologyData{
			Devices:     []webui.TopologyDevice{{ID: "device-1", Name: "NAS"}},
			Networks:    []webui.TopologyNetwork{},
			Memberships: []webui.TopologyMembership{},
			LocalDevice: webui.TopologyLocalDevice{Known: false, Reason: "local-device-not-set"},
		}},
		AllowRemoteOrigin: true,
	})
	localOverlay, err := unknownLocal.Overlay("", "self")
	if err != nil {
		t.Fatal(err)
	}
	if localOverlay.Origin.Known || localOverlay.Notice == "" || len(localOverlay.Devices) != 0 {
		t.Fatalf("本机设备未登记时页面不应中断：%+v", localOverlay)
	}
}

type stubTopologySource struct {
	data *webui.TopologyData
}

func (s stubTopologySource) Topology(_, _ string) (*webui.TopologyData, error) {
	return s.data, nil
}

// TestProbeRunnerStartsTaskWritesResultsAndAudit: 从画布触发探测经任务启动
// 与轮询完成，结果写发起实例本地的探测结果文件，审计来源为 Web UI。
func TestProbeRunnerStartsTaskWritesResultsAndAudit(t *testing.T) {
	fixture := newProbeFixture(t)
	fixture.dial.set("192.168.1.30", 445, nil, 3*time.Millisecond)
	fixture.dial.set("100.64.0.30", 445, &timeoutDialError{}, 1500*time.Millisecond)

	taskID, err := fixture.runner.StartProbe("", "self", []string{"NAS:445"})
	if err != nil {
		t.Fatal(err)
	}
	status := waitForProbeTask(t, fixture.runner, taskID)
	if status.State != "done" || status.Message == "" {
		t.Fatalf("任务应完成并给出汇总：%+v", status)
	}
	if status.From != "self" || len(status.Targets) != 1 || status.Targets[0] != "NAS:445" {
		t.Fatalf("任务应保留起点与目标：%+v", status)
	}

	workstation, err := fixture.store.GetDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	observations, err := fixture.results.List(workstation.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("本地探测结果应按地址写入两条：%+v", observations)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"probe-connectivity"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI || rows[0].Status != "ok" {
		t.Fatalf("Web UI 触发的探测应写一条来源为 Web UI 的审计：%+v", rows)
	}
}

type timeoutDialError struct{}

func (*timeoutDialError) Error() string   { return "i/o timeout" }
func (*timeoutDialError) Timeout() bool   { return true }
func (*timeoutDialError) Temporary() bool { return true }

// TestProbeRunnerRejectsDuplicateAndClientRemoteOrigin: 探测进行中阻止重复
// 触发；客户端只能以本机为起点。
func TestProbeRunnerRejectsDuplicateAndClientRemoteOrigin(t *testing.T) {
	fixture := newProbeFixture(t)
	fixture.dial.release = make(chan struct{})
	fixture.dial.set("192.168.1.30", 22, nil, time.Millisecond)
	first, err := fixture.runner.StartProbe("", "self", []string{"NAS"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.runner.StartProbe("", "self", []string{"NAS"}); !errors.Is(err, webui.ErrProbeTaskRunning) {
		t.Fatalf("进行中重复触发应被拒绝，实际：%v", err)
	}
	close(fixture.dial.release)
	status := waitForProbeTask(t, fixture.runner, first)
	if status.State != "done" {
		t.Fatalf("任务应完成：%+v", status)
	}

	clientRunner := NewProbeRunner(ProbeRunnerDeps{
		Topology:          FromStore(fixture.store),
		AllowRemoteOrigin: false,
	})
	if _, err := clientRunner.StartProbe("", "srv-1", []string{"NAS"}); err == nil || !strings.Contains(err.Error(), "客户端") {
		t.Fatalf("客户端选择服务器起点应被拒绝，实际：%v", err)
	}
}

// TestProbeRunnerRecordsParameterFailure: 参数错误进入失败状态并显示原因，
// 审计状态为 error。
func TestProbeRunnerRecordsParameterFailure(t *testing.T) {
	fixture := newProbeFixture(t)
	taskID, err := fixture.runner.StartProbe("", "self", []string{"拓扑中没有这台设备"})
	if err != nil {
		t.Fatal(err)
	}
	status := waitForProbeTask(t, fixture.runner, taskID)
	if status.State != "error" || status.Message == "" {
		t.Fatalf("参数错误应进入失败状态：%+v", status)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"probe-connectivity"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" {
		t.Fatalf("失败任务也应写审计：%+v", rows)
	}
}

func waitForProbeTask(t *testing.T, runner webui.TopologyProbe, taskID string) webui.TopologyProbeTaskStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := runner.ProbeStatus(taskID)
		if !ok {
			t.Fatalf("任务 %s 不存在", taskID)
		}
		if status.State != "running" {
			return *status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待探测任务完成超时")
	return webui.TopologyProbeTaskStatus{}
}
