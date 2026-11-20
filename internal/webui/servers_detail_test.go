package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// serverDetailTopologyFixture 返回带多级地点、两个网络成员与同机另一个账
// 号的拓扑数据；地点故意与物理局域网覆盖不一致，供提示断言使用。
func serverDetailTopologyFixture() *TopologyData {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-1", Name: "公司", LocationPath: "公司"},
			{ID: "site-2", ParentID: "site-1", Name: "一厂区", LocationPath: "公司 / 一厂区"},
			{ID: "site-3", ParentID: "site-2", Name: "3 号楼", LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "site-4", Name: "机房", LocationPath: "机房"},
		},
		Networks: []TopologyNetwork{
			{ID: "net-1", Name: "办公网", Type: "physical", SiteID: "site-3", SiteName: "3 号楼",
				LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "net-2", Name: "机房网", Type: "physical", SiteID: "site-4", SiteName: "机房",
				LocationPath: "机房"},
			{ID: "net-3", Name: "虚拟网", Type: "virtual"},
		},
		Devices: []TopologyDevice{
			{
				ID: "device-1", Name: "工作站", Kind: "server",
				SiteID: "site-4", LocationPath: "机房",
				LocationMismatches: []TopologyLocationMismatch{{
					NetworkID: "net-1", NetworkName: "办公网",
					DeviceLocationPath:  "机房",
					NetworkLocationPath: "公司 / 一厂区 / 3 号楼",
				}},
				Servers: []TopologyDeviceServer{
					{ID: "srv-fine", Name: "fine", User: "root", Host: "192.0.2.10", Port: 22},
					{ID: "srv-other", Name: "other-account", User: "backup", Host: "192.0.2.10", Port: 22},
				},
			},
			{ID: "device-2", Name: "NAS", Kind: "unmanaged", SiteID: "site-4", LocationPath: "机房"},
		},
		Memberships: []TopologyMembership{
			{DeviceID: "device-1", NetworkID: "net-1", Address: "192.0.2.10"},
			{DeviceID: "device-1", NetworkID: "net-3", Address: "100.64.0.10"},
		},
	}
}

func newServerDetailTestStore() *stubServerStore {
	store := newStubServerStore()
	detail := store.details["srv-fine"]
	detail.DeviceName = "工作站"
	detail.HasDevice = true
	detail.Description = "内部服务"
	detail.Hardware = "NUC"
	detail.Services = "Docker"
	detail.Role = "构建机"
	detail.Caveats = "只允许 owner 登录"
	detail.ShowHostKeyAnchor = true
	sibling := &ServerDetailView{
		ID: "srv-other", Name: "other-account", User: "backup", Host: "192.0.2.10", Port: 22,
		Shell: "posix", CredentialType: "password", Revision: 7,
		Description: "每晚备份任务使用", DeviceName: "工作站", HasDevice: true,
	}
	sibling.ConnectionTest = NewServerConnectionTestResultView(models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网",
		Duration: 31 * time.Millisecond,
	})
	store.details["srv-other"] = sibling
	return store
}

type serverDetailHandlerOptions struct {
	role           Role
	servers        *stubServerStore
	writer         *stubServerWriter
	topology       TopologySource
	topologyWriter TopologyWriter
	software       SoftwareInventorySource
	access         *stubAccessStore
	tester         ServerConnectionTester
}

func newServerDetailTestHandler(t *testing.T, options serverDetailHandlerOptions) (http.Handler, *http.Cookie) {
	t.Helper()
	if options.access == nil {
		options.access = &stubAccessStore{}
	}
	handler, startup, err := New(Config{
		Role: options.role, Servers: options.servers, ServerWriter: options.writer,
		Topology: options.topology, TopologyWriter: options.topologyWriter,
		SoftwareInventory:      options.software,
		ServerConnectionTester: options.tester,
		Access:                 options.access,
		VersionHint:            func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	response := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return handler, cookies[0]
}

// TestServerDetailShowsOneColumnCardsAndLegacyTabRedirects 校验详情页只保留
// 一列分区卡片；旧分段查询地址回到统一详情页。
func TestServerDetailShowsOneColumnCardsAndLegacyTabRedirects(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, forbidden := range []string{
		`class="segmented server-detail-tabs"`, `aria-label="服务器详情分段"`, "地址与地点",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("统一详情页不应再包含 %q：\n%s", forbidden, body)
		}
	}

	labels := []string{
		`aria-label="连接"`, `<h2>地点</h2>`, `<h2>地址</h2>`, `<h2>说明</h2>`,
		`<h2>同一台机器上的其他账号</h2>`, `<h2>授权</h2>`, `<h2>软件清单</h2>`,
		`<h2>高级操作</h2>`,
	}
	position := 0
	for _, label := range labels {
		found := strings.Index(body[position:], label)
		if found < 0 {
			t.Fatalf("统一详情页缺少卡片标记 %q：\n%s", label, body)
		}
		position += found + len(label)
	}
	if !strings.Contains(body, `<details class="advanced-card" id="server-advanced-actions">`) {
		t.Fatalf("高级操作应默认折叠：\n%s", body)
	}

	for _, testCase := range []struct{ oldPath, want string }{
		{"/ui/servers/srv-fine?tab=connection", "/ui/servers/srv-fine"},
		{"/ui/servers/srv-fine?tab=addresses", "/ui/servers/srv-fine"},
		{"/ui/servers/srv-fine?tab=authorization", "/ui/servers/srv-fine"},
		{"/ui/servers/srv-fine?tab=software", "/ui/servers/srv-fine"},
		{"/ui/servers/srv-fine?tab=connection&edit=1", "/ui/servers/srv-fine?edit=connection"},
		{"/ui/servers/srv-fine?tab=addresses&location=1", "/ui/servers/srv-fine?edit=location"},
		{"/ui/servers/srv-fine?edit=1", "/ui/servers/srv-fine?edit=connection"},
		{"/ui/servers/srv-fine?location=1", "/ui/servers/srv-fine?edit=location"},
		{"/ui/servers/srv-fine/edit", "/ui/servers/srv-fine?edit=connection"},
	} {
		request := httptest.NewRequest(http.MethodGet, testCase.oldPath, nil)
		request.Host = "127.0.0.1:18080"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		response := recorder.Result()
		_ = response.Body.Close()
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("旧地址 %s 状态码 = %d，应为 303", testCase.oldPath, response.StatusCode)
		}
		if location := response.Header.Get("Location"); location != testCase.want {
			t.Fatalf("旧地址 %s 跳转到 %q，应为 %q", testCase.oldPath, location, testCase.want)
		}
	}
}

