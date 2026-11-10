package clientops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
)

// TestSoftwareInventoryCollectorForwarderContract 覆盖取分派与上交结果的请
// 求方法、路径、设备码头和 JSON 契约。
func TestSoftwareInventoryCollectorForwarderContract(t *testing.T) {
	lastSuccess := time.Unix(1770000000, 0)
	var submitted softwareinventory.CollectionSubmission
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer devcode" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/software-inventory/assignments":
			if r.Method != http.MethodPost {
				t.Errorf("assignments method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []map[string]any{{
					"device_id": "device-1", "device_name": "工作站", "server_id": "server-1",
					"server_name": "入口", "sudo": true, "pending_immediate": true,
					"last_success_at": lastSuccess.Unix(), "due": true,
				}},
			})
		case "/software-inventory/results":
			if r.Method != http.MethodPost {
				t.Errorf("results method = %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("decode submission: %v", err)
			}
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{URL: srv.URL, Token: "devcode", Pin: mcpserver.SPKIFingerprint(srv.Certificate())})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()

	assignments, err := forwarder.SoftwareCollectionAssignments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].DeviceName != "工作站" ||
		assignments[0].ServerName != "入口" || !assignments[0].Sudo ||
		!assignments[0].PendingImmediate || !assignments[0].Due ||
		!assignments[0].LastSuccessAt.Equal(lastSuccess) {
		t.Fatalf("采集分派 = %+v", assignments)
	}

	submission := softwareinventory.CollectionSubmission{
		DeviceID: "device-1", DeviceName: "工作站", ServerID: "server-1",
		Sudo: true, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "nginx", Version: "1.25",
		}},
	}
	if err := forwarder.SubmitSoftwareCollectionResult(context.Background(), submission); err != nil {
		t.Fatal(err)
	}
	if submitted.DeviceID != submission.DeviceID || len(submitted.Items) != 1 ||
		submitted.Items[0].Name != "nginx" || submitted.Sudo != true {
		t.Fatalf("上交结果 = %+v", submitted)
	}
}

// TestSoftwareInventoryCollectorForwarderUnreachable 权威端不可达时取分派与
// 上交结果都直接失败，错误可映射专门退出码。
func TestSoftwareInventoryCollectorForwarderUnreachable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("unreachable test should not reach handler")
	}))
	url := srv.URL
	srv.Close()
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{URL: url, Token: "devcode", Pin: testUnavailablePin})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	if _, err := forwarder.SoftwareCollectionAssignments(context.Background()); !IsSoftwareInventoryUnreachable(err) ||
		!strings.Contains(err.Error(), "没有") {
		t.Fatalf("取分派错误应属于权威端不可达：%v", err)
	}
	submission := softwareinventory.CollectionSubmission{DeviceName: "工作站", Items: []models.SoftwareObservedItem{}}
	if err := forwarder.SubmitSoftwareCollectionResult(context.Background(), submission); !IsSoftwareInventoryUnreachable(err) ||
		!strings.Contains(err.Error(), "没有排队") {
		t.Fatalf("上交错误应属于权威端不可达：%v", err)
	}
}

// TestSoftwareInventoryForwarderCollectSoftwareInventory 钉住客户端立即采集
// 工具的转发请求与响应结构；范围外设备保留权威端拒绝，不可达走固定分支。
func TestSoftwareInventoryForwarderCollectSoftwareInventory(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"device_name":"工作站","requested":false,`+
			`"report":{"device_name":"工作站","server_entry_name":"入口","sudo":false,`+
			`"command_set_version":"posix-v2","system_type":"ubuntu","incomplete":false,`+
			`"notes":[],"item_counts":{}}}`)
	}))
	defer srv.Close()
	forwarder := newTestSoftwareInventoryForwarder(t, srv)
	defer forwarder.Close()

	output, err := forwarder.CollectSoftwareInventory(context.Background(), mcpserver.CollectSoftwareInventoryInput{
		Device: "工作站",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/software-inventory/collect" ||
		gotAuth != "Bearer devcode-1" || gotBody["device"] != "工作站" {
		t.Fatalf("请求形状不对：method=%s path=%s auth=%q body=%+v",
			gotMethod, gotPath, gotAuth, gotBody)
	}
	if output.DeviceName != "工作站" || output.Requested || output.Report == nil ||
		output.Report.CommandSetVersion != softwareinventory.POSIXCommandSetVersion {
		t.Fatalf("立即采集输出 = %+v report=%+v", output, *output.Report)
	}
}

// TestSoftwareInventoryForwarderCollectRejected 保留范围外设备与旧权威端的
// 固定错误文本。
func TestSoftwareInventoryForwarderCollectRejected(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/software-inventory/collect" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "device is not visible to this device code's profile", http.StatusForbidden)
	}))
	defer srv.Close()
	forwarder := newTestSoftwareInventoryForwarder(t, srv)
	defer forwarder.Close()
	_, err := forwarder.CollectSoftwareInventory(context.Background(), mcpserver.CollectSoftwareInventoryInput{
		Device: "范围外设备",
	})
	if err == nil || !strings.Contains(err.Error(), "不在本设备码绑定 profile 的可见范围内") {
		t.Fatalf("范围外设备错误 = %v", err)
	}
}

var testUnavailablePin = "sha256:" + strings.Repeat("a", 64)
