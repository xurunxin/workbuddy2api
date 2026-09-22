// account_manual_test.go 控制台「临时停用 / 恢复 / 复活」动作的契约测试。
//
// 这三个动作是上游运维端点（issue #138/#118，/admin/accounts/{uid}/{disable,enable,
// revive}）在本 fork 的等价实现——上游端点用 api_key 鉴权且挂在 /admin/ 前缀下，
// 而该前缀整体归控制台（控制台路由更具体），故上游端点在本进程内不可达，功能由
// 控制台以会话鉴权提供。这里锁住的是**语义**而非实现：手动位与自动位正交，
// 各自只能被自己的动作清除。
package admin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// manualAccount 造一个已导入的账号（走真实导入路径，确保池里有凭证）。
func manualAccount(t *testing.T, h *Handler, s *loginSession, uid string) {
	t.Helper()
	raw := `{"accessToken":"a","refreshToken":"r","uid":"` + uid + `","nickname":"` + uid + `","expiresAt":4102444800}`
	if w := adminRequest(h, "POST", "/admin/api/accounts/import", raw, s); w.Code != 201 {
		t.Fatalf("import %s: %d %s", uid, w.Code, w.Body.String())
	}
}

// accountStatus 读控制台 status 里该账号的原始 JSON（避免测试跟着服务端结构体改）。
func accountStatus(t *testing.T, h *Handler, s *loginSession, uid string) map[string]any {
	t.Helper()
	w := adminRequest(h, "GET", "/admin/api/status", "", s)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	for _, a := range body.Accounts {
		if a["uid"] == uid {
			return a
		}
	}
	t.Fatalf("account %s missing from status", uid)
	return nil
}

// TestManualDisableRoundTrip 临时停用 → 不参与选号但仍在池里 → 恢复 → 回池。
func TestManualDisableRoundTrip(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")

	if got := h.cfg.Pool.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: 账号应可选, got %+v", got)
	}

	w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{"reason":"观察几天"}`, s)
	if w.Code != 200 {
		t.Fatalf("manual_disable=%d %s", w.Code, w.Body.String())
	}
	a := accountStatus(t, h, s, "u1")
	if a["manual_disabled"] != true {
		t.Errorf("manual_disabled=%v want true", a["manual_disabled"])
	}
	if a["manual_reason"] != "观察几天" {
		t.Errorf("manual_reason=%v", a["manual_reason"])
	}
	// 关键语义：手动停用**不**置自动禁用位——否则「恢复」会解不掉，运维会困惑。
	if a["disabled"] == true {
		t.Error("manual_disable 不应置自动禁用位")
	}
	// 摘出选号，但仍留在池里（凭证与积分照常可读）。
	if got := h.cfg.Pool.Pick(""); got != nil {
		t.Fatalf("停用后仍被选中: %+v", got)
	}
	if h.cfg.Pool.AuthByUID("u1") == nil {
		t.Fatal("停用后账号应仍在池里（签到/保活照常）")
	}

	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_enable", `{}`, s); w.Code != 200 {
		t.Fatalf("manual_enable=%d %s", w.Code, w.Body.String())
	}
	a = accountStatus(t, h, s, "u1")
	if a["manual_disabled"] == true {
		t.Error("manual_enable 后 manual_disabled 应为 false")
	}
	if got := h.cfg.Pool.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("恢复后应可选, got %+v", got)
	}
}

// TestManualDisableReasonFallback 无体/无 reason 是最常见调用形态（脚本），必须可用。
func TestManualDisableReasonFallback(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")

	for _, tc := range []struct{ name, body, want string }{
		{"空体", `{}`, "控制台临时停用"},
		{"空 reason", `{"reason":"   "}`, "控制台临时停用"},
		{"非 JSON", `not-json`, "控制台临时停用"},
		{"正常", `{"reason":"  维护窗口  "}`, "维护窗口"},
	} {
		if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", tc.body, s); w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		if got := accountStatus(t, h, s, "u1")["manual_reason"]; got != tc.want {
			t.Errorf("%s: manual_reason=%v want %q", tc.name, got, tc.want)
		}
	}
}

// TestManualDisableReasonLengthCapped 原因文本超长要截断，不能撑爆状态展示。
func TestManualDisableReasonLengthCapped(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")

	long := strings.Repeat("很", 500)
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{"reason":"`+long+`"}`, s); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	got, _ := accountStatus(t, h, s, "u1")["manual_reason"].(string)
	if n := len([]rune(got)); n != 120 {
		t.Fatalf("manual_reason 长度=%d want 120", n)
	}
}

