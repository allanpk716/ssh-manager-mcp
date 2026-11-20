package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// batchConnectionTesterStub 是批量连接测试处理器的替身：可为每台服务器条
// 目返回不同任务，测试直接改写任务状态，覆盖逐卡状态、汇总与错误路径。
type batchConnectionTesterStub struct {
	mu       sync.Mutex
	starts   []stubServerConnectionStart
	statuses map[string]*ServerConnectionTestStatus
}

func newBatchConnectionTesterStub() *batchConnectionTesterStub {
	return &batchConnectionTesterStub{statuses: map[string]*ServerConnectionTestStatus{}}
}

func (s *batchConnectionTesterStub) StartServerTest(
	serverID string, probeShell bool, source ServerWriteSource, anchor ServerConnectionAnchor,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	taskID := fmt.Sprintf("task-%d", len(s.starts)+1)
	s.starts = append(s.starts, stubServerConnectionStart{
		serverID: serverID, probeShell: probeShell, source: source, anchor: anchor,
	})
	s.statuses[taskID] = &ServerConnectionTestStatus{
		ID: taskID, State: ServerConnectionTestRunning, ServerID: serverID, ProbeShell: probeShell,
	}
	return taskID, nil
}

func (s *batchConnectionTesterStub) ServerTestStatus(taskID string) (*ServerConnectionTestStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.statuses[taskID]
	if !ok {
		return nil, false
	}
	copied := *status
	return &copied, true
}

func (s *batchConnectionTesterStub) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

func (s *batchConnectionTesterStub) finish(taskID string, status ServerConnectionTestStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status.ID = taskID
	s.statuses[taskID] = &status
}

type batchConnectionStartResponse struct {
	Tasks []struct {
		ServerID string `json:"server_id"`
		TaskID   string `json:"task_id"`
	} `json:"tasks"`
}

type batchConnectionStatusResponse struct {
	Cards []struct {
		ServerID string `json:"server_id"`
		TaskID   string `json:"task_id"`
		State    string `json:"state"`
		Title    string `json:"title"`
		Detail   string `json:"detail"`
	} `json:"cards"`
	Done      bool `json:"done"`
	Total     int  `json:"total"`
	Running   int  `json:"running"`
	Succeeded int  `json:"succeeded"`
	Failed    int  `json:"failed"`
	Missing   int  `json:"missing"`
	Reload    bool `json:"reload"`
}

