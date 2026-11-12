package webuiservers

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
)

// TestConnectionTesterRunsServerManagerTestInBackground 用真实 vault 与进程
// 内 SSH 服务器覆盖任务公共边界：启动立即返回，状态先可读，后台完成后变成
// 已验证并带锚与 shell 结论。
func TestConnectionTesterRunsServerManagerTestInBackground(t *testing.T) {
	manager, st := newAdapterManager(t)
	address, _, stopServer := testsshd.Start(t, testsshd.Options{
		Password:  "login-secret",
		AuthDelay: 30 * time.Millisecond,
		Exec: func(command string, _ io.Reader) (string, string, int) {
			switch command {
			case "echo %OS%":
				return "%OS%\r\n", "", 0
			case "echo $PSVersionTable":
				return "\n", "", 0
			}
			return "", "", 1
		},
	})
	defer stopServer()
	host, port := splitConnectionTestAddress(t, address)
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "web", Host: host, Port: port, User: "deploy", ShellType: models.ShellPOSIX,
	}, loginCredential, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	tester := ConnectionTesterFromManager(manager)
	taskID, err := tester.StartServerTest("", added.ServerID, true, webui.ServerWriteSource{
		RemoteAddress: "192.0.2.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, ok := tester.ServerTestStatus(taskID)
	if !ok || status.State != webui.ServerConnectionTestRunning {
		t.Fatalf("启动后应立即可读到进行中状态：%+v ok=%v", status, ok)
	}
	status = waitConnectionTestStatus(t, tester, taskID)
	if status.State != webui.ServerConnectionTestSucceeded || status.AnchorResult == "" ||
		status.AnchorFingerprint == "" || status.ShellDetected != string(models.ShellPOSIX) {
		t.Fatalf("任务完成后应显示已验证、锚与 shell 结论：%+v", status)
	}
	if status.Message != "" || strings.Contains(strings.Join(status.ConnectionNotes, "\n"), "login-secret") {
		t.Fatalf("成功任务不得带错误或凭据：%+v", status)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil || len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI ||
		rows[0].SourceIP != "192.0.2.8" || rows[0].Status != "ok" {
		t.Fatalf("后台任务应写连接测试审计：%v %+v", err, rows)
	}
	if strings.Contains(rows[0].Command, "login-secret") {
		t.Fatalf("连接测试审计不得包含登录密码：%+v", rows[0])
	}
}

// TestWriterAddServerCopiesSiblingAnchor 覆盖挂到已有设备时的兄弟条目锚复
// 制：结果页数据带来源条目，存储与审计沿用既有 pin-copy 路径。
func TestWriterAddServerCopiesSiblingAnchor(t *testing.T) {
	manager, st := newAdapterManager(t)
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	siblingCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sibling-login-secret")}
	sibling, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "sibling", Host: "127.0.0.1", Port: 22, User: "deploy",
		ShellType: models.ShellPOSIX,
	}, siblingCredential, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	siblingServer, err := st.GetServer(sibling.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := []byte("sibling-host-key-anchor")
	if err := st.SaveEntryPin(siblingServer, anchor); err != nil {
		t.Fatal(err)
	}

	result, err := WriterFromManager(manager).AddServer(webui.ServerAddForm{
		Name: "web", User: "deploy", Host: "127.0.0.1", Port: 22, Shell: "auto", CredentialAction: "none",
		DeviceChoice: "existing", ExistingDeviceName: "工作站",
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.6"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AnchorCopied || result.AnchorCopyFromServerName != "sibling" {
		t.Fatalf("结果页应带兄弟锚复制来源：%+v", result)
	}
	pin, err := st.GetEntryPinMeta(result.Detail.ID)
	if err != nil || pin == nil || string(pin.Blob) != string(anchor) {
		t.Fatalf("新条目应复制同设备同端口兄弟锚：%v %+v", err, pin)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"pin-copy"}})
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Command, `"来源服务器条目":"sibling"`) {
		t.Fatalf("锚复制审计应带来源条目：%v %+v", err, rows)
	}
}

