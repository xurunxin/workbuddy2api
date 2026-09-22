// admin_status_test.go /status 的手动停用状态契约（上游 issue #138/#118）。
//
// 历史背景：上游把「临时停用/恢复/复活」开在 /admin/accounts/{uid}/{disable,enable,
// revive} 上（api_key 鉴权 + config admin.enabled 开关）。本 fork 的内嵌管理控制台
// 占用了整个 /admin/ 前缀，二者同路径无法并存，故按维护决策**统一到控制台**：
// 三个动作由控制台的账号页提供（会话鉴权 + CSRF，见 internal/admin 的
// account_manual_test.go 与此处的 /status 契约）。
//
// 本文件保留的是**服务端仍需保证**的部分：/status 如实透出 manual_disabled /
// manual_reason / disabled 三位信息，且手动停用归入 disabled 计数——控制台与
// 任何外部面板都依赖这个载荷来区分「运维主动摘的」与「系统判定坏的」。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// adminStatusResp 与 pool.Status 的手动停用字段同构（测试侧独立声明，
// 避免测试跟着实现改字段名）。
type adminStatusResp struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason"`
	Disabled       bool   `json:"disabled"`
}

// TestStatusExposesManualDisabled /status 透出双位状态与原因，且计数口径闭合。
func TestStatusExposesManualDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "secret"})
	p.SetManualDisabled("u1", true, "面板摘除")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []adminStatusResp `json:"accounts"`
		Disabled int               `json:"disabled"`
		Healthy  int               `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	a := body.Accounts[0]
	if !a.ManualDisabled {
		t.Error("status 应透出 manual_disabled=true")
	}
	if a.Disabled {
		t.Error("手动停用不应同时置自动禁用位（两位正交）")
	}
	if a.ManualReason != "面板摘除" {
		t.Errorf("manual_reason=%q", a.ManualReason)
	}
	// 计数口径：手动停用与自动禁用同归 disabled——对「多少号不参与选号」
	// 这个运维问题二者等价，分开会让 total/healthy/cooling/disabled 不闭合。
	if body.Disabled != 1 || body.Healthy != 0 {
		t.Errorf("counts disabled=%d healthy=%d, want 1/0", body.Disabled, body.Healthy)
	}
}

// TestStatusRequiresAPIKey /status 仍需 api_key（与上游同源鉴权）。
func TestStatusRequiresAPIKey(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "secret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 without key", rec.Code)
	}
}
