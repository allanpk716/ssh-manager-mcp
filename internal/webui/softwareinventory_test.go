package webui

import (
	"context"
	"strings"
	"testing"
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

type stubSoftwareInventorySource struct{}

func (stubSoftwareInventorySource) SoftwareInventoryDevices() ([]SoftwareInventoryDeviceSummary, error) {
	return nil, nil
}

func (stubSoftwareInventorySource) SoftwareInventoryDevice(string) (*SoftwareInventoryDevice, error) {
	return nil, nil
}

func (stubSoftwareInventorySource) SearchSoftwareInventory(string) ([]SoftwareInventorySearchRow, error) {
	return nil, nil
}

func (stubSoftwareInventorySource) RequestSoftwareInventoryCollection(
	context.Context, string, SoftwareInventoryWriteSource,
) (SoftwareInventoryCollectionResult, error) {
	return SoftwareInventoryCollectionResult{}, nil
}
