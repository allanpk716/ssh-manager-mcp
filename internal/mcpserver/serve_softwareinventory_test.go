package mcpserver

// 票 #185：权威端「查询软件清单」只读接口（POST /software-inventory，
// 设备码认证）。测试写法沿用 server-metadata 处理器：进程内
// ServeRunner + httptest，覆盖设备码无效被拒与 profile 可见范围。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// softwareInventoryFixture 造两台受管设备与一台不受管设备：laptop 的
// profile 只授权 gpu 所在设备，phone 的 profile 授权另一台；不受管设备
// 的清单任何 profile 都看不到。
type softwareInventoryFixture struct {
	srv         *httptest.Server
	st          *store.Store
	laptopToken string
	phoneToken  string
}

func newSoftwareInventoryRunner(t *testing.T) *softwareInventoryFixture {
	t.Helper()
	st := newStoreAt(t, filepath.Join(t.TempDir(), "software-inventory.db"))
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, deviceName := range []string{"gpu-设备", "mirror-设备", "不受管设备"} {
		if _, err := st.AddDevice(deviceName, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"gpu", "mirror"} {
		if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: name, Host: "192.0.2.10", Port: 22, User: "u",
			AuthMethod: models.AuthPassword, CredentialID: credentialID,
		}, nil, nil, name+"-设备"); err != nil {
			t.Fatal(err)
		}
	}
	gpu, err := st.GetServerByName("gpu")
	if err != nil || gpu == nil {
		t.Fatalf("取 gpu 服务器条目失败：%v %+v", err, gpu)
	}
	mirror, err := st.GetServerByName("mirror")
	if err != nil || mirror == nil {
		t.Fatalf("取 mirror 服务器条目失败：%v %+v", err, mirror)
	}
	laptopProfile, err := st.AddProfile("laptop-profile")
	if err != nil {
		t.Fatal(err)
	}
	phoneProfile, err := st.AddProfile("phone-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(laptopProfile, []string{gpu.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(phoneProfile, []string{mirror.ID}); err != nil {
		t.Fatal(err)
	}
	manager := softwareinventory.New(st)
	declarations := []softwareinventory.Declaration{
		{
			Device: "gpu-设备", Category: softwareinventory.CategoryServiceProgram,
			Name: "nginx", Version: "1.25.3",
			ListeningPorts: []softwareinventory.ListeningPort{{
				Address: "0.0.0.0", Port: 80, Protocol: "tcp",
				Exposure: softwareinventory.ExposureAllAddresses,
			}},
		},
		{
			Device: "gpu-设备", Category: softwareinventory.CategorySystemPackage,
			Name: "libc6", Version: "2.39-0ubuntu8",
		},
		{
			Device: "mirror-设备", Category: softwareinventory.CategoryServiceProgram,
			Name: "PostgreSQL", Version: "16.2",
		},
		{
			Device: "不受管设备", Category: softwareinventory.CategoryOperatingSystem,
			Name: "NAS 系统", Version: "7.2.1",
		},
	}
	for _, declaration := range declarations {
		if _, err := manager.AddDeclaration(declaration, softwareinventory.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, laptopToken, err := st.AddCacheToken("laptop", laptopProfile)
	if err != nil {
		t.Fatal(err)
	}
	_, phoneToken, err := st.AddCacheToken("phone", phoneProfile)
	if err != nil {
		t.Fatal(err)
	}
	return &softwareInventoryFixture{srv: srv, st: st, laptopToken: laptopToken, phoneToken: phoneToken}
}

// postSoftwareInventory 发起一次 POST /software-inventory，返回状态码与
// 响应体。token 为空表示不带设备码。
func postSoftwareInventory(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/software-inventory", strings.NewReader(body))
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

// TestSoftwareInventoryEndpoint_RejectsInvalidDeviceCode 设备码缺失或无效
// 都必须被拒绝，不能落到任何查询逻辑。
func TestSoftwareInventoryEndpoint_RejectsInvalidDeviceCode(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	cases := []struct {
		name  string
		token string
	}{
		{"缺失设备码", ""},
		{"无效设备码", "not-a-real-device-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postSoftwareInventory(t, fx.srv, tc.token, `{"name":"nginx"}`)
			if status != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，应为 401；响应体：%s", status, body)
			}
		})
	}
}

