package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accesskey"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/catalog"
	"workbuddy2api/internal/server"
)

func TestManagedKeysAuthenticateEveryAPIRouteAndRevokePersistently(t *testing.T) {
	h := testAdmin(t)
	path := filepath.Join(t.TempDir(), "api-keys.json")
	store, err := accesskey.Open(path, "legacy-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.KeyStore = store
	api := server.NewHandler(server.Config{Pool: h.cfg.Pool, Upstream: h.cfg.Upstream, APIKey: "must-not-bypass-store", ValidateAPIKey: store.Validate})
	s := loginAs(t, h)
	if w := adminRequest(h, "POST", "/admin/api/keys", `{"name":"QA client"}`, nil); w.Code != 401 {
		t.Fatal("keys exposed without management login")
	}
	badCSRF := &loginSession{cookie: s.cookie, csrf: "incorrect"}
	if w := adminRequest(h, "POST", "/admin/api/keys", `{"name":"QA client"}`, badCSRF); w.Code != 403 {
		t.Fatal("key creation missing CSRF guard")
	}
	for _, body := range []string{`{}`, `{"name":" "}`, `{"name":"a","secret":"attacker-key"}`} {
		if w := adminRequest(h, "POST", "/admin/api/keys", body, s); w.Code != 400 {
			t.Fatalf("accepted invalid creation: %d", w.Code)
		}
	}
	w := adminRequest(h, "POST", "/admin/api/keys", `{"name":"QA client"}`, s)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		Key    accesskey.View `json:"key"`
		Secret string         `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Secret == "" {
		t.Fatal("missing one-time secret")
	}
	w = adminRequest(h, "GET", "/admin/api/keys", "", s)
	if strings.Contains(w.Body.String(), created.Secret) || strings.Contains(w.Body.String(), "legacy-test-secret") || strings.Contains(w.Body.String(), `"hash"`) {
		t.Fatal("list leaked secret")
	}

	routes := []struct{ method, path, body string }{
		{"GET", "/v1/models", ""}, {"GET", "/status", ""},
		{"POST", "/v1/chat/completions", "{}"}, {"POST", "/v1/responses", "{}"},
		{"GET", "/v1/responses/missing", ""}, {"DELETE", "/v1/responses/missing", ""},
	}
	check := func(secret string, authorized bool) {
		t.Helper()
		for _, route := range routes {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			if secret != "" {
				r.Header.Set("Authorization", "Bearer "+secret)
			}
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			if (w.Code != 401) != authorized {
				t.Fatalf("%s %s: authorized=%v status=%d", route.method, route.path, authorized, w.Code)
			}
		}
	}
	check(created.Secret, true)
	check("legacy-test-secret", true)
	check("must-not-bypass-store", false)
	check("", false)
	if w := adminRequest(h, "POST", "/admin/api/keys/"+created.Key.ID+"/revoke", `{}`, badCSRF); w.Code != 403 {
		t.Fatal("revocation missing CSRF")
	}
	check(created.Secret, true)
	for i := 0; i < 2; i++ {
		if w := adminRequest(h, "POST", "/admin/api/keys/"+created.Key.ID+"/revoke", `{}`, s); w.Code != 200 {
			t.Fatal("revocation not idempotent", w.Code)
		}
	}
	check(created.Secret, false)
	check("legacy-test-secret", true)
	if w := adminRequest(h, "POST", "/admin/api/keys/missing/revoke", `{}`, s); w.Code != 404 {
		t.Fatal("missing key status")
	}
	// Revoking the last key must not turn authentication off, even after restart
	// with the old deployment configuration still present.
	legacyID := store.List()[0].ID
	if w := adminRequest(h, "POST", "/admin/api/keys/"+legacyID+"/revoke", `{}`, s); w.Code != 200 {
		t.Fatal(w.Code)
	}
	reopened, err := accesskey.Open(path, "legacy-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	api = server.NewHandler(server.Config{Pool: h.cfg.Pool, Upstream: h.cfg.Upstream, ValidateAPIKey: reopened.Validate})
	check(created.Secret, false)
	check("legacy-test-secret", false)
	check("", false)
}

func TestAdminAccountModelsProvenanceAndPermissions(t *testing.T) {
	var failed atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("wrong account credential")
		}
		if failed.Load() {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("fixture-token"))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"real-id","name":"Friendly model","credits":"x0.25 credits"}],"agents":[{"name":"cli","models":["real-id"]}]}}`))
	}))
	defer up.Close()
	h := testAdmin(t)
	h.cfg.Upstream.ChatBaseCN = up.URL
	h.cfg.Pool.Add(&auth.Auth{UID: "account-a", Domain: "cn", AccessToken: "fixture-token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	s := loginAs(t, h)
	path := "/admin/api/accounts/account-a/models"
	if w := adminRequest(h, "GET", path, "", nil); w.Code != 401 {
		t.Fatal("models lack management auth")
	}
	if w := adminRequest(h, "GET", "/admin/api/accounts/missing/models", "", s); w.Code != 404 {
		t.Fatal(w.Code)
	}
	get := func(method, source string) catalog.View {
		t.Helper()
		url := path
		if method == "POST" {
			url += "/refresh"
		}
		w := adminRequest(h, method, url, `{}`, s)
		if w.Code != 200 {
			t.Fatalf("models: %d %s", w.Code, w.Body)
		}
		var v catalog.View
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if v.Source != source || v.AccountUID != "account-a" || len(v.Models) != 1 || v.Models[0].ID != "real-id" || v.Models[0].CreditMultiplier == nil || *v.Models[0].CreditMultiplier != .25 {
			t.Fatalf("incorrect catalog: %+v", v)
		}
		if strings.Contains(w.Body.String(), "fixture-token") {
			t.Fatal("token leaked from upstream error")
		}
		return v
	}
	v := get("GET", "upstream")
	get("GET", "cache")
	failed.Store(true)
	stale := get("POST", "stale")
	if !stale.Stale || stale.Error == "" || !stale.FetchedAt.Equal(*v.FetchedAt) {
		t.Fatal("stale metadata not preserved")
	}
	st, _ := h.cfg.Pool.Status("account-a")
	if st.Cooling {
		t.Fatal("metadata failure penalized account")
	}
	// A reauthorized account must not inherit the previous identity's cached list.
	h.cfg.Pool.Add(&auth.Auth{UID: "account-a", Domain: "cn", AccessToken: "fixture-token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if w := adminRequest(h, "GET", path, "", s); w.Code != 503 || strings.Contains(w.Body.String(), "fixture-token") {
		t.Fatal("uncached failure not reported safely")
	}
}
