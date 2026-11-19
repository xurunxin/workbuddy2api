package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestCheckinRetryBusinessCode10000(t *testing.T) {
	for _, tc := range []struct {
		code int
		msg  string
		want bool
	}{
		{10000, "temporary error", true},
		{100000, "temporary error", false},
		{10001, "code=10000 msg=temporary error", false},
	} {
		t.Run(fmt.Sprintf("code_%d", tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
					fmt.Fprintf(w, `{"code":%d,"msg":%q}`, tc.code, tc.msg)
					return
				}
				w.Write([]byte(`{"code":0,"data":{}}`))
			}))
			defer srv.Close()
			p := pool.New("")
			p.Add(&auth.Auth{UID: "u", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
			s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, RetryDelayMinutes: 1, RetryMaxPerDay: 1})
			s.runCheckin(triggerSchedule)
			r := s.Ledger().Runs()["checkin"]
			if travelDay(r.Finished) != travelDay(r.Finished.Add(time.Minute)) {
				t.Skip("retry cannot cross CST day")
			}
			if got := s.Ledger().RetryStates()["checkin"].Next != nil; got != tc.want {
				t.Fatalf("retry armed=%v want %v", got, tc.want)
			}
		})
	}
	if retryableCheckinError(&upstream.Error{Status: 400, Msg: "code=10000 msg=error"}) {
		t.Fatal("HTTP 400 must not be treated as a 2xx business error")
	}
}

func TestCheckinRetryTargetsOnlyTransientFailures(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			mu.Lock()
			calls[token]++
			n := calls[token]
			mu.Unlock()
			if token == "transient" && n == 1 {
				http.Error(w, "unavailable", 503)
				return
			}
			if token == "permanent" {
				http.Error(w, "bad request", 400)
				return
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			if token == "resource" {
				http.Error(w, "unavailable", 503)
				return
			}
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()
	p := pool.New("")
	for _, uid := range []string{"ok", "transient", "permanent", "resource"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: uid, RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, RetryDelayMinutes: 1, RetryMaxPerDay: 1})
	s.runCheckin(triggerSchedule)
	r := s.Ledger().Runs()["checkin"]
	if travelDay(r.Finished) != travelDay(r.Finished.Add(time.Minute)) {
		t.Skip("retry cannot cross CST day")
	}
	if r.AllFailed || r.Fail != 3 {
		t.Fatalf("initial run: %+v", r)
	}
	st := s.Ledger().RetryStates()["checkin"]
	if st.Next == nil || st.Used != 1 {
		t.Fatalf("retry not armed: %+v", st)
	}
	if !s.Ledger().ConsumeRetry("checkin", *st.Next) {
		t.Fatal("retry not consumed")
	}
	s.runCheckin(triggerRetry)
	mu.Lock()
	defer mu.Unlock()
	for uid, want := range map[string]int{"ok": 1, "transient": 2, "permanent": 1, "resource": 1} {
		if calls[uid] != want {
			t.Errorf("%s calls=%d want %d", uid, calls[uid], want)
		}
	}
	if got := s.Ledger().Runs()["checkin"]; got.Trigger != triggerRetry || got.OK != 1 || got.Total != 1 {
		t.Fatalf("retry run: %+v", got)
	}
}

func TestCheckinPendingRetryKeepsTimeAndUpdatesTargets(t *testing.T) {
	stub := &checkinStub{checkinStatus: 503, checkinBody: "unavailable"}
	s, _ := newCheckinS(t, stub)
	s.cfg.RetryDelayMinutes, s.cfg.RetryMaxPerDay = 1, 2
	s.runCheckin(triggerSchedule)
	first := s.Ledger().RetryStates()["checkin"]
	if first.Next == nil {
		t.Skip("retry cannot cross CST day")
	}
	s.runCheckin(triggerSchedule)
	second := s.Ledger().RetryStates()["checkin"]
	if second.Next == nil || !second.Next.Equal(*first.Next) || second.Used != 1 {
		t.Fatalf("pending retry was replaced: before=%+v after=%+v", first, second)
	}
	stub.checkinStatus, stub.checkinBody = 0, `{"code":0,"data":{}}`
	s.runCheckin(triggerSchedule)
	s.checkinRetryMu.Lock()
	n := len(s.checkinRetry)
	s.checkinRetryMu.Unlock()
	if n != 0 {
		t.Fatalf("successful scheduled run left %d targets", n)
	}
}

