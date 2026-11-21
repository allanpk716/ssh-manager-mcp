package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSoftwareInventoryScheduleFormSavesDailyTime 覆盖首页的全局每天采集时
// 间表单。「立即全部采集」改为后台采集轮次，重定向与进度页断言在
// softwareinventory_round_test.go 里。
func TestSoftwareInventoryScheduleFormSavesDailyTime(t *testing.T) {
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", StatusLabels: []string{"正常"},
		}},
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
}
