package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewParsesPageTemplatesAtConstruction(t *testing.T) {
	h, _, err := New(Config{
		Role:        RoleStandalone,
		Servers:     &stubServerSource{},
		VersionHint: func() string { return "" },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	root := h.(*handler)
	if len(root.pages) == 0 {
		t.Fatal("New 应在构造时解析页面模板")
	}
	for name, parsed := range root.pages {
		if parsed.full == nil || parsed.fragment == nil {
			t.Fatalf("页面 %s 的整页或片段模板未解析", name)
		}
	}
}

func TestRenderPageUsesTemplatesParsedAtConstruction(t *testing.T) {
	h, _, err := New(Config{
		Role:        RoleStandalone,
		Servers:     &stubServerSource{},
		VersionHint: func() string { return "" },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	root := h.(*handler)
	if _, ok := root.pages["servers.tmpl"]; !ok {
		t.Fatal("服务器列表页模板应在构造时解析")
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	req.Host = "127.0.0.1:18080"
	rec := httptest.NewRecorder()
	root.writePage(rec, req, http.StatusOK, "servers.tmpl", pageData{})
	if rec.Code != http.StatusOK {
		t.Fatalf("使用构造时解析的模板渲染状态码 = %d，应为 200", rec.Code)
	}
}
