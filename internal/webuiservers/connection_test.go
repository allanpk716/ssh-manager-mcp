package webuiservers

import (
	"context"
	"errors"
	"fmt"
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
	"ssh-manager-mcp/internal/servermanage"
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
	taskID, err := tester.StartServerTest(added.ServerID, true, webui.ServerWriteSource{
		RemoteAddress: "192.0.2.8",
	}, webui.ServerConnectionAnchor{})
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
	if status.Result == nil || !status.Result.Succeeded || status.Result.CompletedAt.IsZero() ||
		status.Result.SuccessAddress != host || status.Result.SuccessNetworkLabel() != "其他（公网或未登记网络）" ||
		status.Result.Duration <= 0 || len(status.Result.Attempts) != 1 {
		t.Fatalf("任务完成后应带结构化连接结果：%+v", status.Result)
	}
	persisted, err := st.ServerConnectionTestResult(added.ServerID)
	if err != nil || persisted == nil || !persisted.Succeeded || persisted.SuccessAddress != host ||
		len(persisted.Attempts) != 1 {
		t.Fatalf("后台任务完成后应写入最近一次连接测试结果：%v %+v", err, persisted)
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
	pinRows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"pin-tofu"}})
	if err != nil || len(pinRows) != 1 || pinRows[0].Source != store.AuditSourceWebUI ||
		pinRows[0].SourceIP != "192.0.2.8" {
		t.Fatalf("首次锚定审计应标明 Web UI 来源：%v %+v", err, pinRows)
	}
}