// TestServerDetailConnectionEditorDrawer 校验连接分区使用底部弹层，只包含连
// 接相关字段，且不再把主机地址与说明混入连接编辑。
func TestServerDetailConnectionEditorDrawer(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=connection")
	for _, want := range []string{
		`data-server-detail-drawer="connection"`, `role="dialog"`, `aria-modal="true"`,
		`aria-labelledby="server-detail-drawer-title"`,
		`<h2 id="server-detail-drawer-title">编辑连接</h2>`,
		`for="edit-name"`, `for="edit-user"`, `for="edit-port"`, `for="edit-shell"`,
		`for="edit-tags"`, `name="expose_host"`, `name="credential_action"`,
		`name="sudo_password"`, `data-cancel-detail-edit`, "保存连接信息",
		`name="edit_scope" value="connection"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("连接编辑弹层缺少 %q：\n%s", want, body)
		}
	}
	for _, forbidden := range []string{
		`id="edit-host"`, `name="host"`, `name="description"`, `name="hardware"`,
		`name="services"`, `name="role"`, `name="special_handling"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("连接编辑弹层不应包含 %q：\n%s", forbidden, body)
		}
	}
	if strings.Count(body, `role="dialog"`) != 1 {
		t.Fatalf("详情页一次只应渲染一个编辑弹层：\n%s", body)
	}
}

// TestServerDetailLocationEditorDrawer 校验地点分区弹层沿用地点树、就地新
// 建地点与「不登记地点」，普通页面不再内联渲染地点表单。
func TestServerDetailLocationEditorDrawer(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=location")
	for _, want := range []string{
		`data-server-detail-drawer="location"`, `<h2 id="server-detail-drawer-title">编辑地点</h2>`,
		`action="/ui/servers/srv-fine/location"`, `value="公司 / 一厂区 / 3 号楼"`,
		`value="机房"`, "不登记地点", `name="site"`, "保存地点", "新建地点",
		`data-cancel-detail-edit`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("地点编辑弹层缺少 %q：\n%s", want, body)
		}
	}
	if !strings.Contains(body, "return_to=%2Fui%2Fservers%2Fsrv-fine%3Fedit%3Dlocation") {
		t.Fatalf("新建地点应回到地点编辑弹层：\n%s", body)
	}

	plain := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	if strings.Contains(plain, `name="site"`) || strings.Contains(plain, `role="dialog"`) {
		t.Fatalf("未请求编辑时地点卡片不应同时渲染预览与编辑表单：\n%s", plain)
	}
}

// TestServerDetailDescriptionEditorDrawer 校验说明分区按五个既有元数据字段
// 分组编辑，弹层不夹带连接字段。
func TestServerDetailDescriptionEditorDrawer(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=description")
	for _, want := range []string{
		`data-server-detail-drawer="description"`,
		`<h2 id="server-detail-drawer-title">编辑说明</h2>`,
		`for="edit-role">用途`, `for="edit-services">部署了什么`, `for="edit-hardware">硬件`,
		`for="edit-caveats">注意事项`, `for="edit-description">备注`,
		`name="role" rows="2">构建机</textarea>`, `name="services" rows="2">Docker</textarea>`,
		`name="hardware" rows="2">NUC</textarea>`, `name="special_handling" rows="2">只允许 owner 登录</textarea>`,
		`name="description" rows="2">内部服务</textarea>`, `name="edit_scope" value="description"`,
		"保存说明", `data-cancel-detail-edit`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("说明编辑弹层缺少 %q：\n%s", want, body)
		}
	}
	for _, forbidden := range []string{
		`name="user"`, `name="port"`, `name="credential_action"`, `name="sudo_password"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("说明编辑弹层不应包含 %q：\n%s", forbidden, body)
		}
	}
}

// TestServerDetailAccountsCardAndNoteEditor 校验同机账号卡片显示说明与最近
// 测试状态，编辑层只改各账号显示说明并写入各自条目。
func TestServerDetailAccountsCardAndNoteEditor(t *testing.T) {
	store := newServerDetailTestStore()
	writer := &stubServerWriter{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: store, writer: writer,
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		"同一台机器上的其他账号", `href="/ui/servers/srv-other"`, "other-account", "backup",
		"每晚备份任务使用", "上次测试：", "已连上", `href="/ui/servers/srv-fine?edit=accounts"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("同机账号卡片缺少 %q：\n%s", want, body)
		}
	}

	drawer := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=accounts")
	for _, want := range []string{
		`data-server-detail-drawer="accounts"`,
		`<h2 id="server-detail-drawer-title">编辑其他账号说明</h2>`,
		"只编辑显示用的说明；账号的登录信息在各自详情页修改。",
		`name="account_note_srv-other" value="每晚备份任务使用"`,
		`name="account_revision_srv-other" value="7"`,
		`name="edit_scope" value="accounts"`, `href="/ui/servers/srv-other"`,
	} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("同机账号编辑弹层缺少 %q：\n%s", want, drawer)
		}
	}
	for _, forbidden := range []string{`name="user"`, `name="port"`, `name="credential_action"`} {
		if strings.Contains(drawer, forbidden) {
			t.Fatalf("同机账号编辑弹层不应包含 %q：\n%s", forbidden, drawer)
		}
	}

	response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/edit", url.Values{
		"edit_scope": {"accounts"}, "server_id": {"srv-fine"},
		"account_note_srv-other":     {"夜里跑备份任务"},
		"account_revision_srv-other": {"7"},
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("账号说明保存状态码 = %d，应为 303", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "/ui/servers/srv-fine?saved=accounts#server-card-accounts" {
		t.Fatalf("账号说明保存跳转到 %q", location)
	}
	if len(writer.editIDs) != 1 || writer.editIDs[0] != "srv-other" {
		t.Fatalf("账号说明应写入同机账号条目，实际 = %v", writer.editIDs)
	}
	if len(writer.edits) != 1 || writer.edits[0].Description == nil ||
		*writer.edits[0].Description != "夜里跑备份任务" || writer.edits[0].User != nil {
		t.Fatalf("账号说明写入字段 = %#v，应只写显示说明", writer.edits)
	}

	clientHandler, clientCookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		role: RoleClient, servers: store,
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	clientPage := serversGet(t, clientHandler, clientCookie, "/ui/servers/srv-fine")
	if strings.Contains(clientPage, "?edit=accounts") || strings.Contains(clientPage, "?edit=connection") {
		t.Fatalf("客户端详情页不应出现编辑入口：\n%s", clientPage)
	}
}