// TestSoftwareInventoryEndpoint_OnlyProfileVisibleDevices 有效设备码也只能
// 查到自己 profile 可见的受管设备；授权外受管设备与不受管设备都不返回。
func TestSoftwareInventoryEndpoint_OnlyProfileVisibleDevices(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	status, body := postSoftwareInventory(t, fx.srv, fx.laptopToken, `{}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，应为 200；响应体：%s", status, body)
	}
	var result softwareinventory.QueryResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("解析响应体失败：%v\n%s", err, body)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "gpu-设备" {
		t.Fatalf("查询结果 = %+v，应只有 gpu-设备", result.Devices)
	}
	if strings.Contains(body, "PostgreSQL") || strings.Contains(body, "NAS 系统") {
		t.Fatalf("授权外设备或不受管设备的清单泄漏了：%s", body)
	}
	if !result.Devices[0].Collection.NeverCollected || !result.Devices[0].Collection.Stale {
		t.Fatalf("采集状态应表示从未采集：%+v", result.Devices[0].Collection)
	}

	// 另一个 profile 用同一接口查自己的范围。
	status, body = postSoftwareInventory(t, fx.srv, fx.phoneToken, `{"name":"postgres"}`)
	if status != http.StatusOK {
		t.Fatalf("phone 状态码 = %d：%s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "mirror-设备" {
		t.Fatalf("phone 查询结果 = %+v", result.Devices)
	}
}

// TestSoftwareInventoryEndpoint_IncludesObservedOnlyDevices 权威端只读接
// 口与本地统一查询共用同一份实测加声明合成结果。
func TestSoftwareInventoryEndpoint_IncludesObservedOnlyDevices(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	observed := models.SoftwareObservedItem{
		Category: softwareinventory.CategoryServiceProgram,
		Name:     "observed-only", Version: "2.0",
	}
	if err := fx.st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Items:             []models.SoftwareObservedItem{observed},
		CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	status, body := postSoftwareInventory(t, fx.srv, fx.laptopToken, `{"name":"observed-only"}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d；响应体：%s", status, body)
	}
	var result softwareinventory.QueryResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("解析响应体失败：%v\n%s", err, body)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 1 ||
		result.Devices[0].Items[0].Source != softwareinventory.SourceObserved {
		t.Fatalf("只有实测、没有声明的条目应能从权威端接口查到：%+v", result.Devices)
	}
}

// TestSoftwareInventoryEndpoint_FiltersAndErrors 过滤条件原样生效；范围外
// 设备与非法参数各有明确的状态码。
func TestSoftwareInventoryEndpoint_FiltersAndErrors(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)

	// 系统包默认不含，显式包含后返回。
	_, body := postSoftwareInventory(t, fx.srv, fx.laptopToken, `{"device":"gpu-设备"}`)
	var result softwareinventory.QueryResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "libc6") || len(result.Devices) != 1 {
		t.Fatalf("系统包默认不应返回：%s", body)
	}
	_, body = postSoftwareInventory(t, fx.srv, fx.laptopToken,
		`{"device":"gpu-设备","include_system_packages":true}`)
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "libc6") {
		t.Fatalf("显式包含系统包失败：%s", body)
	}

	// 最低暴露级别与类别。
	_, body = postSoftwareInventory(t, fx.srv, fx.laptopToken,
		`{"minimum_exposure":"public_network"}`)
	if strings.Contains(body, "nginx") {
		t.Fatalf("监听所有地址不应满足公网最低级别：%s", body)
	}
	_, body = postSoftwareInventory(t, fx.srv, fx.laptopToken,
		`{"category":"operating_system"}`)
	if strings.Contains(body, "nginx") {
		t.Fatalf("类别过滤失败：%s", body)
	}

	// 范围外设备：403；非法参数：400；方法错误：405。
	if status, body := postSoftwareInventory(t, fx.srv, fx.laptopToken,
		`{"device":"不受管设备"}`); status != http.StatusForbidden {
		t.Fatalf("范围外设备状态码 = %d：%s", status, body)
	}
	if status, body := postSoftwareInventory(t, fx.srv, fx.laptopToken,
		`{"category":"not-a-category"}`); status != http.StatusBadRequest {
		t.Fatalf("非法类别状态码 = %d：%s", status, body)
	}
	req, err := http.NewRequest(http.MethodGet, fx.srv.URL+"/software-inventory", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.laptopToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d，应为 405", res.StatusCode)
	}
}
