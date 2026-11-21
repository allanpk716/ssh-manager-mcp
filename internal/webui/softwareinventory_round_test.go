package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSoftwareInventoryCollectAllStartsRoundAndRedirects 验证「立即全部采
// 集」改为发起后台采集轮次并立即重定向到进度页。
func TestSoftwareInventoryCollectAllStartsRoundAndRedirects(t *testing.T) {
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", StatusLabels: []string{"正常"},
		}},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	home := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/")
	homeBody := readResponseBody(t, home)
	if !strings.Contains(homeBody, "立即全部采集") {
		t.Fatalf("首页应显示立即全部采集入口：\n%s", homeBody)
	}
	csrf := hiddenInputMetaOrHiddenValue(t, homeBody)

	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/collect-all",
		url.Values{"csrf_token": {csrf}},
	)
	if response.StatusCode != http.StatusSeeOther ||
		response.Header.Get("Location") != "/ui/software-inventory/round" {
		t.Fatalf("立即全部采集应跳到进度页，状态码=%d Location=%s",
			response.StatusCode, response.Header.Get("Location"))
	}
	if !source.startRoundCalled || source.startRoundDevice != "" ||
		source.startRoundAddress != "198.51.100.9" {
		t.Fatalf("立即全部采集应交给来源接口发起全部轮次：%+v", source)
	}
}

// TestSoftwareInventoryCollectAllFailureShowsError 验证发起轮次失败时返回
// 整页错误而不是跳转。
func TestSoftwareInventoryCollectAllFailureShowsError(t *testing.T) {
	source := &stubSoftwareInventorySource{
		startRoundErr: errors.New("存储不可用"),
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	home := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, home))

	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/collect-all",
		url.Values{"csrf_token": {csrf}},
	)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("发起轮次失败应返回 500，得到 %d", response.StatusCode)
	}
	body := readResponseBody(t, response)
	if !strings.Contains(body, "立即全部采集失败") {
		t.Fatalf("失败页面应说明立即全部采集失败：\n%s", body)
	}
}

// TestSoftwareInventoryCollectAllWithCancelledRequestContext 验证提交「立即
// 全部采集」的请求到达时上下文已被取消（浏览器提交后立刻断开），处理函数
// 照样完成发起并跳转进度页：发起的采集轮次使用进程自己的上下文，不随请求
// 取消。
func TestSoftwareInventoryCollectAllWithCancelledRequestContext(t *testing.T) {
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", StatusLabels: []string{"正常"},
		}},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	home := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, home))

	// 手工构造请求（现成的表单提交辅助函数没有注入请求上下文的入口），
	// 把已取消的上下文挂到请求上，模拟提交后立刻断开的浏览器。
	request := httptest.NewRequest(http.MethodPost, "/ui/software-inventory/collect-all",
		strings.NewReader(url.Values{"csrf_token": {csrf}}.Encode()))
	request.Host = "127.0.0.1:18080"
	request.RemoteAddr = "198.51.100.9:45678"
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:18080")
	requestCtx, disconnect := context.WithCancel(request.Context())
	request = request.WithContext(requestCtx)
	disconnect()
	if request.Context().Err() == nil {
		t.Fatal("前置条件：请求上下文应已取消")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	response := recorder.Result()
	if response.StatusCode != http.StatusSeeOther ||
		response.Header.Get("Location") != "/ui/software-inventory/round" {
		t.Fatalf("请求上下文已取消时也应照常发起并跳转进度页，状态码=%d Location=%s",
			response.StatusCode, response.Header.Get("Location"))
	}
	if !source.startRoundCalled {
		t.Fatal("请求上下文已取消时也应发起采集轮次")
	}
}

func roundViewForTest() *SoftwareInventoryRoundView {
	return &SoftwareInventoryRoundView{
		StartedAt:   time.Date(2026, 11, 21, 16, 50, 5, 0, time.Local),
		Running:     true,
		Refreshing:  true,
		StatusText:  "采集进行中",
		SummaryText: "已完成 0/3 台，成功 0 台，等待客户端实例 1 台，失败 0 台",
		Devices: []SoftwareInventoryRoundDeviceView{
			{DeviceName: "工作站", StatusText: "采集中"},
			{DeviceName: "备份机", StatusText: "排队中"},
			{DeviceName: "客户端采集设备", StatusText: "已登记，等待客户端实例 mini6 执行，实例从未同步"},
		},
	}
}

// TestSoftwareInventoryRoundPageRendersRunningRound 验证进度页逐台渲染各状
// 态，进行中带定时自动刷新属性。
func TestSoftwareInventoryRoundPageRendersRunningRound(t *testing.T) {
	source := &stubSoftwareInventorySource{roundView: roundViewForTest()}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/round")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("进度页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "<h1>采集进度</h1>") {
		t.Fatalf("进度页应有标题：\n%s", body)
	}
	if !strings.Contains(body, `section class="software-inventory-round" id="software-inventory-round"`) ||
		!strings.Contains(body, `hx-get="/ui/software-inventory/round"`) ||
		!strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Fatalf("进行中的进度页应带每 2 秒自动刷新：\n%s", body)
	}
	if !strings.Contains(body, "采集进行中：已完成 0/3 台，成功 0 台，等待客户端实例 1 台，失败 0 台") {
		t.Fatalf("进度页应显示状态与汇总：\n%s", body)
	}
	for _, wanted := range []string{
		"<h3>工作站</h3>", "采集中",
		"<h3>备份机</h3>", "排队中",
		"<h3>客户端采集设备</h3>", "已登记，等待客户端实例 mini6 执行，实例从未同步",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("进度页缺少 %q：\n%s", wanted, body)
		}
	}
}

