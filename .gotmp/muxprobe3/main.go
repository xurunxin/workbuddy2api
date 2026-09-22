package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

func main() {
	r := httptest.NewRequest("GET", "/a/u1", nil)
	fmt.Printf("URL.Path=%q Host=%q Method=%q RequestURI=%q\n", r.URL.Path, r.Host, r.Method, r.RequestURI)

	// 直接看 mux 内部会用到的匹配：注册普通模式（无通配）作为对照
	mux := http.NewServeMux()
	mux.HandleFunc("/a/u1", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "literal HIT") })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	fmt.Printf("literal /a/u1 -> %d %q\n", rec.Code, rec.Body.String())

	// 服务端真实请求路径（通过 httptest.Server）验证通配匹配
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "server saw path=%s", req.URL.Path)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/accounts/u1/disable")
	if err != nil {
		fmt.Println("get err:", err)
		return
	}
	defer resp.Body.Close()
	buf := make([]byte, 200)
	n, _ := resp.Body.Read(buf)
	fmt.Printf("real server -> %d %q\n", resp.StatusCode, string(buf[:n]))
}