// TestManualAndAutoDisableAreOrthogonal 两个位正交：各自只能被自己的动作清除。
func TestManualAndAutoDisableAreOrthogonal(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")

	// 造出系统自动禁用（连续 session dead 达阈值）。
	for i := 0; i < 3; i++ {
		h.cfg.Pool.NoteSessionDead("u1")
	}
	if st, _ := h.cfg.Pool.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已自动禁用")
	}

	// 手动停用叠加：两位同时为真，状态里两个字段都要透出（面板据此分别提示）。
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{"reason":"叠加"}`, s); w.Code != 200 {
		t.Fatalf("manual_disable=%d", w.Code)
	}
	a := accountStatus(t, h, s, "u1")
	if a["manual_disabled"] != true || a["disabled"] != true {
		t.Fatalf("叠加态应为两位皆真: %+v", a)
	}

	// manual_enable 只解手动位：自动位仍在，账号仍不可选。
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_enable", `{}`, s); w.Code != 200 {
		t.Fatalf("manual_enable=%d", w.Code)
	}
	a = accountStatus(t, h, s, "u1")
	if a["manual_disabled"] == true {
		t.Error("manual_enable 应清手动位")
	}
	if a["disabled"] != true {
		t.Error("manual_enable 不应清自动位")
	}
	if got := h.cfg.Pool.Pick(""); got != nil {
		t.Fatalf("自动位仍在，不应可选, got %+v", got)
	}

	// revive 只解自动位 → 两位都空，账号回池。
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/revive", `{}`, s); w.Code != 200 {
		t.Fatalf("revive=%d", w.Code)
	}
	a = accountStatus(t, h, s, "u1")
	if a["disabled"] == true || a["manual_disabled"] == true {
		t.Fatalf("revive 后两位都应为假: %+v", a)
	}
	if got := h.cfg.Pool.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("两位皆清后应回池, got %+v", got)
	}

	// 反过来：revive 不解除手动停用（运维意图不能被一次 revive 悄悄撤销）。
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{"reason":"运维摘除"}`, s); w.Code != 200 {
		t.Fatalf("manual_disable=%d", w.Code)
	}
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/revive", `{}`, s); w.Code != 200 {
		t.Fatalf("revive=%d", w.Code)
	}
	if a := accountStatus(t, h, s, "u1"); a["manual_disabled"] != true {
		t.Error("revive 不应解除手动停用")
	}
	if got := h.cfg.Pool.Pick(""); got != nil {
		t.Fatalf("手动位仍在，不应可选, got %+v", got)
	}
}

// TestManualDisableUnknownUID 未知 uid → 404（三个动作一致），且不得静默成功。
func TestManualDisableUnknownUID(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	for _, action := range []string{"manual_disable", "manual_enable", "revive"} {
		w := adminRequest(h, "POST", "/admin/api/accounts/nope/"+action, `{}`, s)
		if w.Code != 404 {
			t.Errorf("%s unknown uid: code=%d want 404", action, w.Code)
		}
	}
}

// TestManualDisableRequiresSession 未登录不得操作（会话鉴权而非 api_key）。
func TestManualDisableRequiresSession(t *testing.T) {
	h := testAdmin(t)
	s := loginAs(t, h)
	manualAccount(t, h, s, "u1")
	w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{}`, nil)
	if w.Code != 401 {
		t.Fatalf("code=%d want 401 without session", w.Code)
	}
	if st, _ := h.cfg.Pool.Status("u1"); st.ManualDisabled {
		t.Fatal("未鉴权请求不应改变状态")
	}
}

// TestManualDisablePersistsOnFlush 停用意图要落盘（上游设计要点：重启保留运维意图）。
// 直接断言 state.json 的内容——不依赖重启，避免测试受 flusher goroutine 时序影响。
func TestManualDisablePersistsOnFlush(t *testing.T) {
	stateFile := t.TempDir() + "/state.json"
	h := New(Config{Password: "test-only-password", AuthDir: t.TempDir(), Pool: pool.New(stateFile), Upstream: upstream.New()})
	s := loginAs(t, h)
	if w := adminRequest(h, "POST", "/admin/api/accounts/import",
		`{"accessToken":"a","refreshToken":"r","uid":"u1","expiresAt":4102444800}`, s); w.Code != 201 {
		t.Fatalf("import %d %s", w.Code, w.Body.String())
	}
	if w := adminRequest(h, "POST", "/admin/api/accounts/u1/manual_disable", `{"reason":"持久化"}`, s); w.Code != 200 {
		t.Fatalf("manual_disable %d", w.Code)
	}
	h.cfg.Pool.Flush()

	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var state struct {
		Accounts map[string]struct {
			ManualDisabled bool   `json:"manual_disabled"`
			ManualReason   string `json:"manual_reason"`
			Disabled       bool   `json:"disabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	acc, ok := state.Accounts["u1"]
	if !ok {
		t.Fatalf("state.json 缺少账号 u1: %s", raw)
	}
	if !acc.ManualDisabled {
		t.Error("state.json 应保留 manual_disabled=true（重启后运维意图不丢）")
	}
	if acc.ManualReason != "持久化" {
		t.Errorf("state.json manual_reason=%q want 持久化", acc.ManualReason)
	}
	if acc.Disabled {
		t.Error("手动停用不应写自动禁用位")
	}
}
