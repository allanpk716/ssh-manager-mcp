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

// renderFullPage 渲染带布局外壳的整页。
func renderFullPage(w http.ResponseWriter, status int, page string, data pageData) {
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

// renderError 渲染中文错误页。
func renderError(w http.ResponseWriter, status int, title, message string) {
	renderFullPage(w, status, "error.tmpl", pageData{
		Title:        title,
		ErrorTitle:   title,
		ErrorMessage: message,
	})
}
