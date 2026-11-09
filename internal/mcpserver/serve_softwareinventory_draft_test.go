package mcpserver

// 票 #188：权威端「提交声明草稿」受审计转发写接口（POST
// /software-declaration-drafts，设备码认证）。核对设备在 profile 可见范
// 围内；只写草稿，不改生效声明；审计记录设备码编号。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// postSoftwareDeclarationDrafts 发起一次 POST /software-declaration-drafts。
func postSoftwareDeclarationDrafts(
	t *testing.T, srv *httptest.Server, token, body string,
) (int, string) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost, srv.URL+"/software-declaration-drafts", strings.NewReader(body),
	)
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

// TestSoftwareDeclarationDraftsEndpoint_ShapeGuards 方法、请求体上限与请求
// 体形状不合法时直接拒绝，不触碰 vault。
func TestSoftwareDeclarationDraftsEndpoint_ShapeGuards(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	req, err := http.NewRequest(
		http.MethodGet, fx.srv.URL+"/software-declaration-drafts", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.laptopToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d，应为 405", res.StatusCode)
	}

	if status, body := postSoftwareDeclarationDrafts(
		t, fx.srv, fx.laptopToken, "{",
	); status != http.StatusBadRequest || !strings.Contains(body, "unparseable request") {
		t.Fatalf("坏 JSON 应回 400：%d %s", status, body)
	}
	if status, _ := postSoftwareDeclarationDrafts(
		t, fx.srv, fx.laptopToken, `{"declarations":[]}`,
	); status != http.StatusBadRequest {
		t.Fatal("空草稿列表应回 400")
	}
	big := `{"declarations":[{"note":"` + strings.Repeat("x", 300*1024) + `"}]}`
	if status, _ := postSoftwareDeclarationDrafts(
		t, fx.srv, fx.laptopToken, big,
	); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求体应回 413，实际 %d", status)
	}
	if status, _ := postSoftwareDeclarationDrafts(
		t, fx.srv, "", `{"declarations":[{"device":"gpu-设备","category":"service_program","name":"x","version":"1"}]}`,
	); status != http.StatusUnauthorized {
		t.Fatalf("缺设备码应回 401，实际 %d", status)
	}
}

// TestSoftwareDeclarationDraftsEndpoint_DeviceCodeAndVisibility 草稿涉及的
// 每台设备都必须在设备码绑定 profile 的可见范围内。
func TestSoftwareDeclarationDraftsEndpoint_DeviceCodeAndVisibility(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	status, body := postSoftwareDeclarationDrafts(
		t, fx.srv, fx.laptopToken, `{"declarations":[
		{"device":"gpu-设备","category":"service_program","name":"nginx","version":"1.27.0"},
		{"device":"mirror-设备","category":"service_program","name":"redis","version":"7.2"}
	]}`)
	if status != http.StatusForbidden || strings.Contains(body, "mirror") {
		t.Fatalf("越界设备应回 403 且不泄露设备名：%d %s", status, body)
	}
	drafts, err := fx.st.SoftwareDeclarationDrafts(store.AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("整批被拒后不应留下草稿：%+v", drafts)
	}
}

// TestSoftwareDeclarationDraftsEndpoint_WritesDraftsOnly 成功提交只写草稿、
// 返回草稿编号，并在审计里记录设备码编号。
func TestSoftwareDeclarationDraftsEndpoint_WritesDraftsOnly(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	status, body := postSoftwareDeclarationDrafts(
		t, fx.srv, fx.laptopToken, `{"declarations":[
		{
			"device":"gpu-设备","category":"service_program","name":"业务网关","version":"2.1",
			"listening_ports":[{"address":"0.0.0.0","port":8443,"protocol":"tcp","exposure":"virtual_local_area_network"}]
		},
		{"device":"gpu-设备","category":"operating_system","name":"Ubuntu","version":"24.04"}
	]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，应为 200；响应体：%s", status, body)
	}
	var result struct {
		DraftIDs []string `json:"draft_ids"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("解析响应体失败：%v\n%s", err, body)
	}
	if len(result.DraftIDs) != 2 || result.DraftIDs[0] == "" {
		t.Fatalf("应返回两个草稿编号：%s", body)
	}

	drafts, err := fx.st.SoftwareDeclarationDrafts(store.AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 {
		t.Fatalf("权威端应落两条草稿：%+v", drafts)
	}
	query, err := softwareinventory.New(fx.st).Query(softwareinventory.QueryOptions{
		Name: "业务网关", Visibility: store.AllSoftwareDevices(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(query.Devices) != 0 {
		t.Fatalf("草稿不应影响生效查询：%+v", query.Devices)
	}
	rows, err := fx.st.QueryAudit(store.AuditFilter{Actions: []string{"software-declaration-draft"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("每条草稿都应写审计：%+v", rows)
	}
	for _, row := range rows {
		var summary map[string]any
		if err := json.Unmarshal([]byte(row.Command), &summary); err != nil {
			t.Fatal(err)
		}
		if summary["device_code"] != "laptop" || row.Source != store.AuditSourceForward {
			t.Fatalf("草稿提交审计应记录设备码编号：%v %s", summary, row.Source)
		}
	}
}
