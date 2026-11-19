package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthorityClientListShowsInstancesAndPairingCard(t *testing.T) {
	store := pairingTestStore()
	store.clients = []ClientSummary{{
		ID: "client-1", Name: "笔记本", ProfileName: "运维", ServerCount: 2,
		LastPullAt:    time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
		ClientVersion: "v0.22.0", RecommendedVersion: "v0.23.0", MachineName: "工作站", Paired: true,
	}}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/clients/", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("客户端列表状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		"<h1>客户端</h1>", "笔记本", "运维 · 2 台服务器",
		"最后同步 2026-11-18 10:00:00", "客户端版本 v0.22.0", "机器 工作站",
		"经配对接入", "超过 1 天未同步", "可升级", "3 个配对请求待审批", `href="/ui/clients/pairing"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端列表应包含 %q：\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"设备码", "项目令牌", "站点", "网络成员", "登录条目", "兜底主机", "cache token"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("客户端列表不应出现 %q：\n%s", forbidden, body)
		}
	}
}

func TestAuthorityClientDetailShowsProfileStatusMachineAndResources(t *testing.T) {
	store := pairingTestStore()
	store.client = &ClientDetail{
		ClientSummary: ClientSummary{
			ID: "client-1", Name: "笔记本", ProfileName: "运维", ServerCount: 2,
			LastPullAt:    time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
			ClientVersion: "v0.22.0", MachineName: "工作站", Paired: true,
		},
		ProfileID: "profile-ops", RecommendedVersion: "v0.22.0", TokenPrefix: "abc123",
		Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}, {ID: "server-old", Name: "old-1"}},
		Machine: &ClientMachine{ID: "machine-1", Name: "工作站", SitePath: "公司 / 一号楼"},
		Software: &ClientSoftwareSummary{
			LastCollectedAt: time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC),
			ItemCount:       3, PendingExposureCount: 1, TopItems: []string{"Ubuntu 24.04.1", "nginx 1.24.0"},
		},
		Secrets: []ClientResource{{ID: "secret-1", Name: "MATRIX_TOKEN", Note: "浏览器测试机密项", ViaServers: []string{"db-1"}}},
		Shares:  []ClientResource{{ID: "share-1", Name: "datasets", Note: "临时数据", ViaServers: []string{"db-1"}}},
	}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/clients/client-1", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("客户端详情状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		"<h1>笔记本</h1>", "经配对接入", "profile", "运维", "web-1", "old-1",
		"最后同步", "客户端版本", "推荐版本", "v0.22.0", "与推荐版本一致",
		"设备码前缀", "abc123…", "所在机器", "机器 工作站", "公司 / 一号楼",
		"最近采集 2026-11-18 09:30:00 · 3 个软件 · 1 个待确认暴露",
		"Ubuntu 24.04.1", "查看全部", "立即采集",
		"它能用到的机密项与共享存储", "MATRIX_TOKEN", "浏览器测试机密项", "经过：db-1",
		"datasets", "临时数据", "共享存储 · 经过：db-1", "危险区", "移除客户端",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端详情应包含 %q：\n%s", want, body)
		}
	}
}

func TestAuthorityClientProfileChangeShowsPreviewBeforeWrite(t *testing.T) {
	store := pairingTestStore()
	store.client = &ClientDetail{
		ClientSummary: ClientSummary{ID: "client-1", Name: "笔记本", ProfileName: "运维"},
		ProfileID:     "profile-ops",
	}
	store.clientProfilePreview = &ClientProfilePreview{
		ClientName: "笔记本", CurrentProfileID: "profile-ops", CurrentProfileName: "运维",
		DesiredProfileID: "profile-dev", DesiredProfileName: "开发",
		AddedServers: []AccessServerRef{{ID: "server-db", Name: "db-1"}},
		LostServers:  []AccessServerRef{{ID: "server-web", Name: "web-1"}},
		AddedSecrets: []ClientResource{{ID: "secret-db", Name: "DB_TOKEN", Note: "数据库机密项", ViaServers: []string{"db-1"}}},
		LostSecrets:  []ClientResource{{ID: "secret-web", Name: "WEB_TOKEN", Note: "网站机密项", ViaServers: []string{"web-1"}}},
		AddedShares:  []ClientResource{{ID: "share-db", Name: "datasets", Note: "数据集"}},
		LostShares:   []ClientResource{{ID: "share-web", Name: "backups", Note: "备份"}},
	}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/profile?profile=profile-dev", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("更换 profile 预览状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		"<h1>更换 profile</h1>", "运维 → 开发", "将新增可见：1 台 · db-1", "将失去可见：1 台 · web-1",
		"将新增可用机密项：1 项 · DB_TOKEN", "将失去可用机密项：1 项 · WEB_TOKEN",
		"将新增可用共享存储：1 项 · datasets", "将失去可用共享存储：1 项 · backups",
		`name="expected_profile" value="profile-ops"`, `action="/ui/clients/client-1/profile"`, "确认更换",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("更换 profile 预览应包含 %q：\n%s", want, body)
		}
	}
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/profile", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-dev"}, "expected_profile": {"profile-ops"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/clients/client-1?notice=profile" {
		t.Fatalf("确认更换状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), responseBody)
	}
	if store.changeProfileClientID != "client-1" || store.changeProfileExpected != "profile-ops" ||
		store.changeProfileDesired != "profile-dev" || store.changeProfileSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("更换 profile 参数不正确：%+v", store)
	}
}

func TestAuthorityClientProfilePreviewUsesCommonControlsAndEmptyState(t *testing.T) {
	store := pairingTestStore()
	store.client = &ClientDetail{
		ClientSummary: ClientSummary{ID: "client-1", Name: "笔记本", ProfileName: "运维"},
		ProfileID:     "profile-ops",
	}
	store.clientProfilePreview = &ClientProfilePreview{
		ClientName: "笔记本", CurrentProfileID: "profile-ops", CurrentProfileName: "运维",
		DesiredProfileID: "profile-dev", DesiredProfileName: "开发",
	}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/profile?profile=profile-dev", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("空变更预览状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	if strings.Contains(body, "0 项：") || strings.Contains(body, "0 台：") {
		t.Fatalf("空增减项不得显示“0 项：”：\n%s", body)
	}
	for _, want := range []string{"无", `class="primary-button"`, `class="secondary-button"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("更换 profile 预览应包含 %q：\n%s", want, body)
		}
	}
}

