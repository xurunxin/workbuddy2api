package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
)

func main() {
	fmt.Println("GODEBUG =", os.Getenv("GODEBUG"))
	for _, g := range os.Environ() {
		if strings.HasPrefix(g, "GODEBUG=") || strings.HasPrefix(g, "GO") {
			fmt.Println(" env:", g)
		}
	}

	// 最小用例：单段通配
	mux := http.NewServeMux()
	mux.HandleFunc("/a/{uid}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "HIT %s", r.PathValue("uid"))
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/a/u1", nil))
	fmt.Printf("minimal /a/{uid} -> %d %q\n", rec.Code, rec.Body.String())

	// 方法 + 通配
	mux2 := http.NewServeMux()
	mux2.HandleFunc("GET /b/{uid}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "HIT %s", r.PathValue("uid"))
	})
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, httptest.NewRequest("GET", "/b/u1", nil))
	fmt.Printf("minimal GET /b/{uid} -> %d %q\n", rec2.Code, rec2.Body.String())
}