// TestServerDetailAccountNotesRejectsBadRevisionsBeforeWrite 校验整份同机
// 账号说明提交先核对全部修订号；解析失败或已过期时不写入、不显示成功。
func TestServerDetailAccountNotesRejectsBadRevisionsBeforeWrite(t *testing.T) {
	cases := []struct {
		name     string
		revision string
		status   int
		notice   string
	}{
		{
			name:     "解析失败",
			revision: "seven",
			status:   http.StatusBadRequest,
			notice:   "账号说明的修订号无效，请重新打开编辑层后再保存。",
		},
		{
			name:     "修订号过期",
			revision: "6",
			status:   http.StatusConflict,
			notice:   "服务器条目已被其他入口修改，以下为最新值；请核对后重新提交。",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newServerDetailTestStore()
			writer := &stubServerWriter{}
			handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
				servers: store, writer: writer,
				topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
				topologyWriter: &stubTopologyWriter{},
			})
			response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/edit", url.Values{
				"edit_scope": {"accounts"}, "server_id": {"srv-fine"},
				"account_note_srv-other":     {"新说明"},
				"account_revision_srv-other": {testCase.revision},
			})
			body := readResponseBody(t, response)
			if response.StatusCode != testCase.status || !strings.Contains(body, testCase.notice) {
				t.Fatalf("状态码=%d，应含 %q：\n%s", response.StatusCode, testCase.notice, body)
			}
			if strings.Contains(body, "已保存账号说明") {
				t.Fatalf("修订号无效时不得显示保存成功：\n%s", body)
			}
			if !strings.Contains(body, `data-server-detail-drawer="accounts"`) ||
				!strings.Contains(body, `name="account_note_srv-other" value="新说明"`) {
				t.Fatalf("失败后应保留账号说明弹层与输入：\n%s", body)
			}
			if !strings.Contains(body, "每晚备份任务使用") {
				t.Fatalf("失败后应显示存储中的原说明：\n%s", body)
			}
			if len(writer.editIDs) != 0 {
				t.Fatalf("修订号无效时不应写入任何账号说明，实际 = %v", writer.editIDs)
			}
		})
	}
}

// TestServerDetailAccountNotesWriteConflictDoesNotShowSuccess 校验写入阶段
// 返回修订号冲突时重新渲染账号说明弹层，而不是跳转成保存成功。
func TestServerDetailAccountNotesWriteConflictDoesNotShowSuccess(t *testing.T) {
	store := newServerDetailTestStore()
	writer := &stubServerWriter{conflict: &ServerEditConflictView{
		Current: &ServerDetailView{ID: "srv-other", Name: "other-account", Revision: 8},
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: store, writer: writer,
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})
	response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/edit", url.Values{
		"edit_scope": {"accounts"}, "server_id": {"srv-fine"},
		"account_note_srv-other":     {"新说明"},
		"account_revision_srv-other": {"7"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("写入冲突状态码 = %d，应为 409", response.StatusCode)
	}
	if !strings.Contains(body, "服务器条目已被其他入口修改，以下为最新值；请核对后重新提交。") ||
		!strings.Contains(body, `data-server-detail-drawer="accounts"`) {
		t.Fatalf("写入冲突后应重新渲染账号说明弹层：\n%s", body)
	}
	if strings.Contains(body, "已保存账号说明") {
		t.Fatalf("写入冲突时不得显示保存成功：\n%s", body)
	}
	if !strings.Contains(body, "每晚备份任务使用") {
		t.Fatalf("写入冲突后仍应显示存储中的原说明：\n%s", body)
	}
}

// TestServerDetailUnifiedPageShowsCardsAndFields 校验统一详情页的连接、地
// 点、地址与说明卡片内容与高级操作占位。
func TestServerDetailUnifiedPageShowsCardsAndFields(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		`id="server-card-connection"`, `href="/ui/servers/srv-fine?edit=connection"`,
		"连接", "登录用户", "root", "端口", "认证方式", "密码",
		"sudo 密码", "主机密钥指纹", "shell 类型",
		`action="/ui/servers/srv-fine/test"`, "测试连接",
		`id="server-card-location"`, `href="/ui/servers/srv-fine?edit=location"`,
		`class="location-value"`, "机房", "地点登记在机器上",
		`id="server-card-address"`, "办公网", "物理局域网", "192.0.2.10",
		"虚拟网", "虚拟局域网", "100.64.0.10", "其他（公网或未登记网络）",
		"地点不一致", "公司 / 一厂区 / 3 号楼", `data-detail-edit-button="address"`,
		`id="server-card-description"`, `href="/ui/servers/srv-fine?edit=description"`,
		"用途", "构建机", "部署了什么", "Docker", "硬件", "NUC",
		"注意事项", "只允许 owner 登录", "备注", "内部服务",
		"高级操作", `href="/ui/servers/srv-fine/delete"`, "删除服务器",
		"这里只放低频且有风险的操作。",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("统一详情页缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, `role="dialog"`) || strings.Contains(body, `name="description"`) {
		t.Fatalf("未请求编辑时不应同时渲染预览与编辑表单：\n%s", body)
	}
	for _, forbidden := range []string{
		`action="/ui/servers/srv-fine/address/add"`,
		`action="/ui/servers/srv-fine/address/edit"`,
		`action="/ui/servers/srv-fine/address/remove"`,
		"编辑地址由后续版本提供",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("地址卡片不应再保留逐行写入入口 %q：\n%s", forbidden, body)
		}
	}

	bare := serversGet(t, handler, cookie, "/ui/servers/srv-bare")
	if !strings.Contains(bare, "同一台机器上的其他账号") || !strings.Contains(bare, "未关联机器") {
		t.Fatalf("未关联机器时仍应有账号卡片与说明：\n%s", bare)
	}
}

// TestServerDetailAddressEditorDrawer 校验地址分区使用一份「网络 + 地址」
// 清单编辑，下拉包含全部网络与其他地址选项。
func TestServerDetailAddressEditorDrawer(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=address")
	for _, want := range []string{
		`data-server-detail-drawer="address"`,
		`<h2 id="server-detail-drawer-title">编辑地址</h2>`,
		`action="/ui/servers/srv-fine/address"`,
		`name="address_network"`, `name="address"`,
		`value="other"`, "其他（公网或未登记网络）",
		`value="办公网"`, `value="机房网"`, `value="虚拟网"`,
		`name="revision" value="4"`, `data-remove-address`, `data-add-detail-address`,
		"保存地址",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("地址编辑弹层缺少 %q：\n%s", want, body)
		}
	}
	if strings.Count(body, `data-detail-address-row`) != 3 {
		t.Fatalf("地址编辑弹层应有三个初始行（两个网络成员与一个其他地址）：\n%s", body)
	}
}