func TestCheckinRetryRestartDoesNotRunWholePool(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unavailable", 503) }))
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "one", AccessToken: "one", RefreshToken: "rt", ExpiresAt: 9999999999})
	cfg := Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, LedgerFile: filepath.Join(t.TempDir(), "ledger.json"), RetryDelayMinutes: 1, RetryMaxPerDay: 1}
	s := New(cfg)
	s.runCheckin(triggerSchedule)
	st := s.Ledger().RetryStates()["checkin"]
	if st.Next == nil {
		t.Skip("retry cannot cross CST day")
	}
	before := calls.Load()
	restored := New(cfg)
	if !restored.takeRetryTrigger(taskCheckin, *st.Next) {
		t.Fatal("retry not restored")
	}
	restored.runCheckin(triggerRetry)
	if calls.Load() != before {
		t.Fatalf("restart caused %d extra requests", calls.Load()-before)
	}
}

func TestCheckinRetryWaitsForManualRun(t *testing.T) {
	for _, manualSuccess := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual_failure", true: "manual_success"}[manualSuccess], func(t *testing.T) {
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
					n := calls.Add(1)
					if n == 1 {
						close(entered)
						<-release
					}
					if n == 1 && !manualSuccess {
						http.Error(w, "unavailable", 503)
						return
					}
				}
				w.Write([]byte(`{"code":0,"data":{}}`))
			}))
			defer srv.Close()
			p := pool.New("")
			p.Add(&auth.Auth{UID: "u", AccessToken: "u", RefreshToken: "rt", ExpiresAt: 9999999999})
			s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})
			now := time.Now()
			decision := s.Ledger().PlanRetry("checkin", now.Add(-time.Second), time.Millisecond, 1)
			if decision.Reason != "" {
				t.Skipf("cannot arm retry near day boundary: %s", decision.Reason)
			}
			s.checkinRetry = map[string]struct{}{"u": {}}
			s.checkinRetryDay = travelDay(now)
			manualDone := make(chan struct{})
			go func() { defer close(manualDone); s.CheckinAll() }()
			<-entered
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.dispatch(ctx, taskCheckin)
			if st := s.Ledger().RetryStates()["checkin"]; st.Next == nil {
				t.Fatal("cancelled wait consumed retry")
			}
			done := make(chan struct{})
			go func() { defer close(done); s.dispatch(context.Background(), taskCheckin) }()
			close(release)
			<-manualDone
			<-done
			want := int32(2)
			if manualSuccess {
				want = 1
			}
			if got := calls.Load(); got != want {
				t.Fatalf("checkin calls=%d want %d", got, want)
			}
			if st := s.Ledger().RetryStates()["checkin"]; st.Next != nil {
				t.Fatal("retry not consumed after lock acquired")
			}
		})
	}
}

func TestCheckinManualSuccessRemovesPendingTarget(t *testing.T) {
	stub := &checkinStub{checkinStatus: 503, checkinBody: "unavailable"}
	s, _ := newCheckinS(t, stub)
	s.cfg.RetryDelayMinutes, s.cfg.RetryMaxPerDay = 1, 1
	s.runCheckin(triggerSchedule)
	stub.checkinStatus, stub.checkinBody = 0, `{"code":0,"data":{}}`
	if _, err := s.CheckinAll(); err != nil {
		t.Fatal(err)
	}
	s.checkinRetryMu.Lock()
	n := len(s.checkinRetry)
	s.checkinRetryMu.Unlock()
	if n != 0 {
		t.Fatalf("manual success left %d pending targets", n)
	}
	// Dispatching an empty target never repeats the manual success.
	if out, err := s.runCheckin(triggerRetry); err != nil || out != nil {
		t.Fatalf("out=%v err=%v", out, err)
	}
}
