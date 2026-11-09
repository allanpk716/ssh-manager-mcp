package clientops

// 票 #185：软件清单在线查询的客户端转发器。构造与传输安全沿用元数据
// 编辑转发器（固定证书校验、不跟随重定向、设备码认证）；权威端不可达
// 时给出固定错误文本，供命令行映射专门退出码。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/softwareinventory"
)

// newTestSoftwareInventoryForwarder 按生产形态构造一个指向测试服务器的
// 转发器：固定传输层安全 + 设备码。
func newTestSoftwareInventoryForwarder(t *testing.T, srv *httptest.Server) *SoftwareInventoryForwarder {
	t.Helper()
	pin := mcpserver.SPKIFingerprint(srv.Certificate())
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: srv.URL, Token: "devcode-1:" + pin, Pin: pin,
	})
	if err != nil {
		t.Fatalf("NewSoftwareInventoryForwarder: %v", err)
	}
	return forwarder
}

// TestSoftwareInventoryForwarder_HappyPath 钉住请求形状与结果解析。
func TestSoftwareInventoryForwarder_HappyPath(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"devices":[{"device_id":"d1","device_name":"gpu-设备",`+
			`"collection":{"never_collected":true,"stale":true},`+
			`"items":[{"device_id":"d1","category":"service_program","category_label":"服务程序",`+
			`"name":"nginx","version":"1.25.3","source":"声明","collected_at":"2026-11-01T08:00:00Z",`+
			`"listening_ports":[{"address":"0.0.0.0","port":80,"protocol":"tcp","exposure":"all_addresses"}]}]}]}`)
	}))
	defer srv.Close()

	forwarder := newTestSoftwareInventoryForwarder(t, srv)
	defer forwarder.Close()
	result, err := forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Device: "gpu-设备", Name: "nginx", MinimumExposure: "all_addresses",
		Category: "service_program", IncludeSystemPackages: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/software-inventory" ||
		gotAuth != "Bearer devcode-1" {
		t.Fatalf("请求形状不对：method=%s path=%s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if gotBody["device"] != "gpu-设备" || gotBody["minimum_exposure"] != "all_addresses" ||
		gotBody["include_system_packages"] != true {
		t.Fatalf("请求体 = %+v", gotBody)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "gpu-设备" ||
		len(result.Devices[0].Items) != 1 || result.Devices[0].Items[0].Name != "nginx" ||
		result.Devices[0].Items[0].CollectedAt == nil ||
		result.Devices[0].Items[0].ListeningPorts[0].Exposure != softwareinventory.ExposureAllAddresses {
		t.Fatalf("结果解析 = %+v", result)
	}
}

// TestSoftwareInventoryForwarder_UnreachableFixedText 权威端不可达时返回
// 固定错误文本，并且能被专门判定函数识别。
func TestSoftwareInventoryForwarder_UnreachableFixedText(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // 立即关闭：后续请求必然不可达
	forwarder := newTestSoftwareInventoryForwarder(t, srv)
	defer forwarder.Close()
	_, err := forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{})
	if err == nil {
		t.Fatal("权威端不可达时应报错")
	}
	if err.Error() != SoftwareInventoryUnreachableText {
		t.Fatalf("错误文本不是固定契约：\n得到：%q\n想要：%q", err.Error(), SoftwareInventoryUnreachableText)
	}
	if !IsSoftwareInventoryUnreachable(err) {
		t.Fatal("IsSoftwareInventoryUnreachable 应识别传输失败")
	}
	var wrapper *softwareInventoryError
	if !errors.As(err, &wrapper) {
		t.Fatal("错误应可按类型识别")
	}
}

// TestSoftwareInventoryForwarder_NoCredentialFailsClosed 缺凭据时与不可达
// 同样固定报错，不做任何本地回退。
func TestSoftwareInventoryForwarder_NoCredentialFailsClosed(t *testing.T) {
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{})
	if err == nil || err.Error() != SoftwareInventoryUnreachableText ||
		!IsSoftwareInventoryUnreachable(err) {
		t.Fatalf("缺凭据应固定报错：%v", err)
	}
}

// TestSoftwareInventoryForwarder_PlaintextRefused 明文权威端拒绝查询。
func TestSoftwareInventoryForwarder_PlaintextRefused(t *testing.T) {
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: "http://plain.example", Token: "code",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{})
	if err == nil || err.Error() != softwareInventoryPlaintextText {
		t.Fatalf("明文应拒绝：%v", err)
	}
	if IsSoftwareInventoryUnreachable(err) {
		t.Fatal("明文拒绝不是权威端不可达")
	}
	// 固定了证书却配了非 https 地址：构造期就拒绝。
	if _, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: "http://plain.example", Token: "code", Pin: "sha256/" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}); err == nil {
		t.Fatal("固定证书配明文地址必须在构造期报错")
	}
}

// TestSoftwareInventoryForwarder_StatusBranches 钉住非 200 状态码的固定
// 文本。
func TestSoftwareInventoryForwarder_StatusBranches(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		want        string
		unreachable bool
	}{
		{
			name:        "401 设备码被拒",
			status:      http.StatusUnauthorized,
			want:        softwareInventoryRejectedText,
			unreachable: false,
		},
		{
			name:        "403 范围外设备",
			status:      http.StatusForbidden,
			want:        softwareInventoryForbiddenText,
			unreachable: false,
		},
		{
			name:        "404 旧权威端",
			status:      http.StatusNotFound,
			want:        softwareInventoryUnsupportedText,
			unreachable: false,
		},
		{
			name:        "500 服务端故障按不可达处理",
			status:      http.StatusInternalServerError,
			want:        SoftwareInventoryUnreachableText,
			unreachable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			forwarder := newTestSoftwareInventoryForwarder(t, srv)
			defer forwarder.Close()
			_, err := forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("错误文本 = %v，想要 %q", err, tc.want)
			}
			if got := IsSoftwareInventoryUnreachable(err); got != tc.unreachable {
				t.Fatalf("不可达判定 = %v，想要 %v", got, tc.unreachable)
			}
		})
	}
}
