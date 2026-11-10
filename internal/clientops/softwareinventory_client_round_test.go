package clientops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

func newClientCollectorFixture(t *testing.T, resultStatus int) (*SoftwareInventoryClientCollector, *httptest.Server, *atomic.Int64) {
	t.Helper()
	var submissions atomic.Int64
	serverID := "server-1"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/software-inventory/assignments" {
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"device_id": "device-1", "device_name": "工作站", "server_id": serverID,
				"server_name": "入口", "pending_immediate": true, "due": true,
			}}})
			return
		}
		if r.URL.Path == "/software-inventory/results" {
			submissions.Add(1)
			w.WriteHeader(resultStatus)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: srv.URL, Token: "devcode", Pin: mcpserver.SPKIFingerprint(srv.Certificate()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(forwarder.Close)

	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(filepath.Join(t.TempDir(), "client.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if _, err := opened.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	added, err := opened.AddServerWithCredentialsToDevice(&models.Server{
		Name: "入口", Host: "192.0.2.10", Port: 22, User: "u",
		ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	serverID = added.ServerID
	fixture, err := os.ReadFile(filepath.Join("..", "softwareinventory", "testdata", "posix", "ubuntu2404-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return &SoftwareInventoryClientCollector{
		Forwarder: forwarder,
		OpenStore: func() (*store.Store, func(), error) { return opened, func() {}, nil },
		NewService: func(st *store.Store) *softwareinventory.Service {
			return &softwareinventory.Service{Store: st, RemoteExec: func(
				context.Context, *models.Server, models.ShellType, string, bool,
			) (softwareinventory.RemoteExecResult, error) {
				return softwareinventory.RemoteExecResult{Stdout: string(fixture)}, nil
			}}
		},
	}, srv, &submissions
}

// TestSoftwareInventoryClientCollectorRound 覆盖客户端一轮：在线取分派、逐台
// 采集、逐台上交；客户端本地不保存软件清单。
func TestSoftwareInventoryClientCollectorRound(t *testing.T) {
	collector, _, submissions := newClientCollectorFixture(t, http.StatusOK)
	report, err := collector.CollectAssigned(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 1 || report.Succeeded != 1 || report.Failed != 0 || len(report.Devices) != 1 ||
		report.Devices[0].Report == nil || report.Devices[0].Report.ServerEntryName != "入口" {
		t.Fatalf("客户端采集一轮报告 = %+v", report)
	}
	if submissions.Load() != 1 {
		t.Fatalf("上交次数 = %d，应为 1", submissions.Load())
	}
	st, closeStore, err := collector.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil || len(items) != 0 {
		t.Fatalf("客户端本地不应保存软件清单：%d %v", len(items), err)
	}
}

// TestSoftwareInventoryClientCollectorSubmitFailure 上交不可达或失败时整轮直
// 接失败，仍不把结果保存在客户端本地。
func TestSoftwareInventoryClientCollectorSubmitFailure(t *testing.T) {
	collector, _, submissions := newClientCollectorFixture(t, http.StatusInternalServerError)
	_, err := collector.CollectAssigned(context.Background())
	if err == nil || !IsSoftwareInventoryUnreachable(err) || !strings.Contains(err.Error(), "没有排队") {
		t.Fatalf("上交失败应直接返回权威端不可达错误：%v", err)
	}
	if submissions.Load() != 1 {
		t.Fatalf("失败前应已尝试上交一次：%d", submissions.Load())
	}
	st, closeStore, err := collector.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil || len(items) != 0 {
		t.Fatalf("失败后客户端本地仍不应保存软件清单：%d %v", len(items), err)
	}
}
