// context_catalog.go context_length / max_output_tokens 字段级静态兜底知识表。
//
// 数据来源四级（model-json-dynamic 任务书；查找链入口在 model_catalog.go 的
// ContextWindowListingV4 / MaxOutputTokensListingV4，本文件是第 2 级）：
//   - 上游动态值（ModelInfo.ContextWindow/MaxTokens，即 maxInputTokens/maxOutputTokens）权威，优先；
//   - 本文件静态知识表（远端零值时补齐；model.json 缺失/损坏时的编译期兜底）；
//   - model.json 本地缓存（数据目录，含运行时 models.dev 按需补值，见 model_catalog.go）；
//   - models.dev 按需拉取（异步不阻塞；仍未知 → context_length 1M 兜底——宁可高估
//     不低估：高估代价是客户端不截断、上游报错可重试；低估代价是下游客户端
//     （Codex/ZCode/Claude Code 按 context_length 提前截断）白白丢上下文；
//     max_output_tokens 未知 → 省略字段（输出上限无合估算据，不编造））。
//
// 知识表**一处定义、CN/global 两域共用**：context_length 是模型固有属性——fork
// 706412584 实测结论「两区是同一套 API 的两次部署」，同 id 上下文一致，无按 realm
// 分表必要（与 effort 档位的 realm 分表刻意不同）。
//
// ---- 取值口径（2026-09-18 全面核对，任务书 model-catalog-verify）----
//
// **本节是唯一权威说明；每条注释标注该条的实测档位。**
//
// 实测来源：本机 WorkBuddy 桌面端缓存的 13 份 `/v3/config` **原始上游响应**
// （`~/.workbuddy/cache/acc-product-config-v3*.json`，2026-09-08 ~ 2026-09-18），
// 按 `agents[name=cli].models` 取账号实际可选模型，逐条读 maxInputTokens /
// maxOutputTokens。这不是文档摘抄，是上游真实下发值。
//
// 本次核对推翻的历史取值（旧表按 models.dev 收录值填，与上游实测冲突）：
//   - kimi-k3 → **id 不存在**。CN 账号实际下发的 id 是 `kimi-k3-1`（显示名 Kimi-K3）。
//     旧表用不存在的 id 做兜底键，等于该模型永远查不到兜底值（恒落 1M）。
//   - 各 GLM/Kimi/DeepSeek 的 maxOutput 旧表普遍填 131072 / 262144 / 384000 / 512000
//     （models.dev 收录值），而上游实际下发 32000~128000。**兜底值高于真实上限**比省略
//     更危险：客户端据此认为可产出 131072 输出，请求被上游截断或报错。
//   - kimi-k2.5 context 旧 164000 → 实测 256000。
//   - hy3-preview context 旧 262144 → 实测 192000（与 hy3 同窗口）。
//   - kimi-k2.8-preview context 旧 1048576 → 实测 1000000（上游用十进制 1M）。
//     本表统一采用**上游十进制口径**（1000000 而非 1048576）：兜底值与上游一致才不会
//     在「远端零值→兜底→远端有值」之间来回跳变。
//
// 同一 id 跨档位取值分歧（实测两个账号档位的 maxOutput 不同）：
//
//	glm-5.2 / glm-5.3          48000（基础档） / 64000（完整档）
//	glm-5v-turbo               38000 / 64000
//	minimax-m3                 128000 / 64000
//	deepseek-v4-pro            50000 / 128000
//	fast/balanced/deep-model   档位条目只在完整档下发
//
// 分歧时本表取**较大值**：兜底表只在远端零值时生效，取大值意味着「不低估上限」；
// 且这是两个**实测值**之间的取舍，不是编造（编造指无依据地填一个数）。
//
// 未收录或歧义大者不编造（保持省略，走 1M / 字段省略）。
package upstream

// DefaultContextWindow 知识表也未收录的模型的 context_length 兜底：1M。
// 上游多数大窗口模型的实际量级；高估优于低估（见文件头）。
const DefaultContextWindow int64 = 1000000

// contextCap 一个模型的上下文能力（字段级兜底条目）。
// context 必为正（否则条目无意义，直接走 1M 兜底）；
// maxOutput 为 0 表示输出上限未知 → max_output_tokens 字段省略（不编造）。
type contextCap struct {
	context   int64
	maxOutput int64
}