// TestServerDetailHeaderUsesPreferredNetworkAddress 校验其他地址清空后，
// 页头显示连接候选里的首选网络地址；没有网络地址时不显示空主机。
func TestServerDetailHeaderUsesPreferredNetworkAddress(t *testing.T) {
	cases := []struct {
		name       string
		candidates []TopologyCandidate
		want       string
	}{
		{
			name: "有物理局域网候选",
			candidates: []TopologyCandidate{
				{Address: "203.0.113.9", Source: string(models.DialAddressSourceFallback)},
				{Address: "192.0.2.10", Source: string(models.DialAddressSourcePhysical)},
			},
			want: `root@192.0.2.10:22`,
		},
		{
			name:       "没有网络候选",
			candidates: []TopologyCandidate{{Address: "203.0.113.9", Source: string(models.DialAddressSourceFallback)}},
			want:       `root · 端口 22`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newServerDetailTestStore()
			store.details["srv-fine"].Host = ""
			data := serverDetailTopologyFixture()
			data.Devices[0].Candidates = testCase.candidates
			handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
				servers: store, writer: &stubServerWriter{},
				topology:       &stubTopologySource{data: data},
				topologyWriter: &stubTopologyWriter{},
			})
			body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
			want := `<p class="detail-subtitle mono">` + testCase.want + `</p>`
			if !strings.Contains(body, want) {
				t.Fatalf("页头应显示 %q：\n%s", testCase.want, body)
			}
			if strings.Contains(body, "root@:22") {
				t.Fatalf("其他地址清空后页头不得显示空主机：\n%s", body)
			}
		})
	}
}

// TestServerDetailAddressRangeSuggestions 校验网段提示按地点覆盖计算：只
// 读行说明不会自动归入，编辑弹层提供只改草稿的确认入口。
func TestServerDetailAddressRangeSuggestions(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailNetworkRangeTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailNetworkRangeFixture("site-home")},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	readOnlyHint := "这个地址落在 家A-办公网 的登记网段 192.168.100.0/24 内。可在『编辑』里确认归属，不会自动归入。"
	if !strings.Contains(body, readOnlyHint) {
		t.Fatalf("只读地址行应说明网段覆盖且不会自动归入，缺少 %q：\n%s", readOnlyHint, body)
	}

	drawer := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=address")
	for _, want := range []string{
		"属于 家A-办公网？网段 192.168.100.0/24 覆盖这个地址。",
		`data-confirm-network="家A-办公网"`, ">确认</button>",
	} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("地址编辑弹层缺少 %q：\n%s", want, drawer)
		}
	}
}

// TestServerDetailAddressRangeSuggestionsRequireCoveredLocation 校验设备地
// 点不在物理局域网覆盖地点及其下级内时不出现网段提示。
func TestServerDetailAddressRangeSuggestionsRequireCoveredLocation(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailNetworkRangeTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailNetworkRangeFixture("site-office")},
		topologyWriter: &stubTopologyWriter{},
	})

	drawer := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=address")
	for _, forbidden := range []string{"这个地址落在", "属于 家A-办公网", "data-confirm-network="} {
		if strings.Contains(drawer, forbidden) {
			t.Fatalf("设备地点不在覆盖范围内时不应提示 %q：\n%s", forbidden, drawer)
		}
	}
}

func newServerDetailNetworkRangeTestStore() *stubServerStore {
	store := newServerDetailTestStore()
	store.details["srv-fine"].Host = "192.168.100.41"
	return store
}

func serverDetailNetworkRangeFixture(deviceSiteID string) *TopologyData {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-home", Name: "家A", LocationPath: "家A"},
			{ID: "site-room", ParentID: "site-home", Name: "房间", LocationPath: "家A / 房间"},
			{ID: "site-office", Name: "办公室", LocationPath: "办公室"},
		},
		Networks: []TopologyNetwork{{
			ID: "net-home", Name: "家A-办公网", Type: "physical",
			SiteID: "site-home", SiteName: "家A", LocationPath: "家A",
			NetworkRange: "192.168.100.0/24",
		}},
		Devices: []TopologyDevice{{
			ID: "device-1", Name: "工作站", Kind: "server", SiteID: deviceSiteID,
			LocationPath: map[string]string{
				"site-home": "家A", "site-room": "家A / 房间", "site-office": "办公室",
			}[deviceSiteID],
			Servers: []TopologyDeviceServer{{ID: "srv-fine", Name: "fine"}},
		}},
	}
}

// TestServerDetailAddressListWrite 校验整份地址表单走服务器写入口，并把
// 其他地址行转换成清空语义。
func TestServerDetailAddressListWrite(t *testing.T) {
	writer := &stubServerWriter{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: writer,
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/address", url.Values{
		"revision":        {"4"},
		"address_network": {"办公网", "虚拟网"},
		"address":         {"192.0.2.10", "100.64.0.10"},
	})
	if response.StatusCode != http.StatusSeeOther ||
		response.Header.Get("Location") != "/ui/servers/srv-fine?saved=address#server-card-address" {
		t.Fatalf("地址保存状态码=%d 跳转=%s", response.StatusCode, response.Header.Get("Location"))
	}
	if len(writer.addressEdits) != 1 {
		t.Fatalf("应调用一次地址列表写入：%+v", writer.addressEdits)
	}
	edit := writer.addressEdits[0]
	if edit.id != "srv-fine" || edit.form.ExpectedRevision != 4 || len(edit.form.Rows) != 2 ||
		edit.form.Rows[0].Network != "办公网" || edit.form.Rows[0].Address != "192.0.2.10" ||
		edit.form.Rows[1].Network != "虚拟网" || edit.form.Rows[1].Address != "100.64.0.10" {
		t.Fatalf("地址列表写入参数不正确：%+v", edit)
	}
	if edit.source.RemoteAddress != "192.0.2.1" {
		t.Fatalf("地址列表写入来源不正确：%+v", edit.source)
	}
}

