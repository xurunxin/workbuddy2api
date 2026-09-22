package upstream

import (
	"reflect"
	"testing"
)

// TestClassifyModelRouterTiers 自动路由档位识别（用户明确要求：自动/快速/均衡/极致
// 这些明显是自动路由模型的档位，单列不混入普通模型）。
//
// 白名单闭集：只收录已实测确认的档位 id——漏判只是让新档位暂时出现在普通模型里
// （可见、可纠正），误判会把真实对话模型从客户端列表里静默藏掉（不可见、难发现）。
func TestClassifyModelRouterTiers(t *testing.T) {
	tiers := []struct {
		id, tier string
	}{
		{"auto", "自动"},           // CN 基础档（in=168000/out=32000）
		{"fast-model", "快速"},     // 完整档 x0.21（in=300000/out=48000）
		{"balanced-model", "均衡"}, // 完整档 x0.65
		{"deep-model", "极致"},     // 完整档 x1.20
		{"default-model", "默认"},  // global（国际版）历史静态名单
		{"primary-model", "极致"},  // global 历史静态名单（与 deep-model 同族）
		{"default", "默认"},        // 上游 models[] 全量条目里的别名档位
	}
	for _, c := range tiers {
		if got := ClassifyModel(c.id, nil, 0); got != KindRouter {
			t.Errorf("ClassifyModel(%q)=%q want router", c.id, got)
		}
		if got := RouterTierName(c.id); got != c.tier {
			t.Errorf("RouterTierName(%q)=%q want %q", c.id, got, c.tier)
		}
		if !IsRouterTier(c.id) {
			t.Errorf("IsRouterTier(%q)=false want true", c.id)
		}
	}
	// 大小写/空白容差（归一化）。
	if !IsRouterTier("  AUTO  ") {
		t.Error("IsRouterTier must normalize case and whitespace")
	}
}

// TestClassifyModelNotRouterTiers 反例：普通模型绝不能被误判成档位
// （误判的代价是模型从客户端列表里静默消失）。
func TestClassifyModelNotRouterTiers(t *testing.T) {
	for _, id := range []string{
		"deepseek-v4-pro", "glm-5.2", "kimi-k3-1", "hy3", "hy4-preview",
		"gpt-5.4", "deepseek-v4.1-flash",
		// 名字里带 model 但确是具体模型（不在白名单）。
		"my-model", "fast", "balanced", "deep",
	} {
		if IsRouterTier(id) {
			t.Errorf("%q must NOT be classified as router tier", id)
		}
		if got := ClassifyModel(id, nil, 0); got == KindRouter {
			t.Errorf("ClassifyModel(%q)=router want non-router", id)
		}
	}
}

