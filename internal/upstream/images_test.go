package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestImageGenerateUsesRealmBaseAndDefaultPath 图像端点按 realm 切 base、默认路径
// 为 /v2/images/{generations,edits}（**已实测确认**，见 images.go 文件头），
// 鉴权与 CommonHeaders 与 chat 同款。
func TestImageGenerateUsesRealmBaseAndDefaultPath(t *testing.T) {
	var gotPath, gotAuthz, gotUA string
	var gotBody ImageRequest
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuthz = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		return jsonResp(200, `{"created":1700000000,"data":[{"b64_json":"QUJD"}]}`), nil
	})
	cn := &auth.Auth{AccessToken: "at-cn", UID: "cn1", Domain: "www.codebuddy.cn"}

	resp, status, _, err := c.ImageGenerate(context.Background(), cn, ImageRequest{
		Model: "hunyuan-image-alpha", Prompt: "a cat",
	})
	if err != nil || status != 200 {
		t.Fatalf("image generate: status=%d err=%v", status, err)
	}
	if gotPath != defaultImageGeneratePath {
		t.Errorf("path=%q want %q", gotPath, defaultImageGeneratePath)
	}
	if gotAuthz != "Bearer at-cn" {
		t.Errorf("authz=%q want Bearer at-cn", gotAuthz)
	}
	if gotUA == "" {
		t.Error("User-Agent must be set (CommonHeaders)")
	}
	if gotBody.Model != "hunyuan-image-alpha" || gotBody.Prompt != "a cat" {
		t.Errorf("upstream body=%+v want model+prompt", gotBody)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON != "QUJD" {
		t.Errorf("parsed response=%+v want one b64 image", resp)
	}
}