func TestConnectionTesterFromOpenerUsesShutdownContext(t *testing.T) {
	manager, st := newAdapterManager(t)
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "shutdown", User: "deploy", Host: "127.0.0.1", Port: 1, ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tester := ConnectionTesterFromOpenerWithContext(func() (*servermanage.Manager, func(), error) {
		return manager, func() {}, nil
	}, ctx)

	taskID, err := tester.StartServerTest(added.ServerID, true, webui.ServerWriteSource{}, webui.ServerConnectionAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	status := waitConnectionTestStatus(t, tester, taskID)
	if status.State != webui.ServerConnectionTestFailed || status.Message != "连接被取消" {
		t.Fatalf("已关闭的任务应继承关闭上下文并立即失败：%+v", status)
	}
}

// TestConnectionTesterRunsBatchTasksIndependently 用真实 vault、进程内
// SSH 服务器与两个失败端口覆盖批量公共边界：每台任务启动后立即可读、快
// 失败端口不被慢握手阻塞、完成结果按服务器条目互不覆盖、每台一条审计且
// 不泄露登录密码。
func TestConnectionTesterRunsBatchTasksIndependently(t *testing.T) {
	manager, st := newAdapterManager(t)
	address, _, stopServer := testsshd.Start(t, testsshd.Options{
		Password:  "login-secret",
		AuthDelay: 600 * time.Millisecond,
	})
	defer stopServer()
	host, okPort := splitConnectionTestAddress(t, address)
	batch := []struct {
		name string
		port int
	}{
		{name: "batch-ok", port: okPort},
		{name: "batch-down-1", port: 1},
		{name: "batch-down-2", port: 2},
	}
	serverIDs := make(map[string]string, len(batch))
	for _, item := range batch {
		added, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: item.name, Host: host, Port: item.port, User: "deploy",
			ShellType: models.ShellPOSIX,
		}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		serverIDs[item.name] = added.ServerID
	}

	tester := ConnectionTesterFromManager(manager)
	source := webui.ServerWriteSource{RemoteAddress: "192.0.2.9"}
	taskIDs := make(map[string]string, len(batch))
	for _, item := range batch {
		taskID, err := tester.StartServerTest(
			serverIDs[item.name], false, source, webui.ServerConnectionAnchor{},
		)
		if err != nil {
			t.Fatal(err)
		}
		taskIDs[item.name] = taskID
	}
	for _, item := range batch {
		status, ok := tester.ServerTestStatus(taskIDs[item.name])
		if !ok || status.State != webui.ServerConnectionTestRunning ||
			status.ServerID != serverIDs[item.name] {
			t.Fatalf("%s 启动后应立即可读自己的进行中状态：%+v ok=%v", item.name, status, ok)
		}
	}

	for _, name := range []string{"batch-down-1", "batch-down-2"} {
		finished := waitConnectionTestStatus(t, tester, taskIDs[name])
		if finished.State != webui.ServerConnectionTestFailed || finished.Result == nil ||
			finished.Result.Succeeded || finished.Result.FailureLabel == "" {
			t.Fatalf("%s 应失败并带白话分类：%+v", name, finished.Result)
		}
	}
	slow, ok := tester.ServerTestStatus(taskIDs["batch-ok"])
	if !ok || slow.State != webui.ServerConnectionTestRunning {
		t.Fatalf("慢握手未完成前不应被其他失败任务阻塞：%+v ok=%v", slow, ok)
	}
	success := waitConnectionTestStatus(t, tester, taskIDs["batch-ok"])
	if success.State != webui.ServerConnectionTestSucceeded || success.Result == nil ||
		!success.Result.Succeeded || success.Result.SuccessAddress != host {
		t.Fatalf("可连服务器应完成自己的成功结果：%+v", success.Result)
	}

	for _, item := range batch {
		persisted, err := st.ServerConnectionTestResult(serverIDs[item.name])
		if err != nil || persisted == nil {
			t.Fatalf("%s 应持久保存最近一次结果：%v %+v", item.name, err, persisted)
		}
		wantSucceeded := item.name == "batch-ok"
		if persisted.Succeeded != wantSucceeded {
			t.Fatalf("%s 的持久结果应为 succeeded=%v：%+v", item.name, wantSucceeded, persisted)
		}
		if persisted.SuccessAddress != "" && persisted.SuccessAddress != host ||
			len(persisted.Attempts) != 1 || persisted.Attempts[0].Address != host ||
			persisted.Attempts[0].OK != wantSucceeded {
			t.Fatalf("%s 的候选地址结论应属于本台：%+v", item.name, persisted.Attempts)
		}
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil || len(rows) != len(batch) {
		t.Fatalf("每台服务器条目应各写一条连接测试审计：%v %+v", err, rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.9" {
			t.Fatalf("批量连接测试审计应沿用 Web UI 来源：%+v", row)
		}
		if strings.Contains(row.Command, "login-secret") {
			t.Fatalf("连接测试审计不得包含登录密码：%s", row.Command)
		}
	}
}

// TestConnectionTesterKeepsRunningTasksBeyondRetention 覆盖保留策略的批量
// 边界：任务数超过保留数量时，仍在运行的任务不会被清理，页面仍能读到每
// 台状态；全部结束后只回收最早完成的部分。
func TestConnectionTesterKeepsRunningTasksBeyondRetention(t *testing.T) {
	const taskCount = connectionTaskRetention + 8
	release := make(chan struct{})
	tester := ConnectionTesterFromOpener(func() (*servermanage.Manager, func(), error) {
		<-release
		return nil, func() {}, errors.New("加密库暂不可用")
	})
	taskIDs := make([]string, 0, taskCount)
	for i := 0; i < taskCount; i++ {
		taskID, err := tester.StartServerTest(
			fmt.Sprintf("srv-batch-%d", i), false, webui.ServerWriteSource{}, webui.ServerConnectionAnchor{},
		)
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, taskID)
	}
	for _, taskID := range taskIDs {
		status, ok := tester.ServerTestStatus(taskID)
		if !ok || status.State != webui.ServerConnectionTestRunning {
			t.Fatalf("运行中的任务不应因保留数量被清理：%+v ok=%v", status, ok)
		}
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		missing, running := 0, 0
		for _, taskID := range taskIDs {
			status, ok := tester.ServerTestStatus(taskID)
			if !ok {
				missing++
				continue
			}
			if status.State == webui.ServerConnectionTestRunning {
				running++
			}
		}
		if missing >= taskCount-connectionTaskRetention && running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("全部结束后应只保留 %d 条状态，实际缺失 %d、运行中 %d",
				connectionTaskRetention, missing, running)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestConnectionTesterReusesRunningTaskForSameServer 覆盖同一服务器条目的
// 去重边界：任务运行中再次启动时拿回原任务，不写第二条审计；原任务完成
// 后再启动才创建新任务。
func TestConnectionTesterReusesRunningTaskForSameServer(t *testing.T) {
	manager, st := newAdapterManager(t)
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "reuse", User: "deploy", Host: "127.0.0.1", Port: 1, ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	tester := ConnectionTesterFromOpener(func() (*servermanage.Manager, func(), error) {
		<-release
		return manager, func() {}, nil
	})

	firstID, err := tester.StartServerTest(
		added.ServerID, false, webui.ServerWriteSource{}, webui.ServerConnectionAnchor{},
	)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := tester.StartServerTest(
		added.ServerID, false, webui.ServerWriteSource{}, webui.ServerConnectionAnchor{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID {
		t.Fatalf("同一服务器条目运行中应拿回同一个任务：%q %q", firstID, secondID)
	}

	close(release)
	finished := waitConnectionTestStatus(t, tester, firstID)
	if finished.State != webui.ServerConnectionTestFailed {
		t.Fatalf("原任务应先完成并保存失败结论：%+v", finished)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("运行中重复启动不应写第二条连接测试审计：%v %+v", err, rows)
	}

	thirdID, err := tester.StartServerTest(
		added.ServerID, false, webui.ServerWriteSource{}, webui.ServerConnectionAnchor{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if thirdID == firstID {
		t.Fatalf("原任务完成后应创建新任务，仍拿到 %q", thirdID)
	}
	third := waitConnectionTestStatus(t, tester, thirdID)
	if third.State != webui.ServerConnectionTestFailed {
		t.Fatalf("新任务应独立完成：%+v", third)
	}
	rows, err = st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil || len(rows) != 2 {
		t.Fatalf("完成后的再次测试应写新审计：%v %+v", err, rows)
	}
}

// TestConnectionTesterOpenFailureIsNotCanceled 覆盖任务公共边界：打不开加
// 密库这类非取消错误归为其他连接失败，显示固定白话说明，不回显底层错误。
func TestConnectionTesterOpenFailureIsNotCanceled(t *testing.T) {
	openErr := errors.New("open vault with master-key-secret failed")
	tester := ConnectionTesterFromOpener(func() (*servermanage.Manager, func(), error) {
		return nil, func() {}, openErr
	})

	taskID, err := tester.StartServerTest("srv-missing", true, webui.ServerWriteSource{}, webui.ServerConnectionAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	status := waitConnectionTestStatus(t, tester, taskID)
	if status.State != webui.ServerConnectionTestFailed {
		t.Fatalf("打开加密库失败的任务应失败：%+v", status)
	}
	if status.Message == "连接被取消" || strings.Contains(status.Message, "master-key-secret") ||
		status.Message == "" {
		t.Fatalf("非取消错误不得显示为连接被取消或底层错误：%q", status.Message)
	}
	if status.Result == nil || status.Result.FailureLabel != "其他连接失败" || !status.Result.CompletedAt.IsZero() {
		t.Fatalf("非取消错误应归为其他连接失败且不带零值完成时间：%+v", status.Result)
	}
}

// TestWriterAddServerCopiesSiblingAnchor 覆盖挂到已有设备时的同一设备、同
// 一端口的其他服务器条目锚复制：结果页数据带来源条目，存储与审计沿用既
// 有 pin-copy 路径。
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
		t.Fatalf("新条目应复制同一设备、同一端口的其他服务器条目锚：%v %+v", err, pin)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"pin-copy"}})
	if err != nil || len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI ||
		rows[0].SourceIP != "192.0.2.6" ||
		!strings.Contains(rows[0].Command, `"来源服务器条目":"sibling"`) {
		t.Fatalf("锚复制审计应带来源条目：%v %+v", err, rows)
	}
	if !strings.Contains(rows[0].Command, `"fingerprint"`) ||
		strings.Contains(rows[0].Command, `"主机密钥指纹"`) {
		t.Fatalf("锚复制审计的指纹摘要键应使用英文键名：%s", rows[0].Command)
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
		"port": {"1"}, "shell": {"auto"},
		"address_network": {"other"}, "address": {"127.0.0.1"},
		"credential_action": {"password"}, "password": {"login-secret"},
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
	for _, want := range []string{"未验证", "shell 未验证", "默认 POSIX", "端口不通"} {
		if !strings.Contains(finalBody, want) {
			t.Fatalf("失败结果页缺少 %q：\n%s", want, finalBody)
		}
	}
	saved, err := st.GetServerByName("down")
	if err != nil || saved == nil || saved.ShellType != models.ShellPOSIX ||
		saved.AuthMethod != models.AuthPassword || saved.DeviceID == "" {
		t.Fatalf("连接失败后条目与凭据应保留：%v %+v", err, saved)
	}
	savedDevice, err := st.GetDeviceByID(saved.DeviceID)
	if err != nil || savedDevice == nil {
		t.Fatalf("设备应保留：%v %+v", err, savedDevice)
	}
	_ = profileID
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
