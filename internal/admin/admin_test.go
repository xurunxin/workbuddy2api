package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

type loginSession struct {
	cookie *http.Cookie
	csrf   string
}

func testAdmin(t *testing.T) *Handler {
	t.Helper()
	return New(Config{Password: "test-only-password", AuthDir: t.TempDir(), Pool: pool.New(""), Upstream: upstream.New()})
}
func adminRequest(h *Handler, method, path, body string, s *loginSession) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if s != nil {
		r.AddCookie(s.cookie)
		r.Header.Set("X-CSRF-Token", s.csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func loginAs(t *testing.T, h *Handler) *loginSession {
	t.Helper()
	w := adminRequest(h, "POST", "/admin/api/login", `{"password":"test-only-password"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login status %d", w.Code)
	}
	var v struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return &loginSession{w.Result().Cookies()[0], v.CSRF}
}

func TestAdminAuthenticationIsolation(t *testing.T) {
	h := testAdmin(t)
	for _, path := range []string{"/admin/api/status", "/admin/api/config"} {
		if w := adminRequest(h, "GET", path, "", nil); w.Code != 401 {
			t.Errorf("%s=%d", path, w.Code)
		}
	}
	if w := adminRequest(h, "POST", "/admin/api/login", `{"password":"wrong"}`, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	s := loginAs(t, h)
	if !s.cookie.HttpOnly || s.cookie.Path != "/admin" || s.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie attributes")
	}
	if w := adminRequest(h, "GET", "/admin/api/status", "", s); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(h, "POST", "/admin/api/logout", `{}`, s); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(h, "GET", "/admin/api/status", "", s); w.Code != 401 {
		t.Fatal("logged out session remains valid")
	}
	h.cfg.Password = ""
	if w := adminRequest(h, "POST", "/admin/api/login", `{"password":""}`, nil); w.Code != 503 {
		t.Fatal("empty password enabled management")
	}
}
func TestAdminCSRFAndBodyValidation(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	for _, tc := range []struct {
		origin, csrf, ctype string
		want                int
	}{{"https://evil.example", s.csrf, "application/json", 403}, {"", "wrong", "application/json", 403}, {"", s.csrf, "text/plain", 415}} {
		r := httptest.NewRequest("POST", "http://localhost/admin/api/logout", strings.NewReader(`{}`))
		r.AddCookie(s.cookie)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CSRF-Token", tc.csrf)
		r.Header.Set("Content-Type", tc.ctype)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("got %d want %d", w.Code, tc.want)
		}
	}
	if w := adminRequest(h, "POST", "/admin/api/accounts/import", `{} {}`, s); w.Code != 400 {
		t.Fatal("accepted trailing JSON")
	}
}
func TestAdminLoginRateLimitAndExpiry(t *testing.T) {
	h := testAdmin(t)
	for i := 0; i < 8; i++ {
		adminRequest(h, "POST", "/admin/api/login", `{"password":"wrong"}`, nil)
	}
	if w := adminRequest(h, "POST", "/admin/api/login", `{"password":"test-only-password"}`, nil); w.Code != 429 {
		t.Fatal("unlimited attempts")
	}
	h.mu.Lock()
	h.attempts = make(map[string]attempt)
	h.mu.Unlock()
	s := loginAs(t, h)
	h.mu.Lock()
	v := h.sessions[s.cookie.Value]
	v.expires = time.Now().Add(-time.Second)
	h.sessions[s.cookie.Value] = v
	h.mu.Unlock()
	if w := adminRequest(h, "GET", "/admin/api/status", "", s); w.Code != 401 {
		t.Fatal("expired session valid")
	}
}
func TestAdminImportCreditsAndReauthorization(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access" {
			t.Error("wrong bearer")
		}
		io.WriteString(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":750}]}}}}`)
	}))
	defer up.Close()
	h.cfg.Upstream.BillingBaseCN = up.URL
	var raw = []byte(`{"accessToken":"fake-access","refreshToken":"fake-refresh","uid":"u1","nickname":"test","expiresAt":4102444800}`)
	if w := adminRequest(h, "POST", "/admin/api/accounts/import", string(raw), s); w.Code != 201 {
		t.Fatalf("import %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(h.cfg.AuthDir, "workbuddy-u1.json")); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/credits", `{}`, s); w.Code != 200 {
		t.Fatalf("credits %d %s", w.Code, w.Body.String())
	}
	st, _ := h.cfg.Pool.Status("u1")
	if st.Credits != 750 {
		t.Fatal(st.Credits)
	}
	w := adminRequest(h, "GET", "/admin/api/status", "", s)
	if strings.Contains(w.Body.String(), "fake-access") || strings.Contains(w.Body.String(), "fake-refresh") {
		t.Fatal("credential disclosure")
	}
	old := h.cfg.Pool.AuthByUID("u1")
	if w := adminRequest(h, "POST", "/admin/api/accounts/import", string(raw), s); w.Code != 400 {
		t.Fatal("overwrote active credentials")
	}
	adminRequest(h, "POST", "/admin/api/accounts/u1/disable", `{}`, s)
	if w := adminRequest(h, "POST", "/admin/api/accounts/import", string(raw), s); w.Code != 201 {
		t.Fatalf("reauth %d %s", w.Code, w.Body.String())
	}
	if err := old.SaveAtomic(); err == nil {
		t.Fatal("retired credentials can overwrite new file")
	}
	for _, uid := range []string{"../escape", "..", "a/b"} {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["uid"] = uid
		b, _ := json.Marshal(m)
		if w := adminRequest(h, "POST", "/admin/api/accounts/import", string(b), s); w.Code != 400 {
			t.Fatal("path traversal accepted")
		}
	}
}