// TestSoftwareInventoryRoundPageRendersFinishedRound 验证结束后进度页不再
// 带自动刷新属性，成功与失败设备带说明。
func TestSoftwareInventoryRoundPageRendersFinishedRound(t *testing.T) {
	view := roundViewForTest()
	view.Running = false
	view.Refreshing = false
	view.StatusText = "采集已完成"
	view.SummaryText = "已完成 2/2 台，成功 1 台，等待客户端实例 0 台，失败 1 台"
	view.Devices = []SoftwareInventoryRoundDeviceView{
		{DeviceName: "工作站", StatusText: "成功，共 8 条"},
		{DeviceName: "备份机", StatusText: "失败，连接超时，端口不通"},
	}
	source := &stubSoftwareInventorySource{roundView: view}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/round")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("进度页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	if strings.Contains(body, "hx-trigger") || strings.Contains(body, `id="software-inventory-round"`) {
		t.Fatalf("结束后的进度页不应再自动刷新：\n%s", body)
	}
	for _, wanted := range []string{
		"采集已完成：已完成 2/2 台，成功 1 台，等待客户端实例 0 台，失败 1 台",
		"<h3>工作站</h3>", "成功，共 8 条",
		"<h3>备份机</h3>", "失败，连接超时，端口不通",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("进度页缺少 %q：\n%s", wanted, body)
		}
	}
}

// TestSoftwareInventoryRoundPageShowsSingleDeviceScope 验证单台轮次的进度
// 页在整轮状态前标注采集范围，避免误读成全部采集。
func TestSoftwareInventoryRoundPageShowsSingleDeviceScope(t *testing.T) {
	view := roundViewForTest()
	view.ScopeText = "本轮只采集设备 工作站"
	view.SummaryText = "已完成 0/1 台，成功 0 台，等待客户端实例 0 台，失败 0 台"
	view.Devices = []SoftwareInventoryRoundDeviceView{
		{DeviceName: "工作站", StatusText: "采集中"},
	}
	source := &stubSoftwareInventorySource{roundView: view}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	body := readResponseBody(t, doGetWithCookie(t, handler, cookie, "/ui/software-inventory/round"))
	if !strings.Contains(body, "本轮只采集设备 工作站 · 采集进行中：已完成 0/1 台，成功 0 台，等待客户端实例 0 台，失败 0 台") {
		t.Fatalf("单台轮次进度页应在状态前标注采集范围：\n%s", body)
	}
}

// TestSoftwareInventoryRoundPageWithoutRound 验证没有轮次时进度页显示提示。
func TestSoftwareInventoryRoundPageWithoutRound(t *testing.T) {
	source := &stubSoftwareInventorySource{}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/round")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("进度页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "没有进行中的采集") {
		t.Fatalf("没有轮次时进度页应显示提示：\n%s", body)
	}
	if strings.Contains(body, "hx-trigger") {
		t.Fatalf("没有轮次的进度页不应自动刷新：\n%s", body)
	}
}

// TestSoftwareInventoryRoundPageError 验证读取轮次失败返回整页错误。
func TestSoftwareInventoryRoundPageError(t *testing.T) {
	source := &stubSoftwareInventorySource{roundErr: errors.New("存储不可用")}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/round")
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("读取轮次失败应返回 500，得到 %d", response.StatusCode)
	}
}

// TestSoftwareInventoryHomeShowsRoundEntry 验证首页显示进行中或最近一轮采
// 集的入口与汇总，没有轮次时不出现。
func TestSoftwareInventoryHomeShowsRoundEntry(t *testing.T) {
	running := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{Name: "工作站", StatusLabels: []string{"正常"}}},
		roundView: &SoftwareInventoryRoundView{
			StartedAt:   time.Date(2026, 11, 21, 16, 50, 5, 0, time.Local),
			Running:     true,
			Refreshing:  true,
			StatusText:  "采集进行中",
			ScopeText:   "本轮只采集设备 工作站",
			SummaryText: "已完成 1/3 台，成功 1 台，等待客户端实例 0 台，失败 0 台",
		},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, running)
	body := readResponseBody(t, doGetWithCookie(t, handler, cookie, "/ui/software-inventory/"))
	for _, wanted := range []string{
		`href="/ui/software-inventory/round"`, ">采集进行中</a>",
		"2026-11-21 16:50:05", "本轮只采集设备 工作站",
		"已完成 1/3 台，成功 1 台，等待客户端实例 0 台，失败 0 台",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("首页进行中入口缺少 %q：\n%s", wanted, body)
		}
	}

	latest := &stubSoftwareInventorySource{
		devices:   running.devices,
		roundView: &SoftwareInventoryRoundView{StartedAt: time.Date(2026, 11, 21, 16, 50, 5, 0, time.Local), SummaryText: "已完成 3/3 台，成功 3 台，等待客户端实例 0 台，失败 0 台"},
	}
	handler, cookie = newSoftwareInventoryHandler(t, RoleStandalone, latest)
	body = readResponseBody(t, doGetWithCookie(t, handler, cookie, "/ui/software-inventory/"))
	if !strings.Contains(body, ">最近一轮采集</a>") {
		t.Fatalf("首页应显示最近一轮采集入口：\n%s", body)
	}

	none := &stubSoftwareInventorySource{devices: running.devices}
	handler, cookie = newSoftwareInventoryHandler(t, RoleStandalone, none)
	body = readResponseBody(t, doGetWithCookie(t, handler, cookie, "/ui/software-inventory/"))
	if strings.Contains(body, `href="/ui/software-inventory/round"`) {
		t.Fatalf("没有轮次时首页不应出现轮次入口：\n%s", body)
	}
}
