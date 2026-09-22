package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// imageModelsCN CN 目录：一个图像模型（图生图）+ 一个普通对话模型。
const imageModelsCN = `{"code":0,"data":{"models":[
	{"id":"hunyuan-image-alpha-edit","name":"Hunyuan Image Alpha Edit","tags":["image-to-image"]},
	{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},
	{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":64000,"supportsImages":true}
],"agents":[{"name":"cli","models":["hunyuan-image-alpha-edit","hunyuan-image-alpha","glm-5.2"]}]}}`

// imageHandler 构造一个挂了图像模型目录 fake 的 handler（CN 单账号）。
func imageHandler(t *testing.T, cf *catalogFake) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: false})
}

// TestImagesGenerationsHappyPath /v1/images/generations 端到端：图像模型 → 上游图像
// 端点（不是 chat），响应按 OpenAI Images 形状回给客户端。
func TestImagesGenerationsHappyPath(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	body := `{"model":"hunyuan-image-alpha","prompt":"a cat on a mat"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out upstream.ImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(out.Data) != 1 || out.Data[0].B64JSON != "QUJD" {
		t.Errorf("response=%+v want one b64 image", out)
	}
	if out.Model != "hunyuan-image-alpha" {
		t.Errorf("model=%q want hunyuan-image-alpha (echoed for routing confirmation)", out.Model)
	}
	// 上游走的是图像生成端点（不是 chat）。
	if len(cf.imageCalls) != 1 || !strings.HasSuffix(cf.imageCalls[0], "/generations") {
		t.Fatalf("upstream calls=%v want one /generations", cf.imageCalls)
	}
	// 出站 model 已剥离 realm 前缀（此处本就无前缀），prompt 原样送达。
	if !strings.Contains(cf.imageBodies[0], `"model":"hunyuan-image-alpha"`) {
		t.Errorf("upstream body=%s missing model", cf.imageBodies[0])
	}
}

// TestImagesEditsRequiresImage /v1/images/edits 无输入图 → 400 + gateway_hint 指路
// （图生图模型必须有输入图）。
func TestImagesEditsRequiresImage(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits",
		strings.NewReader(`{"model":"hunyuan-image-alpha-edit","prompt":"make it blue"}`)))
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct {
			Code        string  `json:"code"`
			Message     string  `json:"message"`
			GatewayHint *string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Error.Code != "image_required" {
		t.Errorf("code=%q want image_required", e.Error.Code)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "/v1/images/generations") {
		t.Errorf("gateway_hint=%v want pointer to generations endpoint", e.Error.GatewayHint)
	}
	// 请求未打到上游（本地预校验拦截）。
	if len(cf.imageCalls) != 0 {
		t.Errorf("upstream must not be called for a locally-rejected request: %v", cf.imageCalls)
	}
}

// TestImagesEditsWithImageHappyPath 带图 → 走编辑端点。
func TestImagesEditsWithImageHappyPath(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	body := `{"model":"hunyuan-image-alpha-edit","prompt":"add a hat","image":"https://example.com/in.png"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(cf.imageCalls) != 1 || !strings.HasSuffix(cf.imageCalls[0], "/edits") {
		t.Fatalf("upstream calls=%v want one /edits", cf.imageCalls)
	}
	if !strings.Contains(cf.imageBodies[0], `"image":["https://example.com/in.png"]`) {
		t.Errorf("upstream body=%s want image as an ARRAY (upstream EditImageRequest.image is []string)", cf.imageBodies[0])
	}
}

// TestImagesRejectsChatModel 非图像模型打到 /v1/images/* → 400 not_an_image_model，
// 并指路 /v1/models（kind=image / media_models）。不让上游用含糊的 400 打发客户端。
func TestImagesRejectsChatModel(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"glm-5.2","prompt":"hello"}`)))
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct {
			Code        string  `json:"code"`
			GatewayHint *string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Error.Code != "not_an_image_model" {
		t.Errorf("code=%q want not_an_image_model", e.Error.Code)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "kind=image") {
		t.Errorf("gateway_hint=%v want pointer to kind=image models", e.Error.GatewayHint)
	}
	if len(cf.imageCalls) != 0 {
		t.Errorf("upstream must not be called: %v", cf.imageCalls)
	}
}

// TestImagesRejectsMissingPrompt prompt 是必填（上游语义），本地拦截并给可读错误。
func TestImagesRejectsMissingPrompt(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"hunyuan-image-alpha"}`)))
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "prompt is required") {
		t.Errorf("body=%s want prompt-required message", rec.Body.String())
	}
}