// TestImageGenerateEditPathWhenImagePresent 入参带图 → 自动走编辑端点
// （官方 ImageGen 工具描述：带 image 参数即执行 image-to-image）。
func TestImageGenerateEditPathWhenImagePresent(t *testing.T) {
	var gotPath string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		return jsonResp(200, `{"data":[{"url":"https://example.com/a.png"}]}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	if _, _, _, err := c.ImageGenerate(context.Background(), a, ImageRequest{
		Model: "hunyuan-image-alpha-edit", Prompt: "make it blue",
		Image: []string{"https://example.com/in.png"},
	}); err != nil {
		t.Fatalf("image edit: %v", err)
	}
	if gotPath != defaultImageEditPath {
		t.Errorf("path=%q want %q (image present → edit endpoint)", gotPath, defaultImageEditPath)
	}
}

// TestImageGenerateGlobalRealmBase global 账号走 global base（与 chat 同一条
// realm 判定链，不把国际区账号的请求打到中国区端点）。
func TestImageGenerateGlobalRealmBase(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Host
		return jsonResp(200, `{"data":[{"b64_json":"QUJD"}]}`), nil
	})
	c.ChatBaseCN = "https://cn.invalid"
	c.ChatBaseGlobal = "https://global.invalid"
	// globalOn 要求 Client.GlobalEnabled（config global.enabled 的客户端侧闸门）；
	// testClient 默认不开，这里显式开（生产由 main.go 按 config 注入）。
	c.GlobalEnabled = true

	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	if _, _, _, err := c.ImageGenerate(context.Background(), a, ImageRequest{Model: "m", Prompt: "p"}); err != nil {
		t.Fatalf("image generate global: %v", err)
	}
	if gotHost != "global.invalid" {
		t.Errorf("host=%q want global.invalid (realm-aware base)", gotHost)
	}
}

// TestImageGenerateConfiguredPathsWin 配置覆盖路径（上游将来改路径时的适配入口：
// 改配置即可，无需改代码）。
func TestImageGenerateConfiguredPathsWin(t *testing.T) {
	var genPath, editPath string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/edits") {
			editPath = r.URL.Path
		} else {
			genPath = r.URL.Path
		}
		return jsonResp(200, `{"data":[{"b64_json":"QUJD"}]}`), nil
	})
	c.ImageGeneratePath = "/custom/gen"
	c.ImageEditPath = "/custom/edits"
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	if _, _, _, err := c.ImageGenerate(context.Background(), a, ImageRequest{Model: "m", Prompt: "p"}); err != nil {
		t.Fatalf("gen: %v", err)
	}
	if _, _, _, err := c.ImageGenerate(context.Background(), a, ImageRequest{Model: "m", Prompt: "p", Image: []string{"https://e.com/i.png"}}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if genPath != "/custom/gen" {
		t.Errorf("gen path=%q want /custom/gen", genPath)
	}
	if editPath != "/custom/edits" {
		t.Errorf("edit path=%q want /custom/edits", editPath)
	}
}

// TestParseImageResponseShapes 响应解析容忍两种形状（裸形态 / 业务 code 包装形态）。
func TestParseImageResponseShapes(t *testing.T) {
	// 裸形态。
	out, err := parseImageResponse([]byte(`{"created":1,"data":[{"b64_json":"QQ==","revised_prompt":"rp"}]}`))
	if err != nil {
		t.Fatalf("bare shape: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].B64JSON != "QQ==" || out.Data[0].RevisedPrompt != "rp" {
		t.Errorf("bare parsed=%+v", out)
	}
	// 包装形态。
	out, err = parseImageResponse([]byte(`{"code":0,"data":{"created":2,"data":[{"url":"https://e.com/a.png"}]}}`))
	if err != nil {
		t.Fatalf("wrapped shape: %v", err)
	}
	if out.Created != 2 || len(out.Data) != 1 || out.Data[0].URL != "https://e.com/a.png" {
		t.Errorf("wrapped parsed=%+v", out)
	}
	// 包装形态 + 业务错误码。
	if _, err := parseImageResponse([]byte(`{"code":40001,"data":{"data":[]}}`)); err == nil {
		t.Error("non-zero code must be an error")
	}
	// 空图列表：报错而非"成功但没图"。
	if _, err := parseImageResponse([]byte(`{"created":1,"data":[]}`)); err == nil {
		t.Error("empty data must be an error (no silent empty success)")
	}
	// created 缺失 → 补当前时间（不返回 0，OpenAI 客户端会当成 1970）。
	out, err = parseImageResponse([]byte(`{"data":[{"b64_json":"QQ=="}]}`))
	if err != nil {
		t.Fatalf("no created: %v", err)
	}
	if out.Created == 0 {
		t.Error("created must default to now, not zero")
	}
}

// TestImageGenerateClassifiesErrors 上游 >=400 → *Error 分类信封（与 chat 同口径，
// 调用方据此走既有退避/冷却策略）。
func TestImageGenerateClassifiesErrors(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `{"error":{"message":"too many requests"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, body, err := c.ImageGenerate(context.Background(), a, ImageRequest{Model: "m", Prompt: "p"})
	if status != 429 {
		t.Errorf("status=%d want 429", status)
	}
	if len(body) == 0 {
		t.Error("raw body must be returned for error pass-through")
	}
	if err == nil {
		t.Fatal("429 must return an error")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type=%T want *Error", err)
	}
	if ue.Kind != ErrSoftRate {
		t.Errorf("kind=%v want ErrSoftRate", ue.Kind)
	}
}

// TestImageGenerateUnparseableSuccess 上游 200 但响应不可解析 → 报错，
// 不假装成功（不编造一张空图让客户端静默拿到空结果）。
func TestImageGenerateUnparseableSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `not json at all`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, _, err := c.ImageGenerate(context.Background(), a, ImageRequest{Model: "m", Prompt: "p"})
	if status != 200 {
		t.Errorf("status=%d want 200", status)
	}
	if err == nil {
		t.Fatal("unparseable 200 body must be an error")
	}
	if !strings.Contains(err.Error(), "image response parse") {
		t.Errorf("err=%v want parse error", err)
	}
}