// TestHandlerAddServerRunsConnectionTestAndKeepsRowsOnFailure 走完整 HTTP
// 边界：表单提交不被未完成任务阻塞；连接失败后结果页轮询到未验证，已保存
// 的设备、授权与凭据仍在。
func TestHandlerAddServerRunsConnectionTestAndKeepsRowsOnFailure(t *testing.T) {
	manager, st := newAdapterManager(t)
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	tester := ConnectionTesterFromManager(manager)
	handler, startup, err := webui.New(webui.Config{
		Role: webui.RoleStandalone, Servers: FromManager(manager),
		ServerWriter: WriterFromManager(manager), ServerConnectionTester: tester,
		Access: webuiaccess.FromManager(access.New(st)), Port: 18081,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18081"), nil)
	request.Host = "127.0.0.1:18081"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("应签发会话 cookie")
	}
	session := sessionHandler{handler: handler, cookie: *cookies[0]}

	formPage := session.do(http.MethodGet, "/ui/servers/new", nil)
	formBody := integrationBody(t, formPage)
	csrf := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(formBody)
	if len(csrf) != 2 {
		t.Fatalf("新增表单应携带防跨站请求伪造令牌：\n%s", formBody)
	}
	response := session.do(http.MethodPost, "/ui/servers/new", url.Values{
		"csrf_token": {csrf[1]}, "name": {"down"}, "user": {"deploy"},
		"host": {"127.0.0.1"}, "port": {"1"}, "shell": {"auto"},
		"credential_action": {"password"}, "password": {"login-secret"},
		"device_choice": {"new"}, "device_name": {"工作站"},
		"profile_id": {profileID},
	})
	integrationBody(t, response)
	if response.StatusCode != http.StatusSeeOther || !strings.Contains(response.Header.Get("Location"), "task=connect-") {
		t.Fatalf("保存应立即启动后台连接测试，状态码=%d 跳转=%s", response.StatusCode, response.Header.Get("Location"))
	}
	initial := session.do(http.MethodGet, response.Header.Get("Location"), nil)
	initialBody := integrationBody(t, initial)
	if initial.StatusCode != http.StatusOK || strings.Contains(initialBody, "login-secret") {
		t.Fatalf("结果页应及时返回且不回显凭据，状态码=%d：\n%s", initial.StatusCode, initialBody)
	}

	task := regexp.MustCompile(`task=(connect-[^&]+)`).FindStringSubmatch(response.Header.Get("Location"))
	if len(task) != 2 {
		t.Fatalf("跳转应带任务标识：%s", response.Header.Get("Location"))
	}
	status := waitConnectionTestStatus(t, tester, task[1])
	if status.State != webui.ServerConnectionTestFailed || status.Message == "" ||
		!strings.Contains(status.ShellNotice, "shell 未验证") {
		t.Fatalf("不可连服务器应轮询到未验证失败：%+v", status)
	}
	final := session.do(http.MethodGet, response.Header.Get("Location"), nil)
	finalBody := integrationBody(t, final)
	for _, want := range []string{"未验证", "shell 未验证", "默认 POSIX", "connection refused"} {
		if !strings.Contains(finalBody, want) {
			t.Fatalf("失败结果页缺少 %q：\n%s", want, finalBody)
		}
	}
	saved, err := st.GetServerByName("down")
	if err != nil || saved == nil || saved.ShellType != models.ShellPOSIX ||
		saved.AuthMethod != models.AuthPassword || saved.DeviceID == "" {
		t.Fatalf("连接失败后条目与凭据应保留：%v %+v", err, saved)
	}
	if device, err := st.GetDevice("工作站"); err != nil || device == nil {
		t.Fatalf("设备应保留：%v %+v", err, device)
	}
	if granted, err := st.ServersForProfile(profileID); err != nil || len(granted) != 1 || granted[0] != saved.ID {
		t.Fatalf("profile 授权应保留：%v %v", granted, err)
	}
}

func waitConnectionTestStatus(
	t *testing.T, tester webui.ServerConnectionTester, taskID string,
) *webui.ServerConnectionTestStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := tester.ServerTestStatus(taskID)
		if !ok {
			t.Fatalf("任务 %s 不存在", taskID)
		}
		if status.State != webui.ServerConnectionTestRunning {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("连接测试任务超时未完成")
	return nil
}

func splitConnectionTestAddress(t *testing.T, address string) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}
