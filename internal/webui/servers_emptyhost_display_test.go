package webui

import (
	"net/http"
	"strings"
	"testing"
)

// emptyHostTopologySource 提供一台名为「工作站」的设备；带候选时物理局域
// 网候选 192.0.2.10，不带候选时设备没有任何连接候选。
type emptyHostTopologySource struct {
	withCandidate bool
}

func (s emptyHostTopologySource) Topology(string, string) (*TopologyData, error) {
	device := TopologyDevice{ID: "device-1", Name: "工作站"}
	if s.withCandidate {
		device.Candidates = []TopologyCandidate{{Address: "192.0.2.10", Source: "physical"}}
	}
	return &TopologyData{
		Networks: []TopologyNetwork{{ID: "net-physical", Name: "家A局域网", Type: "physical"}},
		Devices:  []TopologyDevice{device},
	}, nil
}

// newEmptyHostTestHandler 构造带拓扑来源与一台空主机条目的测试 Handler。
func newEmptyHostTestHandler(t *testing.T, withCandidate bool) (http.Handler, *http.Cookie) {
	t.Helper()
	store := newStubServerStore()
	store.details["srv-bare"] = &ServerDetailView{
		ID: "srv-bare", Name: "bare", User: "ops", Host: "", Port: 22, Shell: "posix",
		DeviceName: "工作站", HasDevice: true,
	}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: emptyHostTopologySource{withCandidate: withCandidate}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	return handler, cookie
}

// TestServerAddResultEmptyHostShowsPreferredAddress：新增结果页的「连接」行
// 对空主机条目显示首选网络地址；没有候选时退为「用户 · 端口 N」。
func TestServerAddResultEmptyHostShowsPreferredAddress(t *testing.T) {
	h, cookie := newEmptyHostTestHandler(t, true)
	page := serversGet(t, h, cookie, "/ui/servers/new/result?server=srv-bare")
	if !strings.Contains(page, "ops@192.0.2.10:22") {
		t.Fatalf("新增结果页「连接」行应显示首选网络地址：\n%s", page)
	}
	if strings.Contains(page, "ops@:22") {
		t.Fatalf("新增结果页不得显示空主机假地址：\n%s", page)
	}

	h, cookie = newEmptyHostTestHandler(t, false)
	page = serversGet(t, h, cookie, "/ui/servers/new/result?server=srv-bare")
	if !strings.Contains(page, "ops · 端口 22") {
		t.Fatalf("无候选时新增结果页应退为「用户 · 端口 N」：\n%s", page)
	}
	if strings.Contains(page, "ops@:22") {
		t.Fatalf("新增结果页不得显示空主机假地址：\n%s", page)
	}
}

// TestServerCompleteEmptyHostShowsPreferredAddress：补全页的「连接」行对空
// 主机条目显示首选网络地址；没有候选时退为「用户 · 端口 N」。
func TestServerCompleteEmptyHostShowsPreferredAddress(t *testing.T) {
	h, cookie := newEmptyHostTestHandler(t, true)
	page := serversGet(t, h, cookie, "/ui/servers/complete?queue=srv-bare&position=0")
	if !strings.Contains(page, "ops@192.0.2.10:22") {
		t.Fatalf("补全页「连接」行应显示首选网络地址：\n%s", page)
	}

	h, cookie = newEmptyHostTestHandler(t, false)
	page = serversGet(t, h, cookie, "/ui/servers/complete?queue=srv-bare&position=0")
	if !strings.Contains(page, "ops · 端口 22") {
		t.Fatalf("无候选时补全页应退为「用户 · 端口 N」：\n%s", page)
	}
	if strings.Contains(page, "ops@:22") {
		t.Fatalf("补全页不得显示空主机假地址：\n%s", page)
	}
}
