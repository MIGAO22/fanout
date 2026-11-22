package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// 界面文件放在 web/ 下随二进制一起编译，改样式不用碰 Go 代码。
//
//go:embed web/index.html web/app.js
var webFS embed.FS

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	serveEmbed(w, "web/index.html", "text/html; charset=utf-8")
}

// handleAppJS 单独挂一条精确路由。Go 的 ServeMux 里精确匹配优先于 "/" 子树，
// 所以它不会被 handleIndex 的 404 分支吃掉。
func handleAppJS(w http.ResponseWriter, r *http.Request) {
	// 不显式声明的话 http 会把 JS 嗅探成 text/plain，浏览器直接拒绝执行脚本
	serveEmbed(w, "web/app.js", "application/javascript; charset=utf-8")
}

func serveEmbed(w http.ResponseWriter, name, ctype string) {
	b, err := fs.ReadFile(webFS, name)
	if err != nil {
		http.Error(w, "内嵌资源缺失: "+name, http.StatusInternalServerError)
		return
	}
	// 内嵌内容随版本变，不给缓存；否则 --update 换二进制后浏览器还在跑旧脚本
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
