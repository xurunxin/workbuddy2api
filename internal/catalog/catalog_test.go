package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func catalogClient(server *httptest.Server) *upstream.Client {
	return &upstream.Client{HTTP: server.Client(), ChatBaseCN: server.URL, BillingBaseCN: server.URL}
}

func catalogModels(id string) string {
	return `{"code":0,"data":{"models":[{"id":"` + id + `","name":"` + id + `","credits":"x0.25","tags":["original"]}],"agents":[{"name":"cli","models":["` + id + `"]}]}}`
}

func TestServiceSeparatesAccountsInvalidatesPointerAndClonesCache(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls[token]++
		mu.Unlock()
		_, _ = w.Write([]byte(catalogModels("model-" + token)))
	}))
	defer server.Close()

	svc := New(catalogClient(server))
	a1 := &auth.Auth{UID: "same-uid", AccessToken: "one", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	a2 := &auth.Auth{UID: "other-uid", AccessToken: "two", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	v1, err := svc.Get(context.Background(), a1, false)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := svc.Get(context.Background(), a2, false)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Models[0].ID != "model-one" || v2.Models[0].ID != "model-two" || v1.Source != "upstream" || v2.Source != "upstream" {
		t.Fatalf("account catalog mixed: first=%+v second=%+v", v1, v2)
	}
	// Returned fields must be independent from the cache held by Service.
	v1.Models[0].Tags[0] = "poison"
	*v1.Models[0].CreditMultiplier = 99
	cached, err := svc.Get(context.Background(), a1, false)
	if err != nil || cached.Source != "cache" {
		t.Fatalf("cached Get = %+v, %v", cached, err)
	}
	if cached.Models[0].Tags[0] != "original" || cached.Models[0].CreditMultiplier == nil || *cached.Models[0].CreditMultiplier != 0.25 {
		t.Fatalf("caller mutation contaminated cache: %+v", cached.Models[0])
	}

	// Reauthorization has the same UID but a new pointer; it must never reuse
	// models fetched under the old account credentials.
	newA1 := &auth.Auth{UID: "same-uid", AccessToken: "three", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	reauthorized, err := svc.Get(context.Background(), newA1, false)
	if err != nil || reauthorized.Models[0].ID != "model-three" || reauthorized.Source != "upstream" {
		t.Fatalf("pointer replacement reused stale catalog: %+v, %v", reauthorized, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["one"] != 1 || calls["two"] != 1 || calls["three"] != 1 {
		t.Fatalf("unexpected account fetch counts: %#v", calls)
	}
}

func TestServiceTTLForceStaleFailureNegativeCacheAndRecovery(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	const upstreamBody = "sensitive-upstream-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, upstreamBody, http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(catalogModels("stable")))
	}))
	defer server.Close()
	svc := New(catalogClient(server))
	a := &auth.Auth{UID: "uid", AccessToken: "private-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	first, err := svc.Get(context.Background(), a, false)
	if err != nil || first.Source != "upstream" || first.FetchedAt == nil {
		t.Fatalf("initial fetch = %+v, %v", first, err)
	}
	if _, err := svc.Get(context.Background(), a, false); err != nil || calls.Load() != 1 {
		t.Fatalf("fresh cache should avoid fetch: calls=%d err=%v", calls.Load(), err)
	}

	svc.mu.Lock()
	e := svc.entries[a.UID]
	svc.mu.Unlock()
	e.mu.Lock()
	expiredAt := time.Now().Add(-TTL - time.Second)
	e.fetched = expiredAt
	e.mu.Unlock()
	fail.Store(true)
	stale, err := svc.Get(context.Background(), a, false)
	if err == nil || stale.Source != "stale" || !stale.Stale || len(stale.Models) != 1 || stale.FetchedAt == nil || !stale.FetchedAt.Equal(expiredAt) {
		t.Fatalf("expired failure should return dated stale data: view=%+v err=%v", stale, err)
	}
	if stale.Models[0].CreditMultiplier == nil || *stale.Models[0].CreditMultiplier != 0.25 {
		t.Fatalf("stale multiplier changed: %+v", stale.Models[0])
	}
	if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), upstreamBody) || strings.Contains(stale.Error, upstreamBody) {
		t.Fatalf("public catalog failure leaked sensitive upstream data: err=%q view=%+v", err, stale)
	}
	if _, err := svc.Get(context.Background(), a, false); err == nil || calls.Load() != 2 {
		t.Fatalf("failure cooldown should be a negative cache: calls=%d err=%v", calls.Load(), err)
	}
	fail.Store(false)
	refreshed, err := svc.Get(context.Background(), a, true)
	if err != nil || refreshed.Source != "upstream" || calls.Load() != 3 {
		t.Fatalf("force refresh should bypass failure cooldown: view=%+v calls=%d err=%v", refreshed, calls.Load(), err)
	}
}

