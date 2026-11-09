package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSoftwareInventoryNavFollowsCapability 覆盖软件清单导航入口：只有可写
// 角色配置了软件清单来源时显示；客户端即使误配置来源也不显示。
func TestSoftwareInventoryNavFollowsCapability(t *testing.T) {
	pageBody := func(role Role, enabled bool) string {
		cfg := Config{
			Role:        role,
			Servers:     &stubServerSource{},
			Access:      &stubAccessStore{},
			VersionHint: func() string { return "" },
			Port:        18080,
		}
		if role == RoleClient {
			cfg.Access = nil
		}
		if enabled {
			cfg.SoftwareInventory = stubSoftwareInventorySource{}
		}
		handler, startup, err := New(cfg)
		if err != nil {
			t.Fatalf("构造 Web UI Handler 失败：%v", err)
		}
		resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
		cookies := resp.Cookies()
		if len(cookies) != 1 {
			t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
		}
		page := doGetWithCookie(t, handler, cookies[0], "/ui/")
		body := readResponseBody(t, page)
		return body
	}

	if body := pageBody(RoleStandalone, true); !strings.Contains(body, `href="/ui/software-inventory/"`) ||
		!strings.Contains(body, `aria-label="软件清单区域"`) {
		t.Fatalf("单机配置软件清单来源后应显示导航入口：\n%s", body)
	}
	if body := pageBody(RoleStandalone, false); strings.Contains(body, `href="/ui/software-inventory/"`) {
		t.Fatalf("未配置软件清单来源时不应显示导航入口：\n%s", body)
	}
	if body := pageBody(RoleClient, true); strings.Contains(body, `href="/ui/software-inventory/"`) {
		t.Fatalf("客户端 Web UI 不应显示软件清单导航入口：\n%s", body)
	}
}

type stubSoftwareInventorySource struct {
	device      *SoftwareInventoryDevice
	deviceErr   error
	searchRows  []SoftwareInventorySearchRow
	searchQuery string
}

func (s stubSoftwareInventorySource) SoftwareInventoryDevices() ([]SoftwareInventoryDeviceSummary, error) {
	return nil, nil
}

func (s stubSoftwareInventorySource) SoftwareInventoryDevice(deviceName string) (*SoftwareInventoryDevice, error) {
	if s.deviceErr != nil {
		return nil, s.deviceErr
	}
	if s.device == nil || s.device.Name != deviceName {
		return nil, &SoftwareInventoryDeviceNotFoundError{DeviceName: deviceName}
	}
	return s.device, nil
}

func (s stubSoftwareInventorySource) SearchSoftwareInventory(namePart string) ([]SoftwareInventorySearchRow, error) {
	s.searchQuery = namePart
	return s.searchRows, nil
}

func (s stubSoftwareInventorySource) RequestSoftwareInventoryCollection(
	context.Context, string, SoftwareInventoryWriteSource,
) (SoftwareInventoryCollectionResult, error) {
	return SoftwareInventoryCollectionResult{}, nil
}

