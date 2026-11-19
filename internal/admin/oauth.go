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
	// targetUID 非空 = 这是「重新登录」流程：授权完成后只允许替换该 UID 的凭证。
	// 用户登错号（授权返回另一个 UID）时必须报错而不是顺手新建/覆盖别的账号。
	targetUID string
	// hold 重新登录期间把目标账号临时摘出选号池（manual_disabled）的现场；
	// 流程结束/取消/过期时恢复运维原来的手动停用状态。
	hold accountHold
	// reauth 是否已给目标账号打上 Pool 的「重登中」标记（调度器刷新 token 会跳过）。
	reauth bool
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
	if f.targetUID != "" {
		view["target_uid"] = f.targetUID
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

// releaseFlowHoldLocked 恢复重新登录期间临时摘除的账号状态。调用方必须已持有 h.mu。
// 本方法只调 pool 的公开原语（不回调 admin），锁序 h.mu → pool.mu 与账号操作一致。
func (h *Handler) releaseFlowHoldLocked(f *loginFlow) {
	if f == nil {
		return
	}
	if f.reauth {
		h.cfg.Pool.EndReauth(f.hold.uid)
		f.reauth = false
	}
	if f.hold.uid == "" {
		return
	}
	h.releaseAccount(f.hold)
	f.hold = accountHold{}
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
	h.startOAuthFlow(w, r, "")
}

// startRelogin 为已有账号开启重新登录（OAuth）流程：与「添加授权账号」同一条
// 设备授权链路，但流程绑定目标 UID，授权完成后只替换该账号的凭证。
//
// 关键差异（也是旧版"账号无法重新登录"的修复点）：
//   - 不再要求账号先被系统禁用：任何状态的已有账号都能重新登录；
//   - 流程开始即把账号临时摘出选号池，等在途请求自然结束，避免凭证对象替换时
//     仍被新请求选中；
//   - 完成后恢复运维原有的手动停用状态（重新登录只换凭证，不擅自改变运维意图）。
func (h *Handler) startRelogin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !safeUID.MatchString(uid) {
		fail(w, 404, "账号不存在")
		return
	}
	if h.cfg.Pool.AuthByUID(uid) == nil {
		fail(w, 404, "账号不存在")
		return
	}
	owner, _, _ := h.sessionFor(r)
	h.mu.Lock()
	busy := h.busy[uid]
	if !busy {
		// 跨会话也要互斥：同一个账号被两个 OAuth 流程同时重登，后完成的会覆盖先完成的
		// 凭证，用户会看到"刚登录好又被顶掉"。同一会话的重复点击交给 startOAuthFlow
		// 返回已有流程（幂等）。
		for _, f := range h.logins {
			if f.owner != owner && f.targetUID == uid && !f.completed {
				busy = true
				break
			}
		}
	}
	h.mu.Unlock()
	if busy {
		fail(w, 409, "该账号正在执行其他操作或重新登录，请稍后重试")
		return
	}
	h.startOAuthFlow(w, r, uid)
}

