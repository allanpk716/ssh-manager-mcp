package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAuthorityServerWizardStagesCredentialForReverify 覆盖第 1 步的五分
// 钟重新验证守卫：未重新验证时不写入；重新验证后暂存凭据只续交一次，页面
// 不回显凭据内容。
func TestAuthorityServerWizardStagesCredentialForReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: store, ServerWriter: writer, Topology: stubAddTopologySource{},
		Access: &stubAccessStore{}, Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	formPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new", cookie)
	formBody := readResponseBody(t, formPage)
	csrf := hiddenInputValue(t, formBody, "csrf_token")
	submit := url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"login-secret"}, "shell": {"auto"},
	}

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", submit, cookie)
	if rejected.StatusCode != http.StatusSeeOther ||
		rejected.Header.Get("Location") != `/ui/reverify?next=`+url.QueryEscape("/ui/servers/new?resume=1") {
		t.Fatalf("带凭据提交应先重新验证，状态码=%d 跳转=%q", rejected.StatusCode, rejected.Header.Get("Location"))
	}
	if len(writer.adds) != 0 {
		t.Fatalf("未重新验证时不得写入服务器，实际 %+v", writer.adds)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/reverify?next="+url.QueryEscape("/ui/servers/new?resume=1"), cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {"/ui/servers/new?resume=1"},
	}, cookie)
	if reverified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证后应回到新增表单，状态码=%d", reverified.StatusCode)
	}

	resumed := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new?resume=1", cookie)
	resumedBody := readResponseBody(t, resumed)
	for _, want := range []string{"已保留表单状态", `name="resume_credentials" value="1"`} {
		if !strings.Contains(resumedBody, want) {
			t.Fatalf("重新验证后的表单应携带暂存凭据标记，缺少 %q：\n%s", want, resumedBody)
		}
	}
	if strings.Contains(resumedBody, "login-secret") {
		t.Fatalf("暂存凭据不得回显到页面：\n%s", resumedBody)
	}
	delete(submit, "password")
	submit.Set("resume_credentials", "1")
	accepted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", submit, cookie)
	if accepted.StatusCode != http.StatusSeeOther || !strings.Contains(accepted.Header.Get("Location"), "/ui/servers/new/test?") {
		t.Fatalf("重新验证后应续交凭据并进入连接测试，状态码=%d 跳转=%q", accepted.StatusCode, accepted.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].Password != "login-secret" {
		t.Fatalf("应沿用暂存登录密码写入，实际 %+v", writer.adds)
	}
	if again := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new?resume=1", cookie); !strings.Contains(readResponseBody(t, again), "已超过五分钟") {
		t.Fatalf("暂存凭据只能使用一次：\n%s", readResponseBody(t, again))
	}
}

func TestServerWizardDeviceContextKeepsExistingMachine(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)

	page := serversGet(t, h, cookie, "/ui/servers/new?device="+url.QueryEscape("工作站"))
	for _, want := range []string{
		"将新增到机器 工作站 上的另一个账号",
		`name="device_choice" value="existing"`,
		`name="existing_device" value="工作站"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("设备详情入口应显示并保留同一台机器，缺少 %q：\n%s", want, page)
		}
	}
	response := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, page, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"none"}, "shell": {"auto"},
		"device_choice": {"existing"}, "existing_device": {"工作站"},
	})
	if response.StatusCode != http.StatusSeeOther || !strings.Contains(response.Header.Get("Location"), "same_machine=true") {
		t.Fatalf("挂到已有机器时应跳过地点步骤，状态码=%d 跳转=%s", response.StatusCode, response.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].DeviceChoice != "existing" || writer.adds[0].ExistingDeviceName != "工作站" {
		t.Fatalf("提交应沿用已有机器，实际 %+v", writer.adds)
	}
}

func TestServerWizardRejectsHalfFilledAddressRow(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)
	page := serversGet(t, h, cookie, "/ui/servers/new")

	response := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, page, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"家A局域网"}, "address": {""},
		"credential_action": {"none"}, "shell": {"auto"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "每行网络地址必须同时选择网络并填写地址") {
		t.Fatalf("半填的地址行应被拒绝，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if len(writer.adds) != 0 {
		t.Fatalf("校验失败不得写入，实际 %+v", writer.adds)
	}
}

func TestServerWizardAddAddressStagesCredentialWithoutScript(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)
	page := serversGet(t, h, cookie, "/ui/servers/new")
	csrf := hiddenInputValue(t, page, "csrf_token")

	appended := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"login-secret"},
		"shell": {"auto"}, "add_address": {"1"},
	})
	appendedBody := readResponseBody(t, appended)
	if appended.StatusCode != http.StatusOK || !strings.Contains(appendedBody, `name="resume_credentials" value="1"`) {
		t.Fatalf("无脚本追加地址行应暂存凭据，状态码=%d 页面：\n%s", appended.StatusCode, appendedBody)
	}
	if strings.Contains(appendedBody, "login-secret") {
		t.Fatalf("追加行时不得回显登录密码：%s", appendedBody)
	}
	saved := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other", "other"}, "address": {"192.0.2.20", "192.0.2.21"},
		"credential_action": {"password"}, "resume_credentials": {"1"}, "shell": {"auto"},
	})
	if saved.StatusCode != http.StatusSeeOther || len(writer.adds) != 1 || writer.adds[0].Password != "login-secret" {
		t.Fatalf("最终提交应沿用暂存密码，状态码=%d 写入=%+v", saved.StatusCode, writer.adds)
	}
}
