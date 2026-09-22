// images.go 图像模型（text-to-image / image-to-image）的上游调用与网关侧
// /v1/images/* API 服务的上游半部分。
//
// ---- 定位与边界（任务书 image-api）----
//
// 上游模型目录里的图像模型（实测：hunyuan-image-alpha 文生图、hunyuan-image-alpha-edit
// 图生图、hunyuan-image-v3.0 / v3.0-art）**不是对话模型**：把它们送进
// /v1/chat/completions 会被上游按 11102/11133 拒绝。本文件让 coding agent
// （Claude Code / Codex / dsh 等）能像调用 chat 一样，通过网关账号池调用它们。
//
// ---- 实测结论（2026-09-18 真机联调，OpenWrt 网关部署后逐项验证）----
//
// 以下**均已实测确认**，不再是推测：
//
//   - **端点路径**：`/v2/images/generations`（文生图）与 `/v2/images/edits`（图生图）。
//     真机调用 generations 返回了真实生成结果（腾讯云 COS 签名 URL），edits 返回了
//     上游业务错误（见下条）——两者都证明路径正确、确实打到了上游对应 handler。
//   - **请求体 schema**：上游 `EditImageRequest.image` 的 Go 类型是 **`[]string`**。
//     字符串形态被上游明确拒绝：
//     `{"code":11101,"msg":"Unmarshal create image params failed with error:
//     json: cannot unmarshal string into Go struct field EditImageRequest.image
//     of type []string"}`。
//     故 ImageRequest.Image 用切片，网关对外再由 handler 兼容单字符串形态。
//   - **响应形态**：`{"created":…,"data":[{"url":"https://…myqcloud.com/…png?q-sign-…"}]}`
//     （带签名的临时 URL），与 OpenAI Images 形状一致，parseImageResponse 直接可用。
//   - 模型 id 与 tags（/v3/config 与 /console/enterprises/personal/models 原始响应）；
//   - 图像模型在模型目录里**没有** maxInputTokens/maxOutputTokens/supportsImages
//     （实测字段只有 id/name/tags）；
//   - 官方客户端的图像能力是**独立工具**（ImageGen / ImageEdit，见 /v3/config 的
//     tools[] 与 prompts[] 里的 tool-imagegen-description / tool-imageedit-description），
//     不经过 chat completions；官方工具描述明确「生成图像会调用额外模型并产生独立
//     积分消耗，单张约 5-10 积分」，与 hunyuan-image-v3.0-art 的 credits=x5.00 印证。
//
// 当初为何找不到端点（保留作排查记录）：本机所有 WorkBuddy 客户端产物
// （app.asar 全量字符串、内置插件、日志、产品配置缓存）里都没有它——"images/generations"
// 的命中全来自第三方 openai SDK 与 PDF 渲染器；app.asar 里图像调用走 ACP 工具桥
// （ImageGenToolAdapter，工具名 image_gen / image_edit），**执行的宿主在服务端**，
// 端点不打包进客户端。端点最终是靠真机调用上游错误信息反推确认的。
//
// 路径仍保留配置覆盖（upstream.image_generate_path / image_edit_path）以备上游变更。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// 图像端点默认路径（**已实测确认**，见文件头「实测结论」）。
//
// CN 与 global 各自可配：两区是同一套 API 的两次部署（fork 706412584 结论），但
// 端点可用性须分别验证——global 侧历史多次出现「/console 500、/v2 200」这类家族
// 差异，不能假定 CN 通了 global 也通。
const (
	defaultImageGeneratePath = "/v2/images/generations"
	defaultImageEditPath     = "/v2/images/edits"
)

// ImageRequest 一次图像生成/编辑请求（网关侧 /v1/images/* 解析后的归一形态）。
//
// Prompt 必填；Image 非空即图生图（对应上游 image-to-image 模型），为空即文生图。
// Image 支持 http(s) URL 与 data:image/...;base64, 两种形态（与 responses_media.go
// 的 validateMediaURL 同口径）。
//
// **Image 是切片（[]string）——上游实测要求**：2026-09-18 网关真机联调，上游对
// 字符串形态的 image 明确拒绝：
//
//	{"code":11101,"msg":"Unmarshal create image params failed with error:
//	 json: cannot unmarshal string into Go struct field EditImageRequest.image of type []string"}
//
// 即上游 `EditImageRequest.image` 的 Go 类型是 `[]string`（支持多图输入）。
// 网关对外仍**兼容单字符串**（OpenAI Images API 的惯例是单图），由 handler 归一成
// 切片后出站——见 server.imageRequest 的 image/image_url 兼容解析。
type ImageRequest struct {
	Model  string   `json:"model"`           // 裸模型名（无 realm 前缀；由 handler 剥离）
	Prompt string   `json:"prompt"`          // 必填：生成/编辑指令
	Image  []string `json:"image,omitempty"` // 图生图输入（URL 或 data URI）；上游要求数组
	N      int      `json:"n,omitempty"`     // 生成张数（0 → 省略，用上游默认）
	Size   string   `json:"size,omitempty"`  // 尺寸（如 "1024x1024"；空 → 省略）
	Seed   *int64   `json:"seed,omitempty"`  // 随机种子（nil → 省略）
}

