package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func oauthFlowFromResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var flow map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &flow); err != nil {
		t.Fatalf("invalid oauth response: %v", err)
	}
	return flow
}

func TestOAuthCurrentRecoveryIsolationAndIdempotentStart(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	other := loginAs(t, h)
	var stateCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/state" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		stateCalls.Add(1)
		io.WriteString(w, `{"code":0,"data":{"state":"private-state","authUrl":"https://www.codebuddy.cn/login?state=private-state"}}`)
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL

	started := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if started.Code != 201 {
		t.Fatalf("start=%d %s", started.Code, started.Body.String())
	}
	first := oauthFlowFromResponse(t, started)
	id, ok := first["id"].(string)
	if !ok || id == "" || first["status"] != "pending" {
		t.Fatalf("unexpected start flow %#v", first)
	}
	if first["auth_url"] == nil || first["poll_interval_seconds"] != float64(3) {
		t.Fatalf("missing pending fields %#v", first)
	}
	if strings.Contains(started.Body.String(), `"state"`) || strings.Contains(started.Body.String(), "oauth-access") {
		t.Fatal("upstream state or token leaked as a response field")
	}

	current := adminRequest(h, "GET", "/admin/api/oauth/current", ``, s)
	if current.Code != 200 {
		t.Fatalf("current=%d %s", current.Code, current.Body.String())
	}
	var recovered struct {
		Flow map[string]any `json:"flow"`
	}
	if err := json.Unmarshal(current.Body.Bytes(), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Flow["id"] != id || recovered.Flow["status"] != "pending" {
		t.Fatalf("flow was not recovered: %#v", recovered.Flow)
	}
	if recovered.Flow["auth_url"] == nil {
		t.Fatal("recovered pending flow lost auth_url")
	}

	otherCurrent := adminRequest(h, "GET", "/admin/api/oauth/current", ``, other)
	if otherCurrent.Code != 200 || strings.TrimSpace(otherCurrent.Body.String()) != `{"flow":null}` {
		t.Fatalf("other session saw flow: %d %s", otherCurrent.Code, otherCurrent.Body.String())
	}
	duplicate := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if duplicate.Code != 200 {
		t.Fatalf("duplicate start=%d %s", duplicate.Code, duplicate.Body.String())
	}
	duplicateFlow := oauthFlowFromResponse(t, duplicate)
	if duplicateFlow["id"] != id || duplicateFlow["status"] != "pending" {
		t.Fatalf("duplicate start changed flow: %#v", duplicateFlow)
	}
	if stateCalls.Load() != 1 {
		t.Fatalf("duplicate start called upstream %d times", stateCalls.Load())
	}
}

func TestOAuthPendingBusinessCodeWithoutData(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	var tokenCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			io.WriteString(w, `{"code":0,"data":{"state":"pending-state","authUrl":"https://www.codebuddy.cn/login?state=pending-state"}}`)
		case "/v2/plugin/auth/token":
			if tokenCalls.Add(1) == 1 {
				io.WriteString(w, `{"code":1001,"msg":"login ing"}`)
			} else {
				io.WriteString(w, `{"code":0}`)
			}
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL

	started := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if started.Code != 201 {
		t.Fatalf("start=%d %s", started.Code, started.Body.String())
	}
	id := oauthFlowFromResponse(t, started)["id"].(string)
	poll := adminRequest(h, "POST", "/admin/api/oauth/"+id+"/poll", `{}`, s)
	if poll.Code != 202 || strings.TrimSpace(poll.Body.String()) != `{"status":"pending"}` {
		t.Fatalf("business pending=%d %s", poll.Code, poll.Body.String())
	}

	h.mu.Lock()
	h.logins[id].lastPoll = time.Time{}
	h.mu.Unlock()
	poll = adminRequest(h, "POST", "/admin/api/oauth/"+id+"/poll", `{}`, s)
	if poll.Code != 502 {
		t.Fatalf("code=0 without data=%d %s", poll.Code, poll.Body.String())
	}
}

func TestOAuthCurrentStartingAndCancelRace(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	stateStarted := make(chan struct{})
	releaseState := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/state" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		once.Do(func() { close(stateStarted) })
		<-releaseState
		io.WriteString(w, `{"code":0,"data":{"state":"late-state","authUrl":"https://www.codebuddy.cn/login?state=late-state"}}`)
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL

	startDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		startDone <- adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	}()
	<-stateStarted

	current := adminRequest(h, "GET", "/admin/api/oauth/current", ``, s)
	if current.Code != 200 || !strings.Contains(current.Body.String(), `"status":"starting"`) {
		t.Fatalf("starting flow not visible: %d %s", current.Code, current.Body.String())
	}
	var starting struct {
		Flow map[string]any `json:"flow"`
	}
	if err := json.Unmarshal(current.Body.Bytes(), &starting); err != nil {
		t.Fatal(err)
	}
	if _, hasAuthURL := starting.Flow["auth_url"]; hasAuthURL {
		t.Fatal("starting flow exposed auth_url before upstream completion")
	}

	cancel := adminRequest(h, "DELETE", "/admin/api/oauth/"+starting.Flow["id"].(string), `{}`, s)
	if cancel.Code != 200 {
		t.Fatalf("cancel starting=%d %s", cancel.Code, cancel.Body.String())
	}
	close(releaseState)
	if start := <-startDone; start.Code != 409 {
		t.Fatalf("cancelled start=%d %s", start.Code, start.Body.String())
	}
	current = adminRequest(h, "GET", "/admin/api/oauth/current", ``, s)
	if current.Code != 200 || strings.TrimSpace(current.Body.String()) != `{"flow":null}` {
		t.Fatalf("cancelled flow survived: %d %s", current.Code, current.Body.String())
	}
}

