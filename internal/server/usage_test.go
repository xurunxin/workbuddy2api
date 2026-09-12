package server

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/accesskey"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usage"
)

func TestUsageChatAndResponsesExactlyOnce(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", endpoint, stream), func(t *testing.T) {
				dir := t.TempDir()
				keys, err := accesskey.Open(filepath.Join(dir, "keys.json"), "")
				if err != nil {
					t.Fatal(err)
				}
				key, secret, err := keys.Create("test")
				if err != nil {
					t.Fatal(err)
				}
				store, err := usage.Open(filepath.Join(dir, "usage.json"))
				if err != nil {
					t.Fatal(err)
				}
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
				h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999}), Upstream: up, IdentifyAPIKey: keys.Identify, Usage: store})
				body := fmt.Sprintf(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":%v}`, stream)
				if endpoint == "/v1/responses" {
					body = fmt.Sprintf(`{"model":"glm-5.2","input":"hi","stream":%v}`, stream)
				}
				r := httptest.NewRequest("POST", endpoint, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+secret)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("%d %s", w.Code, w.Body)
				}
				rows, _ := store.Snapshot("", "")
				if len(rows) != 1 || rows[0].KeyID != key.ID || rows[0].Requests != 1 || rows[0].Input != 1 || rows[0].Output != 1 || rows[0].Missing != 0 {
					t.Fatal(rows)
				}
				if err := keys.Revoke(key.ID); err != nil {
					t.Fatal(err)
				}
				r = httptest.NewRequest("POST", endpoint, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+secret)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal(w.Code)
				}
				rows, _ = store.Snapshot("", "")
				if rows[0].Requests != 1 {
					t.Fatal("rejected key counted", rows)
				}
			})
		}
	}
}

func TestUsageFailureAndMissingUsage(t *testing.T) {
	store, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: testPoolWith(), Usage: store})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test"}`)))
	rows, _ := store.Snapshot("", "")
	if w.Code != 503 || len(rows) != 1 || rows[0].Failures != 1 || rows[0].Missing != 1 {
		t.Fatal(w.Code, rows)
	}
}
