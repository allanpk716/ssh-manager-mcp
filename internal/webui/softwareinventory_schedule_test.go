package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSoftwareInventoryScheduleAndCollectAllForms 覆盖首页的全局每天采集
// 时间表单与「立即全部采集」报告。
func TestSoftwareInventoryScheduleAndCollectAllForms(t *testing.T) {
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", StatusLabels: []string{"正常"},
		}},
		collectAllResult: SoftwareInventoryCollectionRoundResult{
			Succeeded: 1, Requested: 1, Failed: 1,
			Devices: []SoftwareInventoryCollectionDeviceResult{
				{DeviceName: "工作站"},
				{DeviceName: "备份机", Requested: true, CollectorName: "mini6"},
				{DeviceName: "失败设备", Err: "连接超时，端口不通"},
			},
		},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	home := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/")
	homeBody := readResponseBody(t, home)
	if !strings.Contains(homeBody, `name="daily_time"`) ||
		!strings.Contains(homeBody, `value="01:00"`) ||
		!strings.Contains(homeBody, "立即全部采集") {
		t.Fatalf("首页应显示默认每天采集时间与立即全部采集入口：\n%s", homeBody)
	}
	csrf := hiddenInputMetaOrHiddenValue(t, homeBody)

	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/schedule",
		url.Values{"csrf_token": {csrf}, "daily_time": {"02:30"}},
	)
	if response.StatusCode != http.StatusSeeOther ||
		response.Header.Get("Location") != "/ui/software-inventory/?schedule=saved" {
		t.Fatalf("保存每天采集时间应跳回首页，状态码=%d Location=%s",
			response.StatusCode, response.Header.Get("Location"))
	}
	if !source.dailyTimeSetCalled || source.dailyTimeValue != "02:30" ||
		source.dailyTimeAddress != "198.51.100.9" {
		t.Fatalf("每天采集时间应交给来源接口：%+v", source)
	}
	savedPage := doGetWithCookie(t, handler, cookie, response.Header.Get("Location"))
	savedBody := readResponseBody(t, savedPage)
	if !strings.Contains(savedBody, "每天采集时间已保存。") || !strings.Contains(savedBody, `value="02:30"`) {
		t.Fatalf("保存后首页应显示提示与新时间：\n%s", savedBody)
	}

	response = postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/collect-all",
		url.Values{"csrf_token": {csrf}},
	)
	if response.StatusCode != http.StatusSeeOther ||
		response.Header.Get("Location") != "/ui/software-inventory/?collect_all=done" {
		t.Fatalf("立即全部采集应跳回首页，状态码=%d Location=%s",
			response.StatusCode, response.Header.Get("Location"))
	}
	if !source.collectAllCalled || source.collectAllAddress != "198.51.100.9" {
		t.Fatalf("立即全部采集应交给来源接口：%+v", source)
	}
	reportPage := doGetWithCookie(t, handler, cookie, response.Header.Get("Location"))
	reportBody := readResponseBody(t, reportPage)
	for _, wanted := range []string{
		"立即全部采集完成：成功 1 台，等待客户端实例 1 台，失败 1 台。",
		"工作站：采集成功", "备份机：已登记，等待客户端实例 mini6 执行",
		"失败设备：失败，连接超时，端口不通",
	} {
		if !strings.Contains(reportBody, wanted) {
			t.Fatalf("立即全部采集报告缺少 %q：\n%s", wanted, reportBody)
		}
	}
}
