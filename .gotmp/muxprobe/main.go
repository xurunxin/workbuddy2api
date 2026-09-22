package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
)

func probe(name, pattern, method, path string) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Printf("%-46s -> PANIC %v\n", name, rec)
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "HIT uid=%s action=%s", r.PathValue("uid"), r.PathValue("action"))
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	fmt.Printf("%-46s -> %d %q\n", name, rec.Code, rec.Body.String())
}

func main() {
	fmt.Println("go:", runtime.Version())
	probe("POST /admin/accounts/{uid}/{action}", "POST /admin/accounts/{uid}/{action}", "POST", "/admin/accounts/u1/disable")
	probe("POST /admin/accounts/{uid}/disable", "POST /admin/accounts/{uid}/disable", "POST", "/admin/accounts/u1/disable")
	probe("POST /admin/accounts/{uid}", "POST /admin/accounts/{uid}", "POST", "/admin/accounts/u1")
	probe("POST /admin/x/{uid}/y", "POST /admin/x/{uid}/y", "POST", "/admin/x/u1/y")
	probe("POST /a/{uid}/b", "POST /a/{uid}/b", "POST", "/a/u1/b")
	probe("/admin/accounts/{uid}/{action} (no method)", "/admin/accounts/{uid}/{action}", "POST", "/admin/accounts/u1/disable")
	probe("GET /admin/accounts/{uid}/{action}", "GET /admin/accounts/{uid}/{action}", "GET", "/admin/accounts/u1/disable")
}