// TestServerDetailAddressWriteKeepsDraft 校验表单错误与修订号冲突都保持
// 地址弹层打开，并回显 owner 提交的整份草稿。
func TestServerDetailAddressWriteKeepsDraft(t *testing.T) {
	cases := []struct {
		name     string
		writer   *stubServerWriter
		status   int
		notice   string
		revision string
	}{
		{
			name:     "表单错误",
			writer:   &stubServerWriter{addressErr: &ServerFormError{Message: "地址不能包含空白字符。"}},
			status:   http.StatusBadRequest,
			notice:   "地址不能包含空白字符。",
			revision: "4",
		},
		{
			name: "修订号冲突",
			writer: &stubServerWriter{conflict: &ServerEditConflictView{
				Current: &ServerDetailView{
					ID: "srv-fine", Name: "fine", Revision: 5,
					DeviceName: "工作站", HasDevice: true,
				},
			}},
			status:   http.StatusConflict,
			notice:   "服务器条目已被其他入口修改，以下为最新值；请核对后重新提交。",
			revision: "4",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
				servers: newServerDetailTestStore(), writer: testCase.writer,
				topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
				topologyWriter: &stubTopologyWriter{},
			})
			response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/address", url.Values{
				"revision":        {testCase.revision},
				"address_network": {"机房网", "other"},
				"address":         {"10.0.0.9", "203.0.113.10"},
			})
			if len(testCase.writer.addressEdits) != 1 ||
				testCase.writer.addressEdits[0].form.Rows[0].Address != "10.0.0.9" {
				t.Fatalf("写入前应解析整份草稿：%+v", testCase.writer.addressEdits)
			}
			body := readResponseBody(t, response)
			if response.StatusCode != testCase.status || !strings.Contains(body, testCase.notice) {
				t.Fatalf("状态码=%d，应含 %q：\n%s", response.StatusCode, testCase.notice, body)
			}
			for _, want := range []string{
				`data-server-detail-drawer="address"`, `value="10.0.0.9"`,
				`value="203.0.113.10"`, `value="机房网"`, `value="other"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("失败后应保留整份地址草稿，缺少 %q：\n%s", want, body)
				}
			}
		})
	}
}

// TestServerDetailAddressClientReadOnly 校验客户端直接请求地址编辑参数时
// 也不渲染写表单。
func TestServerDetailAddressClientReadOnly(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		role: RoleClient, servers: newServerDetailTestStore(),
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?edit=address")
	for _, forbidden := range []string{
		`data-detail-edit-button="address"`, `name="address_network"`, `role="dialog"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("客户端地址卡片不应包含 %q：\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "仅显示本地快照，修改请到权威端。") {
		t.Fatalf("客户端地址卡片应提示到权威端修改：\n%s", body)
	}
}

// TestServerDetailLocationAndMoveWrites 校验地点与改挂写操作都走拓扑写来
// 源并回到统一详情页对应卡片；旧逐行地址入口不再注册。
func TestServerDetailLocationAndMoveWrites(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: writer,
	})

	cases := []struct {
		target string
		form   url.Values
		want   string
	}{
		{
			"/ui/servers/srv-fine/location",
			url.Values{"site": {"机房"}},
			"site-edit",
		},
		{
			"/ui/servers/srv-fine/move",
			url.Values{"device": {"NAS"}, "confirm": {"1"}},
			"move",
		},
	}
	for _, testCase := range cases {
		response := serversPost(t, handler, cookie, testCase.target, testCase.form)
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s 状态码 = %d，应为 303", testCase.target, response.StatusCode)
		}
		location := response.Header.Get("Location")
		if !strings.HasPrefix(location, "/ui/servers/srv-fine?") {
			t.Fatalf("%s 跳转地址 = %q，应回到统一详情页", testCase.target, location)
		}
	}
	if len(writer.siteEdits) != 1 || writer.siteEdits[0] != [2]string{"工作站", "机房"} {
		t.Fatalf("机器地点写入 = %v，应为 工作站 -> 机房", writer.siteEdits)
	}
	if len(writer.addCalls) != 0 || len(writer.editCalls) != 0 || len(writer.removeCalls) != 0 {
		t.Fatalf("不应再使用逐行地址写入：%v %v %v", writer.addCalls, writer.editCalls, writer.removeCalls)
	}
	if len(writer.serverMoves) != 1 || writer.serverMoves[0] != [2]string{"srv-fine", "NAS"} {
		t.Fatalf("改挂写入 = %v", writer.serverMoves)
	}
	for _, target := range []string{
		"/ui/servers/srv-fine/address/add",
		"/ui/servers/srv-fine/address/edit",
		"/ui/servers/srv-fine/address/remove",
	} {
		response := serversPost(t, handler, cookie, target, url.Values{"network": {"办公网"}})
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("旧地址入口 %s 状态码 = %d，应为 404", target, response.StatusCode)
		}
	}
}

// TestServerDetailMoveRequiresUnderstanding 校验改挂必须由服务端确认
// 「我已了解」，不能只依赖浏览器禁用提交按钮。
func TestServerDetailMoveRequiresUnderstanding(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: writer,
	})

	response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/move", url.Values{
		"device": {"NAS"},
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("未勾选确认时状态码 = %d，应为 400", response.StatusCode)
	}
	body := readResponseBody(t, response)
	if !strings.Contains(body, "请先勾选「我已了解」。") {
		t.Fatalf("未勾选确认时缺少服务端拒绝提示：\n%s", body)
	}
	if !strings.Contains(body, `data-server-move-confirm`) ||
		!strings.Contains(body, `<option value="NAS" selected>`) {
		t.Fatalf("未勾选确认被拒绝后应保留确认框与目标设备：\n%s", body)
	}
	if len(writer.serverMoves) != 0 {
		t.Fatalf("未勾选确认时不应写入，实际调用：%v", writer.serverMoves)
	}
}

// TestServerDetailMoveFailureKeepsChoice 校验适配层失败时保留确认框、
// 目标设备与勾选状态，并显示既有中文错误。
func TestServerDetailMoveFailureKeepsChoice(t *testing.T) {
	writer := &stubTopologyWriter{nextPlainError: errors.New("database unavailable")}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: writer,
	})

	response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/move", url.Values{
		"device": {"NAS"}, "confirm": {"1"},
	})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("适配层失败时状态码 = %d，应为 500", response.StatusCode)
	}
	body := readResponseBody(t, response)
	for _, want := range []string{
		"保存失败，请稍后重试。",
		`data-server-move-confirm`,
		`<option value="NAS" selected>`,
		`name="confirm" value="1" checked`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("适配层失败后的确认框缺少 %q：\n%s", want, body)
		}
	}
	if len(writer.serverMoves) != 1 {
		t.Fatalf("适配层失败前应尝试一次写入，实际调用：%v", writer.serverMoves)
	}
}

