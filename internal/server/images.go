// images.go 网关侧 /v1/images/* API 服务：把上游的**图像模型**
// （hunyuan-image-alpha 文生图 / hunyuan-image-alpha-edit 图生图等）暴露成 coding
// agent 可调用的 HTTP 接口。
//
// ---- 为什么需要独立接口（任务书 image-api）----
//
// 图像模型不是对话模型：它们不接受 messages，走 /v1/chat/completions 会被上游按
// 11102/11133 拒绝。但它们与对话模型**共享同一个账号池与积分**（实测图像模型
// hunyuan-image-v3.0-art 的 credits=x5.00，官方工具描述亦称「生成图像会调用额外模型
// 并产生独立积分消耗，单张约 5-10 积分」）。所以正确形态不是「网关不列它」，而是
// 「列出来 + 给它自己的入口」：coding agent 读 /v1/models 发现 kind=image，
// 转手调 /v1/images/generations（文生图）或 /v1/images/edits（图生图）。
//
// ---- 路由与账号（与 chat 完全同构）----
//
//   - model 支持 "[realm:]model" 前缀（resolveModel），与 chat 同一套协议：
//     `global:hunyuan-image-alpha-edit` 走国际区账号，裸名走中国区；
//   - 选号复用 Pool.PickExcludingForRealm（realm 过滤 + 在途名额 + 冷却/熔断），
//     与 chat 同一条轮转/退避/错误处置链路（applyErrorPolicy、rotateBackoff、
//     WAF IP fail-fast、NoteError/NoteSuccess/NoteModelCost）——图像调用同样吃账号
//     配额，不能绕过池治理；
//   - 不做会话粘性与系统提示词改写：图像请求无 messages，粘性键无意义。
//
// ---- 与 chat 的差异（刻意）----
//
//   - 请求体上限更小（默认与 chat 同用 server.max_body_mb；图像入参是 prompt +
//     一张图，通常远小于长对话）；
//   - 不注入 effort / thinking / prompt_cache_key：那些是对话协议字段；
//   - 响应不做 SSE 流式：上游图像生成通常一次性返回（官方工具描述称耗时约 1-3 分钟，
//     但那是视频；图像为同步返回）。若将来确认上游支持流式，在此文件加分支即可。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// imagesBodyLimit 图像请求体上限（默认与 chat 共用 MaxBodyBytes）。
// 单独取值的意义：图像入参是 prompt + 一张内联图（base64 后约 1-4MB 常见），
// 用 chat 的上限（20MB）足够；此处只做防御性下限，避免 MaxBodyBytes 被配成极小值时
// 图像接口连一张图都收不下。
const imagesMinBodyBytes int64 = 8 << 20

// imageRequest 入站请求体（OpenAI Images API 兼容 + 少量扩展）。
//
// Image 用 json.RawMessage 接：上游要求数组（[]string，实测 11101），而 OpenAI 惯例
// 与多数客户端是单字符串，两种形态都要收（见 images 字段解析的 imageStrings）。
type imageRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Image          json.RawMessage `json:"image,omitempty"`
	ImageURL       json.RawMessage `json:"image_url,omitempty"` // image 的别名（OpenAI 风格命名）
	N              int             `json:"n,omitempty"`
	Size           string          `json:"size,omitempty"`
	Seed           *int64          `json:"seed,omitempty"`
	ResponseFormat string          `json:"response_format,omitempty"` // 仅接受 b64_json/url（透传下游时忽略）
}

// imageStrings 把 image / image_url 字段归一成 []string，容忍三种入参形态：
//   - 字符串："https://..." 或 "data:image/png;base64,..."（OpenAI 惯例，单图）；
//   - 字符串数组：["https://...", ...]（上游原生形态，多图）；
//   - 缺失/null：空切片（文生图）。
//
// 形态非法（数字/对象等）→ 返回错误（本地 400 比让上游报 11101 可读）。
func imageStrings(raw json.RawMessage, field string) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	// 单字符串形态。
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if s = strings.TrimSpace(s); s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	// 数组形态（元素必须都是字符串）。
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		out := make([]string, 0, len(arr))
		for _, s := range arr {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s must be a string or an array of strings", field)
}

func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	h.imagesEndpoint(w, r, false)
}

func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	h.imagesEndpoint(w, r, true)
}