// contextCapFallback context_length / max_output_tokens 知识表（CN/global 共用）。
// 每条的取值来源见文件头「取值口径」；注释里的「实测」指上述 13 份 /v3/config 原始响应。
//
// 覆盖范围原则：**只收录实测观测过 cli 名单里出现过的 id**，以及 global 域名下
// 无法直测、按官方公开值保守填的模型（gpt-* / gemini-*）。上游目录里存在但从不进
// cli 名单的模型（nes-/codewise-/completion- 等非对话条目）不收录——它们本就被
// nonChatModel 挡在对话目录外，收录只会让表膨胀。
var contextCapFallback = map[string]contextCap{
	// ---- 自动路由档位（models/router；不是具体模型，透出时单列）----
	// ctx=168000 是基础档（auto）；300000 是完整档（fast/balanced/deep）。
	// 同一虚拟档位跨档位不同窗口，取大值。
	"auto":           {context: 168000, maxOutput: 32000}, // 实测（基础档 cli 名单；out 32000）
	"default":        {context: 200000, maxOutput: 24000}, // 实测（无 cli 名单，models[] 全量条目）
	"fast-model":     {context: 300000, maxOutput: 48000}, // 实测（完整档 x0.21；in 300000/out 48000）
	"balanced-model": {context: 300000, maxOutput: 48000}, // 实测（完整档 x0.65；in 300000/out 48000）
	"deep-model":     {context: 300000, maxOutput: 48000}, // 实测（完整档 x1.20；in 300000/out 48000）

	// ---- GLM 家族（z-ai）----
	"glm-5.3":       {context: 1000000, maxOutput: 64000}, // 实测（in 1000000；out 48000 基础档 / 64000 完整档）
	"glm-5.3-flash": {context: 1000000, maxOutput: 32000}, // 实测（in 1000000/out 32000）
	"glm-5.2":       {context: 1000000, maxOutput: 64000}, // 实测（in 1000000；out 48000 基础档 / 64000 完整档）
	"glm-5.1":       {context: 200000, maxOutput: 48000},  // 实测（in 200000/out 48000）
	"glm-5.0-turbo": {context: 200000, maxOutput: 48000},  // 实测（cli 名单 09-18 基础档；in 200000/out 48000）
	"glm-5.0":       {context: 200000, maxOutput: 48000},  // 实测（cli 名单 09-18 基础档；in 200000/out 48000）
	"glm-4.7":       {context: 200000, maxOutput: 48000},  // 实测（cli 名单 09-18 基础档；in 200000/out 48000）
	"glm-4.6":       {context: 168000, maxOutput: 32000},  // 实测（in 168000/out 32000）
	"glm-4.6v":      {context: 128000, maxOutput: 32000},  // 实测（in 128000/out 32000，多模态 V 系）
	"glm-5v-turbo":  {context: 200000, maxOutput: 64000},  // 实测（in 200000；out 38000 基础档 / 64000 完整档）

	// ---- Kimi 家族（moonshot）----
	"kimi-k3-1":         {context: 1000000, maxOutput: 32000}, // 实测（显示名 Kimi-K3；in 1000000/out 32000）
	"kimi-k2.8-preview": {context: 1000000, maxOutput: 64000}, // 实测（in 1000000/out 64000，与其他 k2.x 不同）
	"kimi-k2.7":         {context: 256000, maxOutput: 32000},  // 实测（显示名 Kimi-K2.7-Code；in 256000/out 32000）
	"kimi-k2.6":         {context: 256000, maxOutput: 32000},  // 实测（in 256000/out 32000）
	"kimi-k2.5":         {context: 256000, maxOutput: 32000},  // 实测（in 256000/out 32000；旧表 164000 系 global 外推，已纠正）
	"kimi-k2-thinking":  {context: 256000, maxOutput: 32000},  // 实测（in 256000/out 32000）
	// kimi-k3 是 global（国际版）历史静态名单里的 id（global_models.go GlobalModelNames，
	// PLAN §7.2 附录）；CN 侧从未下发。保留作 global 兜底键，与 CN 的 kimi-k3-1 并存
	// （两者不同 id，绝不互相覆盖）。1M/131072 取 models.dev moonshotai 官方收录值——
	// global 无法直测，且该 id 未在任何实测快照出现过。
	"kimi-k3": {context: 1048576, maxOutput: 131072},

	// ---- MiniMax / 混元（tencent）----
	"minimax-m3":           {context: 512000, maxOutput: 128000}, // 实测（in 512000；out 128000 基础档 / 64000 完整档）
	"minimax-m2.7":         {context: 200000, maxOutput: 48000},  // 实测（in 200000/out 48000）
	"minimax-m2.5":         {context: 200000, maxOutput: 48000},  // 实测（in 200000/out 48000）
	"hy3":                  {context: 192000, maxOutput: 64000},  // 实测（in 192000/out 64000）
	"hy3-x":                {context: 192000, maxOutput: 64000},  // 实测（显示名同为 Hy3，但 id/倍率不同——不可按显示名合并）
	"hy3-preview":          {context: 192000, maxOutput: 64000},  // 实测（in 192000/out 64000；旧表 262144 已纠正）
	"hy4-preview":          {context: 1000000, maxOutput: 64000}, // 实测（in 1000000/out 64000）
	"hy4-preview-x":        {context: 1000000, maxOutput: 64000}, // 实测（in 1000000/out 64000）
	"hy4-preview-dev":      {context: 1000000, maxOutput: 64000}, // 实测（in 1000000/out 64000，与 hy4-preview 同值）
	"hunyuan-2.0-instruct": {context: 128000, maxOutput: 16000},  // 实测（in 128000/out 16000）
	"hunyuan-2.0-thinking": {context: 128000, maxOutput: 24000},  // 实测（in 128000/out 24000；该模型 disabledMultimodal=true）
	"hunyuan-chat":         {context: 128000, maxOutput: 8192},   // 实测（显示名 Hunyuan-Turbos；in 128000/out 8192）

	// ---- DeepSeek 家族 ----
	"deepseek-v4-pro":     {context: 1000000, maxOutput: 128000}, // 实测（in 1000000；out 50000 基础档 / 128000 完整档）
	"deepseek-v4-flash":   {context: 1000000, maxOutput: 50000},  // 实测（in 1000000/out 50000）
	"deepseek-v4.1-flash": {context: 1000000, maxOutput: 128000}, // 实测（in 1000000/out 128000）
	"deepseek-v3-2-volc":  {context: 96000, maxOutput: 32000},    // 实测（显示名 DeepSeek-V3.2；in 96000/out 32000）
	"deepseek-v3-1-volc":  {context: 96000, maxOutput: 32000},    // 实测（in 96000/out 32000）
	"deepseek-v3-1-lkeap": {context: 96000, maxOutput: 32000},    // 实测（in 96000/out 32000）
	"deepseek-v3-1":       {context: 96000, maxOutput: 32000},    // 实测（in 96000/out 32000）
	// v3 系列老条目：实测 96K~112K 上下文，**绝不能落 1M 兜底**（会误导客户端不截断）。
	"deepseek-v3-0324-lkeap": {context: 112000, maxOutput: 16000}, // 实测（in 112000/out 16000）
	"deepseek-v3-0324":       {context: 96000, maxOutput: 8192},   // 实测（in 96000/out 8192）
	"deepseek-r1-0528-lkeap": {context: 96000, maxOutput: 16000},  // 实测（in 96000/out 16000）
	"deepseek-r1-0528":       {context: 96000, maxOutput: 8192},   // 实测（in 96000/out 8192）

	// ---- 上游 default-* 别名条目（models[] 里指向具体 Claude 系）----
	"default-1.1": {context: 200000, maxOutput: 8192},  // 实测（显示名 Claude-3.7-Sonnet；in 200000/out 8192）
	"default-1.2": {context: 200000, maxOutput: 24000}, // 实测（显示名 Claude-4.0-Sonnet；in 200000/out 24000）

	// ---- 第三方托管条目 ----
	"kimi-k2-instruct-taiji": {context: 31000, maxOutput: 8192}, // 实测（in 31000/out 8192，火山/太极托管）

	// ---- OpenAI / Google（global 域家族）----
	// 以下模型**不在 CN 目录任何快照里**（CN 侧仅有国产模型），属 global 域；
	// global 无法直测，取 models.dev 全 provider 共识值。保留作 global 兜底键。
	"gpt-6-astra":      {context: 1050000, maxOutput: 128000}, // models.dev 共识（全 provider 一致）
	"gpt-5.6-sol":      {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.6-terra":    {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.6-luna":     {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.5":          {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.4":          {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.3-codex":    {context: 400000, maxOutput: 128000},  // models.dev（全 provider 一致 400000/128000）
	"gemini-3.5-flash": {context: 1048576, maxOutput: 65536},  // models.dev 共识（Google 用二进制 1M 口径）
}

// ContextWindowListing 模型在 /v1/models 的 context_length（三级查找）：
// remote（上游 maxInputTokens）>0 时权威；否则查知识表；仍未收录 → DefaultContextWindow
// （1M，宁可高估不低估）。绝不再透出假 131072。
func ContextWindowListing(model string, remote int64) int64 {
	if remote > 0 {
		return remote
	}
	if cap, ok := contextCapFallback[model]; ok && cap.context > 0 {
		return cap.context
	}
	return DefaultContextWindow
}

// MaxOutputTokensListing 模型在 /v1/models 的 max_output_tokens（三级查找）：
// remote（上游 maxOutputTokens）>0 时权威；否则查知识表；仍未收录 → ok=false
// （调用方省略字段，不编造输出上限）。与 ContextWindowListing 的 1M 兜底刻意不同：
// 输出上限无「宁可高估」的安全侧，未知即省略。
func MaxOutputTokensListing(model string, remote int64) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if cap, ok := contextCapFallback[model]; ok && cap.maxOutput > 0 {
		return cap.maxOutput, true
	}
	return 0, false
}