func newServerBatchConnectionTestEnv(
	t *testing.T, role Role, tester ServerConnectionTester,
) (*batchConnectionTesterStub, http.Handler, *http.Cookie) {
	t.Helper()
	store := newStubServerStore()
	store.rows = append(store.rows, ServerListEntry{
		ID: "srv-auto", Name: "auto", User: "root", Host: "192.0.2.12", Port: 22,
	})
	store.details["srv-auto"] = &ServerDetailView{
		ID: "srv-auto", Name: "auto", User: "root", Host: "192.0.2.12", Port: 22,
	}
	var writer ServerWriter
	if role != RoleClient {
		writer = &stubServerWriter{}
	}
	handler, startup, err := New(Config{
		Role: role, Servers: store, ServerWriter: writer,
		ServerConnectionTester: tester, Topology: serverListTopologySource{},
		Access: &stubAccessStore{}, VersionHint: func() string { return "" },
		Now: func() time.Time {
			return time.Date(2026, 11, 20, 15, 4, 5, 0, time.Local)
		},
		Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	stub, _ := tester.(*batchConnectionTesterStub)
	return stub, handler, cookie
}

func decodeBatchConnectionResponse(t *testing.T, response *http.Response, value any) {
	t.Helper()
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("批量连接测试接口应返回 JSON，实际 %q", contentType)
	}
	if err := json.NewDecoder(response.Body).Decode(value); err != nil {
		t.Fatalf("解析批量连接测试 JSON 失败：%v", err)
	}
}

// TestServerBatchConnectionTestStartsOneTaskPerServer 覆盖批量启动与逐卡
// 状态：重复的服务器条目只启动一个任务，进行中与完成状态按台返回，汇总
// 计数准确。
func TestServerBatchConnectionTestStartsOneTaskPerServer(t *testing.T) {
	stub, handler, cookie := newServerBatchConnectionTestEnv(t, RoleStandalone, newBatchConnectionTesterStub())

	response := serversPost(t, handler, cookie, "/ui/servers/batch/connection-test", url.Values{
		"server": {"srv-fine", "srv-auto", "srv-fine"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("批量启动状态码 = %d，应为 200", response.StatusCode)
	}
	var started batchConnectionStartResponse
	decodeBatchConnectionResponse(t, response, &started)
	if len(started.Tasks) != 2 ||
		started.Tasks[0].ServerID != "srv-fine" || started.Tasks[0].TaskID != "task-1" ||
		started.Tasks[1].ServerID != "srv-auto" || started.Tasks[1].TaskID != "task-2" {
		t.Fatalf("应按首次出现顺序为每台各返回一个任务：%+v", started.Tasks)
	}
	if stub.startCount() != 2 {
		t.Fatalf("重复的服务器条目不应重复启动，实际启动 %d 次", stub.startCount())
	}
	stub.mu.Lock()
	first, second := stub.starts[0], stub.starts[1]
	stub.mu.Unlock()
	if first.probeShell || !second.probeShell {
		t.Fatalf("shell 探测应沿用单台语义（已登记 shell 不探测）：%+v %+v", first, second)
	}
	if first.source.RemoteAddress == "" || first.source.RemoteAddress != second.source.RemoteAddress {
		t.Fatalf("批量任务应记录同一个来源地址：%+v %+v", first, second)
	}

	statusBody := serversGet(t, handler, cookie, "/ui/servers/batch/connection-test?task=task-1&task=task-2")
	var runningStatus batchConnectionStatusResponse
	if err := json.Unmarshal([]byte(statusBody), &runningStatus); err != nil {
		t.Fatalf("解析进行中状态失败：%v\n%s", err, statusBody)
	}
	if runningStatus.Done || runningStatus.Running != 2 || runningStatus.Total != 2 ||
		len(runningStatus.Cards) != 2 || runningStatus.Cards[0].State != "running" ||
		runningStatus.Cards[0].Title != "正在连接" {
		t.Fatalf("两台都应返回逐卡进行中状态：%+v", runningStatus)
	}

	completedAt := time.Date(2026, 11, 20, 10, 12, 0, 0, time.Local)
	stub.finish("task-1", ServerConnectionTestStatus{
		State: ServerConnectionTestSucceeded, ServerID: "srv-fine",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestSucceeded, CompletedAt: completedAt, Succeeded: true,
			SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网",
			Duration: 120 * time.Millisecond, Persisted: true,
		},
	})
	stub.finish("task-2", ServerConnectionTestStatus{
		State: ServerConnectionTestFailed, ServerID: "srv-auto",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestFailed, CompletedAt: completedAt,
			FailureLabel: "端口不通", Persisted: true,
		},
	})
	doneBody := serversGet(t, handler, cookie, "/ui/servers/batch/connection-test?task=task-1&task=task-2")
	var doneStatus batchConnectionStatusResponse
	if err := json.Unmarshal([]byte(doneBody), &doneStatus); err != nil {
		t.Fatalf("解析完成状态失败：%v\n%s", err, doneBody)
	}
	if !doneStatus.Done || doneStatus.Succeeded != 1 || doneStatus.Failed != 1 || doneStatus.Running != 0 {
		t.Fatalf("全部完成后应给出准确汇总：%+v", doneStatus)
	}
	if doneStatus.Cards[0].Title != "今天 10:12 已连上" ||
		!strings.Contains(doneStatus.Cards[0].Detail, "192.0.2.10") ||
		!strings.Contains(doneStatus.Cards[0].Detail, "耗时 120 毫秒") {
		t.Fatalf("成功卡片应复用 #326 的短状态与汇总文案：%+v", doneStatus.Cards[0])
	}
	if doneStatus.Cards[1].Title != "今天 10:12 连不上：端口不通" {
		t.Fatalf("失败卡片应显示连不上与白话原因：%+v", doneStatus.Cards[1])
	}
	if doneStatus.Missing != 0 || doneStatus.Reload {
		t.Fatalf("任务都存在时不应要求刷新：%+v", doneStatus)
	}
}

// TestServerBatchConnectionTestStartErrorPaths 覆盖启动入口错误路径：空选
// 择、不存在的服务器条目、缺少连接测试来源与方法不允许都不启动任何任务。
func TestServerBatchConnectionTestStartErrorPaths(t *testing.T) {
	cases := []struct {
		name      string
		form      url.Values
		noTester  bool
		wantCode  int
		wantError string
	}{
		{
			name: "未选择服务器", form: url.Values{}, wantCode: http.StatusBadRequest,
			wantError: "请先选择要测试的服务器。",
		},
		{
			name: "服务器不存在", form: url.Values{"server": {"srv-missing"}},
			wantCode: http.StatusBadRequest, wantError: "选中的服务器不存在",
		},
		{
			name: "缺少连接测试来源", form: url.Values{"server": {"srv-fine"}}, noTester: true,
			wantCode: http.StatusBadRequest, wantError: "当前部署没有提供连接测试来源。",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var tester ServerConnectionTester
			if !testCase.noTester {
				tester = newBatchConnectionTesterStub()
			}
			stub, handler, cookie := newServerBatchConnectionTestEnv(t, RoleStandalone, tester)
			response := serversPost(t, handler, cookie, "/ui/servers/batch/connection-test", testCase.form)
			if response.StatusCode != testCase.wantCode {
				t.Fatalf("状态码 = %d，应为 %d", response.StatusCode, testCase.wantCode)
			}
			var body struct {
				Error string `json:"error"`
			}
			decodeBatchConnectionResponse(t, response, &body)
			if !strings.Contains(body.Error, testCase.wantError) {
				t.Fatalf("错误说明 = %q，应包含 %q", body.Error, testCase.wantError)
			}
			if stub != nil && stub.startCount() != 0 {
				t.Fatalf("错误路径不应启动任务，实际启动 %d 次", stub.startCount())
			}
		})
	}

	stub, handler, cookie := newServerBatchConnectionTestEnv(t, RoleStandalone, newBatchConnectionTesterStub())
	request := httptest.NewRequest(http.MethodPut, "/ui/servers/batch/connection-test", nil)
	request.Host = "127.0.0.1:18080"
	request.Header.Set("Origin", "http://127.0.0.1:18080")
	request.Header.Set("X-CSRF-Token", serversCSRF(t, handler, cookie))
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT 状态码 = %d，应为 405", recorder.Code)
	}
	if stub.startCount() != 0 {
		t.Fatalf("方法不允许时不应启动任务，实际启动 %d 次", stub.startCount())
	}
}