func TestOAuthBoundToSessionAndSavesAccount(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	other := loginAs(t, h)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			io.WriteString(w, `{"code":0,"data":{"state":"private-state","authUrl":"https://www.codebuddy.cn/login?state=private-state"}}`)
		case "/v2/plugin/auth/token":
			io.WriteString(w, `{"code":0,"data":{"accessToken":"oauth-access","refreshToken":"oauth-refresh","expiresIn":3600}}`)
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer oauth-access" {
				t.Error("missing account auth")
			}
			io.WriteString(w, `{"code":0,"data":{"uid":"oauth-user","nickname":"OAuth"}}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	w := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("start=%d %s", w.Code, w.Body.String())
	}
	var flow struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &flow)
	path := "/admin/api/oauth/" + flow.ID + "/poll"
	if w := adminRequest(h, "POST", path, `{}`, other); w.Code != 404 {
		t.Fatal("another session polled flow")
	}
	w = adminRequest(h, "POST", path, `{}`, s)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "completed") {
		t.Fatalf("poll=%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "oauth-access") || strings.Contains(w.Body.String(), "oauth-refresh") {
		t.Fatal("oauth exposed tokens")
	}
	a := h.cfg.Pool.AuthByUID("oauth-user")
	if a == nil {
		t.Fatal("oauth account missing")
	}
	saved, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.Parse(saved)
	if err != nil || parsed.UID != "oauth-user" {
		t.Fatal("invalid saved account")
	}
	if w := adminRequest(h, "POST", path, `{}`, s); w.Code != 200 {
		t.Fatal("completion not idempotent")
	}
}

func TestAdminStaticAndAPINotFound(t *testing.T) {
	h := testAdmin(t)
	for _, p := range []string{"/admin/", "/admin/app.js", "/admin/style.css"} {
		w := adminRequest(h, "GET", p, "", nil)
		if w.Code != 200 {
			t.Fatalf("%s=%d", p, w.Code)
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing CSP")
		}
	}
	if w := adminRequest(h, "GET", "/admin/api/nonexistent", "", nil); w.Code != 404 {
		t.Fatal("API fallback served HTML")
	}
}

func TestCreditsRefreshesExpiredCredentialsBeforeQuery(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			io.WriteString(w, `{"code":0,"data":{"accessToken":"refreshed-test-token","refreshToken":"refreshed-test-refresh","expiresIn":3600}}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-test-token" {
			t.Error("balance queried with expired token")
		}
		io.WriteString(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":90}]}}}}`)
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	h.cfg.Upstream.BillingBaseCN = up.URL
	w := adminRequest(h, "POST", "/admin/api/accounts/import", `{"accessToken":"expired","refreshToken":"test-refresh","expiresAt":1,"uid":"expired"}`, s)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	w = adminRequest(h, "POST", "/admin/api/accounts/expired/credits", `{}`, s)
	if w.Code != 200 {
		t.Fatalf("credits=%d %s", w.Code, w.Body.String())
	}
	st, _ := h.cfg.Pool.Status("expired")
	if st.Credits != 90 {
		t.Fatal(st.Credits)
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, "workbuddy-expired.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil || a.AccessToken != "refreshed-test-token" {
		t.Fatal("refreshed credentials not persisted")
	}
}