func TestAuthorityClientMachineChoiceWritesAssociation(t *testing.T) {
	store := pairingTestStore()
	store.client = &ClientDetail{
		ClientSummary: ClientSummary{ID: "client-1", Name: "笔记本"},
		ProfileID:     "profile-ops",
	}
	store.machineChoices = []ClientMachineChoice{{
		ID: "machine-1", Name: "工作站", SitePath: "公司 / 一号楼", Available: true,
	}}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/machine", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("选择机器状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		"<h1>选择机器</h1>", "公司 / 一号楼", `value="machine-1"`, "工作站", "新建一台机器", "name=\"new_machine\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("选择机器页应包含 %q：\n%s", want, body)
		}
	}
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/machine", url.Values{
		"csrf_token": {csrfToken}, "machine_id": {"machine-1"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/clients/client-1?notice=machine" {
		t.Fatalf("关联机器状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), responseBody)
	}
	if store.linkMachineClientID != "client-1" || store.linkMachineID != "machine-1" ||
		store.linkMachineSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("关联机器参数不正确：%+v", store)
	}
}

func TestAuthorityClientRemoveRequiresConfirmAndRevokes(t *testing.T) {
	store := pairingTestStore()
	store.client = &ClientDetail{
		ClientSummary: ClientSummary{ID: "client-1", Name: "笔记本"},
		ProfileID:     "profile-ops",
	}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/remove", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK || !strings.Contains(body, "移除客户端 笔记本") ||
		!strings.Contains(body, "无法再同步") {
		t.Fatalf("移除确认页状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	csrfToken := hiddenInputValue(t, body, "csrf_token")
	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/clients/client-1/remove", url.Values{
		"csrf_token": {csrfToken}, "confirm": {"1"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/clients/?notice=removed" {
		t.Fatalf("确认移除状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), responseBody)
	}
	if store.removeClientID != "client-1" || store.removeClientSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("移除参数不正确：%+v", store)
	}
}