// TestServerDetailMoveSuccessNoticeShowsAnchorResult 校验改挂成功提示包含
// 目标设备与主机密钥指纹三种复制结果。
func TestServerDetailMoveSuccessNoticeShowsAnchorResult(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: writer,
	})

	cases := []struct {
		outcome string
		notice  string
		want    string
	}{
		{"copied", "已复制同机账号 other-account 的主机密钥锚。", "已改挂到目标设备 NAS。主机密钥指纹已复制。"},
		{"none", "", "已改挂到目标设备 NAS。无可复制的主机密钥指纹。"},
		{"conflict", "同机同端口的其他账号主机密钥指纹不一致，未复制主机密钥锚。", "已改挂到目标设备 NAS。同机账号的主机密钥指纹不一致，未复制。"},
	}
	for _, testCase := range cases {
		writer.nextMoveResult = TopologyServerMoveResult{
			ToDevice: "NAS", AnchorNotice: testCase.notice, AnchorCopyOutcome: testCase.outcome,
		}
		response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/move", url.Values{
			"device": {"NAS"}, "confirm": {"1"},
		})
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s 改挂状态码 = %d，应为 303", testCase.outcome, response.StatusCode)
		}
		location := strings.SplitN(response.Header.Get("Location"), "#", 2)[0]
		body := serversGet(t, handler, cookie, location)
		if !strings.Contains(body, `<div class="notice notice-success"><p>`+testCase.want+`</p></div>`) {
			t.Fatalf("指纹结果 %s 的成功提示不正确：\n%s", testCase.outcome, body)
		}
	}
	if len(writer.serverMoves) != 3 {
		t.Fatalf("三种指纹结果应分别调用一次写入，实际调用：%v", writer.serverMoves)
	}
}

// TestServerDetailMoveConfirmationDialog 校验改挂入口先打开确认框，普通
// 详情页不再直接渲染写表单。
func TestServerDetailMoveConfirmationDialog(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		"这里只放低频且有风险的操作。改挂会改变这个条目关联的设备。",
		`href="/ui/servers/srv-fine?move=confirm#server-advanced-actions"`,
		">改挂到另一台设备</a>",
		`href="/ui/servers/srv-fine/delete"`, ">删除服务器条目</a>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("高级操作缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/ui/servers/srv-fine/move"`) {
		t.Fatalf("普通详情页不应直接渲染改挂写表单：\n%s", body)
	}

	confirmed := serversGet(t, handler, cookie, "/ui/servers/srv-fine?move=confirm")
	for _, want := range []string{
		`data-server-move-confirm`,
		`<h2 id="server-move-confirm-title">确认改挂</h2>`,
		"改挂后这个条目将使用目标机器的地点与地址；登录信息、授权、其他地址不变；软件清单留在原机器。",
		`name="device"`,
		`name="confirm" value="1"`,
		">我已了解</label>",
		`data-move-submit disabled`,
		`href="/ui/servers/srv-fine?advanced=open#server-advanced-actions"`,
	} {
		if !strings.Contains(confirmed, want) {
			t.Fatalf("改挂确认框缺少 %q：\n%s", want, confirmed)
		}
	}
}

// TestServerDetailAuthorizationCardShowsThreeSwitchGroups 校验授权卡片三组
// 开关：名称与说明两行、机密项环境变量与最近写入、profile 客户端、共享存
// 储提供方与挂载路径，以及各自的确认表单入口；客户端只读。
func TestServerDetailAuthorizationCardShowsThreeSwitchGroups(t *testing.T) {
	store := newServerDetailTestStore()
	detail := store.details["srv-fine"]
	detail.SecretChoices = []ServerDetailSecretChoice{{
		ServerDetailSecret: ServerDetailSecret{
			ID: "secret-1", Name: "API_TOKEN", Note: "接口令牌第一行\n第二行", EnvironmentName: "API_TOKEN",
		},
		Checked: false,
	}}
	detail.ShowDeliveryRecords = true
	detail.DeliveryRecords = []SecretDeliveryRecordRow{{
		ServerName: "fine", Path: "/srv/app/.env", WrittenAt: time.Unix(1760000000, 0),
		Secrets: []SecretDeliverySecretRow{{Name: "API_TOKEN", ValueVersion: 2}},
	}}
	detail.SharedStorageChoices = []ServerDetailSharedStorageChoice{{
		PasswordSecretID: "secret-share", PasswordSecretName: "DATASET_PASSWORD",
		SharedStorages: []ServerDetailSharedStorage{{
			ID: "share-1", Name: "datasets", ProviderDeviceName: "NAS",
			Note:             "数据集备份第一行\n第二行",
			PasswordSecretID: "secret-share", PasswordSecretName: "DATASET_PASSWORD",
			MountPoint: "/mnt/sshmgr/datasets",
		}},
		Checked: true,
	}}
	detail.ProfileChoices = []ServerDetailProfileChoice{{
		ServerDetailProfile: ServerDetailProfile{ID: "profile-1", Name: "全部"},
		Checked:             false,
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: store, writer: &stubServerWriter{},
		access: &stubAccessStore{
			clients: []ClientSummary{{
				ID: "client-1", Name: "laptop-client", ProfileID: "profile-1", ProfileName: "全部",
			}},
		},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		`id="server-card-authorization"`,
		"Profile", "机密项", "共享存储",
		`class="resource-name"`, `API_TOKEN`, `class="resource-note"`, "接口令牌第一行",
		"环境变量 $API_TOKEN", "最近写入 /srv/app/.env",
		`name="cell__secret-1__srv-fine"`,
		`action="/ui/secrets/authorization/preview"`, `name="return_to" value="/ui/servers/srv-fine"`,
		"能看到这台服务器的客户端", "laptop-client",
		`action="/ui/servers/profile-authorization/preview"`, `name="profile" value="profile-1"`,
		"由 NAS 提供", "/mnt/sshmgr/datasets",
		"数据集备份第一行",
		`name="authorization_view" value="server-shares"`,
		`name="cell__secret-share__srv-fine"`,
		"下发记录", "/srv/app/.env",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情授权卡片缺少 %q：\n%s", want, body)
		}
	}

	clientHandler, clientCookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		role: RoleClient, servers: store,
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	clientPage := serversGet(t, clientHandler, clientCookie, "/ui/servers/srv-fine")
	for _, forbidden := range []string{
		`action="/ui/secrets/authorization/preview"`,
		`action="/ui/servers/profile-authorization/preview"`,
	} {
		if strings.Contains(clientPage, forbidden) {
			t.Fatalf("客户端授权卡片不得包含写表单 %q：\n%s", forbidden, clientPage)
		}
	}
	if !strings.Contains(clientPage, "仅显示本客户端可用的部分，修改请到权威端。") {
		t.Fatalf("客户端授权卡片应提示到权威端修改：\n%s", clientPage)
	}
}