// TestImagesRejectsBadImageURI 入参图片形态预校验：非法 data URI / 非 http(s)
// 一律本地 400（不替客户端取图、不把非法入参转发给上游）。
func TestImagesRejectsBadImageURI(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	for _, bad := range []string{
		`file:///etc/passwd`,
		`ftp://example.com/a.png`,
		`data:text/plain;base64,QUJD`, // 非 image/*
		`data:image/png,notbase64`,    // 缺 ;base64
	} {
		body := `{"model":"hunyuan-image-alpha-edit","prompt":"x","image":` + jsonString(bad) + `}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("image=%q status=%d want 400 body=%s", bad, rec.Code, rec.Body.String())
		}
	}
	if len(cf.imageCalls) != 0 {
		t.Errorf("upstream must not be called for invalid image URIs: %v", cf.imageCalls)
	}
	// 合法 data URI 应放行到上游。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits",
		strings.NewReader(`{"model":"hunyuan-image-alpha-edit","prompt":"x","image":"data:image/png;base64,QUJD"}`)))
	if rec.Code != 200 {
		t.Errorf("valid data URI status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
}

// TestImagesRealmPrefixRoutesToRealmAccount realm 前缀协议与 chat 同构：
// `global:<图像模型>` 必须选 global 账号（不跨域用 CN 号顶上）。
func TestImagesRealmPrefixRoutesToRealmAccount(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	globalBody := `{"code":0,"data":{"models":[
		{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]}
	]}}`
	cf := newCatalogFake(t, imageModelsCN, globalBody)
	// 池中只有 CN 号：global 前缀请求必须 503（不跨 realm 用 CN 号顶上）。
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"global:hunyuan-image-alpha","prompt":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 (no global account) body=%s", rec.Code, rec.Body.String())
	}
	if len(cf.imageCalls) != 0 {
		t.Errorf("no upstream call expected without a global account: %v", cf.imageCalls)
	}
}

// TestImagesUpstreamErrorPassthrough 上游错误按 chat 同口径透传：message 是上游
// 原文，限流映射 429。
func TestImagesUpstreamErrorPassthrough(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	cf.imageStatus = 429
	cf.imageBody = `{"code":6004,"msg":"model usage limit reached, resets at 12:00"}`
	h := imageHandler(t, cf)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"hunyuan-image-alpha","prompt":"x"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model usage limit reached") {
		t.Errorf("body=%s want upstream message pass-through", rec.Body.String())
	}
}

// TestImagesSuccessRecordsCostObservation 上游下发 usage.credits 时记入成本账本
// （图像模型单价高，账本让选号把便宜号排前面）。
func TestImagesSuccessRecordsCostObservation(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	cf.imageBody = `{"created":1,"data":[{"b64_json":"QUJD"}],"usage":{"credits":5.0,"images":1}}`
	h := imageHandler(t, cf)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"hunyuan-image-alpha","prompt":"x"}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := h.cfg.Pool.Status("cn1")
	if !ok {
		t.Fatal("account status missing")
	}
	found := false
	for _, mc := range st.ModelCosts {
		if mc.Model == "hunyuan-image-alpha" {
			found = true
		}
	}
	if !found {
		t.Errorf("model_costs=%+v want an entry for hunyuan-image-alpha", st.ModelCosts)
	}
}

// TestChatRejectsImageModel 图像模型误投对话入口 → 本地 400 not_a_chat_model +
// gateway_hint 指路专用端点，**不罚账号、不打上游**。
//
// 为什么必须有这道守卫：图像模型现在**在目录里**（否则客户端发现不了它们，
// 见 upstream.nonChatModel 注释），客户端一不留神就会把它们当对话模型发过来；
// 交给上游只会得到含糊的 11102/11133 参数错误，排查成本高。
func TestChatRejectsImageModel(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	// 缓存冷时 hint 会**并列**给出两个图像端点（tags 未知，猜一个可能在图生图上
	// 指错路），故断言「给出了可操作的 /v1/images/* 指向」而非某一个具体端点。
	cases := []string{
		"hunyuan-image-alpha-edit",
		"hunyuan-image-alpha",
		"global:hunyuan-image-alpha",
	}
	for _, model := range cases {
		body := `{"model":` + jsonString(model) + `,"messages":[{"role":"user","content":"hi"}]}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("%s: status=%d want 400 body=%s", model, rec.Code, rec.Body.String())
			continue
		}
		var e struct {
			Error struct {
				Code        string  `json:"code"`
				GatewayHint *string `json:"gateway_hint"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Errorf("%s: decode: %v", model, err)
			continue
		}
		if e.Error.Code != "not_a_chat_model" {
			t.Errorf("%s: code=%q want not_a_chat_model", model, e.Error.Code)
		}
		if e.Error.GatewayHint == nil ||
			!strings.Contains(*e.Error.GatewayHint, "/v1/images/") ||
			!strings.Contains(*e.Error.GatewayHint, "media_models") {
			t.Errorf("%s: gateway_hint=%v want actionable /v1/images/* + media_models pointer", model, e.Error.GatewayHint)
		}
	}
	// 全程零上游 chat 调用（本地拦截，不打上游、不罚号）。
	if len(cf.imageCalls) != 0 {
		t.Errorf("guard must not call upstream: %v", cf.imageCalls)
	}
	// 账号健康：本地拦截不产生失败计数（选错入口与账号无关）。
	st, ok := h.cfg.Pool.Status("cn1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.ErrTotal != 0 || st.ConsecutiveFails != 0 {
		t.Errorf("guard must not penalize the account: err_total=%d consecutive=%d", st.ErrTotal, st.ConsecutiveFails)
	}
}

// TestChatGuardDoesNotBlockChatModels 守卫不得误伤普通模型与自动路由档位
// （误伤会让正常请求被本地拒绝，比漏拦严重得多）。
func TestChatGuardDoesNotBlockChatModels(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)
	for _, model := range []string{"glm-5.2", "auto", "fast-model", "balanced-model", "deep-model"} {
		if kind := h.modelKindOf(model); kind == upstream.KindImage || kind == upstream.KindVideo {
			t.Errorf("modelKindOf(%q)=%q — chat guard would wrongly reject it", model, kind)
		}
	}
	// 档位必须分类为 router（用于 routing_models 分组），且不被守卫拦。
	if kind := h.modelKindOf("fast-model"); kind != upstream.KindRouter {
		t.Errorf("modelKindOf(fast-model)=%q want router", kind)
	}
	// 未知模型保守放行（交上游裁决，不误拒）。
	if kind := h.modelKindOf("some-brand-new-model"); kind != upstream.KindChat {
		t.Errorf("modelKindOf(unknown)=%q want chat (conservative passthrough)", kind)
	}
}

// TestChatGuardHotPathDoesNotFetchUpstream 守卫在 chat 热路径上**绝不发起上游调用**：
// 即便目录缓存完全冷（未探测过），modelKindOf 也只走 id 家族判定。
//
// 这是关键约束——守卫若触发目录拉取，每个 chat 请求在缓存冷时都会多打一次上游模型
// 接口（chat 与模型目录本不该耦合，且放大上游请求量）。
func TestChatGuardHotPathDoesNotFetchUpstream(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	// catalogHits 统计模型目录端点的上游调用次数。
	var catalogHits int
	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	base := cf.up.HTTP.Transport
	cf.up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "models") || r.URL.Path == "/v3/config" {
			catalogHits++
		}
		return base.RoundTrip(r)
	})}
	h := imageHandler(t, cf)

	// 目录缓存冷 → 用 id 家族判定，且不得打上游。
	if kind := h.modelKindOf("hunyuan-image-alpha-edit"); kind != upstream.KindImage {
		t.Errorf("cold-cache kind=%q want image (id-family fallback)", kind)
	}
	if kind := h.modelKindOf("fast-model"); kind != upstream.KindRouter {
		t.Errorf("cold-cache fast-model kind=%q want router", kind)
	}
	if catalogHits != 0 {
		t.Errorf("modelKindOf must not fetch the catalog on the chat hot path: %d upstream calls", catalogHits)
	}
}

// jsonString 把字符串安全地编码进 JSON（测试构造请求体用）。
func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// TestImageModelFullDiscoveryLoop 端到端闭环：目录发现 → 拿到 id 与 kind →
// 用该 id 成功调 /v1/images/*。这是「图像 API 真的可用」的判据——
// 只有目录可见（否则客户端无从得知 id）且端点可调（否则发现了也没用）才算闭环。
func TestImageModelFullDiscoveryLoop(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)

	// 1) 从 /v1/models 发现图像模型（客户端唯一发现渠道）。
	var listResp struct {
		MediaModels []map[string]any `json:"media_models"`
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode models: %v body=%s", err, rec.Body.String())
	}
	var discoveredID, discoveredKind string
	for _, m := range listResp.MediaModels {
		if id, _ := m["id"].(string); strings.HasSuffix(id, "hunyuan-image-alpha-edit") {
			discoveredID = id
			discoveredKind, _ = m["kind"].(string)
		}
	}
	if discoveredID == "" {
		t.Fatalf("image model not discoverable via /v1/models: media_models=%v", listResp.MediaModels)
	}
	if discoveredKind != string(upstream.KindImage) {
		t.Errorf("discovered kind=%q want image", discoveredKind)
	}

	// 2) 用发现到的 id（带网关前缀的模型名）调图像端点，验证前缀解析后落到正确上游端点。
	body := `{"model":` + jsonString(discoveredID) + `,"prompt":"make it blue","image":"https://example.com/in.png"}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("image call with discovered id %q: status=%d body=%s", discoveredID, rec.Code, rec.Body.String())
	}
	if len(cf.imageCalls) != 1 || !strings.HasSuffix(cf.imageCalls[0], "/edits") {
		t.Errorf("upstream calls=%v want one /edits", cf.imageCalls)
	}
}

