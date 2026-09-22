package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

// 验证：在默认（Go 1.22+ 新 mux）语义下，主 mux 上更具体的
// /admin/accounts/{uid}/{action} 会盖过控制台的 /admin/ 前缀子树——
// 即上游运维端点与内嵌控制台可以共存于同一路径前缀。
func main() {
	upstreamOps := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream ops uid=%s action=%s", r.PathValue("uid"), r.PathValue("action"))
	})
	console := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "console %s", r.URL.Path)
	})

	for _, order := range []string{"ops-first", "ops-last"} {
		mux := http.NewServeMux()
		register := func() {
			if order == "ops-first" {
				mux.HandleFunc("POST /admin/accounts/{uid}/{action}", upstreamOps)
				mux.Handle("/admin", console)
				mux.Handle("/admin/", console)
				return
			}
			mux.Handle("/admin", console)
			mux.Handle("/admin/", console)
			mux.HandleFunc("POST /admin/accounts/{uid}/{action}", upstreamOps)
		}
		register()
		fmt.Println("---", order)
		for _, tc := range []struct{ method, path string }{
			{"POST", "/admin/accounts/u1/disable"},
			{"GET", "/admin/app.js"},
			{"GET", "/admin/"},
		} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			fmt.Printf("  %-5s %-30s -> %d %s\n", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
