package webui

import (
	"embed"
	"html/template"
	"net/http"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// pageData 是所有页面模板共享的渲染数据；后续票新增页面时在这里扩展字段，
// 不另建一套传递结构。
type pageData struct {
	Title        string
	Servers      []ServerRow
	ErrorTitle   string
	ErrorMessage string
}

// renderPage 按请求渲染页面：带 HX-Request 请求头（htmx 局部刷新）时只渲染
// 内容区片段，普通请求渲染带布局外壳的整页。
func renderPage(w http.ResponseWriter, r *http.Request, status int, page string, data pageData) {
	if isFragmentRequest(r) {
		tmpl, err := template.ParseFS(templateFS, "templates/"+page)
		if err != nil {
			http.Error(w, "页面模板加载失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_ = tmpl.ExecuteTemplate(w, "content", data)
		return
	}
	tmpl, err := template.ParseFS(templateFS, "templates/layout.tmpl", "templates/"+page)
	if err != nil {
		http.Error(w, "页面模板加载失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// 写出头后无法再改状态码；模板执行失败时至少保留已写出的页面开头。
		return
	}
}

// isFragmentRequest 判断是否为 htmx 发起的局部刷新请求。
func isFragmentRequest(r *http.Request) bool {
	return r != nil && r.Header.Get("HX-Request") == "true"
}

// renderError 渲染中文错误页；htmx 请求得到片段，普通请求得到整页。
func renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	renderPage(w, r, status, "error.tmpl", pageData{
		Title:        title,
		ErrorTitle:   title,
		ErrorMessage: message,
	})
}
