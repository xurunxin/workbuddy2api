// model_kind.go 模型分类（kind）与能力（附件）判定的单一事实来源。
//
// 背景（任务书 model-catalog-verify / model-attachments）：
//   - 上游 /v3/config 与 /console/enterprises/personal/models 的 models[] 里混着四类
//     条目：**对话模型**、**自动路由档位**（auto / fast-model / balanced-model /
//     deep-model 等）、**图像/视频模型**（tags 标 text-to-image / image-to-image /
//     text-to-video / image-to-video）、**非对话专用模型**（completion-*/codewise-*、
//     maxOutputTokens≤256 的 tiny 模型）。混淆的后果有两层：客户端把档位当普通模型
//     平铺展示（用户原话「自动、快速、均衡…明显是自动路由模型的档位」）、以及图像模型
//     被当成对话模型送进 /v1/chat/completions（上游 11102/11133 报错）。
//   - 判定只依据**上游下发的客观信号**（id / tags / supportsImages / disabledMultimodal），
//     不维护第二张手工表——上游新增模型时分类自动跟随，不会漏判成 chat。
//
// 术语：kind 是本网关对外的分类枚举（透出到 /v1/models 的 kind 字段与
// routing_models / media_models 分组键）；router tier 是自动路由档位的展示名。
package upstream

import (
	"sort"
	"strings"
)

// ModelKind 模型分类枚举（/v1/models 条目的 kind 字段取值）。
type ModelKind string

const (
	// KindChat 普通对话模型（含推理/多模态对话）。chat/completions 与 responses 的正常服务对象。
	KindChat ModelKind = "chat"
	// KindRouter 自动路由档位（auto / 快速 / 均衡 / 极致 等）：上游按任务自动挑模型的
	// 虚拟档位，不是某个具体模型。单列展示、不并入普通模型列表（用户明确要求）。
	KindRouter ModelKind = "router"
	// KindImage 图像模型（文生图 text-to-image / 图生图 image-to-image）。
	// 服务入口是 /v1/images/generations 与 /v1/images/edits，不是 chat。
	KindImage ModelKind = "image"
	// KindVideo 视频模型（文生视频 / 图生视频）。本网关暂无对应入口，仅分类透出。
	KindVideo ModelKind = "video"
	// KindCompletion 非对话专用模型（代码补全 / NES / 嵌入 / tiny 输出）：
	// 仍不进程目录（沿用 nonChatModel 的历史口径），kind 只为可观测性保留。
	KindCompletion ModelKind = "completion"
)

// modelRouterTiers 自动路由档位白名单：id → 中英展示名。
//
// **刻意用闭集而非「-model 后缀」正则**：分类的错误方向不对称——漏判只是让新档位
// 暂时出现在普通模型里（可见、可纠正），误判会把一个真实对话模型从客户端列表里
// 静默藏掉（不可见、难发现）。故只收录已实测确认的档位 id。
//
// 实测来源（2026-09-18，本机 WorkBuddy 桌面端 cache/acc-product-config-v3*.json，
// 上游 https://copilot.tencent.com/v3/config 的原始响应缓存，models[] 全字段）：
//
//	fast-model     快速  x0.21  in=300000 out=48000 vendor=f isDefault=true
//	balanced-model 均衡  x0.65  in=300000 out=48000 vendor=f
//	deep-model     极致  x1.20  in=300000 out=48000 vendor=f
//	auto           自动  in=168000 out=32000（CN 历史配置）
//
// default-model / primary-model 来自 global（国际版）历史静态名单（global_models.go
// 的 GlobalModelNames，PLAN §7.2 附录），同族档位一并收录。
var modelRouterTiers = map[string]string{
	"auto":           "自动",
	"default":        "默认",
	"default-model":  "默认",
	"fast-model":     "快速",
	"balanced-model": "均衡",
	"primary-model":  "极致",
	"deep-model":     "极致",
}

// RouterTierName 返回自动路由档位的展示名；非档位返回空串。
func RouterTierName(id string) string {
	return modelRouterTiers[normalizeModelID(id)]
}

// IsRouterTier 报告模型 id 是否为自动路由档位。
func IsRouterTier(id string) bool {
	_, ok := modelRouterTiers[normalizeModelID(id)]
	return ok
}