// TestServerDetailSoftwareCardUsesDeviceSummaryAndEntry 校验软件清单卡片使
// 用设备软件清单页的摘要，并撤下声明、认领、草稿、待办与待确认暴露入口。
func TestServerDetailSoftwareCardUsesDeviceSummaryAndEntry(t *testing.T) {
	lastSuccess := time.Unix(1760000100, 0)
	software := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name: "工作站",
		Summary: SoftwareInventoryDeviceSummary{
			Name: "工作站", StatusLabels: []string{"清单过期", "采集失败"},
		},
		Status: SoftwareInventoryStatus{
			Managed: true, CollectorLabel: "权威端", ServerEntry: "fine", LastSuccessAt: &lastSuccess,
			LastFailureReason: "连接超时，端口不通",
		},
		Groups: []SoftwareInventoryGroup{{
			CategoryLabel: "系统包",
			Items: []SoftwareInventoryItem{
				{Name: "nginx", Version: "1.25", Source: "实测", CollectedAt: &lastSuccess},
				{Name: "内部工具", Version: "2.0", Source: "声明", ConfirmedAt: &lastSuccess},
			},
		}},
	}, todos: SoftwareInventoryTodos{
		PendingExposure: []SoftwareInventoryPendingExposureDevice{{
			DeviceName: "工作站",
			Ports: []SoftwareInventoryPendingPort{
				{Address: "192.0.2.10", Port: 8080, Protocol: "tcp", ExposureLabel: "未确认", ObservedStableKey: "stable-web"},
			},
		}},
		UnidentifiedProcesses: []SoftwareInventoryUnidentifiedProcess{{
			DeviceName: "工作站", ProcessName: "legacy-agent", ExecutablePath: "/opt/legacy/agent",
			ObservedStableKey: "stable-legacy",
			ListeningPorts:    []SoftwareInventoryPort{{Address: "192.0.2.10", Port: 9100, Protocol: "tcp", ExposureLabel: "未确认"}},
		}},
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		software: software,
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		`id="server-card-software"`, "软件清单", "设备", "工作站", "采集时间", "清单过期", "采集失败",
		"连接超时，端口不通",
		`action="/ui/software-inventory/collect?device=%E5%B7%A5%E4%BD%9C%E7%AB%99"`,
		`name="return_to" value="/ui/servers/srv-fine"`, "立即采集",
		`href="/ui/software-inventory/devices?device=%E5%B7%A5%E4%BD%9C%E7%AB%99"`, "查看软件清单",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情软件清单卡片缺少 %q：\n%s", want, body)
		}
	}
	for _, forbidden := range []string{
		"声明", "认领", "草稿", "待办", "待确认暴露", "未识别进程",
		"/ui/software-inventory/todos", "/ui/software-inventory/drafts",
		"192.0.2.10:8080", "legacy-agent",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("软件清单卡片不应再出现 %q：\n%s", forbidden, body)
		}
	}

	clientHandler, clientCookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		role: RoleClient, servers: newServerDetailTestStore(),
	})
	clientPage := serversGet(t, clientHandler, clientCookie, "/ui/servers/srv-fine")
	if !strings.Contains(clientPage, `id="server-card-software"`) ||
		!strings.Contains(clientPage, "软件清单在权威端查看。") {
		t.Fatalf("客户端软件清单卡片应保留只读说明：\n%s", clientPage)
	}
	if strings.Contains(clientPage, "/ui/software-inventory/devices") ||
		strings.Contains(clientPage, "立即采集") {
		t.Fatalf("客户端软件清单卡片不应出现入口或采集表单：\n%s", clientPage)
	}

	bare := serversGet(t, handler, cookie, "/ui/servers/srv-bare")
	if !strings.Contains(bare, "未关联机器") {
		t.Fatalf("未关联机器的软件清单卡片应有说明：\n%s", bare)
	}
}

