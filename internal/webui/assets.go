package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// assetURLs 以静态资源文件名为键，值是模板使用的带内容哈希地址。
type assetURLs map[string]string

// hashedAssetFiles 是唯一的静态资源登记表；third-party.json 是清单文件，
// 不作为可访问资源分发。
var hashedAssetFiles = []string{"htmx.min.js", "app.css", "app.js"}

// loadAssets 计算每个静态资源的内容哈希并生成带哈希的地址。内容变化后地址
// 随之变化，浏览器不会继续使用旧版本的脚本或样式。
func loadAssets() (assetURLs, map[string]string, error) {
	urls := make(assetURLs, len(hashedAssetFiles))
	files := make(map[string]string, len(hashedAssetFiles))
	for _, name := range hashedAssetFiles {
		content, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			return assetURLs{}, nil, fmt.Errorf("读取内嵌静态资源 %s 失败：%w", name, err)
		}
		sum := sha256.Sum256(content)
		ext := path.Ext(name)
		base := strings.TrimSuffix(name, ext)
		address := "/ui/assets/" + base + "." + hex.EncodeToString(sum[:])[:12] + ext
		files[address] = name // 以完整地址为键，静态资源处理器直接按请求路径查找
		urls[name] = address
	}
	return urls, files, nil
}

// handleAsset 提供带内容哈希的静态资源：地址里的哈希保证内容不变，因此可以
// 长期缓存。静态资源不要求会话，但同样经过回环 Host 校验。
func (h *handler) handleAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "静态资源只接受 GET 请求。")
		return
	}
	name, ok := h.assetFiles[r.URL.Path]
	if !ok {
		h.renderError(w, r, http.StatusNotFound, "静态资源不存在", "请求的静态资源不存在。")
		return
	}
	content, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "静态资源读取失败", "静态资源读取失败，请重新运行 `sshmgr ui`。")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", assetContentType(name))
	_, _ = w.Write(content)
}

// assetContentType 按扩展名给出静态资源的内容类型。
func assetContentType(name string) string {
	switch path.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
