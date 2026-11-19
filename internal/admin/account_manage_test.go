// account_manage_test.go 控制台账号管理的三项便利能力契约：
//   - 重新登录（重新走 OAuth 替换已有账号凭证，不再要求先被系统禁用）；
//   - 排序（持久化的展示顺序，只影响列表，不改变选号权重）；
//   - 移除（删除凭证文件并从池中移除，有在途请求时必须拒绝）。
package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// oauthTestServer 模拟设备授权三端点。access/refresh/uid 可定制，便于验证重登
// 是否真的替换了凭证、以及登错号时是否拒绝覆盖。
func oauthTestServer(t *testing.T, access, refresh, uid, nickname string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			io.WriteString(w, `{"code":0,"data":{"state":"private-state","authUrl":"https://www.codebuddy.cn/login?state=private-state"}}`)
		case "/v2/plugin/auth/token":
			io.WriteString(w, `{"code":0,"data":{"accessToken":"`+access+`","refreshToken":"`+refresh+`","expiresIn":3600}}`)
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer "+access {
				t.Error("missing account auth")
			}
			io.WriteString(w, `{"code":0,"data":{"uid":"`+uid+`","nickname":"`+nickname+`"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func oauthFlowID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		ID        string `json:"id"`
		TargetUID string `json:"target_uid"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("invalid oauth response: %s", w.Body.String())
	}
	return out.ID
}

// TestReloginPreservesExplicitRealm 手工指定的 realm=global（domain 为空）不能
// 因为一次 OAuth 重登被回落推断成 cn——否则账号会被路由到错误的上游域。
func TestReloginPreservesExplicitRealm(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "new-access", "new-refresh", "u1", "U1")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	raw := `{"accessToken":"a","refreshToken":"r","uid":"u1","nickname":"U1","realm":"global","expiresAt":4102444800}`
	if w := adminRequest(h, "POST", "/admin/api/accounts/import", raw, s); w.Code != 201 {
		t.Fatalf("import=%d %s", w.Code, w.Body.String())
	}
	w := adminRequest(h, "POST", "/admin/api/accounts/u1/relogin", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("relogin start=%d %s", w.Code, w.Body.String())
	}
	flowID := oauthFlowID(t, w)
	if w := adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s); w.Code != 200 {
		t.Fatalf("poll=%d %s", w.Code, w.Body.String())
	}
	if got := h.cfg.Pool.AuthByUID("u1").Realm(); got != "global" {
		t.Fatalf("realm=%q want global（显式 realm 应在重登后保留）", got)
	}
}

// TestOAuthAddFlowReloginsExistingAccount 「添加授权账号」路径撞上已有 UID 时，
// 旧版会报「请先停用」（而临时停用不算系统禁用，活跃号永远登不回去）；现在应
// 自动按重新登录处理，直接替换凭证。
func TestOAuthAddFlowReloginsExistingAccount(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "new-access", "new-refresh", "u1", "U1")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	manualAccount(t, h, s, "u1")

	w := adminRequest(h, "POST", "/admin/api/oauth/start", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("start=%d %s", w.Code, w.Body.String())
	}
	flowID := oauthFlowID(t, w)
	w = adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s)
	if w.Code != 200 || !containsJSONField(w.Body.String(), "relogin", "true") {
		t.Fatalf("poll=%d %s", w.Code, w.Body.String())
	}
	if got := h.cfg.Pool.AuthByUID("u1").AccessTokenValue(); got != "new-access" {
		t.Fatalf("已有账号未被替换: %q", got)
	}
	if st, _ := h.cfg.Pool.Status("u1"); st.ManualDisabled || st.Disabled {
		t.Fatalf("重登后应可用: %+v", st)
	}
}