// TestServerDetailTestConnectionStartsTask 校验详情页测试连接启动任务后原
// 位轮询：局部请求不跳转，成功片段展示结构化结果并同步页头上次测试。
func TestServerDetailTestConnectionStartsTask(t *testing.T) {
	tester := &stubServerConnectionTester{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{}, tester: tester,
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	initial := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	if !strings.Contains(initial, `id="server-connection-test-live"`) ||
		!strings.Contains(initial, `aria-live="polite"`) || !strings.Contains(initial, "从未测试") {
		t.Fatalf("详情页应有稳定的连接测试动态结果区：\n%s", initial)
	}
	liveStart := strings.Index(initial, `id="server-connection-test-live"`)
	formStart := strings.Index(initial, `action="/ui/servers/srv-fine/test"`)
	if liveStart < 0 || formStart < 0 || !strings.Contains(initial[liveStart:formStart], "</div>") {
		t.Fatalf("测试连接表单应在动态结果区域外，完成后不得被局部替换删除：\n%s", initial)
	}
	csrf := serversCSRF(t, handler, cookie)
	request := httptest.NewRequest(http.MethodPost, "/ui/servers/srv-fine/test", strings.NewReader(url.Values{
		"csrf_token": {csrf},
	}.Encode()))
	request.Host = "127.0.0.1:18080"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:18080")
	request.Header.Set("HX-Request", "true")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != http.StatusOK || response.Header.Get("Location") != "" {
		t.Fatalf("局部测试连接不应跳转，状态码=%d 跳转=%q", response.StatusCode, response.Header.Get("Location"))
	}
	running := readResponseBody(t, response)
	for _, want := range []string{
		`id="server-connection-test-result"`, "正在连接…",
		`hx-get="/ui/servers/srv-fine/test?task=task-1"`,
	} {
		if !strings.Contains(running, want) {
			t.Fatalf("进行中连接测试片段缺少 %q：\n%s", want, running)
		}
	}
	if len(tester.starts) != 1 || tester.starts[0].serverID != "srv-fine" {
		t.Fatalf("测试连接应启动 srv-fine 任务，实际 %v", tester.starts)
	}

	runningPoll := httptest.NewRequest(http.MethodGet, "/ui/servers/srv-fine/test?task=task-1", nil)
	runningPoll.Host = "127.0.0.1:18080"
	runningPoll.Header.Set("HX-Request", "true")
	runningPoll.AddCookie(cookie)
	runningPollRecorder := httptest.NewRecorder()
	handler.ServeHTTP(runningPollRecorder, runningPoll)
	runningPollResponse := runningPollRecorder.Result()
	runningPollBody := readResponseBody(t, runningPollResponse)
	if runningPollResponse.StatusCode != http.StatusNoContent || runningPollBody != "" {
		t.Fatalf("进行中轮询应返回 204 且不改写无障碍动态提醒区域，状态码=%d 响应=%q",
			runningPollResponse.StatusCode, runningPollBody)
	}

	tester.status = &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestSucceeded, ServerID: "srv-fine",
		HandshakeAddress: "192.0.2.10:22",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestSucceeded, Succeeded: true,
			CompletedAt: time.Unix(1_782_000_000, 0), SuccessAddress: "192.0.2.10",
			SuccessNetworkName: "办公网", Duration: 42 * time.Millisecond,
			Persisted: true,
			Attempts: []ServerConnectionTestAttemptView{{
				NetworkName: "办公网", Address: "192.0.2.10", OK: true,
				Duration: 40 * time.Millisecond,
			}},
		},
	}
	poll := httptest.NewRequest(http.MethodGet, "/ui/servers/srv-fine/test?task=task-1", nil)
	poll.Host = "127.0.0.1:18080"
	poll.Header.Set("HX-Request", "true")
	poll.AddCookie(cookie)
	pollRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pollRecorder, poll)
	page := readResponseBody(t, pollRecorder.Result())
	for _, want := range []string{
		"已连上 · 用的地址 192.0.2.10（办公网）· 耗时 42 毫秒",
		"上次测试：", "已连上", `id="server-last-test"`, `hx-swap-oob="true"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("详情页连接测试结果缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "已保存，") || strings.Contains(page, "服务器条目已保存") {
		t.Fatalf("详情页连接测试结果不得沿用新增结果页的已保存文案：\n%s", page)
	}
}

// TestServerDetailConnectionTestFailureFragmentCoversCandidates 校验失败结
// 果只显示白话分类，候选地址默认收起，其他地址使用固定页面指称。
func TestServerDetailConnectionTestFailureFragmentCoversCandidates(t *testing.T) {
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestFailed, ServerID: "srv-fine",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestFailed, FailureLabel: "端口不通",
			Hint: "请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。",
			Attempts: []ServerConnectionTestAttemptView{{
				Address: "192.0.2.10", Source: models.DialAddressSourceFallback,
				FailureLabel: "端口不通", Duration: 23 * time.Millisecond,
			}},
		},
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{}, tester: tester,
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/servers/srv-fine/test?task=task-1", nil)
	request.Host = "127.0.0.1:18080"
	request.Header.Set("HX-Request", "true")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := readResponseBody(t, recorder.Result())
	for _, want := range []string{
		"连不上", "端口不通", `aria-expanded="false"`, "展开看每个候选地址的结果",
		"请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。",
		"其他（公网或未登记网络）", "192.0.2.10", "耗时 23 毫秒",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("失败连接测试片段缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "connection refused") || strings.Contains(page, "login-secret") {
		t.Fatalf("失败连接测试片段不得显示底层错误或凭据：\n%s", page)
	}
	if !strings.Contains(page, `hidden`) {
		t.Fatalf("候选地址默认应收起：\n%s", page)
	}
}

// TestServerDetailConnectionTestFragmentWithoutPersistedResult 校验没有持久
// 结果时页头带外更新不渲染零值时间，原有「上次测试」显示保持不变。
func TestServerDetailConnectionTestFragmentWithoutPersistedResult(t *testing.T) {
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestFailed, ServerID: "srv-fine",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestFailed, FailureLabel: "其他连接失败",
		},
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{}, tester: tester,
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/servers/srv-fine/test?task=task-1", nil)
	request.Host = "127.0.0.1:18080"
	request.Header.Set("HX-Request", "true")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := readResponseBody(t, recorder.Result())
	for _, want := range []string{"连不上", "其他连接失败"} {
		if !strings.Contains(page, want) {
			t.Fatalf("未持久化的失败片段缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, `id="server-last-test"`) ||
		strings.Contains(page, "上次测试：0001-01-01") || strings.Contains(page, "hx-swap-oob") {
		t.Fatalf("没有持久结果时不得带外更新页头为零值时间：\n%s", page)
	}
}

// TestServerDetailShowsPersistedConnectionTestResult 校验详情页页头与连接
// 卡片都能回显加密库里最近一次完成结果。
func TestServerDetailShowsPersistedConnectionTestResult(t *testing.T) {
	store := newServerDetailTestStore()
	detail := store.details["srv-fine"]
	detail.ConnectionTest = NewServerConnectionTestResultView(models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网", Duration: 42 * time.Millisecond,
		Attempts: []models.ServerConnectionTestAttempt{{
			NetworkName: "办公网", Address: "192.0.2.10", OK: true, Duration: 40 * time.Millisecond,
		}},
	})
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: store, writer: &stubServerWriter{},
	})
	page := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		`id="server-last-test"`, "上次测试：", "已连上", "已连上 · 用的地址 192.0.2.10（办公网）· 耗时 42 毫秒",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("持久连接测试结果回显缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "服务器列表卡片上的显示") {
		t.Fatalf("详情页不得带原型中仅作演示的列表卡片区块：\n%s", page)
	}
}

// TestSecretPreviewFromServerDetailShowsClientImpact 校验从服务器详情页提交
// 的机密项开关预览复用 #301 的客户端实际取得途径计算。
func TestSecretPreviewFromServerDetailShowsClientImpact(t *testing.T) {
	access := &stubAccessStore{
		profiles: []AccessProfileSummary{{
			ID: "profile-1", Name: "运维",
			Servers: []AccessServerRef{{ID: "srv-db", Name: "db-1"}},
		}},
		clients: []ClientSummary{{
			ID: "client-1", Name: "笔记本", ProfileID: "profile-1", ProfileName: "运维",
		}},
	}
	servers := &stubServerSource{rows: []ServerRow{
		{ID: "srv-db", Name: "db-1"}, {ID: "srv-web", Name: "web-1"},
	}}
	writer := &authorizationSecretWriter{preview: SecretAuthorizationPreview{
		Changes: []SecretAuthorizationChange{{
			SecretID: "secret-1", SecretName: "MATRIX_TOKEN",
			Added: []SecretAuthorizationServer{{ID: "srv-db", Name: "db-1"}},
		}},
		TotalAdded: 1, Summary: "授权变更摘要",
	}}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: servers, Access: access,
		Secrets: authorizationSecretSource{rows: []SecretRow{{
			ID: "secret-1", Name: "MATRIX_TOKEN", Note: "浏览器测试机密项",
			AllowedServers: []string{"web-1"},
		}}},
		SecretsWriter: writer, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	page := doGetWithCookie(t, handler, cookie, "/ui/secrets/")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, page))
	response := postSecretForm(t, handler, cookie, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/srv-db"},
		"secret_id": {"secret-1"}, "server_id": {"srv-db"},
		secretCellName("secret-1", "srv-db"): {"1"},
	})
	body := readResponseBody(t, response)
	for _, want := range []string{
		"实际能拿到它的客户端如何变化", "笔记本", "现在无法拿到", "提交后经 db-1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情来源的机密项预览缺少 %q：\n%s", want, body)
		}
	}
}
