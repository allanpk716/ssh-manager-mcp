package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type stubAddTopologySource struct{}

func (stubAddTopologySource) Topology(string, string) (*TopologyData, error) {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-1", Name: "家A", LocationPath: "家A"},
			{ID: "site-2", Name: "公司", ParentID: "site-1", LocationPath: "家A / 公司"},
		},
		Networks: []TopologyNetwork{
			{ID: "net-physical", Name: "家A局域网", Type: "physical", SiteID: "site-1", SiteName: "家A"},
			{ID: "net-virtual", Name: "虚拟网", Type: "virtual"},
		},
		Devices: []TopologyDevice{{
			ID: "device-1", Name: "工作站", SiteID: "site-1", LocationPath: "家A",
			Servers: []TopologyDeviceServer{{ID: "srv-fine"}},
		}},
		Memberships: []TopologyMembership{{DeviceID: "device-1", NetworkID: "net-physical", Address: "192.168.1.20"}},
	}, nil
}

func TestServerWizardStepOneRunsConnectionTestBeforeNextStep(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	tester := &stubServerConnectionTester{}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, writer, tester)

	page := serversGet(t, h, cookie, "/ui/servers/new")
	for _, want := range []string{
		"连接与地址", `name="address_network"`, `value="other"`, "其他（公网或未登记网络）",
		"私钥文件", "改为粘贴私钥", "sudo 密码", "测试连接并下一步",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("第 1 步缺少 %q：\n%s", want, page)
		}
	}
	for _, forbidden := range []string{"站点", "网络成员", "兜底主机", "登录条目"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("第 1 步不得出现 %q：\n%s", forbidden, page)
		}
	}

	response := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, page, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"none"}, "shell": {"auto"}, "return_to": {"/ui/topology/"},
	})
	if response.StatusCode != http.StatusSeeOther || !strings.Contains(response.Header.Get("Location"), "/ui/servers/new/test?") {
		t.Fatalf("第 1 步应进入连接测试，状态码=%d 跳转=%s", response.StatusCode, response.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].Host != "192.0.2.20" || len(tester.starts) != 1 {
		t.Fatalf("第 1 步应先保存连接再启动测试：%+v %+v", writer.adds, tester.starts)
	}
}

func TestServerWizardTestGateAndFollowingSteps(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{
		ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22,
		DeviceName: "wizard 设备", Profiles: []ServerDetailProfile{{ID: "profile-1", Name: "运维"}},
	}
	writer := &stubServerWriter{}
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestSucceeded, ServerID: "srv-new",
		AnchorFingerprint: "SHA256:test-host-key", AnchorResult: "首次连接已记录主机密钥锚",
	}}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, writer, tester)

	testPage := serversGet(t, h, cookie, "/ui/servers/new/test?server=srv-new&task=task-1")
	if !strings.Contains(testPage, "SHA256:test-host-key") || !strings.Contains(testPage, "继续选择地点") {
		t.Fatalf("测试成功后才能进入地点步骤：\n%s", testPage)
	}

	sitePage := serversGet(t, h, cookie, "/ui/servers/new/site?server=srv-new&task=task-1")
	for _, want := range []string{"搜索地点", "家A / 公司", "当场新建地点", "新地点名字", "跳过地点"} {
		if !strings.Contains(sitePage, want) {
			t.Fatalf("地点步骤缺少 %q：\n%s", want, sitePage)
		}
	}

	profilePage := serversGet(t, h, cookie, "/ui/servers/new/profile?server=srv-new&task=task-1&site_action=new&new_site_name=机房")
	if !strings.Contains(profilePage, "将对 0 个客户端可见") || !strings.Contains(profilePage, "完成新增") {
		t.Fatalf("Profile 步骤应显示实时可见数并可跳过：\n%s", profilePage)
	}
	finish := serversPost(t, h, cookie, "/ui/servers/new/profile", url.Values{
		"csrf_token": {hiddenInputValue(t, profilePage, "csrf_token")},
		"server_id":  {"srv-new"}, "task_id": {"task-1"},
		"site_action": {"new"}, "new_site_name": {"机房"}, "profile_id": {"profile-1"},
	})
	if finish.StatusCode != http.StatusSeeOther || !strings.Contains(finish.Header.Get("Location"), "/ui/servers/new/result?") {
		t.Fatalf("完成新增应进入结果页，状态码=%d 跳转=%s", finish.StatusCode, finish.Header.Get("Location"))
	}
	if len(writer.finishes) != 1 || writer.finishes[0].form.NewSiteName != "机房" {
		t.Fatalf("完成新增应提交新地点与 profile：%+v", writer.finishes)
	}
}

func TestServerAddResultShowsConnectionTestOutcome(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{
		ID: "srv-new", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22, Shell: "posix",
	}
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestSucceeded, ServerID: "srv-new", ProbeShell: true,
		AnchorFingerprint: "SHA256:known-host-key-fingerprint", AnchorResult: "首次连接已记录主机密钥锚",
		HandshakeAddress: "100.64.0.20:22", ShellDetected: "powershell",
		ShellEvidence: "PowerShell 输出了版本表", ConnectionNotes: []string{"虚拟网络 第1个地址呈现了不同的主机密钥，已跳过"},
	}}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

	success := serversGet(t, h, cookie, "/ui/servers/new/result?server=srv-new&task=task-1")
	for _, want := range []string{
		"已验证", "SHA256:known-host-key-fingerprint", "首次连接已记录主机密钥锚",
		"100.64.0.20:22", "shell 类型已保存为 powershell",
	} {
		if !strings.Contains(success, want) {
			t.Fatalf("成功结果页缺少 %q：\n%s", want, success)
		}
	}
}

type stubServerConnectionStart struct {
	serverID   string
	probeShell bool
	source     ServerWriteSource
	anchor     ServerConnectionAnchor
}

type stubServerConnectionTester struct {
	starts []stubServerConnectionStart
	status *ServerConnectionTestStatus
}

func (s *stubServerConnectionTester) StartServerTest(
	serverID string, probeShell bool, source ServerWriteSource, anchor ServerConnectionAnchor,
) (string, error) {
	s.starts = append(s.starts, stubServerConnectionStart{
		serverID: serverID, probeShell: probeShell, source: source, anchor: anchor,
	})
	return "task-1", nil
}

func (s *stubServerConnectionTester) ServerTestStatus(taskID string) (*ServerConnectionTestStatus, bool) {
	if taskID != "task-1" {
		return nil, false
	}
	if s.status != nil {
		return s.status, true
	}
	return &ServerConnectionTestStatus{
		ID: taskID, State: ServerConnectionTestRunning, ServerID: "srv-new", ProbeShell: true,
	}, true
}

func newServersTestHandlerWithConnectionTester(
	t *testing.T, role Role, store ServerSource, writer ServerWriter, tester ServerConnectionTester,
) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role: role, Servers: store, ServerWriter: writer,
		ServerConnectionTester: tester, Topology: stubAddTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	response := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d 个", len(cookies))
	}
	return handler, cookies[0]
}

func newServersTestHandlerWithTopology(
	t *testing.T, role Role, store ServerSource, writer ServerWriter,
) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role: role, Servers: store, ServerWriter: writer, Topology: stubAddTopologySource{},
		Access: &stubAccessStore{}, VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	response := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d 个", len(cookies))
	}
	return handler, cookies[0]
}
