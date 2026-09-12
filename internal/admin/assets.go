package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
)

//go:embed web/index.html web/app.js web/style.css
var webFiles embed.FS

// assets intentionally serves only the three console resources. The API uses
// more-specific mux patterns; other admin paths remain a 404.
func (h *Handler) assets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	name := ""
	switch r.URL.Path {
	case "/admin/":
		name = "web/index.html"
	case "/admin/app.js":
		name = "web/app.js"
	case "/admin/style.css":
		name = "web/style.css"
	default:
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(webFiles, name)
	if err != nil {
		http.Error(w, "管理界面资源不可用", http.StatusInternalServerError)
		return
	}
	if name == "web/app.js" {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	} else if path.Ext(name) == ".css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	_, _ = w.Write(b)
}
