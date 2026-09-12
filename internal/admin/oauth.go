package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

type loginFlow struct {
	owner, state, authURL string
	expires, lastPoll     time.Time
	client                *http.Client
	busy                  bool
	completed             bool
	uid, nickname         string
}

const oauthPollIntervalSeconds = 3

// oauthFlowView is the only representation of a flow sent to the browser.
// In particular, the upstream state and the access/refresh tokens never cross
// this boundary. auth_url intentionally contains the state because the
// upstream authorization page needs it to correlate the login.
func oauthFlowView(id string, f *loginFlow) map[string]any {
	status := "starting"
	if f.completed {
		status = "completed"
	} else if f.authURL != "" {
		status = "pending"
	}
	view := map[string]any{
		"id":                    id,
		"status":                status,
		"expires_at":            f.expires,
		"poll_interval_seconds": oauthPollIntervalSeconds,
	}
	if f.authURL != "" && !f.completed {
		view["auth_url"] = f.authURL
	}
	if f.completed {
		view["uid"] = f.uid
		view["nickname"] = f.nickname
	}
	return view
}

// OAuth uses the same device authorization endpoints as cmd/login. Credentials
// and upstream state stay on the server; each browser session owns its flow.
func (h *Handler) oauthJSON(r *http.Request, client *http.Client, method, path, token string) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, strings.TrimRight(h.cfg.Upstream.ChatBaseCN, "/")+path, bytes.NewBufferString("{}"))
	if err != nil {
		return nil, 0, fmt.Errorf("无法创建授权请求")
	}
	h.cfg.Upstream.CommonHeaders(req, &auth.Auth{AccessToken: token})
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接授权服务，请稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("授权服务返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("读取授权响应失败")
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, 0, fmt.Errorf("授权服务返回无效响应")
	}
	if envelope.Code == nil {
		return nil, 0, fmt.Errorf("授权服务返回无效响应")
	}
	// The token endpoint uses a non-zero business code while the user has not
	// finished authorization and may omit data entirely. Preserve that code so
	// pollOAuth can report pending. A successful envelope still needs data;
	// otherwise a malformed response must not be treated as a pending login.
	if *envelope.Code == 0 {
		data := bytes.TrimSpace(envelope.Data)
		if len(data) == 0 || bytes.Equal(data, []byte("null")) {
			return nil, 0, fmt.Errorf("授权服务返回无效响应")
		}
	}
	return envelope.Data, *envelope.Code, nil
}

func validAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "codebuddy.cn" || strings.HasSuffix(host, ".codebuddy.cn") || host == "tencent.com" || strings.HasSuffix(host, ".tencent.com") || host == "qq.com" || strings.HasSuffix(host, ".qq.com")
}

func (h *Handler) startOAuth(w http.ResponseWriter, r *http.Request) {
	owner, _, _ := h.sessionFor(r)
	h.mu.Lock()
	h.cleanupLocked()
	for k, f := range h.logins {
		if f.owner == owner {
			if f.completed {
				delete(h.logins, k)
				continue
			}
			view := oauthFlowView(k, f)
			h.mu.Unlock()
			respond(w, 200, view)
			return
		}
	}
	if len(h.logins) >= 128 {
		h.mu.Unlock()
		fail(w, 429, "授权流程过多，请稍后重试")
		return
	}
	id := randomID()
	f := &loginFlow{owner: owner, expires: time.Now().Add(5 * time.Minute), busy: true}
	h.logins[id] = f
	h.mu.Unlock()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar, Transport: h.cfg.Upstream.HTTP.Transport, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	// The flow is server-owned. Let the upstream state request finish if the
	// browser refreshes or closes the original start request, so current can
	// recover the flow afterwards.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	startRequest := r.Clone(startCtx)
	data, code, err := h.oauthJSON(startRequest, client, "POST", "/v2/plugin/auth/state?platform=CLI", "")
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err == nil && (code != 0 || json.Unmarshal(data, &st) != nil || st.State == "" || !validAuthURL(st.AuthURL)) {
		err = fmt.Errorf("授权服务未返回有效授权链接")
	}
	h.mu.Lock()
	if err != nil {
		s, sessionExists := h.sessions[owner]
		if h.logins[id] != f || !f.expires.After(time.Now()) || !sessionExists || !s.expires.After(time.Now()) {
			if h.logins[id] == f {
				delete(h.logins, id)
			}
			h.mu.Unlock()
			fail(w, 409, "授权流程已取消或过期")
			return
		}
		if h.logins[id] == f {
			delete(h.logins, id)
		}
		h.mu.Unlock()
		fail(w, 502, err.Error())
		return
	}
	s, sessionExists := h.sessions[owner]
	if h.logins[id] != f || !f.expires.After(time.Now()) || !sessionExists || !s.expires.After(time.Now()) {
		if h.logins[id] == f {
			delete(h.logins, id)
		}
		h.mu.Unlock()
		fail(w, 409, "授权流程已取消或过期")
		return
	}
	f.state = st.State
	f.authURL = st.AuthURL
	f.client = client
	f.busy = false
	view := oauthFlowView(id, f)
	h.mu.Unlock()
	respond(w, 201, view)
}

