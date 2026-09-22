package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// catalogFake 单一 fake 上游，同时服务 CN（Bearer at_cn）与 global（其他）两域的
// 模型目录端点 + 图像端点。按 realm 返回**不同**的模型表，用于验证两区模型不互相混入。
type catalogFake struct {
	up *upstream.Client

	cnBody     string
	globalBody string

	imageStatus int
	imageBody   string
	imageCalls  []string // 图像端点收到的路径
	imageBodies []string // 图像端点收到的请求体
}

func newCatalogFake(t *testing.T, cnBody, globalBody string) *catalogFake {
	t.Helper()
	cf := &catalogFake{
		cnBody:      cnBody,
		globalBody:  globalBody,
		imageStatus: 200,
		imageBody:   `{"created":1700000000,"data":[{"b64_json":"QUJD"}]}`,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		isCN := r.Header.Get("Authorization") == "Bearer at_cn"
		switch {
		case strings.Contains(r.URL.Path, "/images/"):
			raw, _ := io.ReadAll(r.Body)
			cf.imageCalls = append(cf.imageCalls, r.URL.Path)
			cf.imageBodies = append(cf.imageBodies, string(raw))
			w.WriteHeader(cf.imageStatus)
			_, _ = io.WriteString(w, cf.imageBody)
			return
		case r.URL.Path == "/v3/config" || strings.Contains(r.URL.Path, "/personal/models"):
			w.WriteHeader(200)
			if isCN {
				_, _ = io.WriteString(w, cf.cnBody)
			} else {
				_, _ = io.WriteString(w, cf.globalBody)
			}
			return
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"code":404}`)
	}))
	t.Cleanup(ts.Close)
	cf.up = &upstream.Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     strings.TrimSuffix(ts.URL, "/"),
		ChatBaseGlobal: strings.TrimSuffix(ts.URL, "/"),
		GlobalEnabled:  true,
	}
	return cf
}

// modelsJSON 构造对象形态的模型目录响应（含 agents[cli] 过滤所需的 id 列表）。
func modelsJSON(models string, cliIDs ...string) string {
	quoted := make([]string, 0, len(cliIDs))
	for _, id := range cliIDs {
		quoted = append(quoted, `"`+id+`"`)
	}
	return `{"code":0,"data":{"models":[` + models + `],"agents":[{"name":"cli","models":[` +
		strings.Join(quoted, ",") + `]}]}}`
}

// TestModelsResponseGroupsRoutingAndMedia /v1/models 的分组键：data 全量（每条带
// kind），routing_models 只含自动路由档位，media_models 只含图像/视频模型。
//
// 用户要求：「自动、快速、均衡等这些明显是自动路由模型的档位单列，不和普通模型混一起」。
func TestModelsResponseGroupsRoutingAndMedia(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cnBody := modelsJSON(`
		{"id":"fast-model","name":"快速","maxInputTokens":300000,"maxOutputTokens":48000,"supportsImages":true},
		{"id":"balanced-model","name":"均衡","maxInputTokens":300000,"maxOutputTokens":48000},
		{"id":"deep-model","name":"极致","maxInputTokens":300000,"maxOutputTokens":48000},
		{"id":"auto","name":"Auto","maxInputTokens":168000,"maxOutputTokens":32000},
		{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":64000,"supportsImages":true},
		{"id":"deepseek-v4.1-flash","name":"DS","maxInputTokens":1000000,"maxOutputTokens":128000}
	`, "fast-model", "balanced-model", "deep-model", "auto", "glm-5.2", "deepseek-v4.1-flash")

	cf := newCatalogFake(t, cnBody, `{"code":0,"data":{"models":[]}}`)
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: false})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data          []map[string]any `json:"data"`
		RoutingModels []map[string]any `json:"routing_models"`
		MediaModels   []map[string]any `json:"media_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	byID := map[string]map[string]any{}
	for _, m := range out.Data {
		byID[m["id"].(string)] = m
	}
	// data 全量保留（OpenAI 兼容：按 id 选模型的既有客户端不因分组而找不到模型）。
	if len(out.Data) != 6 {
		t.Fatalf("data len=%d want 6 (all kinds kept for compatibility)", len(out.Data))
	}
	// 每条都带 kind。
	for _, id := range []string{"cn:fast-model", "cn:glm-5.2"} {
		if byID[id]["kind"] == nil {
			t.Errorf("%s missing kind field", id)
		}
	}
	// routing_models 只含 4 个档位。
	if len(out.RoutingModels) != 4 {
		t.Fatalf("routing_models=%v want 4 tier entries", idsOf(out.RoutingModels))
	}
	wantRouting := map[string]bool{"cn:fast-model": true, "cn:balanced-model": true, "cn:deep-model": true, "cn:auto": true}
	for _, m := range out.RoutingModels {
		id := m["id"].(string)
		if !wantRouting[id] {
			t.Errorf("routing_models contains non-tier %q", id)
		}
		if m["kind"] != string(upstream.KindRouter) {
			t.Errorf("%s kind=%v want router", id, m["kind"])
		}
		if m["router_tier"] == nil {
			t.Errorf("%s missing router_tier (display name)", id)
		}
	}
	if idsOf(out.RoutingModels)[0] != "cn:fast-model" {
		t.Errorf("routing_models order=%v want source order preserved", idsOf(out.RoutingModels))
	}
	// 普通模型不得出现在 routing_models 里。
	for _, m := range out.RoutingModels {
		if id := m["id"].(string); id == "cn:glm-5.2" || id == "cn:deepseek-v4.1-flash" {
			t.Errorf("plain model %q must NOT be in routing_models", id)
		}
	}
	// 无媒体模型 → media_models 是空数组（非 null）。
	if out.MediaModels == nil {
		t.Error("media_models must be [] not null when empty")
	}
	if len(out.MediaModels) != 0 {
		t.Errorf("media_models=%v want empty", idsOf(out.MediaModels))
	}
	// 附件能力透出。
	if byID["cn:glm-5.2"]["supports_attachments"] != true {
		t.Error("glm-5.2 supports_attachments want true (supportsImages=true)")
	}
	if att, _ := byID["cn:glm-5.2"]["attachments"].([]any); len(att) != 1 || att[0] != "image" {
		t.Errorf("glm-5.2 attachments=%v want [image]", byID["cn:glm-5.2"]["attachments"])
	}
	if byID["cn:deepseek-v4.1-flash"]["supports_attachments"] != false {
		t.Error("model without supportsImages must report supports_attachments=false")
	}
}

// TestModelsResponseSeparatesRealms 两区模型严格分离（用户要求：控制台对应账号要
// 区分中国区和国际区的模型，不要将国际区的模型混入）。
//
// 断言：CN 清单只出 cn:* 前缀、global 清单只出 global:* 前缀，同一 id 在两区
// 各自独立成条（不因同名而互相覆盖），且 CN 不出现只属于 global 的模型 id。
func TestModelsResponseSeparatesRealms(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cnBody := modelsJSON(`
		{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":64000},
		{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000}
	`, "glm-5.2", "hy3")
	globalBody := modelsJSON(`
		{"id":"gpt-5.4","name":"GPT-5.4","maxInputTokens":1050000,"maxOutputTokens":128000},
		{"id":"gemini-3.5-flash","name":"Gemini","maxInputTokens":1048576,"maxOutputTokens":65536},
		{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":64000}
	`, "gpt-5.4", "gemini-3.5-flash", "glm-5.2")

	cf := newCatalogFake(t, cnBody, globalBody)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cnIDs := map[string]bool{}
	globalIDs := map[string]bool{}
	for _, m := range out.Data {
		id, _ := m["id"].(string)
		switch {
		case strings.HasPrefix(id, "cn:"):
			cnIDs[strings.TrimPrefix(id, "cn:")] = true
		case strings.HasPrefix(id, "global:"):
			globalIDs[strings.TrimPrefix(id, "global:")] = true
		}
	}
	// CN 只出 CN 模型：国际区独有模型不得混入 CN 面。
	for _, banned := range []string{"gpt-5.4", "gemini-3.5-flash"} {
		if cnIDs[banned] {
			t.Errorf("global-only model %q leaked into the cn list", banned)
		}
	}
	// global 只出 global 探测结果：CN 独有模型（hy3）不得混入 global 面。
	if globalIDs["hy3"] {
		t.Error("cn-only model hy3 leaked into the global list")
	}
	// 同名模型（glm-5.2）两区各自独立成条，前缀区分，互不覆盖。
	if !cnIDs["glm-5.2"] {
		t.Error("cn:glm-5.2 missing")
	}
	if !globalIDs["glm-5.2"] {
		t.Error("global:glm-5.2 missing (same id in both realms must stay separate)")
	}
}

// TestModelsResponseMediaDiscoverable 媒体模型在**两区都可见**且正确分类
// （本次方向修正：旧实现把媒体模型挡在目录外，导致 /v1/images/* 无可发现渠道）。
//
// 关键契约：
//   - 媒体模型进 data（客户端靠它发现模型）+ 带 kind=image/video；
//   - 它们**同时**进 media_models 分组（客户端据此单列 / 不走对话）；
//   - 它们**不进** routing_models（档位组只放自动路由档位）。
func TestModelsResponseMediaDiscoverable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	globalBody := modelsJSON(`
		{"id":"gpt-5.4","name":"GPT-5.4","maxInputTokens":1050000,"maxOutputTokens":128000},
		{"id":"hunyuan-image-alpha-edit","name":"Hunyuan Image Alpha Edit","tags":["image-to-image"]},
		{"id":"kling-v3-t2v","name":"Kling T2V","tags":["text-to-video"]},
		{"id":"completion-gf","name":"completion-gf","maxOutputTokens":8192}
	`, "gpt-5.4", "hunyuan-image-alpha-edit", "kling-v3-t2v", "completion-gf")

	cf := newCatalogFake(t, `{"code":0,"data":{"models":[],"agents":[]}}`, globalBody)
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data          []map[string]any `json:"data"`
		MediaModels   []map[string]any `json:"media_models"`
		RoutingModels []map[string]any `json:"routing_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, m := range out.Data {
		byID[m["id"].(string)] = m
	}
	// 媒体模型在 data 里且分类正确。
	if got := byID["global:hunyuan-image-alpha-edit"]["kind"]; got != string(upstream.KindImage) {
		t.Errorf("global:hunyuan-image-alpha-edit kind=%v want image (must be discoverable)", got)
	}
	if got := byID["global:kling-v3-t2v"]["kind"]; got != string(upstream.KindVideo) {
		t.Errorf("global:kling-v3-t2v kind=%v want video", got)
	}
	// 也进 media_models 分组。
	mediaIDs := idsOf(out.MediaModels)
	if len(mediaIDs) != 2 {
		t.Errorf("media_models=%v want the 2 media models", mediaIDs)
	}
	// 不进 routing_models。
	for _, id := range idsOf(out.RoutingModels) {
		if strings.Contains(id, "hunyuan") || strings.Contains(id, "kling") {
			t.Errorf("media model %q must not be in routing_models", id)
		}
	}
	// 无入口的非对话条目仍被剔除。
	if _, ok := byID["global:completion-gf"]; ok {
		t.Error("completion-gf must be filtered out (no gateway endpoint serves it)")
	}
}

// idsOf 取条目 id 列表（断言辅助）。
func idsOf(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, m := range entries {
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	return out
}
