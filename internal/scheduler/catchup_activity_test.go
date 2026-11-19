package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestNextWakeFromCatchesSlotDuringPreviousRun(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, ActivityHours: []int{10}, TravelDisabled: true, KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true})
	cursor := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	now := cursor.Add(2 * time.Hour)
	at, kinds := s.nextWakeFrom(cursor, now)
	if want := cursor.Add(time.Hour); !at.Equal(want) || len(kinds) != 1 || kinds[0] != taskActivity {
		t.Fatalf("missed activity slot: at=%v kinds=%v want %v/activity", at, kinds, want)
	}
	at, kinds = s.nextWakeFrom(at, now)
	if at.Before(now) || hasKind(kinds, taskActivity) && at.Equal(cursor.Add(time.Hour)) {
		t.Fatalf("already handled slot returned again: at=%v kinds=%v", at, kinds)
	}
}

func TestCatchupPreservesRetryDayAndConsumesOnce(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	actual := time.Date(2026, 10, 29, 12, 0, 0, 0, zone)
	cursor := actual.Add(-16 * time.Hour)
	s := New(Config{CheckinHours: []int{9}, TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true})
	decision := s.Ledger().PlanRetry("checkin", actual, time.Minute, 1)
	if decision.Reason != "" {
		t.Fatalf("cannot plan retry: %s", decision.Reason)
	}
	at, kinds := s.nextWakeFrom(cursor, actual)
	if !at.Equal(time.Date(2026, 10, 29, 9, 0, 0, 0, zone)) || !hasKind(kinds, taskCheckin) {
		t.Fatalf("missed normal slot: at=%v kinds=%v", at, kinds)
	}
	next, kinds := s.nextWakeFrom(at, actual)
	if !next.Equal(decision.At) || !hasKind(kinds, taskCheckin) {
		t.Fatalf("retry lost during catch-up: at=%v kinds=%v", next, kinds)
	}
	if !s.Ledger().ConsumeRetry("checkin", decision.At) || s.Ledger().ConsumeRetry("checkin", decision.At) {
		t.Fatal("retry must be consumed exactly once")
	}
	next, _ = s.nextWakeFrom(at, decision.At)
	if !next.After(decision.At) {
		t.Fatalf("consumed retry selected again: %v", next)
	}
}

func TestActivityRefreshesExpiredTokenBeforeReport(t *testing.T) {
	fastActivity(t)
	var refreshed, reported atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			refreshed.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		case r.URL.Path == "/v2/report":
			if refreshed.Load() != 1 || r.Header.Get("Authorization") != "Bearer new" {
				t.Error("report did not use the refreshed credential")
			}
			reported.Add(1)
			w.Write([]byte(`{"code":0}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour).Unix()})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})
	s.runActivity(context.Background(), triggerSchedule)
	if refreshed.Load() != 1 || reported.Load() != 1 || s.Ledger().Runs()["activity"].OK != 1 {
		t.Fatalf("refresh=%d report=%d ledger=%+v", refreshed.Load(), reported.Load(), s.Ledger().Runs()["activity"])
	}
}

func TestActivityRefreshFailureIsCountedAndRetried(t *testing.T) {
	fastActivity(t)
	var reports atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			reports.Add(1)
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour).Unix()})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, RetryDelayMinutes: 1, RetryMaxPerDay: 1})
	s.runActivity(context.Background(), triggerSchedule)
	run := s.Ledger().Runs()["activity"]
	if reports.Load() != 0 || run.Fail != 1 || !run.AllFailed {
		t.Fatalf("reports=%d run=%+v", reports.Load(), run)
	}
	// 跨 CST 日界线时策略会拒绝当日重试，不依赖测试运行的墙上时钟。
	if travelDay(run.Finished) == travelDay(run.Finished.Add(time.Minute)) && s.Ledger().RetryStates()["activity"].Next == nil {
		t.Fatal("expected an armed retry for an entirely failed scheduled run")
	}
}
