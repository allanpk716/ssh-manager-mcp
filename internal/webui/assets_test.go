package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestStaticAssetsServedWithHashedURLsAndLongCache(t *testing.T) {
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

	for name, urlPath := range map[string]string{
		"htmx.min.js": root.assets.HTMX,
		"app.css":     root.assets.Style,
		"app.js":      root.assets.App,
	} {
		want, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatalf("读取内嵌静态资源 %s 失败：%v", name, err)
		}
		if !strings.Contains(urlPath, "/ui/assets/") {
			t.Fatalf("%s 的地址 %q 应在静态资源前缀下", name, urlPath)
		}
		sum := sha256.Sum256(want)
		if !strings.Contains(urlPath, hex.EncodeToString(sum[:])[:12]) {
			t.Fatalf("%s 的地址 %q 应包含内容哈希", name, urlPath)
		}

		resp := doGet(t, h, "127.0.0.1:18080", urlPath)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码 = %d，应为 200", name, resp.StatusCode)
		}
		if string(body) != string(want) {
			t.Fatalf("%s 内容应与内嵌文件一致", name)
		}
		if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("%s 缓存策略 = %q，应为长期不可变缓存", name, got)
		}
		assertSecurityHeaders(t, resp, "静态资源 "+name)
	}
}

func TestPagesReferenceHashedAssetURLs(t *testing.T) {
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

	resp := doGet(t, h, "127.0.0.1:18080", "/ui/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, urlPath := range []string{root.assets.HTMX, root.assets.Style, root.assets.App} {
		if !strings.Contains(page, urlPath) {
			t.Fatalf("页面应引用带哈希的静态资源地址 %q：\n%s", urlPath, page)
		}
	}
	for _, banned := range []string{"https://cdn", "http://cdn", "unpkg.com", "googleapis.com"} {
		if strings.Contains(page, banned) {
			t.Fatalf("页面不得引用外部内容分发网络 %q", banned)
		}
	}
}

func TestStaticAssetRejectsNonLoopbackHost(t *testing.T) {
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
	resp := doGet(t, h, "127.0.0.1.evil.example:18080", root.assets.Style)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("非回环 Host 访问静态资源状态码 = %d，应被拒绝", resp.StatusCode)
	}
}

// thirdPartyManifestEntry 是第三方前端库清单里的一条登记。
type thirdPartyManifestEntry struct {
	File       string `json:"file"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	SourceURL  string `json:"source_url"`
	License    string `json:"license"`
	LicenseURL string `json:"license_url"`
	SHA256     string `json:"sha256"`
}

func TestThirdPartyManifestMatchesEmbeddedFiles(t *testing.T) {
	blob, err := staticFS.ReadFile("static/third-party.json")
	if err != nil {
		t.Fatalf("读取第三方资源清单失败：%v", err)
	}
	var manifest struct {
		Files []thirdPartyManifestEntry `json:"files"`
	}
	if err := json.Unmarshal(blob, &manifest); err != nil {
		t.Fatalf("第三方资源清单不是合法 JSON：%v", err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("第三方资源清单至少应登记 htmx")
	}
	for _, entry := range manifest.Files {
		if entry.File == "" || entry.Name == "" || entry.Version == "" || entry.SourceURL == "" || entry.License == "" || entry.LicenseURL == "" {
			t.Fatalf("清单条目必须写全文件名、名称、版本、来源网址与许可证：%+v", entry)
		}
		content, err := staticFS.ReadFile("static/" + entry.File)
		if err != nil {
			t.Fatalf("清单登记的 %s 不在内嵌静态资源里：%v", entry.File, err)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
			t.Fatalf("%s 的 SHA-256 = %s，清单登记为 %s", entry.File, got, entry.SHA256)
		}
	}
}

func TestTemplatesContainNoInlineStyleScriptOrEventAttributes(t *testing.T) {
	entries, err := templateContents()
	if err != nil {
		t.Fatalf("遍历内嵌模板失败：%v", err)
	}
	styleAttr := regexp.MustCompile(`(?i)\sstyle\s*=`)
	eventAttr := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	for name, content := range entries {
		if styleAttr.MatchString(content) {
			t.Fatalf("模板 %s 含内联 style 属性", name)
		}
		if eventAttr.MatchString(content) {
			t.Fatalf("模板 %s 含内联事件属性", name)
		}
		if strings.Contains(content, "hx-on") {
			t.Fatalf("模板 %s 含 hx-on", name)
		}
		for _, script := range regexp.MustCompile(`<script[^>]*>`).FindAllString(content, -1) {
			if !strings.Contains(script, "src=") && !strings.Contains(script, `type="application/json"`) {
				t.Fatalf("模板 %s 含非 JSON 的内联脚本：%s", name, script)
			}
		}
	}
}