func TestOAuthCurrentCompletedExpiryAndNextStart(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	var stateCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			stateCalls.Add(1)
			io.WriteString(w, `{"code":0,"data":{"state":"complete-state","authUrl":"https://www.codebuddy.cn/login?state=complete-state"}}`)
		case "/v2/plugin/auth/token":
			io.WriteString(w, `{"code":0,"data":{"accessToken":"oauth-access","refreshToken":"oauth-refresh","expiresIn":3600}}`)
		case "/v2/plugin/login/account":
			io.WriteString(w, `{"code":0,"data":{"uid":"recovery-user","nickname":"Recovered"}}`)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL

	started := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if started.Code != 201 {
		t.Fatalf("start=%d %s", started.Code, started.Body.String())
	}
	flow := oauthFlowFromResponse(t, started)
	id := flow["id"].(string)
	poll := adminRequest(h, "POST", "/admin/api/oauth/"+id+"/poll", `{}`, s)
	if poll.Code != 200 || !strings.Contains(poll.Body.String(), `"status":"completed"`) {
		t.Fatalf("poll=%d %s", poll.Code, poll.Body.String())
	}
	current := adminRequest(h, "GET", "/admin/api/oauth/current", ``, s)
	if current.Code != 200 || !strings.Contains(current.Body.String(), `"status":"completed"`) || !strings.Contains(current.Body.String(), `"uid":"recovery-user"`) || !strings.Contains(current.Body.String(), `"nickname":"Recovered"`) {
		t.Fatalf("completed flow not recoverable: %d %s", current.Code, current.Body.String())
	}
	if strings.Contains(current.Body.String(), "oauth-access") || strings.Contains(current.Body.String(), "oauth-refresh") || strings.Contains(current.Body.String(), "complete-state") {
		t.Fatal("completed current response leaked credentials or upstream state")
	}

	next := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if next.Code != 201 || stateCalls.Load() != 2 {
		t.Fatalf("completed flow did not allow next start: %d calls=%d %s", next.Code, stateCalls.Load(), next.Body.String())
	}
	nextFlow := oauthFlowFromResponse(t, next)
	if nextFlow["id"] == id || nextFlow["status"] != "pending" {
		t.Fatalf("next flow invalid: %#v", nextFlow)
	}

	h.mu.Lock()
	for _, f := range h.logins {
		if f.owner == s.cookie.Value {
			f.expires = time.Now().Add(-time.Second)
		}
	}
	h.mu.Unlock()
	current = adminRequest(h, "GET", "/admin/api/oauth/current", ``, s)
	if current.Code != 200 || strings.TrimSpace(current.Body.String()) != `{"flow":null}` {
		t.Fatalf("expired flow was not cleaned: %d %s", current.Code, current.Body.String())
	}
}

func TestOAuthLogoutDuringStartDoesNotResurrectFlow(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	stateStarted := make(chan struct{})
	releaseState := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(stateStarted)
		<-releaseState
		io.WriteString(w, `{"code":0,"data":{"state":"logout-state","authUrl":"https://www.codebuddy.cn/login?state=logout-state"}}`)
	}))
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	startDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		startDone <- adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	}()
	<-stateStarted
	logout := adminRequest(h, "POST", "/admin/api/logout", `{}`, s)
	if logout.Code != 200 {
		t.Fatalf("logout=%d %s", logout.Code, logout.Body.String())
	}
	close(releaseState)
	if start := <-startDone; start.Code != 409 {
		t.Fatalf("logout start=%d %s", start.Code, start.Body.String())
	}
	if current := adminRequest(h, "GET", "/admin/api/oauth/current", ``, s); current.Code != 401 {
		t.Fatalf("logged out session current=%d %s", current.Code, current.Body.String())
	}
}