// TestSoftwareInventorySearchListsDevices 覆盖跨设备搜索：按软件名返回每台
// 设备上的精确版本、来源与暴露级别。
func TestSoftwareInventorySearchListsDevices(t *testing.T) {
	collectedAt := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	source := stubSoftwareInventorySource{searchRows: []SoftwareInventorySearchRow{
		{DeviceName: "工作站", Item: SoftwareInventoryItem{
			Name: "OpenSSH", Version: "1:8.9p1-3ubuntu0.10", Source: "实测", CollectedAt: &collectedAt,
			ListeningPorts: []SoftwareInventoryPort{{Address: "0.0.0.0", Port: 22, Protocol: "tcp", ExposureLabel: "监听所有地址"}},
		}},
		{DeviceName: "网络存储", Item: SoftwareInventoryItem{
			Name: "OpenSSH", Version: "9.6p1-1", Source: "声明",
			ListeningPorts: []SoftwareInventoryPort{{Address: "192.0.2.20", Port: 22, Protocol: "tcp", ExposureLabel: "局域网"}},
		}},
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/search?query=opens")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("跨设备搜索页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	for _, wanted := range []string{
		"<h1>软件清单搜索</h1>", `name="query"`, "value=\"opens\"",
		"工作站", "网络存储", "OpenSSH", "1:8.9p1-3ubuntu0.10", "9.6p1-1",
		"实测", "声明", "0.0.0.0:22/tcp", "监听所有地址", "192.0.2.20:22/tcp", "局域网",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("跨设备搜索页缺少 %q：\n%s", wanted, body)
		}
	}
}

func newSoftwareInventoryHandler(
	t *testing.T,
	role Role,
	source SoftwareInventorySource,
) (http.Handler, *http.Cookie) {
	t.Helper()
	cfg := Config{
		Role:              role,
		Servers:           &stubServerSource{},
		Access:            &stubAccessStore{},
		SoftwareInventory: source,
		VersionHint:       func() string { return "" },
		Port:              18080,
	}
	if role == RoleClient {
		cfg.Access = nil
	}
	handler, startup, err := New(cfg)
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return handler, cookies[0]
}

// TestSoftwareInventoryDevicePageShowsRequiredColumns 覆盖设备清单页的展示
// 要求：类别分组、精确字段、采集状态、立即采集与变化历史。
func TestSoftwareInventoryDevicePageShowsRequiredColumns(t *testing.T) {
	collectedAt := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	source := stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name: "工作站",
		Status: SoftwareInventoryStatus{
			CollectorLabel: "权威端", ServerEntry: "web-1",
			LastSuccessAt:     &collectedAt,
			LastFailureAt:     &confirmedAt,
			LastFailureReason: "连接超时",
			Stale:             true,
		},
		Groups: []SoftwareInventoryGroup{
			{CategoryLabel: "操作系统", Items: []SoftwareInventoryItem{{
				CategoryLabel: "操作系统", Name: "Ubuntu", Version: "22.04.5 LTS",
				Source: "实测", CollectedAt: &collectedAt,
				ListeningPorts: []SoftwareInventoryPort{{
					Address: "0.0.0.0", Port: 22, Protocol: "tcp", ExposureLabel: "监听所有地址",
				}},
			}}},
			{CategoryLabel: "容器镜像", Items: []SoftwareInventoryItem{{
				CategoryLabel: "容器镜像", Name: "postgres", Version: "16.4",
				Source: "实测", CollectedAt: &collectedAt, BelongsTo: "Docker",
				ContainerTag: "16-alpine", ContainerDigest: "sha256:abcdef", ContainerRunning: true,
			}}},
			{CategoryLabel: "系统包", SystemPackages: true, SystemPackageCount: 2, Items: []SoftwareInventoryItem{
				{CategoryLabel: "系统包", Name: "curl", Version: "8.9.1-1ubuntu2", Source: "实测", CollectedAt: &collectedAt},
				{CategoryLabel: "系统包", Name: "openssh-client", Version: "1:8.9p1-3ubuntu0.10", Source: "实测", CollectedAt: &collectedAt},
			}},
			{CategoryLabel: "服务程序", Items: []SoftwareInventoryItem{{
				CategoryLabel: "服务程序", Name: "自建网关", Version: "1.2.3+build.7",
				Source: "声明", ConfirmedAt: &confirmedAt,
			}}},
		},
		History: []SoftwareInventoryChange{{
			OccurredAt: collectedAt, KindLabel: "版本变化", CategoryLabel: "系统包",
			Name: "curl", OldValue: "8.8.0-1", NewValue: "8.9.1-1ubuntu2",
		}},
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/devices?device="+url.QueryEscape("工作站"))
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("设备清单页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	for _, wanted := range []string{
		"<h1>工作站 · 软件清单</h1>",
		"Ubuntu", "22.04.5 LTS", "实测", "2026-11-01 10:30:00",
		"0.0.0.0:22/tcp", "监听所有地址",
		"postgres", "16.4", "Docker", "16-alpine", "sha256:abcdef", "运行中",
		"权威端", "web-1", "连接超时", "清单已过期",
		"自建网关", "1.2.3", "build.7", "声明", "2026-11-02 11:00:00",
		"系统包（2 条，默认折叠）", "curl", "openssh-client",
		"版本变化", "8.8.0-1", "8.9.1-1ubuntu2",
		"立即采集",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("设备清单页缺少 %q：\n%s", wanted, body)
		}
	}
}