// imagesEndpoint 图像生成（edit=false）/ 图像编辑（edit=true）的统一实现。
//
// edit=true 强制要求入参带图（图生图语义——无图无法编辑）；edit=false 允许带图
// （官方 ImageGen 工具描述：带 image 参数即执行 image-to-image，否则纯文生图），
// 但带图时会自动改投编辑端点（endpoint 语义优先于工具语义）。
func (h *Handler) imagesEndpoint(w http.ResponseWriter, r *http.Request, edit bool) {
	limit := h.cfg.MaxBodyBytes
	if limit < imagesMinBodyBytes {
		limit = imagesMinBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：图像入参请改用可访问的 URL 而非内联 base64，或调大 server.max_body_mb", limit>>20))
		return
	}
	var in imageRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	// image / image_url 两种字段名都收（避免客户端猜错字段名），并归一成切片
	// （上游要求 []string，见 imageStrings 注释）。
	images, err := imageStrings(in.Image, "image")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(images) == 0 {
		if images, err = imageStrings(in.ImageURL, "image_url"); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if edit && len(images) == 0 {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "image_required",
			"/v1/images/edits requires an input image (image or image_url)",
			"image-to-image models need an input image; pass \"image\" as an https URL or a data:image/...;base64, URI (use /v1/images/generations for text-to-image)")
		return
	}
	// 入参图片形态预校验（http(s) URL 或 data:image/...;base64）。
	// 不做远程拉取（与 responses_media.go 同纪律：不替客户端取图、不泄露 URL 内容）。
	for _, img := range images {
		if err := validateMediaURL(img, "image"); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image: "+err.Error())
			return
		}
	}

	realm, bareModel := resolveModel(strings.TrimSpace(in.Model))
	if bareModel == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	// 请求打了 chat 端点才该用的模型（如 deepseek-v4-pro）→ 明确报错，
	// 不让上游用一个语焉不详的 400 打发客户端。
	if !h.isImageCapableModel(bareModel) {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "not_an_image_model",
			"model "+bareModel+" is not an image model",
			"call /v1/models and pick an entry with kind=image (media_models lists them all)")
		return
	}

	req := upstream.ImageRequest{
		Model:  bareModel,
		Prompt: strings.TrimSpace(in.Prompt),
		Image:  images,
		N:      in.N,
		Size:   strings.TrimSpace(in.Size),
		Seed:   in.Seed,
	}
	// 端点语义与入参图片对齐：带图（或编辑端点）即走图生图端点。
	if len(req.Image) > 0 {
		edit = true
	}

	st := newChatStat(time.Now(), nil, false)
	st.model = "image:" + bareModel
	st.mode = "image"
	defer st.done()

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid, st.nick = acct.UID, acct.Nickname
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				h.cfg.Pool.Release(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break
				}
				continue
			}
			acct.BackfillRealm()
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("ERR: [server] image refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		resp, status, respBody, terr := h.cfg.Upstream.ImageGenerate(r.Context(), acct, req)
		h.cfg.Pool.Release(acct.UID)

		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil && status < 400 {
			// 传输层抖动 / 200 但响应不可解析：换号，不喂熔断（与 chat 同口径）。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		if status >= 400 {
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 与 chat 同一条错误处置链：冷却/熔断/连败/模型级 11102 避让。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			// 请求自身的问题（内容拦截/提示词超限/参数非法）：换号无意义，立即回客户端。
			if kind == upstream.ErrContentBlocked || kind == upstream.ErrPromptTooLong {
				h.writeImageUpstreamError(w, kind, status, string(respBody), bareModel, uerr)
				st.status = http.StatusBadRequest
				return
			}
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}

		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 实测消耗入账（与 chat 同一条成本账本：图像模型单价高，账本让选号把便宜号排前面）。
		// tokens 位传"图片张数"：图像调用没有 token 计数，用张数作分母算每张单价，
		// 语义与 chat 的"每千 token 单价"平行（同一账本、不同计价单位，tier 判定
		// 只看 credit 正负，故不冲突）。
		if resp.Usage != nil && resp.Usage.Credits != nil {
			n := resp.Usage.Images
			if n <= 0 {
				n = len(resp.Data)
			}
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, *resp.Usage.Credits, n)
		}
		resp.Model = bareModel
		st.status = http.StatusOK
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 末端错误透传（与 chat 同口径：上游原文优先，本地错误用固定文案）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		if ue.Kind == upstream.ErrSoftRate {
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			msg = s
		}
	} else if lastErr != nil && !errors.As(lastErr, &ue) {
		// 传输层/解析错误：没有上游原文可透传，用可读文案（不编造上游 body）。
		msg = lastErr.Error()
		if strings.Contains(msg, "image response parse") {
			status, code = http.StatusBadGateway, "upstream_parse"
		}
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
}