// TestServerBatchConnectionTestClientRoleHasNoRoute 校验客户端只读角色没
// 有批量连接测试写入口，请求不会到达任务启动。
func TestServerBatchConnectionTestClientRoleHasNoRoute(t *testing.T) {
	stub, handler, cookie := newServerBatchConnectionTestEnv(t, RoleClient, newBatchConnectionTesterStub())
	response := serversPost(t, handler, cookie, "/ui/servers/batch/connection-test", url.Values{
		"server": {"srv-fine"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("客户端只读角色状态码 = %d，应为 404", response.StatusCode)
	}
	if stub.startCount() != 0 {
		t.Fatalf("客户端只读角色不应启动任务，实际启动 %d 次", stub.startCount())
	}
}

// TestServerBatchConnectionTestRequiresCSRF 校验防跨站请求伪造失败时不启
// 动任何任务。
func TestServerBatchConnectionTestRequiresCSRF(t *testing.T) {
	stub, handler, cookie := newServerBatchConnectionTestEnv(t, RoleStandalone, newBatchConnectionTesterStub())
	request := httptest.NewRequest(
		http.MethodPost, "/ui/servers/batch/connection-test",
		strings.NewReader(url.Values{"server": {"srv-fine"}}.Encode()),
	)
	request.Host = "127.0.0.1:18080"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:18080")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("缺少令牌状态码 = %d，应为 403", recorder.Code)
	}
	if stub.startCount() != 0 {
		t.Fatalf("防跨站请求伪造失败不应启动任务，实际启动 %d 次", stub.startCount())
	}
}

// TestServerBatchConnectionTestStatusMissingTaskRequiresReload 覆盖保留策
// 略回收任务后的轮询：已回收任务标记缺失并要求整页读取持久结果，不猜
// 测结论；缺少任务参数直接给出明确错误。
func TestServerBatchConnectionTestStatusMissingTaskRequiresReload(t *testing.T) {
	stub, handler, cookie := newServerBatchConnectionTestEnv(t, RoleStandalone, newBatchConnectionTesterStub())
	response := serversPost(t, handler, cookie, "/ui/servers/batch/connection-test", url.Values{
		"server": {"srv-fine"},
	})
	var started batchConnectionStartResponse
	decodeBatchConnectionResponse(t, response, &started)
	if len(started.Tasks) != 1 {
		t.Fatalf("应启动一个任务：%+v", started.Tasks)
	}
	taskID := started.Tasks[0].TaskID

	missingBody := serversGet(t, handler, cookie,
		"/ui/servers/batch/connection-test?task="+url.QueryEscape(taskID)+"&task=task-missing")
	var missingStatus batchConnectionStatusResponse
	if err := json.Unmarshal([]byte(missingBody), &missingStatus); err != nil {
		t.Fatalf("解析状态失败：%v\n%s", err, missingBody)
	}
	if missingStatus.Done || missingStatus.Running != 1 || missingStatus.Missing != 1 || !missingStatus.Reload {
		t.Fatalf("一台仍在运行、一台已回收时应继续轮询并要求最终刷新：%+v", missingStatus)
	}

	stub.finish(taskID, ServerConnectionTestStatus{
		State: ServerConnectionTestFailed, ServerID: "srv-fine",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestFailed, FailureLabel: "端口不通",
		},
	})
	doneBody := serversGet(t, handler, cookie,
		"/ui/servers/batch/connection-test?task="+url.QueryEscape(taskID)+"&task=task-missing")
	var doneStatus batchConnectionStatusResponse
	if err := json.Unmarshal([]byte(doneBody), &doneStatus); err != nil {
		t.Fatalf("解析完成状态失败：%v\n%s", err, doneBody)
	}
	if !doneStatus.Done || doneStatus.Failed != 1 || doneStatus.Missing != 1 || !doneStatus.Reload {
		t.Fatalf("全部结束后应带缺失计数并要求整页刷新：%+v", doneStatus)
	}

	emptyBody, emptyCode := serversGetStatus(t, handler, cookie, "/ui/servers/batch/connection-test")
	if emptyCode != http.StatusBadRequest || !strings.Contains(emptyBody, "缺少连接测试任务") {
		t.Fatalf("缺少任务参数应返回明确错误，状态码=%d：%s", emptyCode, emptyBody)
	}
}
