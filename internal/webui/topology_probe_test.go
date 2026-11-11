package webui

// 拓扑图探测接口的处理器测试：只断言 JSON 响应、会话、防跨站请求伪造与角
// 色限制；探测本身的执行语义由 webuitopology 适配器测试覆盖。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTopologyProbe 记录收到的请求并返回固定结果。
type stubTopologyProbe struct {
	overlayInstance string
	overlayFrom     string
	startInstance   string
	startFrom       string
	startTargets    []string
	startCalls      int
	nextStartID     string
	nextStartErr    error
	nextOverlay     *TopologyProbeOverlay
	nextOverlayErr  error
}

func (s *stubTopologyProbe) Overlay(instance, from string) (*TopologyProbeOverlay, error) {
	s.overlayInstance = instance
	s.overlayFrom = from
	if s.nextOverlayErr != nil {
		return nil, s.nextOverlayErr
	}
	if s.nextOverlay != nil {
		return s.nextOverlay, nil
	}
	return &TopologyProbeOverlay{
		Origin:  TopologyProbeOrigin{From: from, Known: true, DeviceID: "device-1"},
		Devices: []TopologyProbeDevice{{DeviceID: "device-2", Status: "unprobed"}},
	}, nil
}

func (s *stubTopologyProbe) StartProbe(instance, from string, targets []string) (string, error) {
	s.startCalls++
	s.startInstance = instance
	s.startFrom = from
	s.startTargets = append([]string(nil), targets...)
	if s.nextStartErr != nil {
		return "", s.nextStartErr
	}
	if s.nextStartID == "" {
		s.nextStartID = "task-1"
	}
	return s.nextStartID, nil
}

func (s *stubTopologyProbe) ProbeStatus(taskID string) (*TopologyProbeTaskStatus, bool) {
	if taskID != "task-1" {
		return nil, false
	}
	return &TopologyProbeTaskStatus{
		ID: taskID, State: "done", From: "self", Targets: []string{"NAS"},
		Message: "探测完成：1 台可达。",
	}, true
}

func newProbeTestHandler(t *testing.T, role Role, source TopologySource, clientInstance string, probe TopologyProbe) (http.Handler, Startup) {
	t.Helper()
	handler, startup, err := New(Config{
		Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Topology: source, TopologyProbe: probe, ClientInstance: clientInstance,
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI 失败：%v", err)
	}
	return handler, startup
}

func probeSession(t *testing.T, handler http.Handler, startup Startup) *http.Cookie {
	t.Helper()
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return cookies[0]
}

func probeCSRF(t *testing.T, h http.Handler) string {
	t.Helper()
	root := h.(*handler)
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.session.csrf
}

func probeJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("响应不是 JSON 对象：%s", body)
	}
	return decoded
}

// TestTopologyProbeEndpointsSessionAndCSRF: 三个接口都要求会话；启动接口再
// 要求防跨站请求伪造令牌。
func TestTopologyProbeEndpointsSessionAndCSRF(t *testing.T) {
	handler, startup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", &stubTopologyProbe{})
	cookie := probeSession(t, handler, startup)

	anonymous := httptest.NewRequest(http.MethodGet, "/ui/api/topology/probe/overlay?from=self", nil)
	anonymous.Host = "127.0.0.1:18080"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, anonymous)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无会话读取叠加状态码 = %d，应为 401", recorder.Code)
	}

	noCSRF := postTopologyJSON(t, handler, cookie, "", "/ui/api/topology/probe/start", `{"from":"self","targets":["NAS"]}`)
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("缺令牌启动探测状态码 = %d，应为 403", noCSRF.StatusCode)
	}
	noCSRF.Body.Close()

	csrf := probeCSRF(t, handler)
	started := postTopologyJSON(t, handler, cookie, csrf, "/ui/api/topology/probe/start", `{"from":"self","targets":["NAS"]}`)
	if started.StatusCode != http.StatusOK {
		t.Fatalf("带令牌启动探测状态码 = %d，应为 200", started.StatusCode)
	}
	body := probeJSON(t, started)
	if body["task_id"] != "task-1" {
		t.Fatalf("启动响应应携带任务标识：%v", body)
	}
}

