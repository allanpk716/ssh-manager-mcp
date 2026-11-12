package webuiservers

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func integrationFormCSRF(t *testing.T, h sessionHandler, target string) string {
	t.Helper()
	response := h.do(http.MethodGet, target, nil)
	body := integrationBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("读取 %s 失败，状态码=%d：\n%s", target, response.StatusCode, body)
	}
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("页面 %s 应携带防跨站请求伪造令牌：\n%s", target, body)
	}
	return match[1]
}

func TestHandlerOtherDeviceAddTypeSwitchAndFieldVisibility(t *testing.T) {
	_, _, h := newIntegrationHandler(t)

	typePage := h.do(http.MethodGet, "/ui/add", nil)
	typeBody := integrationBody(t, typePage)
	if typePage.StatusCode != http.StatusOK || !strings.Contains(typeBody, ">服务器</strong>") ||
		!strings.Contains(typeBody, ">其他设备</strong>") {
		t.Fatalf("类型选择页应同时提供服务器与其他设备，状态码=%d：\n%s", typePage.StatusCode, typeBody)
	}

	formPage := h.do(http.MethodGet, "/ui/devices/new", nil)
	formBody := integrationBody(t, formPage)
	if formPage.StatusCode != http.StatusOK || !strings.Contains(formBody, "新增其他设备") ||
		!strings.Contains(formBody, "设备名") || !strings.Contains(formBody, "备注") ||
		!strings.Contains(formBody, "站点") || !strings.Contains(formBody, "物理局域网") ||
		!strings.Contains(formBody, "虚拟局域网成员") {
		t.Fatalf("其他设备表单应提供设备与站点与网络成员字段，状态码=%d：\n%s", formPage.StatusCode, formBody)
	}
	for _, forbidden := range []string{"登录凭据", "profile 授权", "shell 类型", "客户端实例", "向 agent 展示主机地址", "连接测试"} {
		if strings.Contains(formBody, forbidden) {
			t.Fatalf("其他设备表单不应显示 %q：\n%s", forbidden, formBody)
		}
	}
}

func TestHandlerOtherDeviceAddSavesMembershipsAndWritesWebUIAudit(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}

	response := h.do(http.MethodPost, "/ui/devices/new", url.Values{
		"csrf_token":  {integrationFormCSRF(t, h, "/ui/devices/new")},
		"device_name": {"网络存储"}, "note": {"共享照片与备份"},
		"site": {"家A"}, "physical_network": {"家A局域网"}, "physical_address": {"192.168.1.50"},
		"virtual_network": {"虚拟网", ""}, "virtual_address": {"100.64.0.50", ""},
	})
	body := integrationBody(t, response)
	if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/ui/devices/new/result?device=") {
		t.Fatalf("新增其他设备应进入设备结果页，状态码=%d 跳转=%s：\n%s", response.StatusCode, response.Header.Get("Location"), body)
	}

	device, err := st.GetDevice("网络存储")
	if err != nil || device == nil || device.Note != "共享照片与备份" {
		t.Fatalf("其他设备应保存名称与备注：%v %+v", err, device)
	}
	memberships, err := st.ListNetworkMemberships("网络存储")
	if err != nil || len(memberships) != 2 {
		t.Fatalf("其他设备应保存物理与虚拟网络成员：%v %+v", err, memberships)
	}
	if servers, err := st.ListServers(); err != nil || len(servers) != 0 {
		t.Fatalf("其他设备路径不得创建服务器条目：%v %v", servers, err)
	}

	deviceRows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"device.add"}})
	if err != nil || len(deviceRows) != 1 {
		t.Fatalf("设备登记应写一条 device.add 审计：%v %+v", err, deviceRows)
	}
	memberRows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"device.member-add"}})
	if err != nil || len(memberRows) != 2 {
		t.Fatalf("两条网络成员应各写一条 device.member-add 审计：%v %+v", err, memberRows)
	}
	for _, row := range append(deviceRows, memberRows...) {
		if row.Source != store.AuditSourceWebUI {
			t.Fatalf("设备路径审计来源应为 Web UI：%+v", row)
		}
	}
}

func TestHandlerOtherDeviceAddValidationKeepsFieldsAndCreatesNothing(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("占用设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("占用设备", "家A局域网", "192.168.1.60"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		form   url.Values
		notice string
	}{
		{
			name: "站点需要物理成员",
			form: url.Values{
				"device_name": {"半位置设备"}, "note": {"保留备注"}, "site": {"家A"},
			},
			notice: "填写站点时必须同时填写物理局域网与地址",
		},
		{
			name: "网络与地址必须成对",
			form: url.Values{
				"device_name": {"半位置设备"}, "note": {"保留备注"},
				"physical_network": {"家A局域网"},
			},
			notice: "网络成员必须同时填写网络与地址",
		},
		{
			name: "地址也必须选择网络",
			form: url.Values{
				"device_name": {"半位置设备"}, "note": {"保留备注"},
				"physical_address": {"192.168.1.61"}, "virtual_network": {"", "虚拟网"}, "virtual_address": {"", ""},
			},
			notice: "网络成员必须同时填写网络与地址",
		},
		{
			name: "地址冲突显示占用设备",
			form: url.Values{
				"device_name": {"半位置设备"}, "note": {"保留备注"},
				"physical_network": {"家A局域网"}, "physical_address": {"192.168.1.60"},
			},
			notice: "网络 家A局域网 的地址 192.168.1.60 已被设备 占用设备 占用",
		},
		{
			name: "设备重名沿用现有错误",
			form: url.Values{
				"device_name": {"占用设备"}, "note": {"保留备注"},
			},
			notice: "设备名已存在：占用设备",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.form.Set("csrf_token", integrationFormCSRF(t, h, "/ui/devices/new"))
			response := h.do(http.MethodPost, "/ui/devices/new", testCase.form)
			body := integrationBody(t, response)
			if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, testCase.notice) ||
				!strings.Contains(body, `value="`+testCase.form.Get("device_name")+`"`) ||
				!strings.Contains(body, "保留备注") {
				t.Fatalf("校验失败应显示错误并保留字段，状态码=%d：\n%s", response.StatusCode, body)
			}
			if device, err := st.GetDevice("半位置设备"); err == nil || device != nil {
				t.Fatalf("校验失败不得创建设备：%v %+v", err, device)
			}
		})
	}
}

func TestHandlerOtherDeviceAddWithoutLocationCreatesUnassignedDevice(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	response := h.do(http.MethodPost, "/ui/devices/new", url.Values{
		"csrf_token":  {integrationFormCSRF(t, h, "/ui/devices/new")},
		"device_name": {"暂不归类设备"}, "note": {"稍后补充位置"},
	})
	body := integrationBody(t, response)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("站点与网络成员留空应保存为未归类设备，状态码=%d：\n%s", response.StatusCode, body)
	}
	memberships, err := st.ListNetworkMemberships("暂不归类设备")
	if err != nil || len(memberships) != 0 {
		t.Fatalf("站点与网络成员留空不应创建网络成员：%v %+v", err, memberships)
	}
	result := h.do(http.MethodGet, response.Header.Get("Location"), nil)
	resultBody := integrationBody(t, result)
	if result.StatusCode != http.StatusOK || !strings.Contains(resultBody, "暂不归类设备") ||
		!strings.Contains(resultBody, "不进入服务器连接测试队列") {
		t.Fatalf("设备结果页应说明不做连接测试，状态码=%d：\n%s", result.StatusCode, resultBody)
	}
}