// TestReloginKeepsManualDisable 重新登录只换凭证，不擅自解除运维的停用手动位。
func TestReloginKeepsManualDisable(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "new-access", "new-refresh", "u1", "U1")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	manualAccount(t, h, s, "u1")
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{"reason":"观察几天"}`, s); w.Code != 200 {
		t.Fatalf("manual_disable=%d", w.Code)
	}
	w := adminRequest(h, "POST", "/admin/api/accounts/u1/relogin", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("relogin start=%d %s", w.Code, w.Body.String())
	}
	flowID := oauthFlowID(t, w)
	if w := adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s); w.Code != 200 {
		t.Fatalf("poll=%d %s", w.Code, w.Body.String())
	}
	st, _ := h.cfg.Pool.Status("u1")
	if !st.ManualDisabled || st.ManualReason != "观察几天" {
		t.Fatalf("重登后应保留手动停用原因，got %+v", st)
	}
	if h.cfg.Pool.AuthByUID("u1").AccessTokenValue() != "new-access" {
		t.Fatal("凭证未替换")
	}
	// 重登结束后账号级操作恢复可用（不会遗留“重登中”的阻塞状态）。
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_enable", `{}`, s); w.Code != 200 {
		t.Fatalf("manual_enable=%d", w.Code)
	}
}

// TestReloginBlocksAccountActions 重登过程中拒绝刷新积分等账号动作，避免与凭证替换互踩。
func TestReloginBlocksAccountActions(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "new-access", "new-refresh", "u1", "U1")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	manualAccount(t, h, s, "u1")
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/relogin", `{}`, s); w.Code != 201 {
		t.Fatalf("relogin start=%d %s", w.Code, w.Body.String())
	}
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/credits", `{}`, s); w.Code != 409 {
		t.Fatalf("重登中刷新积分应 409，got %d %s", w.Code, w.Body.String())
	}
}

// TestReloginReplacesActiveAccount 活跃账号（未禁用、未手动停用）也能重新登录：
// 流程先把账号临时摘出选号池，完成后替换凭证并恢复原状态。
func TestReloginReplacesActiveAccount(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "new-access", "new-refresh", "u1", "NewNick")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	manualAccount(t, h, s, "u1")
	old := h.cfg.Pool.AuthByUID("u1")

	w := adminRequest(h, "POST", "/admin/api/accounts/u1/relogin", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("relogin start=%d %s", w.Code, w.Body.String())
	}
	if !containsJSONField(w.Body.String(), "target_uid", "u1") {
		t.Fatalf("relogin response missing target_uid: %s", w.Body.String())
	}
	// 流程进行中账号被临时摘出选号池，避免凭证替换时仍有新请求落在旧对象上。
	if st, _ := h.cfg.Pool.Status("u1"); !st.ManualDisabled {
		t.Fatal("重新登录期间账号应临时摘出选号池")
	}
	flowID := oauthFlowID(t, w)
	w = adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s)
	if w.Code != 200 || !containsJSONField(w.Body.String(), "relogin", "true") {
		t.Fatalf("poll=%d %s", w.Code, w.Body.String())
	}
	st, _ := h.cfg.Pool.Status("u1")
	if st.ManualDisabled || st.Disabled {
		t.Fatalf("重登完成后应恢复原状态（可用），got %+v", st)
	}
	a := h.cfg.Pool.AuthByUID("u1")
	if a == nil || a.AccessTokenValue() != "new-access" || a.RefreshTokenValue() != "new-refresh" {
		t.Fatalf("凭证未替换: %+v", a)
	}
	if a.Nickname != "NewNick" {
		t.Errorf("nickname=%q want NewNick", a.Nickname)
	}
	// 旧凭证对象必须退役：调度器手里的旧指针若还能写回，会把新凭证覆盖回旧值。
	if err := old.SaveAtomic(); err == nil {
		t.Fatal("退役的旧凭证仍能写回")
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, "workbuddy-u1.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := auth.Parse(raw)
	if err != nil || saved.AccessToken != "new-access" {
		t.Fatalf("新凭证未落盘: %s", raw)
	}
}

// TestReloginWaitsForInFlight 有在途请求时保持 pending，排空后自动完成替换。
func TestReloginWaitsForInFlight(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "new-access", "new-refresh", "u1", "U1")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	manualAccount(t, h, s, "u1")
	if !h.cfg.Pool.Acquire("u1") {
		t.Fatal("acquire")
	}
	defer h.cfg.Pool.Release("u1")

	w := adminRequest(h, "POST", "/admin/api/accounts/u1/relogin", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("relogin start=%d %s", w.Code, w.Body.String())
	}
	flowID := oauthFlowID(t, w)
	w = adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s)
	if w.Code != 202 || !containsJSONField(w.Body.String(), "status", "pending") {
		t.Fatalf("有在途请求时应 pending: %d %s", w.Code, w.Body.String())
	}
	if a := h.cfg.Pool.AuthByUID("u1"); a.AccessTokenValue() != "a" {
		t.Fatal("在途请求未结束时不应替换凭证")
	}
	h.cfg.Pool.Release("u1")
	// 服务端 3 秒轮询节流：测试直接回拨，避免依赖真实等待。
	h.mu.Lock()
	h.logins[flowID].lastPoll = time.Now().Add(-4 * time.Second)
	h.mu.Unlock()
	w = adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s)
	if w.Code != 200 {
		t.Fatalf("排空后 poll=%d %s", w.Code, w.Body.String())
	}
	if a := h.cfg.Pool.AuthByUID("u1"); a.AccessTokenValue() != "new-access" {
		t.Fatal("排空后应替换凭证")
	}
	if st, _ := h.cfg.Pool.Status("u1"); st.ManualDisabled {
		t.Fatal("完成后应恢复手动停用状态")
	}
}

// TestReloginRejectsDifferentAccount 用户登错号：不得新建/覆盖其他账号，原账号保持原样。
func TestReloginRejectsDifferentAccount(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	up := oauthTestServer(t, "other-access", "other-refresh", "other", "Other")
	defer up.Close()
	h.cfg.Upstream.ChatBaseCN = up.URL
	manualAccount(t, h, s, "u1")

	w := adminRequest(h, "POST", "/admin/api/accounts/u1/relogin", `{}`, s)
	if w.Code != 201 {
		t.Fatalf("relogin start=%d %s", w.Code, w.Body.String())
	}
	flowID := oauthFlowID(t, w)
	w = adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s)
	if w.Code != 409 {
		t.Fatalf("登错号应 409，got %d %s", w.Code, w.Body.String())
	}
	if h.cfg.Pool.AuthByUID("other") != nil {
		t.Fatal("不得把授权返回的其他账号新建进池")
	}
	if a := h.cfg.Pool.AuthByUID("u1"); a.AccessTokenValue() != "a" {
		t.Fatal("目标账号凭证不应被覆盖")
	}
	// 流程已被终止，临时摘除也要恢复。
	if st, _ := h.cfg.Pool.Status("u1"); st.ManualDisabled {
		t.Fatal("流程终止后应恢复手动停用状态")
	}
	if w := adminRequest(h, "POST", "/admin/api/oauth/"+flowID+"/poll", `{}`, s); w.Code != 404 {
		t.Fatalf("失败流程应被删除，got %d", w.Code)
	}
}

// TestRemoveAccountDeletesCredentialAndEntry 移除：删文件 + 出池，未知账号 404。
func TestRemoveAccountDeletesCredentialAndEntry(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")
	manualAccount(t, h, s, "u2")

	w := adminRequest(h, "DELETE", "/admin/api/accounts/u1", `{}`, s)
	if w.Code != 200 {
		t.Fatalf("remove=%d %s", w.Code, w.Body.String())
	}
	if h.cfg.Pool.AuthByUID("u1") != nil {
		t.Fatal("移除后账号仍在池里")
	}
	if _, err := os.Stat(filepath.Join(h.cfg.AuthDir, "workbuddy-u1.json")); !os.IsNotExist(err) {
		t.Fatalf("凭证文件未删除: %v", err)
	}
	if h.cfg.Pool.AuthByUID("u2") == nil {
		t.Fatal("误删其他账号")
	}
	if w := adminRequest(h, "DELETE", "/admin/api/accounts/u1", `{}`, s); w.Code != 404 {
		t.Fatalf("重复移除应 404，got %d", w.Code)
	}
}

// TestRemoveAccountRefusesInFlight 有在途请求时拒绝移除，且不改动账号状态。
func TestRemoveAccountRefusesInFlight(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")
	if !h.cfg.Pool.Acquire("u1") {
		t.Fatal("acquire")
	}
	defer h.cfg.Pool.Release("u1")
	if w := adminRequest(h, "DELETE", "/admin/api/accounts/u1", `{}`, s); w.Code != 409 {
		t.Fatalf("有在途请求应 409，got %d %s", w.Code, w.Body.String())
	}
	if h.cfg.Pool.AuthByUID("u1") == nil {
		t.Fatal("拒绝移除时账号不应消失")
	}
	if st, _ := h.cfg.Pool.Status("u1"); st.ManualDisabled {
		t.Fatal("拒绝移除时应恢复临时摘除")
	}
}

// TestReorderAccountsPersists 排序影响 status 列表顺序并落盘；未知 UID 被拒绝。
func TestReorderAccountsPersists(t *testing.T) {
	stateFile := t.TempDir() + "/state.json"
	h := New(Config{Password: "test-only-password", AuthDir: t.TempDir(), Pool: pool.New(stateFile), Upstream: upstream.New()})
	s := loginAs(t, h)
	for _, uid := range []string{"u3", "u1", "u2"} {
		manualAccount(t, h, s, uid)
	}
	// 默认按 UID 升序（u1,u2,u3）。
	if got := statusUIDs(t, h, s); got != "u1,u2,u3" {
		t.Fatalf("默认顺序=%s", got)
	}
	w := adminRequest(h, "PUT", "/admin/api/accounts/order", `{"uids":["u3","u1","u2"]}`, s)
	if w.Code != 200 {
		t.Fatalf("reorder=%d %s", w.Code, w.Body.String())
	}
	if got := statusUIDs(t, h, s); got != "u3,u1,u2" {
		t.Fatalf("排序后=%s", got)
	}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		AccountOrder []string `json:"account_order"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.AccountOrder) != 3 || state.AccountOrder[0] != "u3" {
		t.Fatalf("account_order 未落盘: %v", state.AccountOrder)
	}
	if w := adminRequest(h, "PUT", "/admin/api/accounts/order", `{"uids":["u3","nope"]}`, s); w.Code != 400 {
		t.Fatalf("未知 uid 应 400，got %d", w.Code)
	}
}

func statusUIDs(t *testing.T, h *Handler, s *loginSession) string {
	t.Helper()
	w := adminRequest(h, "GET", "/admin/api/status", "", s)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Accounts []struct {
			UID string `json:"uid"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	out := ""
	for i, a := range body.Accounts {
		if i > 0 {
			out += ","
		}
		out += a.UID
	}
	return out
}

func containsJSONField(body, key, value string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	if b, ok := v.(bool); ok {
		return (value == "true" && b) || (value == "false" && !b)
	}
	return v == value
}