// TestTopologyProbeStartValidationAndConflicts: 空目标与非法 JSON 返回 400；
// 已有任务进行中返回 409；适配层错误原样显示。
func TestTopologyProbeStartValidationAndConflicts(t *testing.T) {
	probeStub := &stubTopologyProbe{}
	handler, startup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", probeStub)
	cookie := probeSession(t, handler, startup)
	csrf := probeCSRF(t, handler)

	empty := postTopologyJSON(t, handler, cookie, csrf, "/ui/api/topology/probe/start", `{"from":"self","targets":[]}`)
	if empty.StatusCode != http.StatusBadRequest {
		t.Fatalf("空目标状态码 = %d，应为 400", empty.StatusCode)
	}
	if message := probeJSON(t, empty)["error"]; !strings.Contains(message.(string), "目标") {
		t.Fatalf("空目标错误提示错误：%v", message)
	}

	broken := postTopologyJSON(t, handler, cookie, csrf, "/ui/api/topology/probe/start", `{not-json`)
	if broken.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 JSON 状态码 = %d，应为 400", broken.StatusCode)
	}
	broken.Body.Close()

	probeStub.nextStartErr = ErrProbeTaskRunning
	conflict := postTopologyJSON(t, handler, cookie, csrf, "/ui/api/topology/probe/start", `{"from":"self","targets":["NAS"]}`)
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("进行中重复触发状态码 = %d，应为 409", conflict.StatusCode)
	}
	if message := probeJSON(t, conflict)["error"]; !strings.Contains(message.(string), "正在进行") {
		t.Fatalf("重复触发提示错误：%v", message)
	}

	probeStub.nextStartErr = &TopologyInvalidInputError{Message: "起点服务器不存在"}
	failed := postTopologyJSON(t, handler, cookie, csrf, "/ui/api/topology/probe/start", `{"from":"missing","targets":["NAS"]}`)
	if failed.StatusCode != http.StatusBadRequest {
		t.Fatalf("适配层错误状态码 = %d，应为 400", failed.StatusCode)
	}
	if message := probeJSON(t, failed)["error"]; message != "起点服务器不存在" {
		t.Fatalf("适配层错误应原样显示：%v", message)
	}
}

func TestTopologyProbeStartRejectsOversizedJSON(t *testing.T) {
	probeStub := &stubTopologyProbe{}
	handler, startup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", probeStub)
	cookie := probeSession(t, handler, startup)
	csrf := probeCSRF(t, handler)

	body := `{"targets":["` + strings.Repeat("x", topologyWriteMaxRequestBody) + `"]}`
	response := postTopologyJSON(t, handler, cookie, csrf, topologyProbeStartPath, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("探测启动请求超限状态码 = %d，应为 413", response.StatusCode)
	}
	var payload map[string]string
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload["error"] == "" {
		t.Fatalf("探测启动请求超限应返回 JSON 中文错误：%v %v", err, payload)
	}
	if probeStub.startCalls != 0 {
		t.Fatalf("超限请求不应启动探测任务，实际调用 %d 次", probeStub.startCalls)
	}
}

func TestTopologyProbeStartSeparatesValidationAndInternalErrors(t *testing.T) {
	cases := []struct {
		name    string
		error   error
		status  int
		message string
	}{
		{
			name:    "起点不存在返回400并保留提示",
			error:   &TopologyInvalidInputError{Message: "起点服务器不存在"},
			status:  http.StatusBadRequest,
			message: "起点服务器不存在",
		},
		{
			name:    "内部错误返回500与统一提示",
			error:   errors.New("打开拓扑数据失败：数据库连接已断开"),
			status:  http.StatusInternalServerError,
			message: "启动连通探测失败，请稍后重试。",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probeStub := &stubTopologyProbe{nextStartErr: tc.error}
			handler, startup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", probeStub)
			cookie := probeSession(t, handler, startup)
			csrf := probeCSRF(t, handler)
			response := postTopologyJSON(t, handler, cookie, csrf, topologyProbeStartPath,
				`{"from":"missing","targets":["NAS"]}`)
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("状态码 = %d，应为 %d", response.StatusCode, tc.status)
			}
			var payload struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error != tc.message {
				t.Fatalf("错误提示 = %q，应为 %q", payload.Error, tc.message)
			}
		})
	}
}