// TestClassifyModelImageAndVideo 图像/视频模型分类。
//
// 关键回归：**image-to-image 也要认**。实测 hunyuan-image-alpha-edit 的 tags 只有
// image-to-image（没有 text-to-image），旧实现只挡 text-to-image，导致这个图像编辑
// 模型漏网进了对话模型列表——正是本次要修的 bug。
func TestClassifyModelImageAndVideo(t *testing.T) {
	cases := []struct {
		id   string
		tags []string
		want ModelKind
	}{
		// 实测上游条目（/v3/config 原始响应）。
		{"hunyuan-image-alpha", []string{"text-to-image"}, KindImage},
		{"hunyuan-image-alpha-edit", []string{"image-to-image"}, KindImage}, // 本次修复的主角
		{"hunyuan-image-v3.0", []string{"text-to-image"}, KindImage},
		{"hunyuan-image-v3.0-art", []string{"text-to-image", "image-to-image"}, KindImage},
		{"kling-v3-t2v", []string{"text-to-video"}, KindVideo},
		{"kling-v3-i2v", []string{"image-to-video"}, KindVideo},
		// id 家族兜底（上游未下发 tags 的退化形态）。
		{"hunyuan-image-something-new", nil, KindImage},
		{"kling-v4-t2v", nil, KindVideo},
		// 普通对话模型（含多模态对话，tags 无媒体标记）。
		{"glm-5.2", []string{"craft"}, KindChat},
		{"hy3", []string{"craft"}, KindChat},
		{"deepseek-v4.1-flash", nil, KindChat},
		// 非对话专用（沿用 nonChatModel 口径）。
		{"completion-gf", nil, KindCompletion},
		{"codewise-rewrite", nil, KindCompletion},
	}
	for _, c := range cases {
		if got := ClassifyModel(c.id, c.tags, 0); got != c.want {
			t.Errorf("ClassifyModel(%q,%v)=%q want %q", c.id, c.tags, got, c.want)
		}
	}
	// tiny 输出家族（实测上游 hunyuan-3b / hunyuan-7b-dense / codewise-* 的
	// maxOutputTokens=256）——tiny 判定需要真实输出上限参与，故与上表分开断言。
	if got := ClassifyModel("hunyuan-3b", nil, 256); got != KindCompletion {
		t.Errorf("ClassifyModel(hunyuan-3b,256)=%q want completion", got)
	}
	if got := ClassifyModel("hunyuan-7b-dense", nil, 256); got != KindCompletion {
		t.Errorf("ClassifyModel(hunyuan-7b-dense,256)=%q want completion", got)
	}
	// tiny 输出阈值（257 不算 tiny）。
	if got := ClassifyModel("small-257", nil, 257); got != KindChat {
		t.Errorf("ClassifyModel(small-257,257)=%q want chat", got)
	}
	if got := ClassifyModel("small-256", nil, 256); got != KindCompletion {
		t.Errorf("ClassifyModel(small-256,256)=%q want completion", got)
	}
}

// TestClassifyModelRouterBeatsMedia 档位优先于媒体判定：档位是虚拟 id，
// 即便将来上游给档位带上媒体 tag，它仍然是档位（不因 tags 变化而改分类）。
func TestClassifyModelRouterBeatsMedia(t *testing.T) {
	if got := ClassifyModel("fast-model", []string{"text-to-image"}, 0); got != KindRouter {
		t.Errorf("router tier with image tag = %q want router (tier wins)", got)
	}
}

// TestModelAttachments 附件能力判定（任务书 model-attachments）。
//
// 只依据上游下发的客观信号，任一不成立就不声称支持（未知 → 空，不编造能力）。
func TestModelAttachments(t *testing.T) {
	cases := []struct {
		name          string
		tags          []string
		images        bool
		disabledMulti bool
		want          []string
		wantSupports  bool
		reason        string
	}{
		{
			name: "多模态对话模型", tags: []string{"craft"}, images: true,
			want: []string{"image"}, wantSupports: true,
			reason: "supportsImages=true → 接受图片附件",
		},
		{
			name: "纯文本模型", tags: nil, images: false,
			want: nil, wantSupports: false,
			reason: "上游未声明图片能力 → 不声称支持（不编造）",
		},
		{
			name: "显式关闭多模态", tags: nil, images: true, disabledMulti: true,
			want: nil, wantSupports: false,
			reason: "disabledMultimodal=true 压过 supportsImages（显式否认 > 隐含肯定；实测 hunyuan-2.0-thinking）",
		},
		{
			name: "图像编辑模型", tags: []string{"image-to-image"}, images: false,
			want: []string{"image"}, wantSupports: true,
			reason: "图生图模型以输入图片为必需入参，即便上游未置 supportsImages（实测 hunyuan-image-alpha-edit 字段只有 id/name/tags）",
		},
		{
			name: "文生图模型", tags: []string{"text-to-image"}, images: false,
			want: nil, wantSupports: false,
			reason: "纯文生图模型不接受图片输入 → 不声称图片附件（输出是图，不是输入）",
		},
	}
	for _, c := range cases {
		got := ModelAttachments(c.tags, c.images, c.disabledMulti)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ModelAttachments=%v want %v (%s)", c.name, got, c.want, c.reason)
		}
		if s := SupportsAttachments(c.tags, c.images, c.disabledMulti); s != c.wantSupports {
			t.Errorf("%s: SupportsAttachments=%v want %v", c.name, s, c.wantSupports)
		}
	}
}

