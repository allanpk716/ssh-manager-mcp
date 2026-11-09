package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestSoftwareInventoryAssignmentsEndpoint 覆盖只读采集分派：只返回本设备
// 码登记的设备，带服务器条目、sudo、立即采集请求与到期判断。
func TestSoftwareInventoryAssignmentsEndpoint(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	gpu, err := fx.st.GetServerByName("gpu")
	if err != nil || gpu == nil {
		t.Fatalf("取 gpu 条目失败：%v %+v", err, gpu)
	}
	laptopProfile, err := fx.st.AddProfile("collector-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.GrantServers(laptopProfile, []string{gpu.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fx.st.AddCacheToken("collector", laptopProfile); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SetSoftwareCollectionCollector(
		"gpu-设备", "laptop", gpu.ID, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	status, body := postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，应为 200：%s", status, body)
	}
	var response struct {
		Devices []struct {
			DeviceID         string `json:"device_id"`
			DeviceName       string `json:"device_name"`
			ServerID         string `json:"server_id"`
			ServerName       string `json:"server_name"`
			Sudo             bool   `json:"sudo"`
			PendingImmediate bool   `json:"pending_immediate"`
			LastSuccessAt    int64  `json:"last_success_at"`
			Due              bool   `json:"due"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Devices) != 1 {
		t.Fatalf("分派设备数量 = %d：%s", len(response.Devices), body)
	}
	assignment := response.Devices[0]
	if assignment.DeviceName != "gpu-设备" || assignment.ServerID != gpu.ID ||
		assignment.ServerName != "gpu" || !assignment.Due {
		t.Fatalf("采集分派 = %+v", assignment)
	}

	// 写入一分钟前的成功结果后未到期；立即采集请求仍让它出现在分派里。
	fresh := time.Now()
	fx.st.NowFn = func() time.Time { return fresh }
	if err := fx.st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Collector: "collector", ServerID: gpu.ID,
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.RequestSoftwareImmediateCollection("gpu-设备", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	status, body = postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/assignments", `{}`)
	if status != http.StatusOK {
		t.Fatalf("再次取分派状态码 = %d：%s", status, body)
	}
	if !strings.Contains(body, `"pending_immediate":true`) || strings.Contains(body, `"due":false`) {
		t.Fatalf("立即采集请求应让设备到期：%s", body)
	}

	// 其他设备码不是登记的采集执行者，取不到这台设备。
	status, body = postCollectorEndpoint(t, fx.srv, fx.phoneToken, "/software-inventory/assignments", `{}`)
	if status != http.StatusOK || strings.Contains(body, "gpu-设备") {
		t.Fatalf("其他设备码不应看到 collector 的分派：%d %s", status, body)
	}
}

func postCollectorEndpoint(t *testing.T, srv *httptest.Server, token, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	blob, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(blob)
}

// collectorSubmissionFixture 准备一台由 laptop 设备码采集的设备，返回采集
// 用服务器条目。
func collectorSubmissionFixture(t *testing.T) (*softwareInventoryFixture, *models.Server) {
	t.Helper()
	fx := newSoftwareInventoryRunner(t)
	gpu, err := fx.st.GetServerByName("gpu")
	if err != nil || gpu == nil {
		t.Fatalf("取 gpu 条目失败：%v %+v", err, gpu)
	}
	if err := fx.st.SetSoftwareCollectionCollector(
		"gpu-设备", "laptop", gpu.ID, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	return fx, gpu
}

func submissionBody(t *testing.T, fx *softwareInventoryFixture, gpu *models.Server, version string) string {
	t.Helper()
	device, err := fx.st.GetDevice("gpu-设备")
	if err != nil {
		t.Fatal(err)
	}
	item := models.SoftwareObservedItem{
		Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
		Version: version, ListeningPorts: []models.SoftwareListeningPort{{
			Address: "0.0.0.0", Port: 22, Protocol: "tcp",
			Exposure: models.SoftwareExposureAllAddresses,
		}},
	}
	blob, err := json.Marshal(map[string]any{
		"device_id": device.ID, "device_name": device.Name, "server_id": gpu.ID,
		"sudo": true, "command_set_version": "posix-v2", "system_type": "ubuntu",
		"items": []models.SoftwareObservedItem{item}, "started_at": time.Now(), "ended_at": time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

// TestSoftwareInventorySubmissionEndpointChecks 验证上交接口三条核对：设备
// 码有效、实例是登记的采集执行者、服务器条目仍在 profile 里。
func TestSoftwareInventorySubmissionEndpointChecks(t *testing.T) {
	fx, gpu := collectorSubmissionFixture(t)
	body := submissionBody(t, fx, gpu, "1:9.6p1-3ubuntu13.20")
	if status, response := postCollectorEndpoint(t, fx.srv, "", "/software-inventory/results", body); status != http.StatusUnauthorized {
		t.Fatalf("无效设备码状态码 = %d，应为 401：%s", status, response)
	}
	if status, response := postCollectorEndpoint(t, fx.srv, fx.phoneToken, "/software-inventory/results", body); status != http.StatusForbidden ||
		!strings.Contains(response, "不是这台设备登记的采集执行者") {
		t.Fatalf("其他设备码应被 403 拒绝并说明原因：%d %s", status, response)
	}

	tokens, err := fx.st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	var laptopProfile string
	for _, token := range tokens {
		if token.Name == "laptop" {
			laptopProfile = token.ProfileID
		}
	}
	if _, err := fx.st.RevokeServers(laptopProfile, []string{gpu.ID}); err != nil {
		t.Fatal(err)
	}
	status, response := postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/results", body)
	if status != http.StatusForbidden || !strings.Contains(response, "服务器条目已不在") {
		t.Fatalf("profile 移除后应被 403 拒绝并说明原因：%d %s", status, response)
	}
	if err := fx.st.GrantServers(laptopProfile, []string{gpu.ID}); err != nil {
		t.Fatal(err)
	}
	status, response = postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/results", body)
	if status != http.StatusOK {
		t.Fatalf("基线提交状态码 = %d：%s", status, response)
	}
}

// TestSoftwareInventorySubmissionEndpointLandsResult 验证成功上交复用实测
// 落库函数：替换条目、写变化历史、清理一年前历史、写审计并保留采集执行者。
func TestSoftwareInventorySubmissionEndpointLandsResult(t *testing.T) {
	fx, gpu := collectorSubmissionFixture(t)
	oldAt := time.Now().Add(-366 * 24 * time.Hour)
	now := time.Now()
	fx.st.NowFn = func() time.Time { return oldAt }
	if err := fx.st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Collector: "laptop", ServerID: gpu.ID,
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "python3", Version: "3.11",
		}},
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.RequestSoftwareImmediateCollection("gpu-设备", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	fx.st.NowFn = func() time.Time { return now }

	status, response := postCollectorEndpoint(
		t, fx.srv, fx.laptopToken, "/software-inventory/results", submissionBody(t, fx, gpu, "1:9.9p1"),
	)
	if status != http.StatusOK {
		t.Fatalf("成功上交状态码 = %d：%s", status, response)
	}
	items, err := fx.st.SoftwareObservedItemsForDevice("gpu-设备")
	if err != nil || len(items) != 1 || items[0].Name != "openssh-server" || items[0].Version != "1:9.9p1" {
		t.Fatalf("上交后的实测条目 = %+v err=%v", items, err)
	}
	history, err := fx.st.SoftwareChangeHistoryForDevice("gpu-设备")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("成功上交必须写变化历史")
	}
	for _, row := range history {
		if row.OccurredAt.Before(now.Add(-365 * 24 * time.Hour)) {
			t.Fatalf("一年前的历史没有清理：%+v", row)
		}
	}
	audit, err := fx.st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect"}})
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(audit[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["collector"] != "laptop" || summary["server"] != "gpu" || summary["sudo"] != true {
		t.Fatalf("上交审计摘要 = %v", summary)
	}
	collected, err := fx.st.SoftwareCollectionStatusForDevice("gpu-设备")
	if err != nil || collected.Collector != "laptop" || collected.PendingImmediate || collected.LastSuccessAt.IsZero() {
		t.Fatalf("上交后的采集状态 = %+v err=%v", collected, err)
	}
}

// TestSoftwareInventorySubmissionEndpointBodyTooLarge 结果体超过上限必须回
// 413，且不能落任何实测条目。
func TestSoftwareInventorySubmissionEndpointBodyTooLarge(t *testing.T) {
	fx, gpu := collectorSubmissionFixture(t)
	blob, err := json.Marshal(map[string]any{
		"device_id": gpu.DeviceID, "device_name": "gpu-设备", "server_id": gpu.ID,
		"padding": strings.Repeat("x", 17<<20),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, response := postCollectorEndpoint(t, fx.srv, fx.laptopToken, "/software-inventory/results", string(blob))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限状态码 = %d，应为 413：%s", status, response)
	}
	if items, err := fx.st.SoftwareObservedItemsForDevice("gpu-设备"); err != nil || len(items) != 0 {
		t.Fatalf("超限请求不能落实测条目：%d %v", len(items), err)
	}
}
