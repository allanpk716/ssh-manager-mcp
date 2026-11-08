package webui

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type recordingClientServerSource struct {
	instances []string
}

func (s *recordingClientServerSource) Servers(instance string) ([]ServerRow, error) {
	s.instances = append(s.instances, instance)
	return []ServerRow{{Name: "client-server-" + instance}}, nil
}

type stubClientOpsStore struct {
	*stubOpsStore

	switchInstances      []string
	switchSourceIPs      []string
	statusInstances      []string
	updateInstances      []string
	clientAuditInstances []string
	clientAuditRows      []OpsAuditRow
	switchErr            error
	syncInstances        []string
	syncSourceIPs        []string
	syncResult           OpsClientStatus
	syncErr              error
	listFunc             func() ([]OpsClientInstance, error)
	statusFunc           func(string) (OpsClientStatus, error)
}

func (s *stubOpsStore) ListClientInstances() ([]OpsClientInstance, error) {
	return nil, nil
}

func (s *stubOpsStore) ClientStatus(string) (OpsClientStatus, error) {
	return OpsClientStatus{}, nil
}

func (s *stubOpsStore) SwitchClientInstance(string, string) error { return nil }

func (s *stubOpsStore) SyncClient(string, string) (OpsClientStatus, error) {
	return OpsClientStatus{}, nil
}

func (s *stubClientOpsStore) ListClientInstances() ([]OpsClientInstance, error) {
	if s.listFunc != nil {
		return s.listFunc()
	}
	return []OpsClientInstance{
		{Name: "startup", DisplayName: "startup", Complete: true},
		{Name: "laptop", DisplayName: "laptop", Complete: true},
	}, nil
}

func (s *stubClientOpsStore) ClientStatus(instance string) (OpsClientStatus, error) {
	if s.statusFunc != nil {
		return s.statusFunc(instance)
	}
	s.statusInstances = append(s.statusInstances, instance)
	return OpsClientStatus{Instance: instance, InstanceDisplay: instance}, nil
}

func (s *stubClientOpsStore) CheckUpdate(instance string) (OpsUpdateStatus, error) {
	s.updateInstances = append(s.updateInstances, instance)
	return OpsUpdateStatus{}, nil
}

func (s *stubClientOpsStore) QueryClientAudit(instance string, limit int) ([]OpsAuditRow, error) {
	s.clientAuditInstances = append(s.clientAuditInstances, instance)
	return s.clientAuditRows, nil
}

func (s *stubClientOpsStore) SwitchClientInstance(instance, sourceIP string) error {
	s.switchInstances = append(s.switchInstances, instance)
	s.switchSourceIPs = append(s.switchSourceIPs, sourceIP)
	return s.switchErr
}

func (s *stubClientOpsStore) SyncClient(instance, sourceIP string) (OpsClientStatus, error) {
	s.syncInstances = append(s.syncInstances, instance)
	s.syncSourceIPs = append(s.syncSourceIPs, sourceIP)
	if s.syncErr != nil {
		return OpsClientStatus{}, s.syncErr
	}
	return s.syncResult, nil
}

