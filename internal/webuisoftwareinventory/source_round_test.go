package webuisoftwareinventory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// waitRoundView 轮询读取轮次视图，直到满足条件或超时。
func waitRoundView(
	t *testing.T,
	source webui.SoftwareInventorySource,
	done func(view *webui.SoftwareInventoryRoundView) bool,
) *webui.SoftwareInventoryRoundView {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		view, err := source.SoftwareInventoryRound()
		if err != nil {
			t.Fatalf("读取采集轮次失败：%v", err)
		}
		if view != nil && done(view) {
			return view
		}
		select {
		case <-deadline:
			t.Fatal("采集轮次没有在时限内达到预期状态")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSourceStartsRoundAndBuildsRoundView 覆盖适配层的轮次接线：发起后本机
// 设备成功、客户端实例设备等待并带实例名，视图的汇总、状态与刷新标记齐
// 全；等待设备结果落库后切换到已完成。
func TestSourceStartsRoundAndBuildsRoundView(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	addTestDevice(t, st, "客户端采集设备")
	servers, err := st.FullServersForDevice("客户端采集设备")
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("采集执行客户端")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{servers[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("mini6", profileID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(
		"客户端采集设备", "mini6", "", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	open := func() (*store.Store, func(), error) { return st, func() {}, nil }
	rounds := softwareinventory.NewRoundManager(open)
	rounds.Collect = func(_ context.Context, request softwareinventory.CollectRequest) (*softwareinventory.CollectionReport, error) {
		return &softwareinventory.CollectionReport{
			DeviceName: request.DeviceName,
			ItemCounts: map[models.SoftwareCategory]int{
				softwareinventory.CategoryServiceProgram: 3,
			},
		}, nil
	}
	source := FromOpenerWithRoundManager(open, rounds)

	view, err := source.SoftwareInventoryRound()
	if err != nil {
		t.Fatal(err)
	}
	if view != nil {
		t.Fatalf("没有轮次时应返回 nil，得到 %+v", view)
	}

	if err := source.StartSoftwareInventoryCollectionRound(
		"", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	view = waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return !view.Running
	})
	if !view.Refreshing {
		t.Fatalf("仍有等待设备的轮次应继续刷新：%+v", view)
	}
	if view.StatusText != "本机采集已完成，等待客户端实例回传结果" {
		t.Fatalf("整轮状态 = %q，应为等待客户端实例回传结果", view.StatusText)
	}
	if view.ScopeText != "" {
		t.Fatalf("全部轮次不应有采集范围说明：%q", view.ScopeText)
	}
	if view.SummaryText != "已完成 1/2 台，成功 1 台，等待客户端实例 1 台，失败 0 台" {
		t.Fatalf("汇总 = %q", view.SummaryText)
	}
	statusByText := map[string]string{}
	for _, device := range view.Devices {
		statusByText[device.DeviceName] = device.StatusText
	}
	if statusByText["工作站"] != "成功，共 3 条" {
		t.Fatalf("本机设备文案 = %q", statusByText["工作站"])
	}
	if statusByText["客户端采集设备"] != "已登记，等待客户端实例 mini6 执行，实例从未同步" {
		t.Fatalf("等待设备文案 = %q", statusByText["客户端采集设备"])
	}
	if status, err := st.SoftwareCollectionStatusForDevice("客户端采集设备"); err != nil || !status.PendingImmediate {
		t.Fatalf("等待设备应登记立即采集请求：%+v %v", status, err)
	}

	// 客户端实例在轮次开始之后上交新结果：等待设备转成功，页面停止刷新。
	arrivedAt := time.Now().Add(time.Minute)
	st.NowFn = func() time.Time { return arrivedAt }
	if err := st.ReplaceSoftwareObservedItems("客户端采集设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
		},
		ServerID:          servers[0].ID,
		CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	view = waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return !view.Refreshing
	})
	if view.StatusText != "采集已完成" {
		t.Fatalf("全部设备出结果后状态 = %q", view.StatusText)
	}
	if view.SummaryText != "已完成 2/2 台，成功 2 台，等待客户端实例 0 台，失败 0 台" {
		t.Fatalf("完成后的汇总 = %q", view.SummaryText)
	}
	statusByText = map[string]string{}
	for _, device := range view.Devices {
		statusByText[device.DeviceName] = device.StatusText
	}
	if statusByText["客户端采集设备"] != "成功，共 1 条" {
		t.Fatalf("等待转成功文案 = %q", statusByText["客户端采集设备"])
	}
}

// TestSourceStartsSingleDeviceRound 覆盖适配层的单台轮次接线：发起后只含
// 这一台设备，客户端实例执行者登记等待；有轮次进行中时再发起复用当前轮
// 不新建；等待设备结果落库后转成功并停止刷新。
func TestSourceStartsSingleDeviceRound(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	addTestDevice(t, st, "客户端采集设备")
	servers, err := st.FullServersForDevice("客户端采集设备")
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("采集执行客户端")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{servers[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("mini6", profileID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(
		"客户端采集设备", "mini6", "", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	// 工作站采集中先卡住不动，供后面验证进行中轮次的复用。
	inWorkstation := make(chan struct{})
	releaseWorkstation := make(chan struct{})
	open := func() (*store.Store, func(), error) { return st, func() {}, nil }
	rounds := softwareinventory.NewRoundManager(open)
	rounds.Collect = func(_ context.Context, request softwareinventory.CollectRequest) (*softwareinventory.CollectionReport, error) {
		if request.DeviceName == "工作站" {
			close(inWorkstation)
			<-releaseWorkstation
		}
		return &softwareinventory.CollectionReport{
			DeviceName: request.DeviceName,
			ItemCounts: map[models.SoftwareCategory]int{
				softwareinventory.CategoryServiceProgram: 3,
			},
		}, nil
	}
	source := FromOpenerWithRoundManager(open, rounds)

	if err := source.StartSoftwareInventoryCollectionRound(
		"客户端采集设备", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	view := waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return !view.Running
	})
	if len(view.Devices) != 1 || view.Devices[0].DeviceName != "客户端采集设备" {
		t.Fatalf("单台轮次应只包含客户端采集设备：%+v", view.Devices)
	}
	if view.StatusText != "本机采集已完成，等待客户端实例回传结果" {
		t.Fatalf("整轮状态 = %q", view.StatusText)
	}
	if !view.Refreshing {
		t.Fatalf("仍有等待设备的轮次应继续刷新：%+v", view)
	}
	if view.SummaryText != "已完成 0/1 台，成功 0 台，等待客户端实例 1 台，失败 0 台" {
		t.Fatalf("汇总 = %q", view.SummaryText)
	}
	if !strings.Contains(view.Devices[0].StatusText, "已登记，等待客户端实例 mini6 执行") {
		t.Fatalf("等待设备文案 = %q", view.Devices[0].StatusText)
	}
	if view.ScopeText != "本轮只采集设备 客户端采集设备" {
		t.Fatalf("单台轮次的采集范围说明 = %q", view.ScopeText)
	}

	// 客户端实例在轮次开始之后上交新结果：等待设备转成功，页面停止刷新。
	arrivedAt := time.Now().Add(time.Minute)
	st.NowFn = func() time.Time { return arrivedAt }
	if err := st.ReplaceSoftwareObservedItems("客户端采集设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
		},
		ServerID:          servers[0].ID,
		CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	view = waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return !view.Refreshing
	})
	if view.StatusText != "采集已完成" {
		t.Fatalf("结果送达后的整轮状态 = %q", view.StatusText)
	}
	if view.SummaryText != "已完成 1/1 台，成功 1 台，等待客户端实例 0 台，失败 0 台" {
		t.Fatalf("完成后的汇总 = %q", view.SummaryText)
	}
	if view.Devices[0].StatusText != "成功，共 1 条" {
		t.Fatalf("等待转成功文案 = %q", view.Devices[0].StatusText)
	}

	// 互斥复用：有轮次进行中（本机部分未结束）时再发起，无论范围都复用当
	// 前轮，不新建（等待客户端结果的轮次不算进行中，不拦新轮次）。
	if err := source.StartSoftwareInventoryCollectionRound(
		"工作站", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	<-inWorkstation
	if err := source.StartSoftwareInventoryCollectionRound(
		"客户端采集设备", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	reused, err := source.SoftwareInventoryRound()
	if err != nil {
		t.Fatal(err)
	}
	if len(reused.Devices) != 1 || reused.Devices[0].DeviceName != "工作站" {
		t.Fatalf("进行中轮次存在时再发起应复用它，进度仍只有工作站：%+v", reused.Devices)
	}
	close(releaseWorkstation)
	final := waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return !view.Refreshing
	})
	if final.StatusText != "采集已完成" || len(final.Devices) != 1 ||
		final.Devices[0].DeviceName != "工作站" || final.Devices[0].StatusText != "成功，共 3 条" {
		t.Fatalf("工作站单台轮次应成功结束：%+v", final)
	}
}

// TestSourceRoundSurvivesCancelledRequestContext 验证带已取消的网页请求上
// 下文提交「立即全部采集」后，轮次照样在后台跑完：适配层发起轮次的接口不
// 接收请求上下文，轮次使用进程自己的根上下文执行。
func TestSourceRoundSurvivesCancelledRequestContext(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	open := func() (*store.Store, func(), error) { return st, func() {}, nil }
	rounds := softwareinventory.NewRoundManager(open)
	rounds.Collect = func(_ context.Context, request softwareinventory.CollectRequest) (*softwareinventory.CollectionReport, error) {
		return &softwareinventory.CollectionReport{
			DeviceName: request.DeviceName,
			ItemCounts: map[models.SoftwareCategory]int{
				softwareinventory.CategoryServiceProgram: 3,
			},
		}, nil
	}
	source := FromOpenerWithRoundManager(open, rounds)

	// 模拟发起「立即全部采集」的网页请求在提交后立刻断开：请求上下文已取
	// 消；适配层发起轮次的接口不接收它，轮次无从随请求取消。
	request := httptest.NewRequest(http.MethodPost, "/ui/software-inventory/collect-all", nil)
	requestCtx, disconnect := context.WithCancel(request.Context())
	request = request.WithContext(requestCtx)
	disconnect()
	if request.Context().Err() == nil {
		t.Fatal("前置条件：请求上下文应已取消")
	}

	if err := source.StartSoftwareInventoryCollectionRound(
		"", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	view := waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return view.StatusText == "采集已完成"
	})
	if view.SummaryText != "已完成 1/1 台，成功 1 台，等待客户端实例 0 台，失败 0 台" {
		t.Fatalf("轮次应在请求上下文取消后照样完成，汇总 = %q", view.SummaryText)
	}
}