// TestImageFieldAcceptsStringAndArray image / image_url 字段容忍三种形态
// （上游要求数组，OpenAI 惯例是单字符串，两种都得收）。
//
// 上游实测（2026-09-18 真机联调）：字符串形态被上游拒绝——
//
//	{"code":11101,"msg":"...cannot unmarshal string into Go struct field
//	 EditImageRequest.image of type []string"}
//
// 故网关对外兼容单字符串、对内一律归一成数组出站。本测试锁定该契约。
func TestImageFieldAcceptsStringAndArray(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cases := []struct {
		name     string
		body     string
		wantJSON string
	}{
		{
			name:     "single string (OpenAI convention)",
			body:     `{"model":"hunyuan-image-alpha-edit","prompt":"x","image":"https://e.com/a.png"}`,
			wantJSON: `"image":["https://e.com/a.png"]`,
		},
		{
			name:     "array (upstream native form)",
			body:     `{"model":"hunyuan-image-alpha-edit","prompt":"x","image":["https://e.com/a.png"]}`,
			wantJSON: `"image":["https://e.com/a.png"]`,
		},
		{
			name:     "multi-image array",
			body:     `{"model":"hunyuan-image-alpha-edit","prompt":"x","image":["https://e.com/a.png","https://e.com/b.png"]}`,
			wantJSON: `"image":["https://e.com/a.png","https://e.com/b.png"]`,
		},
		{
			name:     "image_url alias as string",
			body:     `{"model":"hunyuan-image-alpha-edit","prompt":"x","image_url":"https://e.com/a.png"}`,
			wantJSON: `"image":["https://e.com/a.png"]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetModelsCache()
			cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
			h := imageHandler(t, cf)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(c.body)))
			if rec.Code != 200 {
				t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
			}
			if len(cf.imageBodies) != 1 || !strings.Contains(cf.imageBodies[0], c.wantJSON) {
				t.Errorf("upstream body=%v want to contain %s", cf.imageBodies, c.wantJSON)
			}
		})
	}
}

// TestImageFieldRejectsBadShape 非法形态（数字/对象）→ 本地 400，不打上游。
func TestImageFieldRejectsBadShape(t *testing.T) {
	resetModelsCache()
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cf := newCatalogFake(t, imageModelsCN, `{"code":0,"data":{"models":[]}}`)
	h := imageHandler(t, cf)
	for _, body := range []string{
		`{"model":"hunyuan-image-alpha-edit","prompt":"x","image":123}`,
		`{"model":"hunyuan-image-alpha-edit","prompt":"x","image":{"url":"https://e.com/a.png"}}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("body=%s status=%d want 400", body, rec.Code)
		}
	}
	if len(cf.imageCalls) != 0 {
		t.Errorf("bad shapes must be rejected locally: %v", cf.imageCalls)
	}
}
