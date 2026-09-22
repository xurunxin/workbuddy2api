package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestGlobalProbeKeepsMediaFiltersNonChat global 探测的目录准入（任务书
// model-catalog-verify，本次方向修正）：
//   - 图像/视频模型**保留**（带 kind 标记；客户端靠目录发现它们才能调 /v1/images/*）；
//   - 无任何入口的非对话专用模型（completion-*/codewise-*/tiny）**剔除**。
//
// 历史：旧实现把媒体模型也挡掉，与本网关现有的图像接口矛盾（发现渠道被切断）。
func TestGlobalProbeKeepsMediaFiltersNonChat(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"gpt-5.4","name":"GPT-5.4","maxInputTokens":1050000,"maxOutputTokens":128000},
			{"id":"deepseek-v4.1-flash","name":"DS","maxInputTokens":1000000,"maxOutputTokens":128000},
			{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},
			{"id":"hunyuan-image-alpha-edit","name":"Hunyuan Image Alpha Edit","tags":["image-to-image"]},
			{"id":"hunyuan-image-v3.0-art","name":"Art","tags":["text-to-image","image-to-image"]},
			{"id":"kling-v3-t2v","name":"Kling T2V","tags":["text-to-video"]},
			{"id":"kling-v3-i2v","name":"Kling I2V","tags":["image-to-video"]},
			{"id":"completion-gf","name":"completion-gf","maxOutputTokens":8192},
			{"id":"hunyuan-3b","name":"hunyuan-3b","maxOutputTokens":256},
			{"id":"codewise-rewrite","name":"codewise-rewrite","maxOutputTokens":256}
		]}}`
	})
	defer srv.Close()

	c := globalModelsClient(t, srv)
	names := c.FetchGlobalModels(globalAcct())

	want := []string{
		"gpt-5.4", "deepseek-v4.1-flash",
		"hunyuan-image-alpha", "hunyuan-image-alpha-edit", "hunyuan-image-v3.0-art",
		"kling-v3-t2v", "kling-v3-i2v",
	}
	if len(names) != len(want) {
		t.Fatalf("global names=%v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("global names=%v want %v", names, want)
		}
	}
	// 逐个点名断言无入口条目确实被剔除。
	for _, banned := range []string{"completion-gf", "hunyuan-3b", "codewise-rewrite"} {
		for _, got := range names {
			if got == banned {
				t.Errorf("%q must NOT appear (no gateway endpoint serves it)", banned)
			}
		}
	}
}

// TestGlobalProbeRichInfosCarryClassification 富条目（对象形态探测）应带上分类与
// 附件能力字段——global 域与 CN 域共用 dynModelEntry.modelInfo，分类口径必须一致。
func TestGlobalProbeRichInfosCarryClassification(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"fast-model","name":"快速","maxInputTokens":300000,"maxOutputTokens":48000,"supportsImages":true},
			{"id":"gpt-5.4","name":"GPT-5.4","maxInputTokens":1050000,"maxOutputTokens":128000,"supportsImages":true,"tags":["craft"]},
			{"id":"hunyuan-2.0-thinking","name":"Hunyuan Thinking","maxInputTokens":128000,"maxOutputTokens":24000,"supportsImages":true,"disabledMultimodal":true}
		]}}`
	})
	defer srv.Close()

	infos := globalModelsClient(t, srv).FetchGlobalModelInfos(globalAcct())
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}

	if got := byID["fast-model"].Kind; got != KindRouter {
		t.Errorf("fast-model kind=%q want router (auto-routing tier)", got)
	}
	if got := byID["fast-model"].RouterTier; got != "快速" {
		t.Errorf("fast-model router_tier=%q want 快速", got)
	}
	if got := byID["gpt-5.4"].Kind; got != KindChat {
		t.Errorf("gpt-5.4 kind=%q want chat", got)
	}
	if att := byID["gpt-5.4"].Attachments; len(att) != 1 || att[0] != AttachmentImage {
		t.Errorf("gpt-5.4 attachments=%v want [image]", att)
	}
	if !byID["gpt-5.4"].SupportsAttachments {
		t.Error("gpt-5.4 supports_attachments=false want true")
	}
	// disabledMultimodal 压过 supportsImages。
	if got := byID["hunyuan-2.0-thinking"]; got.SupportsAttachments || len(got.Attachments) != 0 {
		t.Errorf("hunyuan-2.0-thinking attachments=%v supports=%v want none (disabledMultimodal wins)", got.Attachments, got.SupportsAttachments)
	}
	if !byID["hunyuan-2.0-thinking"].DisabledMultimodal {
		t.Error("hunyuan-2.0-thinking disabled_multimodal must be true")
	}
}

// TestGlobalProbeNarrowListFiltersIDFamilies 窄表形态（data 为字符串数组）：
// 无 tags 可判，仅按 id 家族剔除**无入口**的非对话条目（codewise-* 等）；
// 媒体模型（hunyuan-image-*/kling-*）保留（它们有专用接口）。
func TestGlobalProbeNarrowListFiltersIDFamilies(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":[
			"gpt-5.4","hunyuan-image-alpha-edit","kling-v3-t2v","codewise-rewrite","glm-5.2"
		]}`
	})
	defer srv.Close()

	names := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())
	want := []string{"gpt-5.4", "hunyuan-image-alpha-edit", "kling-v3-t2v", "glm-5.2"}
	if len(names) != len(want) {
		t.Fatalf("narrow global names=%v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("narrow global names=%v want %v", names, want)
		}
	}
	for _, got := range names {
		if got == "codewise-rewrite" {
			t.Error("codewise-rewrite must be filtered (no gateway endpoint)")
		}
	}
}