// ImageResponse 上游图像响应（本网关对外的响应体）。
//
// **已实测**：上游返回 `{"created":…,"data":[{"url":"https://…myqcloud.com/…png?q-sign-…"}]}`
// ——即 OpenAI Images 的公开形状（带签名的临时 URL，非 b64 内联）。故本结构直接
// 按该形状定义，无需私有字段映射；b64_json 保留以兼容可能返回内联 base64 的形态。
type ImageResponse struct {
	Created int64       `json:"created"`
	Data    []ImageItem `json:"data"`
	// Model 实际服务的模型（网关补写，便于客户端确认路由结果）。
	Model string `json:"model,omitempty"`
	// Usage 上游若下发消耗则透出（未知则省略，不编造）。
	Usage *ImageUsage `json:"usage,omitempty"`
}

// ImageItem 单张图片结果：b64_json 与 url 至少一个非空。
type ImageItem struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
	// RevisedPrompt 上游改写后的提示词（未下发则省略）。
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageUsage 图像消耗（上游下发才透出）。
type ImageUsage struct {
	Credits *float64 `json:"credits,omitempty"`
	Images  int      `json:"images,omitempty"`
}

// imagePath 按 realm 返回图像端点路径：Client 显式配置优先，否则用内置默认。
// edit 区分生成/编辑两个端点。
func (c *Client) imagePath(a *auth.Auth, edit bool) string {
	configured := c.ImageGeneratePath
	if edit {
		configured = c.ImageEditPath
	}
	if configured != "" {
		return configured
	}
	if edit {
		return defaultImageEditPath
	}
	return defaultImageGeneratePath
}

// ImageGenerate 调用上游图像生成（文生图）或图像编辑（图生图）端点。
//
// 与 chat 路径共享同一套基础设施：账号池挑号由调用方（handler）负责，本方法只做
// 「给定账号发一次请求」。请求头复用 CommonHeaders（Origin/Referer/UA/X-IDE-*，
// 官方归因头），鉴权用账号 access token。
//
// 返回值：(响应, HTTP 状态码, 原始 body, error)。状态码 >=400 时 error 为
// *Error（经 Classify 分类，与 chat 路径同口径——调用方据此走既有退避/冷却策略）；
// 200 但响应不可解析同样返回 error（不假装成功、不编造一张空图）。
// body 读失败返回传输层错误（半截 body 不进解析，防误判罚号）。
func (c *Client) ImageGenerate(ctx context.Context, a *auth.Auth, req ImageRequest) (*ImageResponse, int, []byte, error) {
	edit := len(req.Image) > 0
	url := c.chatBase(a) + c.imagePath(a, edit)

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("image payload marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	c.CommonHeaders(httpReq, a)
	httpReq.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.imageBodyLimit()))
	if err != nil {
		return nil, resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			return nil, resp.StatusCode, raw, nil
		}
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		}
		return nil, resp.StatusCode, raw, ue
	}
	out, perr := parseImageResponse(raw)
	if perr != nil {
		return nil, resp.StatusCode, raw, fmt.Errorf("image response parse: %w", perr)
	}
	return out, resp.StatusCode, raw, nil
}

// imageBodyLimit 图像响应体上限：图片以 base64 内联时体积远大于文本
// （单张 1024x1024 PNG 约 1-3MB，base64 后 ×1.37），给 64MB 余量。
func (c *Client) imageBodyLimit() int64 {
	if c.ImageBodyLimit > 0 {
		return c.ImageBodyLimit
	}
	return 64 << 20
}

// parseImageResponse 解析上游图像响应为 ImageResponse。
//
// 容忍两种形状（后者是前者的业务 code 包装，聚合网关常见）：
//  1. 裸形态：{"created":N,"data":[{"b64_json"|"url",...}]}
//  2. 包装形态：{"code":0,"data":{"created":N,"data":[...]}}
//
// data 为空 → 报错（不返回「成功但没有图」的响应，那会让客户端静默拿到空结果）。
func parseImageResponse(raw []byte) (*ImageResponse, error) {
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	body := raw
	// 包装形态判定：有 data 键且 data 是**对象**（裸形态的 data 是数组）。
	trimmed := bytes.TrimSpace(env.Data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if env.Code != 0 {
			return nil, fmt.Errorf("upstream code=%d", env.Code)
		}
		body = env.Data
	}
	var out ImageResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("no image in response")
	}
	if out.Created == 0 {
		out.Created = time.Now().Unix()
	}
	return &out, nil
}

// IsImageModelID 报告 id 是否为图像模型（无 tags 时的 id 家族兜底，供 handler
// 在「请求的模型不在目录里」时决定要不要给图像接口指路）。
func IsImageModelID(id string) bool { return IsImageModel(id, nil) }