// writeImageUpstreamError 图像路径的「请求自身问题」错误透传（内容拦截 / 提示词超限）：
// 状态码与 code 与 chat 路径同口径，message 仍是上游原文（error-passthrough 纪律）。
func (h *Handler) writeImageUpstreamError(w http.ResponseWriter, kind upstream.ErrKind, status int, body, bareModel string, uerr *upstream.Error) {
	code, msg := "invalid_request", strings.TrimSpace(body)
	switch kind {
	case upstream.ErrContentBlocked:
		code = "content_blocked"
		if msg == "" {
			msg = "content blocked by upstream content firewall"
		}
	case upstream.ErrPromptTooLong:
		code = "prompt_too_long"
		if msg == "" {
			msg = "prompt is too long"
		}
	}
	if msg == "" {
		msg = "upstream rejected the request"
	}
	writeOpenAIErrorHint(w, status, code, msg, upstream.GatewayHint(kind, body, upstream.HintContext{Model: bareModel}))
}

// isImageCapableModel 报告模型是否为图像模型（/v1/images/* 的可服务对象）。
func (h *Handler) isImageCapableModel(bareModel string) bool {
	return h.modelKindOf(bareModel) == upstream.KindImage
}

// isImageEditModel 报告模型是否为图像编辑（图生图）模型。
// 目录（缓存）命中取 tags（image-to-image）；未命中保守返回 false（走 generations，
// 由上游决定是否接受——比猜错端点更可诊断）。
func (h *Handler) isImageEditModel(bareModel string) bool {
	edit, _ := h.imageEditKind(bareModel)
	return edit
}

// imageEditKind 报告模型是否为图生图，以及该判定**是否可信**（目录缓存命中）。
// known=false 表示只能用 id 家族猜测（hunyuan-image-*-edit 之类），此时调用方
// 应并列给出两个端点而不是猜一个（见 chatCompletions 的图像模型守卫）。
func (h *Handler) imageEditKind(bareModel string) (edit, known bool) {
	for _, mi := range cachedMediaSnapshot() {
		if mi.ID == bareModel {
			return upstream.IsImageEditModel(mi.Tags), true
		}
	}
	return false, false
}

// modelKindOf 查模型分类（chat/router/image/video/completion）。
//
// **只读缓存，绝不发起上游调用**——与 hintContext 同一条纪律。这是本函数的关键约束：
// 它被 chat 主路径的「图像模型误投」守卫调用，若在此触发目录拉取，每个 chat 请求都会
// 在缓存冷时多打一次上游模型接口（chat 与模型目录本不该耦合，且会放大上游请求量）。
//
// 三级取值：
//  1. 目录缓存命中（TTL 内）→ 用条目 kind（最准：含 tags 判定）；
//  2. 缓存冷/未收录 → upstream.ClassifyModel 的 id 家族判定（档位白名单 +
//     hunyuan-image-* / kling-* / completion-* 前缀）——覆盖全部实测存在的媒体模型；
//  3. 都判不出 → chat（保守：未知模型按对话模型放行，交上游裁决，不误拒正常请求）。
func (h *Handler) modelKindOf(bareModel string) upstream.ModelKind {
	for _, mi := range cachedMediaSnapshot() {
		if mi.ID != bareModel {
			continue
		}
		if mi.Kind != "" {
			return mi.Kind
		}
		return upstream.ClassifyModel(mi.ID, mi.Tags, mi.MaxTokens)
	}
	return upstream.ClassifyModel(bareModel, nil, 0)
}

// cachedMediaSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 供分类判定使用（不发起上游调用，见 modelKindOf 注释）。复用 cachedModelsSnapshot
// 的 CN 缓存——global 域模型在 CN 缓存未命中时由 id 家族判定兜底。
func cachedMediaSnapshot() []upstream.ModelInfo { return cachedModelsSnapshot() }