// TestNonChatModelKeepsMediaModels 回归锚点（本次修正方向反转）：
// nonChatModel 是**目录准入谓词**，图像/视频模型必须**留在目录里**。
//
// 历史：旧实现把 tags 含 text-to-image 的条目挡在目录外（当时网关没有图像入口，
// 挡掉合理）。本网关提供 /v1/images/* 之后该理由失效——目录是客户端发现模型的
// **唯一**渠道，继续挡掉会让图像模型永远不可见、专用接口形同虚设（功能上线即死）。
// 现在的口径：媒体模型进目录 + kind 标记，客户端按 kind 路由；误投对话入口由
// server 侧守卫拦（见 TestChatRejectsImageModel）。
func TestNonChatModelKeepsMediaModels(t *testing.T) {
	for _, c := range []struct {
		id   string
		tags []string
	}{
		{"hunyuan-image-alpha-edit", []string{"image-to-image"}},
		{"hunyuan-image-alpha", []string{"text-to-image"}},
		{"hunyuan-image-v3.0-art", []string{"text-to-image", "image-to-image"}},
		{"kling-v3-t2v", []string{"text-to-video"}},
		{"kling-v3-i2v", []string{"image-to-video"}},
	} {
		if nonChatModel(c.id, 0, c.tags) {
			t.Errorf("%q must STAY in the catalog (media models are discoverable via /v1/models)", c.id)
		}
	}
	// 真正无入口的非对话专用模型仍被剔除。
	for _, id := range []string{"completion-gf", "codewise-rewrite", "nes-embed"} {
		if !nonChatModel(id, 0, nil) {
			t.Errorf("%q must be filtered out (no gateway endpoint serves it)", id)
		}
	}
	// tiny 输出家族（实测 hunyuan-3b / hunyuan-7b-dense 的 maxOutputTokens=256）：
	// 靠 tiny 规则剔除，需要真实输出上限参与判定。
	for _, id := range []string{"hunyuan-3b", "hunyuan-7b-dense", "deepseek-v3-0324-taco-completion"} {
		if !nonChatModel(id, 256, nil) {
			t.Errorf("%q must be filtered out (tiny output ≤256)", id)
		}
	}
	if !nonChatModel("tiny-out", 256, nil) {
		t.Error("maxOutputTokens<=256 must be filtered out")
	}
	// 普通对话模型不受影响。
	for _, id := range []string{"glm-5.2", "hy3", "deepseek-v4.1-flash", "kimi-k3-1", "auto", "fast-model"} {
		if nonChatModel(id, 0, nil) {
			t.Errorf("%q must NOT be filtered out of the catalog", id)
		}
	}
}

// TestIsImageModelIDFallback id 家族兜底：无 tags 时按 id 前缀识别图像模型
// （上游窄表探测/未下发 tags 的退化形态）。
func TestIsImageModelIDFallback(t *testing.T) {
	for _, id := range []string{"hunyuan-image-alpha", "hunyuan-image-alpha-edit", "hunyuan-image-v4"} {
		if !IsImageModelID(id) {
			t.Errorf("%q must be recognized as image model by id family", id)
		}
	}
	for _, id := range []string{"glm-5.2", "hunyuan-chat", "hunyuan-2.0-instruct"} {
		if IsImageModelID(id) {
			t.Errorf("%q must NOT be recognized as image model", id)
		}
	}
}

// TestRouterTierIDsStable 档位清单稳定排序（供文档/断言；不依赖 map 迭代序）。
func TestRouterTierIDsStable(t *testing.T) {
	first := RouterTierIDs()
	second := RouterTierIDs()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("RouterTierIDs unstable: %v vs %v", first, second)
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Fatalf("RouterTierIDs not sorted: %v", first)
		}
	}
}