// startOAuthFlow 是添加账号（target==""）与重新登录（target=uid）共用的授权流程。
// 同一管理会话只允许一个未完成流程：添加/重登复用同一面板，避免两套轮询互相覆盖。
func (h *Handler) startOAuthFlow(w http.ResponseWriter, r *http.Request, target string) {
	owner, _, _ := h.sessionFor(r)
	h.mu.Lock()
	h.cleanupLocked()
	for k, f := range h.logins {
		if f.owner != owner {
			continue
		}
		if f.completed {
			h.releaseFlowHoldLocked(f)
			delete(h.logins, k)
			continue
		}
		if target != "" && f.targetUID != target {
			h.mu.Unlock()
			fail(w, 409, "已有其他授权流程正在进行，请先完成或取消当前授权")
			return
		}
		view := oauthFlowView(k, f)
		h.mu.Unlock()
		respond(w, 200, view)
		return
	}
	if len(h.logins) >= 128 {
		h.mu.Unlock()
		fail(w, 429, "授权流程过多，请稍后重试")
		return
	}
	id := randomID()
	f := &loginFlow{owner: owner, expires: time.Now().Add(5 * time.Minute), busy: true, targetUID: target}
	if target != "" {
		// 重新登录期间先摘出选号池：否则授权等待的几十秒里仍会有新请求落到这个
		// 账号上，凭证替换时它们成了"拿着旧指针的在途请求"。
		f.hold = h.holdAccount(target, "重新登录中")
		if f.hold.uid != "" {
			h.cfg.Pool.BeginReauth(target)
			f.reauth = true
		}
	}
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
				h.releaseFlowHoldLocked(f)
				delete(h.logins, id)
			}
			h.mu.Unlock()
			fail(w, 409, "授权流程已取消或过期")
			return
		}
		if h.logins[id] == f {
			h.releaseFlowHoldLocked(f)
			delete(h.logins, id)
		}
		h.mu.Unlock()
		fail(w, 502, err.Error())
		return
	}
	s, sessionExists := h.sessions[owner]
	if h.logins[id] != f || !f.expires.After(time.Now()) || !sessionExists || !s.expires.After(time.Now()) {
		if h.logins[id] == f {
			h.releaseFlowHoldLocked(f)
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
		uid, nick, target := f.uid, f.nickname, f.targetUID
		h.mu.Unlock()
		out := map[string]any{"status": "completed", "uid": uid, "nickname": nick}
		if target != "" {
			out["target_uid"], out["relogin"] = target, true
		}
		respond(w, 200, out)
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
	target := f.targetUID
	h.mu.Unlock()
	if !valid || !sessionExists {
		fail(w, 409, "授权流程已取消或过期")
		return
	}
	// 重新登录流程只允许操作目标账号：用户在官方页面登了另一个号时，绝不能
	// 顺手把那个号新建进池、更不能覆盖目标账号的凭证。
	if target != "" && acct.UID != target {
		h.mu.Lock()
		if h.logins[id] == f {
			h.releaseFlowHoldLocked(f)
			delete(h.logins, id)
		}
		h.mu.Unlock()
		fail(w, 409, fmt.Sprintf("授权的是账号 %s，与重新登录目标 %s 不一致，请重新发起授权", acct.UID, target))
		return
	}
	existing := h.cfg.Pool.AuthByUID(a.UID) != nil
	if existing {
		// 旧版这里直接报「账号已存在，请先停用」，而「临时停用」并不置系统禁用位，
		// 于是活跃账号永远无法重新登录。正确做法：临时摘出选号池→等在途请求排空
		// →替换凭证→恢复运维原有的手动停用状态。
		h.mu.Lock()
		if h.logins[id] != f {
			h.mu.Unlock()
			fail(w, 409, "授权流程已取消或过期")
			return
		}
		if f.targetUID == "" {
			f.targetUID = a.UID
		}
		if f.hold.uid != a.UID {
			h.releaseFlowHoldLocked(f)
			f.hold = h.holdAccount(a.UID, "重新登录中")
		}
		if f.hold.uid == a.UID && !f.reauth {
			h.cfg.Pool.BeginReauth(a.UID)
			f.reauth = true
		}
		h.mu.Unlock()
		if st, ok := h.cfg.Pool.Status(a.UID); ok && st.InFlight > 0 {
			// 保持 pending：前端继续轮询，在途请求结束后自动完成替换。
			respond(w, 202, map[string]any{
				"status":  "pending",
				"message": fmt.Sprintf("账号仍有 %d 个在途请求，正在等待结束后自动完成", st.InFlight),
			})
			return
		}
	}
	if err := h.saveAccount(a, true); err != nil {
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
	// 重新登录成功：恢复账号原来的手动停用状态（若操作前是手动停用的，保持停用）。
	h.releaseFlowHoldLocked(f)
	target = f.targetUID
	h.mu.Unlock()
	out := map[string]any{"status": "completed", "uid": a.UID, "nickname": a.Nickname}
	if target != "" {
		out["target_uid"], out["relogin"] = target, true
	}
	respond(w, 200, out)
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
	h.releaseFlowHoldLocked(f)
	h.mu.Unlock()
	respond(w, 200, map[string]bool{"ok": true})
}