func (h *Handler) currentOAuth(w http.ResponseWriter, r *http.Request) {
	owner, _, _ := h.sessionFor(r)
	h.mu.Lock()
	h.cleanupLocked()
	var view map[string]any
	for id, f := range h.logins {
		if f.owner == owner {
			view = oauthFlowView(id, f)
			break
		}
	}
	h.mu.Unlock()
	respond(w, 200, map[string]any{"flow": view})
}

func (h *Handler) pollOAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner, _, _ := h.sessionFor(r)
	h.mu.Lock()
	h.cleanupLocked()
	f, ok := h.logins[id]
	if !ok || f.owner != owner {
		h.mu.Unlock()
		fail(w, 404, "授权流程不存在或已过期，请重新开始")
		return
	}
	if f.completed {
		uid, nick := f.uid, f.nickname
		h.mu.Unlock()
		respond(w, 200, map[string]any{"status": "completed", "uid": uid, "nickname": nick})
		return
	}
	if f.busy || time.Since(f.lastPoll) < 3*time.Second {
		h.mu.Unlock()
		w.Header().Set("Retry-After", "3")
		respond(w, 202, map[string]string{"status": "pending"})
		return
	}
	f.busy = true
	f.lastPoll = time.Now()
	client := f.client
	state := f.state
	h.mu.Unlock()
	defer func() { h.mu.Lock(); f.busy = false; h.mu.Unlock() }()
	data, code, err := h.oauthJSON(r, client, "GET", "/v2/plugin/auth/token?state="+url.QueryEscape(state), "")
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if code != 0 || json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" {
		respond(w, 202, map[string]string{"status": "pending"})
		return
	}
	data, code, err = h.oauthJSON(r, client, "GET", "/v2/plugin/login/account?state="+url.QueryEscape(state), tok.AccessToken)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if code != 0 || json.Unmarshal(data, &acct) != nil || !safeUID.MatchString(acct.UID) {
		fail(w, 502, "无法读取授权账号信息，请重新授权")
		return
	}
	a := &auth.Auth{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, Domain: tok.Domain, UID: acct.UID, EnterpriseID: acct.EnterpriseID, Nickname: acct.Nickname}
	if tok.ExpiresIn > 0 && tok.ExpiresIn < 365*24*3600 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	// Logout/cancel/expiry during the network request must prevent account creation.
	h.mu.Lock()
	valid := h.logins[id] == f && f.expires.After(time.Now())
	s, sessionExists := h.sessions[owner]
	sessionExists = sessionExists && s.expires.After(time.Now())
	h.mu.Unlock()
	if !valid || !sessionExists {
		fail(w, 409, "授权流程已取消或过期")
		return
	}
	if err := h.saveAccount(a); err != nil {
		fail(w, 409, err.Error())
		return
	}
	h.mu.Lock()
	f.completed = true
	f.state = ""
	f.authURL = ""
	f.client = nil
	f.uid = a.UID
	f.nickname = a.Nickname
	h.mu.Unlock()
	respond(w, 200, map[string]any{"status": "completed", "uid": a.UID, "nickname": a.Nickname})
}

func (h *Handler) cancelOAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner, _, _ := h.sessionFor(r)
	h.mu.Lock()
	f, ok := h.logins[id]
	if !ok || f.owner != owner {
		h.mu.Unlock()
		fail(w, 404, "授权流程不存在")
		return
	}
	// Starting the authorization URL request is cancellable. The completion
	// path checks the map identity before publishing the upstream state, so a
	// late response cannot resurrect a cancelled flow. Keep the existing guard
	// for an in-flight poll, whose result may be committing an account.
	if f.busy && f.authURL != "" {
		h.mu.Unlock()
		fail(w, 409, "授权状态正在更新，请稍后取消")
		return
	}
	delete(h.logins, id)
	h.mu.Unlock()
	respond(w, 200, map[string]bool{"ok": true})
}