// RouterTierIDs 返回全部已收录的自动路由档位 id（供文档/测试断言，稳定排序）。
func RouterTierIDs() []string {
	out := make([]string, 0, len(modelRouterTiers))
	for id := range modelRouterTiers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// normalizeModelID 归一化模型 id 用于分类比对（去空白 + 小写）。
func normalizeModelID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// ---- tag 判定（上游 tags[] 的客观信号）----

// 上游 tags 里的媒体能力标记（实测取值，见 docs/research 与 v3/config 缓存）：
// text-to-image / image-to-image / text-to-video / image-to-video。
const (
	tagTextToImage  = "text-to-image"
	tagImageToImage = "image-to-image"
	tagTextToVideo  = "text-to-video"
	tagImageToVideo = "image-to-video"
)

// hasTag 报告 tags 是否含指定标记（大小写不敏感、去空白）。
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// IsImageModel 报告模型是否为图像模型：tags 含 text-to-image 或 image-to-image。
//
// 两个 tag 都要认——只认 text-to-image 会让**图像编辑**模型（tags 仅
// image-to-image，实测 hunyuan-image-alpha-edit）漏判成对话模型，正是本次要修的
// 那个 bug（客户端把 hunyuan-image-alpha-edit 当 chat 模型调用必然报错）。
func IsImageModel(id string, tags []string) bool {
	if hasTag(tags, tagTextToImage) || hasTag(tags, tagImageToImage) {
		return true
	}
	// 无 tags 的退化形态（窄表探测/上游未下发 tags）：按 id 家族兜底识别。
	//
	// **只认 hunyuan-image 前缀，不认 kling-**：可灵（kling）是视频家族，其 id
	// （kling-v3-t2v / kling-v3-i2v）虽同属「媒体模型」但归 video 类。用 kling-v
	// 前缀兜底会让视频模型被判成 image（分派错端点，见 TestClassifyModelImageAndVideo）。
	//
	// 宁可多认成图像模型（走专用接口报明确错误），也不要把图像模型放进 chat 通道
	// 让上游报参数错误——这是本前缀兜底存在的理由。
	return strings.HasPrefix(normalizeModelID(id), "hunyuan-image")
}

// IsImageEditModel 报告模型是否为图像编辑（图生图）模型：tags 含 image-to-image。
// 这类模型以**输入图片**为必需入参，服务入口是 /v1/images/edits。
func IsImageEditModel(tags []string) bool {
	return hasTag(tags, tagImageToImage)
}

// IsVideoModel 报告模型是否为视频模型：tags 含 text-to-video 或 image-to-video，
// 或 id 属可灵（kling）家族（无 tags 时的退化兜底）。
//
// 与 IsImageModel 的 id 兜底刻意互斥：hunyuan-image-* → image，kling-* → video。
// 两者的 tag 命名高度相似（image-to-image vs image-to-video），tag 判定已按精确
// 字符串区分，id 兜底也必须各归其类。
func IsVideoModel(id string, tags []string) bool {
	if hasTag(tags, tagTextToVideo) || hasTag(tags, tagImageToVideo) {
		return true
	}
	return strings.HasPrefix(normalizeModelID(id), "kling-")
}

// ---- kind 判定 ----

// ClassifyModel 判定模型分类。判定次序（档位 > 媒体 > 非对话 > 对话）：
//  1. 自动路由档位白名单（router）：档位是虚拟 id，即便将来带上媒体 tag 也仍是档位；
//  2. 图像 tag / id 家族（image）；
//  3. 视频 tag / id 家族（video）；
//  4. 非对话专用（completion，沿用 nonChatModel 的历史口径）；
//  5. 其余为普通对话模型（chat）。
//
// 只看客观信号，未命中任何特征的模型一律归 chat（保守：未知模型按对话模型服务，
// 不因分类而拒绝请求）。
func ClassifyModel(id string, tags []string, maxOutputTokens int64) ModelKind {
	if IsRouterTier(id) {
		return KindRouter
	}
	if IsImageModel(id, tags) {
		return KindImage
	}
	if IsVideoModel(id, tags) {
		return KindVideo
	}
	if isNonChatID(id) || (maxOutputTokens > 0 && maxOutputTokens <= tinyOutputTokens) {
		return KindCompletion
	}
	return KindChat
}

// ---- 附件能力 ----

// AttachmentImage 附件类型枚举：图片（唯一由上游信号可判定的附件类型）。
const AttachmentImage = "image"

// ModelAttachments 返回模型接受的**入站附件**类型（/v1/models 的 attachments 字段）。
//
// 判定只依据上游下发的三个信号，任一不成立即不声称支持（未知 → 空，调用方省略字段，
// 不编造能力）：
//   - supportsImages=true：多模态图片输入；
//   - disabledMultimodal=true：上游**显式**声明关闭多模态（实测 hunyuan-2.0-thinking）
//     ——优先级最高，压过 supportsImages（显式否认 > 隐含肯定）；
//   - tags 含 image-to-image：图生图/图像编辑模型以输入图片为必需入参，即便上游没有
//     置 supportsImages（实测 hunyuan-image-alpha-edit 的模型对象只有 id/name/tags，
//     无 supportsImages 字段），它客观上必然接受图片附件。
//
// 上游当前只下发图片维度的能力信号，故返回值当前只可能是 nil 或 ["image"]；
// 未来上游新增文件/音频信号时在本函数扩展，调用方无需改动。
func ModelAttachments(tags []string, supportsImages, disabledMultimodal bool) []string {
	if disabledMultimodal {
		return nil
	}
	if supportsImages || IsImageEditModel(tags) {
		return []string{AttachmentImage}
	}
	return nil
}

// SupportsAttachments 报告模型是否接受任一附件（/v1/models 的 supports_attachments 布尔）。
func SupportsAttachments(tags []string, supportsImages, disabledMultimodal bool) bool {
	return len(ModelAttachments(tags, supportsImages, disabledMultimodal)) > 0
}