func TestClientInstanceSwitchChangesServerListOnlyForItsSession(t *testing.T) {
	servers := &recordingClientServerSource{}
	ops := &stubClientOpsStore{stubOpsStore: &stubOpsStore{}}
	webHandler, startup, err := New(Config{
		Role: RoleClient, Servers: servers, Ops: ops, Port: 18080, ClientInstance: "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, webHandler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	firstList := doGetWithCookie(t, webHandler, cookie, "/ui/")
	firstList.Body.Close()
	clientPage := doGetWithCookie(t, webHandler, cookie, "/ui/ops/client")
	pageBody := readResponseBody(t, clientPage)
	if clientPage.StatusCode != http.StatusOK || !strings.Contains(pageBody, "客户端") {
		t.Fatalf("客户端页状态码=%d，页面：\n%s", clientPage.StatusCode, pageBody)
	}
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(pageBody)
	if match == nil {
		t.Fatalf("客户端页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}
	form := url.Values{"instance": {"laptop"}, "csrf_token": {match[1]}}
	switchResponse := postAccessWrite(t, webHandler, cookie, "/ui/ops/client/instance", form,
		"http://127.0.0.1:18080", match[1])
	switchResponse.Body.Close()
	if switchResponse.StatusCode != http.StatusSeeOther ||
		switchResponse.Header.Get("Location") != "/ui/ops/client" {
		t.Fatalf("切换实例状态码=%d 跳转=%q", switchResponse.StatusCode, switchResponse.Header.Get("Location"))
	}
	if len(ops.switchInstances) != 1 || ops.switchInstances[0] != "laptop" ||
		len(ops.switchSourceIPs) != 1 || ops.switchSourceIPs[0] != "192.0.2.1" {
		t.Fatalf("实例切换调用不符：%+v %+v", ops.switchInstances, ops.switchSourceIPs)
	}

	switchedList := doGetWithCookie(t, webHandler, cookie, "/ui/")
	switchedList.Body.Close()

	root := webHandler.(*handler)
	root.mu.Lock()
	firstSession := root.session
	secondSession, sessionErr := newSession()
	if sessionErr != nil {
		root.mu.Unlock()
		t.Fatal(sessionErr)
	}
	secondSession.clientInstance = root.cfg.ClientInstance
	root.session = secondSession
	root.mu.Unlock()
	secondCookie := &http.Cookie{Name: cookie.Name, Value: secondSession.id}
	secondList := doGetWithCookie(t, webHandler, secondCookie, "/ui/")
	secondList.Body.Close()
	root.mu.Lock()
	root.session = firstSession
	root.mu.Unlock()
	firstAgain := doGetWithCookie(t, webHandler, cookie, "/ui/")
	firstAgain.Body.Close()

	want := []string{"startup", "laptop", "startup", "laptop"}
	if strings.Join(servers.instances, ",") != strings.Join(want, ",") {
		t.Fatalf("两个会话的服务器列表实例 = %v，应为 %v", servers.instances, want)
	}
}

func TestClientPagesUseSessionInstance(t *testing.T) {
	ops := &stubClientOpsStore{stubOpsStore: &stubOpsStore{}}
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Ops: ops, Port: 18080, ClientInstance: "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	clientPage := doGetWithCookie(t, handler, cookie, "/ui/ops/client")
	pageBody := readResponseBody(t, clientPage)
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(pageBody)
	if match == nil {
		t.Fatalf("客户端页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}
	form := url.Values{"instance": {"laptop"}, "csrf_token": {match[1]}}
	switchResponse := postAccessWrite(t, handler, cookie, "/ui/ops/client/instance", form,
		"http://127.0.0.1:18080", match[1])
	switchResponse.Body.Close()
	if switchResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("切换实例状态码=%d", switchResponse.StatusCode)
	}

	for _, target := range []string{"/ui/ops/client", "/ui/ops/update", "/ui/ops/client-audit"} {
		response := doGetWithCookie(t, handler, cookie, target)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码=%d", target, response.StatusCode)
		}
	}
	if strings.Join(ops.statusInstances, ",") != "startup,laptop" {
		t.Fatalf("客户端页读取实例 = %v，应为 [laptop]", ops.statusInstances)
	}
	if strings.Join(ops.updateInstances, ",") != "laptop" {
		t.Fatalf("升级页读取实例 = %v，应为 [laptop]", ops.updateInstances)
	}
	if strings.Join(ops.clientAuditInstances, ",") != "laptop" {
		t.Fatalf("本机审计页读取实例 = %v，应为 [laptop]", ops.clientAuditInstances)
	}
}

func TestClientInstanceSwitchRejectsIncompleteAndUnsafeRequests(t *testing.T) {
	servers := &recordingClientServerSource{}
	ops := &stubClientOpsStore{stubOpsStore: &stubOpsStore{}, switchErr: errors.New("实例 broken 缺少快照，不能切换")}
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: servers, Ops: ops, Port: 18080, ClientInstance: "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	clientPage := doGetWithCookie(t, handler, cookie, "/ui/ops/client")
	pageBody := readResponseBody(t, clientPage)
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(pageBody)
	if match == nil {
		t.Fatalf("客户端页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}

	incomplete := postAccessWrite(t, handler, cookie, "/ui/ops/client/instance",
		url.Values{"instance": {"broken"}, "csrf_token": {match[1]}},
		"http://127.0.0.1:18080", match[1])
	incompleteBody := readResponseBody(t, incomplete)
	if incomplete.StatusCode != http.StatusBadRequest || !strings.Contains(incompleteBody, "缺少快照") {
		t.Fatalf("材料不齐全的切换应失败，状态码=%d 页面：\n%s", incomplete.StatusCode, incompleteBody)
	}

	missingToken := postAccessWrite(t, handler, cookie, "/ui/ops/client/instance",
		url.Values{"instance": {"laptop"}}, "http://127.0.0.1:18080", "")
	missingToken.Body.Close()
	wrongOrigin := postAccessWrite(t, handler, cookie, "/ui/ops/client/instance",
		url.Values{"instance": {"laptop"}, "csrf_token": {match[1]}},
		"https://evil.example", match[1])
	wrongOrigin.Body.Close()
	if missingToken.StatusCode != http.StatusForbidden || wrongOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("缺令牌或来源不对应返回 403，实际 %d 与 %d",
			missingToken.StatusCode, wrongOrigin.StatusCode)
	}

	list := doGetWithCookie(t, handler, cookie, "/ui/")
	list.Body.Close()
	if strings.Join(servers.instances, ",") != "startup" {
		t.Fatalf("失败与被拒请求后不应切换实例，实际读取实例：%v", servers.instances)
	}
	if len(ops.switchInstances) != 1 || ops.switchInstances[0] != "broken" {
		t.Fatalf("只有带令牌且来源正确的请求应进入切换：%v", ops.switchInstances)
	}
}

func TestClientSyncUsesSessionInstanceAndShowsResult(t *testing.T) {
	ops := &stubClientOpsStore{
		stubOpsStore: &stubOpsStore{},
		syncResult: OpsClientStatus{
			Instance: "laptop", InstanceDisplay: "laptop", SnapshotRevision: 43, ServerCount: 2,
		},
	}
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Ops: ops, Port: 18080, ClientInstance: "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	clientPage := doGetWithCookie(t, handler, cookie, "/ui/ops/client")
	pageBody := readResponseBody(t, clientPage)
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(pageBody)
	if match == nil {
		t.Fatalf("客户端页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}
	form := url.Values{"instance": {"laptop"}, "csrf_token": {match[1]}}
	switchResponse := postAccessWrite(t, handler, cookie, "/ui/ops/client/instance", form,
		"http://127.0.0.1:18080", match[1])
	switchResponse.Body.Close()
	if switchResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("切换实例状态码=%d", switchResponse.StatusCode)
	}

	syncResponse := postAccessWrite(t, handler, cookie, "/ui/ops/client/sync",
		url.Values{"csrf_token": {match[1]}}, "http://127.0.0.1:18080", match[1])
	syncBody := readResponseBody(t, syncResponse)
	if syncResponse.StatusCode != http.StatusOK || !strings.Contains(syncBody, "已同步") ||
		!strings.Contains(syncBody, "43") || !strings.Contains(syncBody, "2 台服务器") {
		t.Fatalf("立即同步状态码=%d 页面：\n%s", syncResponse.StatusCode, syncBody)
	}
	if len(ops.syncInstances) != 1 || ops.syncInstances[0] != "laptop" ||
		len(ops.syncSourceIPs) != 1 || ops.syncSourceIPs[0] != "192.0.2.1" {
		t.Fatalf("立即同步调用不符：%+v %+v", ops.syncInstances, ops.syncSourceIPs)
	}
}

func TestClientPageShowsStatusAndInstanceMaterials(t *testing.T) {
	pulledAt := time.Unix(1800000000, 0)
	ops := &stubClientOpsStore{stubOpsStore: &stubOpsStore{}}
	ops.listFunc = func() ([]OpsClientInstance, error) {
		return []OpsClientInstance{
			{Name: "", DisplayName: "默认实例", SnapshotAge: 90 * time.Second, HasSnapshot: true, Complete: true},
			{Name: "broken", DisplayName: "broken", MissingMaterials: []string{"快照", "数据加密密钥"}},
		}, nil
	}
	ops.statusFunc = func(string) (OpsClientStatus, error) {
		return OpsClientStatus{
			InstanceDisplay: "默认实例", AuthorityURL: "https://authority:7878",
			CertificateFingerprint: "sha256:aa", Profile: "团队", ServerCount: 3,
			PulledAt: pulledAt, SnapshotRevision: 88, RecommendedVersion: "1.2.0",
		}, nil
	}
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Ops: ops, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	response := doGetWithCookie(t, handler, cookie, "/ui/ops/client")
	body := readResponseBody(t, response)
	for _, want := range []string{
		"https://authority:7878", "sha256:aa", "团队", "88", "1.2.0",
		pulledAt.Format("2006-01-02 15:04:05"), "1m30s", "快照、数据加密密钥", "不能切换",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端页缺少 %q：\n%s", want, body)
		}
	}
}