// TestTopologyProbeClientOriginRestriction: 客户端只能以本机为起点；单机可
// 选择受管服务器。
func TestTopologyProbeClientOriginRestriction(t *testing.T) {
	clientProbe := &stubTopologyProbe{}
	client, clientStartup := newProbeTestHandler(t, RoleClient, &stubTopologySource{data: topologyFixture()}, "", clientProbe)
	cookie := probeSession(t, client, clientStartup)
	csrf := probeCSRF(t, client)

	remoteStart := postTopologyJSON(t, client, cookie, csrf, "/ui/api/topology/probe/start", `{"from":"srv-1","targets":["NAS"]}`)
	if remoteStart.StatusCode != http.StatusBadRequest || clientProbe.startFrom != "" {
		t.Fatalf("客户端选择服务器起点应被拒绝，状态码=%d", remoteStart.StatusCode)
	}
	if message := probeJSON(t, remoteStart)["error"]; !strings.Contains(message.(string), "客户端") {
		t.Fatalf("客户端起点提示错误：%v", message)
	}

	localStart := postTopologyJSON(t, client, cookie, csrf, "/ui/api/topology/probe/start", `{"from":"self","targets":["NAS"]}`)
	if localStart.StatusCode != http.StatusOK {
		t.Fatalf("客户端本机起点应允许，状态码=%d", localStart.StatusCode)
	}
	localStart.Body.Close()
	if clientProbe.startFrom != "self" || clientProbe.startInstance != "" {
		t.Fatalf("客户端应传默认实例与本机起点：%+v", clientProbe)
	}

	standaloneProbe := &stubTopologyProbe{}
	standalone, standaloneStartup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", standaloneProbe)
	standaloneCookie := probeSession(t, standalone, standaloneStartup)
	standaloneCSRF := probeCSRF(t, standalone)
	remote := postTopologyJSON(t, standalone, standaloneCookie, standaloneCSRF, "/ui/api/topology/probe/start", `{"from":"srv-1","targets":["NAS"]}`)
	if remote.StatusCode != http.StatusOK || standaloneProbe.startFrom != "srv-1" {
		t.Fatalf("单机选择服务器起点应放行：%d %+v", remote.StatusCode, standaloneProbe)
	}
	remote.Body.Close()
}

// TestTopologyProbeOverlayAndStatusResponses: 叠加接口转发起点并返回叠加
// JSON；状态接口对未知任务返回 404。
func TestTopologyProbeOverlayAndStatusResponses(t *testing.T) {
	probeStub := &stubTopologyProbe{}
	handler, startup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", probeStub)
	cookie := probeSession(t, handler, startup)

	overlayRequest := httptest.NewRequest(http.MethodGet, "/ui/api/topology/probe/overlay?from=srv-1", nil)
	overlayRequest.Host = "127.0.0.1:18080"
	overlayRequest.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, overlayRequest)
	overlay := recorder.Result()
	if overlay.StatusCode != http.StatusOK {
		t.Fatalf("读取叠加状态码 = %d，应为 200", overlay.StatusCode)
	}
	body := probeJSON(t, overlay)
	if probeStub.overlayFrom != "srv-1" {
		t.Fatalf("叠加接口应转发起点：%+v", probeStub)
	}
	devices, _ := body["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("叠加响应应包含设备列表：%v", body)
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/ui/api/topology/probe/status?task=missing", nil)
	statusRequest.Host = "127.0.0.1:18080"
	statusRequest.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, statusRequest)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知任务状态码 = %d，应为 404", recorder.Code)
	}

	statusRequest = httptest.NewRequest(http.MethodGet, "/ui/api/topology/probe/status?task=task-1", nil)
	statusRequest.Host = "127.0.0.1:18080"
	statusRequest.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, statusRequest)
	status := recorder.Result()
	if status.StatusCode != http.StatusOK {
		t.Fatalf("已知任务状态码 = %d，应为 200", status.StatusCode)
	}
	statusBody := probeJSON(t, status)
	if statusBody["state"] != "done" || statusBody["message"] == "" {
		t.Fatalf("任务状态响应错误：%v", statusBody)
	}
}

// TestTopologyProbeRoutesNeedProbeSource: 未注入探测来源的构造不注册探测接
// 口。
func TestTopologyProbeRoutesNeedProbeSource(t *testing.T) {
	handler, startup := newProbeTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", nil)
	cookie := probeSession(t, handler, startup)
	for _, path := range []string{
		"/ui/api/topology/probe/overlay?from=self",
		"/ui/api/topology/probe/status?task=task-1",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = "127.0.0.1:18080"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，应为 404", path, recorder.Code)
		}
	}
	start := postTopologyJSON(t, handler, cookie, probeCSRF(t, handler), "/ui/api/topology/probe/start", `{"from":"self","targets":["NAS"]}`)
	defer start.Body.Close()
	if start.StatusCode != http.StatusNotFound {
		t.Fatalf("未注册启动接口状态码 = %d，应为 404", start.StatusCode)
	}
}
