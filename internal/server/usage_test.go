package server

import (
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/accesskey"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usage"
)

func TestCacheUsageParsing(t *testing.T) {
	for _, tc := range []struct {
		name, frames string
		cached       int64
		known        bool
	}{
		{"missing", `{"prompt_tokens":10,"completion_tokens":2}`, 0, false},
		{"zero", `{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}`, 0, true},
		{"alias", `{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":4}`, 4, true},
		{"precedence", `{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}`, 3, true},
		{"negative", `{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":-1}}`, 0, false},
		{"too large", `{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":11}}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := "data: {\"usage\":" + tc.frames + "}\n\n"
			s := newChatStatsReaderSince(strings.NewReader(frame+frame+"data: [DONE]\n"), time.Now())
			_, _ = io.Copy(io.Discard, s)
			if !s.hasTokenUsage || s.input != 10 || s.tokens != 2 || (s.cached != nil) != tc.known {
				t.Fatalf("stats=%+v", s)
			}
			if s.cached != nil && *s.cached != tc.cached {
				t.Fatal(*s.cached)
			}
		})
	}
}

func TestResponseUsageCacheAlias(t *testing.T) {
	u := map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(2), "prompt_cache_hit_tokens": float64(4)}
	result := responseUsage(u).(map[string]any)
	if result["input_tokens_details"].(map[string]any)["cached_tokens"] != int64(4) {
		t.Fatal(result)
	}
	u["prompt_tokens_details"] = map[string]any{"cached_tokens": float64(0)}
	result = responseUsage(u).(map[string]any)
	if result["input_tokens_details"].(map[string]any)["cached_tokens"] != int64(0) {
		t.Fatal(result)
	}
}

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
				cachedSSE := strings.ReplaceAll(sseOK, `"prompt_tokens":1`, `"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":1}`)
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, cachedSSE, true })
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
				if len(rows) != 1 || rows[0].KeyID != key.ID || rows[0].Requests != 1 || rows[0].Input != 1 || rows[0].Output != 1 || rows[0].Missing != 0 || rows[0].Cached != 1 || rows[0].CacheMissing != 0 {
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