func TestServiceNoCacheFailureHasNegativeCacheAndCanRecover(t *testing.T) {
	var calls atomic.Int32
	var healthy atomic.Bool
	const body = "failure-body-must-not-escape"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			http.Error(w, body, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(catalogModels("recovered")))
	}))
	defer server.Close()
	svc := New(catalogClient(server))
	a := &auth.Auth{UID: "uid", AccessToken: "never-show-this", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	v, err := svc.Get(context.Background(), a, false)
	if err == nil || v.Source != "unavailable" || len(v.Models) != 0 || !v.Stale {
		t.Fatalf("no-cache failure = %+v, %v", v, err)
	}
	if strings.Contains(err.Error(), body) || strings.Contains(err.Error(), a.AccessToken) || strings.Contains(v.Error, body) {
		t.Fatalf("no-cache failure leaked sensitive data: err=%q view=%+v", err, v)
	}
	if _, err := svc.Get(context.Background(), a, false); err == nil || calls.Load() != 1 {
		t.Fatalf("no-cache failure should be negatively cached: calls=%d err=%v", calls.Load(), err)
	}
	healthy.Store(true)
	v, err = svc.Get(context.Background(), a, true)
	if err != nil || v.Source != "upstream" || len(v.Models) != 1 || v.Models[0].ID != "recovered" || calls.Load() != 2 {
		t.Fatalf("force recovery = %+v, calls=%d, err=%v", v, calls.Load(), err)
	}
}

func TestServiceConcurrentGetFetchesOncePerAccount(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte(catalogModels(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))))
	}))
	defer server.Close()
	svc := New(catalogClient(server))
	accounts := []*auth.Auth{
		{UID: "one", AccessToken: "token-one", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		{UID: "two", AccessToken: "token-two", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
	var group sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func(account *auth.Auth) {
			defer group.Done()
			_, err := svc.Get(context.Background(), account, false)
			errs <- err
		}(accounts[i%len(accounts)])
	}
	<-started
	<-started
	close(release)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Get: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("concurrent Gets made %d upstream calls, want one per account", calls.Load())
	}
}

func TestServiceRefreshesExpiredTokenAndPersistsBeforeFetchingModels(t *testing.T) {
	var refreshes, models atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			refreshes.Add(1)
			if r.Header.Get("X-Refresh-Token") != "old-refresh" {
				t.Errorf("refresh token header = %q", r.Header.Get("X-Refresh-Token"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`))
		case "/console/enterprises/personal/models":
			models.Add(1)
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Errorf("models Authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(catalogModels("after-refresh")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1, FilePath: file}
	svc := New(catalogClient(server))
	v, err := svc.Get(context.Background(), a, false)
	if err != nil || v.Source != "upstream" || refreshes.Load() != 1 || models.Load() != 1 {
		t.Fatalf("expired-token Get = %+v refreshes=%d models=%d err=%v", v, refreshes.Load(), models.Load(), err)
	}
	if a.AccessToken != "new-access" || a.RefreshToken != "new-refresh" || a.NeedsRefresh(0) {
		t.Fatalf("auth was not refreshed: %+v", a)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "new-access") || !strings.Contains(string(raw), "new-refresh") {
		t.Fatalf("refreshed credentials were not persisted: %s", raw)
	}
}
