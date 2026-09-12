<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关</b><br>
  OAuth 登录 · 账号池轮转 · 熔断与冷却 · 会话粘性 · 定时签到 / 活跃 / 旅行 / 保活 · 流式 / 非流式
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker_Compose-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
  <a href="https://t.me/sliverkiss_blog"><img alt="Telegram" src="https://img.shields.io/badge/Telegram-%E9%A2%91%E9%81%93-blue?logo=telegram&logoColor=white&style=flat-square"></a>
</p>

---

## 项目简介

WorkBuddy2API 是一个自托管的 **OpenAI 兼容反向代理网关**，将腾讯 CodeBuddy（`copilot.tencent.com`）账号包装为 `/v1/chat/completions` 和 `/v1/responses` 服务，并提供内置 Web 管理控制台。

- 官方不提供 OpenAI 形态的开放 API，本项目通过 **OAuth 设备授权**（`login.sh`）获取账号凭证，在网关侧做 token 自动刷新、账号池调度与流量治理；
- 面向 **个人多账号** 场景：多账号共享、单号故障自动换号、冷却 / 熔断防止雪崩、会话粘性保证多轮上下文不跳号；
- 对客户端只暴露 OpenAI 兼容接口，现有 SDK / 前端 / 工具 **零改造接入**。

> ⚠️ 合规须知：本项目是**非官方**网关，使用 CodeBuddy 账号作为上游，**仅限本人授权账号、本机 / 私有环境测试**。详细边界见[安全与合规](#安全与合规)。

## 核心能力

### 消耗统计

管理后台的「消耗统计」提供按 UTC 日期筛选的请求数、失败数、输入/输出 Token、每日汇总和各 API Key 汇总。Chat Completions 与 Responses 的流式、非流式请求均计入；鉴权失败与查询接口不计入，废弃 Key 的历史记录保留。数据从启用后开始记录，保存于 `state_file` 同目录的 `usage.json`，需持久挂载此目录；当前采用单进程本地存储，不跨副本合并。

「刷新账户积分」查询并保存上游当前有效套餐的已用积分和余额。该快照包含其他客户端的使用，可能受套餐续期、过期及上游结算延迟影响，不等于所选日期的网关消耗。上游没有按网关 API Key 的实扣积分数据，因此不使用模型倍率或 Token 推算扣分。缺失 Token usage 与未知积分会单独标示，不视为零消耗。

| 能力 | 说明 |
|---|---|
| **Web 管理控制台** | `/admin/`：独立密码登录、OAuth 添加账号、JSON 导入、积分刷新、启停账号、模型与积分倍率、接入 API Key 创建 / 废弃、健康状态、配置保存与服务重启；静态资源嵌入二进制，无需 Node 或独立前端容器 |
| **Responses API** | 文本 / 图片 URL 输入、函数调用、实时语义 SSE、`previous_response_id` 续接、响应查询与删除；兼容范围见下文 |
| 🔑 **OAuth 一键登录** | `login.sh` 设备授权流程，自动落盘凭证并重启容器加载新账号 |
| 🔄 **多账号池** | 三因子加权随机选号（积分占比 ×10 + 闲置补偿 + 成功率 ×3），Top-5 候选 + 防惊群 |
| 🛡️ **熔断与冷却** | 429 软冷却 600s 起指数退避（封顶 `soft_rate_max`）、404 固定 60s 短冷却、402 硬冷却至次日 04:00、连续失败熔断、在途租约限流 |
| 🧲 **会话粘性** | 同一会话（`conversation_id`）尽量绑定同一账号，TTL 滚动续期，失败自动解绑，可镜像 Redis 防重启丢失 |
| ⏰ **定时任务** | 签到（09/21 点）+ 活跃上报（10 点，点亮连登 / 解锁领养 + streak 自检）+ 猫猫旅行（09/21 点，独立排程）+ token 保活（22 点），四类独立开关 |
| ⚡ **流式 + 非流式** | 出站强制 `stream:true`；SSE 帧按规范白名单重建；非流式由本地聚合为单响应 |
| 🧠 **推理模型兼容** | DeepSeek 思维链注入（`thinking.type=enabled` + 默认档）、`reasoning_content` 多轮回填、effort 档位自动降级 |
| 💬 **系统提示词体系** | 网关自有提示词替换客户端 system（默认 `custom`），从源头消灭 system 来源的内容误报；`passthrough` 遇拦截自动降级重试 |
| 🗑️ **指纹脱敏** | 出站请求体黑名单指纹字段清洗（可关闭），与提示词体系两层叠加 |
| 📊 **可观测** | 每请求一行表格日志（TTFB / token 速率 / uid）；`/healthz` 带 `service` 身份标识可接负载均衡 / 宿主探活 |
| 💾 **状态持久化** | 池状态本地原子落盘 + Upstash Redis 异步镜像（可选），重启择新恢复 |

## 架构总览

```mermaid
flowchart LR
    Client["客户端 / SDK\nOpenAI 兼容请求"] --> H

    subgraph GWI["WorkBuddy2API 网关 :7863"]
        H["HTTP Handler\n鉴权 · 请求体上限 · 提示词改写 · 轮转"] --> P
        H --> S
        P["账号池\n三因子加权 · 熔断 · 冷却 · 租约"] --> U
        S["会话粘性路由"] -.绑定镜像.-> REDIS
        T["定时调度\n签到 09/21 · 旅行 09/21 · 活跃 10 · 保活 22"] --> P
        U["上游 Client\nChatHTTP 流式 · 短 RPC"]
    end

    P -. "读凭证 (0600)" .-> AUTH[("auths/*.json")]
    P -. "状态镜像" .-> REDIS[("Upstash Redis\n可选")]
    U -->|"chat/completions (SSE)"| CB["CodeBuddy\ncopilot.tencent.com"]
    U -->|"billing / auth / growth"| CB
```

上游请求在出站前经历统一的改写管线（`internal/upstream/payload.go`）：强制 `stream:true`、`developer` 角色归一、tool_choice 归一、DeepSeek 思维链注入、`reasoning_effort` 档位降级、`reasoning_content` 回填、指纹脱敏。

## 快速开始

### 环境要求

- **Docker + Docker Compose**（推荐部署方式，镜像内已含 `app` 低权限用户与全部工具脚本）
- 一个或多个已注册的 CodeBuddy 账号，用于 OAuth 登录
- 宿主机 Go ≥ 1.22（仅源码构建时需要）

### Docker Compose 一键部署

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
cp .env.example .env
```

编辑 `config.json`，将 `api_key` 的 `test_key` 占位符替换为随机密钥（仅首次初始化使用；首次留空且尚未创建密钥时不鉴权）。编辑 `.env`，设置至少 12 位的随机 `WB2A_ADMIN_PASSWORD`；这是独立的管理密码，API 密钥不能用于登录管理台。使用 HTTPS 反向代理时设置 `WB2A_ADMIN_SECURE_COOKIE=true`，代理应保留原始 Host。

```bash
# 启动服务
docker compose up -d --build

# 进程存活检查（空账号池也返回 200，可以先打开控制台添加账号）
curl -s http://localhost:7863/livez

# 添加账号后检查 API 就绪状态（无可用账号时 503）
curl -s http://localhost:7863/healthz
# {"healthy":2,"total":3,"service":"workbuddy2api"}
```

打开 `http://服务器地址:7863/admin/`，输入管理密码，在「账号管理」中开始 OAuth 授权，并打开授权链接完成 CodeBuddy 登录。浏览器自动轮询结果，凭证仅在服务端保存，账号立即入池，无需重启。也可上传或粘贴现有 `workbuddy-*.json` 文件，再点击「刷新积分」读取当前余额。上游真实授权需要在浏览器中由账号所有者完成。

同一管理会话内刷新或关闭后重新打开页面，会从服务端恢复未完成的授权链接、剩余时间和自动检测；也可点击「我已完成授权，立即检测」。授权链接有效期为 5 分钟，过期后可重新开始。退出登录或服务重启会结束原管理会话，需重新发起授权。

**账号 JSON 来源：**已有服务的 `auth_dir`（默认 `auths`）内保存 `workbuddy-<uid>.json`；项目的 `./login.sh` 完成登录后也会生成此文件。仅有 Docker 部署时，可在原服务主机执行 `docker cp workbuddy2api:/app/auths ./workbuddy-auths` 提取，再将对应文件下载到当前电脑上传；自定义容器名或目录需调整命令。支持 `auth` / `account` 嵌套结构与 `accessToken` / `uid` 平铺结构，服务 `config.json` 不是账号文件。凭证文件包含访问令牌，请妥善保管。首次使用建议直接浏览器授权，无需获取 JSON。

**数据持久化与旧部署升级：**默认 Compose 使用 `auths` / `data` 命名卷，首次启动可直接由非 root 用户写入。已有宿主机 `./auths`、`./data` 的部署请继续使用绑定目录，避免切换卷后看不到旧账号：

```bash
# Linux 宿主机：确保目录由镜像中的 app 用户（UID 10001）可写
mkdir -p auths data
sudo chown -R 10001:10001 auths data
docker compose -f docker-compose.yml -f docker-compose.bind.yml up -d --build
```

绑定目录模式可继续使用 `./login.sh`。不要用 `docker compose down -v` 清理需要保留的账号和配置卷。

### Web 管理与配置生效

- 管理入口 `/admin/`，会话有效期 12 小时。服务重启后需要重新登录；未设置管理密码时管理 API 不可使用。
- 管理台展示账号数量、冷却 / 停用 / 并发状态、积分与查询时间。积分是缓存快照，点击刷新才查询上游；查询失败不会把原余额写为零。健康状态不代表已验证真实模型生成。
- 可编辑请求大小、上游超时、提示词模式、定时任务、账号池、冷却和会话设置。接入密钥在「API Key」页独立管理，创建和废弃立即生效，无需重启。密码、完整 API 密钥和账号 token 均不从管理查询接口返回。
- 配置写入 `state_file` 同目录的 `admin-config.json`（默认 `/app/data/admin-config.json`），采用原子写入。基础 `config.json` 可以保持只读挂载。加载顺序为基础配置 → 管理配置 → `WB2A_*` 环境变量；界面会显示环境覆盖项的名称。
- 保存后点击「重启服务」，Docker 的 `restart: unless-stopped` 会拉起新进程并加载配置。重启会短暂中断服务，超过优雅停机窗口的在途请求会断开；源码直接启动时需手动再次启动进程。
- `listen`、凭证 / 状态目录、Redis 连接和管理密码属于部署配置，通过文件或环境变量修改。账号可停用或重新启用；重新授权已有账号前，先停用并等待在途请求结束。

### 模型选择与积分倍率

「模型与积分」页按所选账号查询官方已授权的 CLI 模型目录，展示准确的请求 `model` ID、显示名称、积分倍率、输入 / 输出上限、图片 / 工具 / 推理能力和官方标签。可搜索、按倍率排序、选择模型并复制 ID 或 Responses / Chat Completions 请求示例；选择仅用于生成客户端配置，不改变账号池路由或网关默认模型。

目录来自 `/console/enterprises/personal/models` 的 `agents[name=cli].models`，按 ID 关联模型信息并排除已禁用项。缓存按账号隔离，有效期 15 分钟，手动刷新立即查询；失败时保留旧快照并标记过期与查询错误，无快照时明确报错。`/v1/models` 共享此目录，并返回 `credit_multiplier`、`credit_type`、`credits_label` 及 `source` / `stale` 来源信息；该兼容 API 无目录时仍提供标记为 `source: static` 的历史回退列表，其倍率为未知，不能视为当前账号已验证可用。

根据[官方积分规则](https://www.codebuddy.cn/docs/ide/Account/credits)与[WorkBuddy 模型说明](https://www.codebuddy.cn/docs/workbuddyapp/features/Model)，`×1.00` 表示相对消耗基数，`×0.25` 为相对倍率，**不代表每次请求扣 0.25 积分**。实际扣分还取决于输入 / 输出 token、模型定价与任务复杂度；官方未公开统一的 token 到积分换算公式。Auto 为动态选择，缺失倍率显示未知；`×0.00` 与限时优惠标签按上游原样展示，活动可能变化。模型选择和目录刷新不会执行模型生成。

### 接入 API Key 管理

在「API Key」页为客户端创建具名密钥。完整密钥只在创建成功时显示一次，请当场复制；以后仅显示脱敏前缀与状态。客户端用 `Authorization: Bearer <API Key>` 接入。所有密钥具有相同的网关 API 权限，管理台仍使用独立密码。

升级首次启动时，原 `config.json` / 管理覆盖配置 / `WB2A_API_KEY` 最终生效的密钥自动迁移为「原配置密钥」，保持原客户端可用。密钥哈希与废弃状态原子保存到 `state_file` 同目录的 `api-keys.json`（默认 `/app/data/api-keys.json`，文件权限 0600），新密钥完整值不会写入此文件。后续启动以此文件为准，修改旧配置或环境变量不会恢复已经废弃的密钥；旧配置可能仍含迁移前的密钥，需要与数据卷一起妥善保管。

废弃操作不可恢复：该密钥后续请求立即返回 401，已经开始的请求可继续结束。废弃全部密钥也不会恢复匿名访问，仍可用独立管理密码登录并创建新密钥。首次匿名部署创建首个密钥后，同样永久启用鉴权。所有记录（包括已废弃记录）最多 1000 条。升级、重启与备份应保留整个 `data` 卷；损坏的密钥文件会阻止启动，避免悄悄放开鉴权。

### Responses API 使用

以下示例使用网关模型名，`base_url` 仍为服务器的 `/v1`。协议按 [OpenAI Responses 文档](https://developers.openai.com/api/docs/guides/responses)和[流式事件文档](https://developers.openai.com/api/docs/guides/streaming-responses)适配。

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:7863/v1", api_key="your-api-key")
response = client.responses.create(
    model="deepseek-v4-flash", input="你好，请介绍一下自己"
)
print(response.output_text)

for event in client.responses.create(
    model="deepseek-v4-flash",
    previous_response_id=response.id,
    input="用一句话概括",
    stream=True,
):
    if event.type == "response.output_text.delta":
        print(event.delta, end="", flush=True)
```

支持字符串 / 消息数组 `input`、`instructions`、`input_text`、图片 URL / Data URI、内嵌文件、assistant `output_text`、自定义 `function` 工具及其返回结果（字符串或内容数组）、`tool_choice`、`reasoning.effort` 和主要生成参数。音频和视频统一通过标准 input_file 作为文件输入。工具由调用方执行，网关只传递工具调用与结果。`prompt.mode=custom` 会沿用现有规则替换 `instructions`；希望透传时设置 `passthrough`。

以下内容项既可放在用户消息的 `content` 数组，也可放在 `function_call_output.output` 数组，可与 `input_text` 混排：

| 媒体 | Responses 内容项示例 | 转发到 Chat 上游 |
| --- | --- | --- |
| 图片 | `{"type":"input_image","image_url":"https://example.com/a.png","detail":"high"}` | `image_url`；也支持 `data:image/...;base64,...` |
| 文件 | `{"type":"input_file","filename":"report.pdf","file_data":"data:application/pdf;base64,..."}` | `file`，保留文件名和内容；也可传原始 Base64 |
| 音频文件 | `{"type":"input_file","filename":"clip.wav","file_data":"data:audio/wav;base64,..."}` | `file`，保留音频 MIME 类型、文件名和内容 |
| 视频文件 | `{"type":"input_file","filename":"clip.mp4","file_data":"data:video/mp4;base64,..."}` | `file`，保留视频 MIME 类型、文件名和内容 |

Chat 的 `tool` 消息无法直接承载图片等媒体，因此网关保留带 `tool_call_id` 的文本工具结果，将完整多模态内容按原顺序放到该批工具结果之后的用户消息，并以调用 ID 标注来源。并行调用的工具结果保持相邻，媒体也保存在 `previous_response_id` 续接历史中。纯文本工具输出保持字符串行为。

网关不会下载 URL、读取客户端本地路径、转码、抽帧或执行 OCR / ASR。图片 URL 必须为 HTTP(S) 或图片 Base64 Data URI。文件（包括音频、视频）统一使用标准 `input_file` 的 `filename` 与 `file_data` 字段，推荐通过 Data URI 指明实际 MIME 类型；不新增音视频内容类型，也不将文件转换为 `input_audio` / `video_url`。Responses 中这些专用类型返回不支持错误，已有 Chat Completions 透传行为不变。文件 ID 与文件 URL 暂不支持，请使用内嵌文件。多模态请求沿用现有请求大小和历史缓存限制。媒体转发已用本地假上游测试验证；文件封装只保证传递数据，不代表上游具备相应格式的解析能力。CodeBuddy 对音频、视频等文件的真实推理尚未验收。格式依据：[OpenAI 文件输入](https://developers.openai.com/api/docs/guides/file-inputs)。

兼容 DeepSeek Harness / Pi 等客户端的普通消息与 assistant 历史消息（包括 `input_text` / `output_text`）。以下合法可选偏好会被接受：`reasoning.summary`、`include:["reasoning.encrypted_content"]`、`text.format.type="text"`、`text.verbosity`、`stream_options.include_obfuscation`、`prompt_cache_key` 和 `prompt_cache_retention`。这些字段不会直接透传给 Chat 上游；网关不生成加密推理或混淆数据，也不保证 verbosity 或缓存时长生效。`reasoning.effort` 接受 `none`、`minimal`、`low`、`medium`、`high`、`xhigh`，以及 Harness 使用的 `max` 扩展档位，并沿用 Chat 请求的映射与上游降级规则；`max` 不代表所有模型都原生支持该档位。

当上游返回非空 `reasoning_content` 时，网关将其原样映射为独立的 `reasoning` 输出项（`summary_text`），流式发送 `response.reasoning_summary_part.added/done` 和 `response.reasoning_summary_text.delta/done`，供 Harness / Pi 实时显示思考过程。不依赖模型名白名单，也不要求显式指定 summary；上游未返回推理文本时不生成 reasoning 项。summary 是协议兼容载体，不额外生成或压缩摘要，`concise` / `detailed` 不控制文本长度。存储续轮与客户端回传 reasoning 输出项均保留推理文本。

流式输出是实时的 `response.created`、`response.output_text.delta`、函数参数增量和终态事件，具有递增 `sequence_number`。默认保存响应；可用 `GET /v1/responses/{id}` 查询，`DELETE /v1/responses/{id}` 删除。`store:false` 不保存。

**兼容范围：**响应保存在单进程内存中，30 分钟过期，最多 1000 条、64 MiB 序列化快照，达到上限会淘汰旧记录；重启或切换实例后不可续接。与输入历史展开后仍受请求大小上限约束。不支持 OpenAI 托管内置工具、后台任务、文件 ID / 文件 URL、加密推理输入、结构化输出及自动截断；不支持的功能会明确报错，不宣称完整实现全部 OpenAI 功能。Responses 校验错误返回 `X-Request-ID`，服务器日志记录该 ID、HTTP 状态、错误码及已知参数名，不记录请求正文或密钥。

### 源码构建

```bash
go build ./...
go vet ./...
go test ./...      # 完整测试套件
# Docker 内运行 vet 和完整 race 检查（含编译器，无需宿主机配置 CGO）
docker build --target test .
go run ./cmd/server -config config.json
```

构建二进制：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login ./cmd/login
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o credit ./cmd/credit
```

### 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情，disabled 账号透出 disabled_reason）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 流式聊天
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 非流式聊天（本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置说明

**`config.example.json` 是配置项最完整的参考**：每个字段、默认值与结构都能在其中找到，示例值一律是 `test_key` 之类占位符，**不含任何真实密钥**。下表为字段含义速查。

### 字段速查

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | HTTP 监听地址 |
| `api_key` | 空 | 网关鉴权密钥；**空 = 不鉴权直接放行**（公网必须设置） |
| `admin.password` | 空 | 独立管理密码，至少 12 位；空 = 禁用管理 API，可由 `WB2A_ADMIN_PASSWORD` 覆盖 |
| `admin.secure_cookie` | `false` | HTTPS 反向代理部署时启用 Secure cookie，可由 `WB2A_ADMIN_SECURE_COOKIE` 覆盖 |
| `auth_dir` | `./auths` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 账号池状态持久化文件 |
| `server.max_body_mb` | `8` | 聊天请求体大小上限（MB，0 / 负数启动报错）。超限直接返回 **413 `request_body_too_large`**，不再把半截请求喂给上游 |
| `cooldown.soft_rate` | `600s` | 软限流（429 / 限流文案）冷却基数；同一账号连续触发按 2 倍指数退避 |
| `cooldown.soft_rate_max` | `2h` | 软冷却指数退避封顶 |
| `schedule.checkin_hours` | `[9, 21]` | 每日本地时区整点签到 + 余额查询解冻。空数组 / `null` = 未配置回落默认（不是禁用） |
| `schedule.travel_hours` | `[9, 21]` | 每日本地时区整点推进猫猫旅行状态机（领养 / 派出 / 领奖） |
| `schedule.activity_hours` | `[10]` | 每日本地时区整点对话活跃上报（点亮连登 + 解锁 `first_buddy`） |
| `schedule.keepalive_hours` | `[22]` | 每日本地时区整点刷新 token 保活 |
| `schedule.checkin_enabled` | `true` | 签到总开关；`false` 真正关闭 |
| `schedule.travel_enabled` | `true` | 猫猫旅行总开关（独立于签到） |
| `schedule.activity_enabled` | `true` | 活跃上报总开关 |
| `schedule.keepalive_enabled` | `true` | token 保活总开关 |
| `upstream.timeout_seconds` | `120` | 短 RPC（刷新 / 签到 / 余额 / 模型列表）总时长上限 |
| `upstream.header_timeout_seconds` | 回落 `timeout_seconds` | 聊天首字节前（响应头）上限 |
| `upstream.idle_timeout_seconds` | `300` | 聊天流中空闲上限（活跃续命，静默断流） |
| `upstream.user_agent` | 空 | 出站 User-Agent 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`）。官网「使用端」列按出站 UA 服务端归因；官方 WorkBuddy 桌面 UA 为 `WorkBuddy/<version>`，需要时可配 |
| `features.sanitize_blacklist_fingerprints` | `true` | 出站请求体黑名单指纹脱敏 |
| `prompt.mode` | `custom` | 系统提示词模式：`custom` = 网关用自有提示词替换客户端 system；`passthrough` = 透传客户端原始 system（降级重试仍切中性提示词） |
| `prompt.file` | 空 | 提示词文件路径；空 = 内置默认（约 2KB）；路径非空但不可读 → 启动报错 |
| `upstash.url` / `upstash.token` | 空 | 空 = 纯内存模式（Noop 降级，功能照常） |
| `pool.max_in_flight` | `3` | 单账号最大在途请求数（`0` = 不限） |
| `pool.breaker_threshold` | `3` | 连续失败触发熔断阈值 |
| `pool.breaker_cooldown` | `30m` | 熔断基础退避时长 |
| `pool.breaker_cooldown_max` | `6h` | 熔断指数退避封顶 |
| `pool.idle_weight_per_hour` | `0.5` | 闲置补偿：每小时未使用 +0.5 权重 |
| `pool.idle_weight_max` | `5.0` | 闲置补偿权重封顶 |
| `session_sticky.enabled` | `true` | 会话粘性路由开关 |
| `session_sticky.ttl` | `30m` | 会话绑定 TTL（滚动续期） |
| `session_sticky.gc_interval` | `5m` | 过期绑定 GC 周期 |

### 上游超时语义（三段各归其位）

| 字段 | 作用对象 | 默认 | 行为 |
|---|---|---|---|
| `timeout_seconds` | 短 RPC（token 刷新 / 签到 / 余额 / 模型列表） | `120` | 总时长硬上限，到期报错走换号 / 熔断 |
| `header_timeout_seconds` | 聊天 SSE **首字节前** | `120` | 由 `Transport.ResponseHeaderTimeout` 约束；超时 = 换号重发 |
| `idle_timeout_seconds` | 聊天 SSE **流中空闲** | `300` | 活跃吐数据续命不掐；静默超时才断流释放租约 |

聊天流（`stream` true / false 均同）**没有总时长上限**：聊天使用 `Timeout=0` 的专用 client，长思考 / 长输出不会被掐断。

### 环境变量覆盖

加载顺序：JSON 文件 → `WB2A_*` 环境变量（变量非空才覆盖）：

`WB2A_LISTEN` · `WB2A_API_KEY` · `WB2A_AUTH_DIR` · `WB2A_STATE_FILE` · `WB2A_MAX_BODY_MB` · `WB2A_SOFT_RATE`(duration) · `WB2A_SOFT_RATE_MAX`(duration) · `WB2A_TIMEOUT_SECONDS` · `WB2A_HEADER_TIMEOUT_SECONDS` · `WB2A_IDLE_TIMEOUT_SECONDS` · `WB2A_USER_AGENT` · `WB2A_SANITIZE_FINGERPRINTS`(bool) · `WB2A_PROMPT_MODE` · `WB2A_PROMPT_FILE`

## 核心行为语义

### 系统提示词体系

客户端（Claude Code / Codex 等 CLI）会在 system prompt 注入固定模板句，上游内容审核按**逐字精确匹配**误杀合法流量（HTTP 400 + 审核文案）。网关提供两层防护，互不替代：

1. **提示词体系**（解决 **system / developer 来源**的误报）：由 `prompt.mode` 控制
2. **指纹脱敏**（兜底 **用户 / assistant 消息**里的指纹串）：由 `features.sanitize_blacklist_fingerprints` 控制

| 模式 | 语义 |
|---|---|
| `custom`（默认） | 出站前用网关自有提示词**替换**客户端 system / developer 消息（删除全部 system / developer，头部插入单条 system）；user / assistant / tool 消息逐字不动 |
| `passthrough` | 透传客户端原始 system，不做改写 |

内置默认提示词约 2KB（`internal/prompt/defaultprompt.md`，嵌入二进制）。`prompt.file` 指向自定义提示词文件（自定义人格 / 人设）即整体替换内置默认；**留空 = 内置默认**，路径非空但不可读 → **启动报错**（fail fast，不会静默回落到内置默认）。

### 内容拦截误报与降级重试

`passthrough` 模式请求被上游内容策略拦截（HTTP 400 + `blocked by security policy` / `unapproved channel` / `illegal api invocation` 文案）时，判定为 system 指纹误报：**同请求内**换 Degraded 中性提示词重试一次；第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端，并如实报给调用方。

- 触发降级后持续到**次日 00:00 CST**（Asia/Shanghai）重置；降级期内 `passthrough` 请求直达中性提示词，不再先撞 400
- 降级状态是**进程内存态**，重启清零
- 内容问题非账号问题：`ErrContentBlocked` 不罚账号（无冷却 / 熔断 / 计错），由网关降级重试消化

### 错误分类与账号处置

上游错误由 `Classify` 统一分类（判定优先级：余额耗尽 → session 失效 → 限流文案 → 状态码兜底），账号处置如下：

| 分类 | 触发条件 | 账号处置 | 恢复 |
|---|---|---|---|
| 余额不足 | HTTP 402 / body 含余额关键词 | 硬冷却到**次日 04:00**（本地时区） | 签到（09/21 点）余额恢复自动解冻 |
| 频控 | HTTP 429 / 限流文案（不限状态码） | 软冷却 `soft_rate`（600s 起，连续触发指数退避，封顶 `soft_rate_max`）。**`code 6004`（模型级）带「将在 … 重置」时**冷却到上游重置墙钟并豁免切模型（见[常见问题](#429-code6004模型级限流的冷却语义)） | 到期自动恢复 / 成功清零退避 |
| Session 失效 | body 含 `Offline user session not found` / `12153` | **连续 3 次**才永久禁用（一次 12153 多为临时抖动：网络 / 闪断 / refresh 竞态）；刷新成功 / 任意成功 / 手工复活清计数 | 人工重新登录（`login.sh`）或 `ReviveDisabled` 复活 |
| 上游 404 | HTTP 404 | 软冷却固定 60s（不随 `soft_rate`、不单独退避） | 到期自动恢复 |
| 服务端错误 | HTTP ≥500 | 喂连续失败计数，达阈值熔断 | 熔断到期 / 成功清零 |
| 请求体解析失败 | HTTP 400 + `Unmarshal chat params failed` / code `11101` | **不罚账号，但仍轮转**（客户端畸形 JSON，换号照样 400） | 即时 |
| 内容拦截 | HTTP 400 + 审核文案 | **不罚账号**，`passthrough` 模式走降级重试 | 即时 |
| 客户端错误 | 其余 4xx / 业务 `code≠0` | 不处罚，换号重试 | 即时 |

请求体解析失败（`11101`）与内容拦截一样**不罚账号**：问题在请求内容而非账号健康。请求体的网关侧截断已由 `server.max_body_mb` 的 413 消灭，剩余的 `11101` 只可能是客户端发来的畸形 JSON。

**熔断器**：所有冷却入口与 5xx 共用唯一连续失败计数器 `fails`；累计达 `breaker_threshold`（默认 3）触发熔断，退避 `breaker_cooldown × 2^retryCount`，封顶 `6h`；成功清零。

**软冷却指数退避**（与熔断器并存的第二条升级线）：软限流的**冷却时长**本身也按连续次数退避——同一账号连续触发软冷却时 `soft_rate × 2^(连续次数-1)`，封顶 `soft_rate_max`。计数 `soft_streak` 独立于熔断器的 `fails`，只在**成功**或**签到解冻**时清零，随 `state.json` 持久化。

### 选号策略

1. 过滤：禁用 / 冷却 / 熔断 / 在途占满账号不参与
2. 取 **Top-5** 候选（按三因子权重降序，积分只是因子之一）
3. 三因子加权随机：

   `weight = credits 比例 ×10 + idleWeight + successRate ×3`

   - `credits 比例` = 该号积分 / 候选集最大积分
   - `idleWeight` = `min(闲置小时 × idle_weight_per_hour, idle_weight_max)`，从未使用给满分
   - `successRate` = `successCount/(successCount+errTotal)`，无记录给中性 1.5
4. 防惊群：跳过 100ms 内刚被选中的账号；全冷却时从非禁用、非余额耗尽的软冷却 / 熔断账号中选最早到期者顶班

### 会话粘性

同一会话尽量复用同一账号，多轮对话不跳号：

- 会话键提取顺序：`metadata.conversation_id` → `metadata.conversationId` → `metadata.user_id` → 顶层 `conversation_id` → 顶层 `conversationId`（snake_case 优先于 camelCase）
- TTL 滚动续期（默认 30m），GC 周期 5m；绑定可镜像到 Redis（7 天 TTL）防重启丢失
- 请求失败自动解绑；成功后绑定跟随最终成功账号

### 定时任务

四类任务各自独立排程、各有开关，互不影响。容器时区由 `TZ` 控制（compose 默认 `Asia/Shanghai`）。

| 任务 | 开关（默认 true） | 时刻（默认） | 行为 |
|---|---|---|---|
| 签到 | `schedule.checkin_enabled` | `checkin_hours` `[9, 21]` 整点 | 签到 + 余额查询；余额恢复则解冻冷却账号 |
| 活跃上报 | `schedule.activity_enabled` | `activity_hours` `[10]` 整点 | 对话活跃上报（`chat_request_send` 事件，必须含 `userId`）；点亮连登 + 解锁 `first_buddy`；每号每天 1 次 |
| 猫猫旅行 | `schedule.travel_enabled` | `travel_hours` `[9, 21]` 整点 | 独立排程：无猫领养 / `idle` 派出 / `arrived` 领奖 |
| 保活 | `schedule.keepalive_enabled` | `keepalive_hours` `[22]` 整点 | 全账号刷新 token；session 失效**连续 3 次**才自动禁用 |

**关闭定时任务**：用 `schedule.*_enabled: false` 显式关闭（四个都设 `false` 则调度器不空转，直接阻塞等待退出信号）。注意两点语义：

- **空数组与 `null` 表示「未配置 → 回落默认」**，不是「禁用」；真正关闭请用 `*_enabled: false`
- **禁用不会擦除小时配置**：`*_hours` 原样保留，改回 `true` 即恢复原时点；小时值必须是 0-23，非法值启动即报错
- 关签到会把「余额恢复即解冻」一起关掉，被硬冷却的账号只能等次日 04:00 自然到期

#### 活跃上报（独立排程）

对池内每个可用账号在 `activity_hours`（默认 `[10]` 整点）发送一条对话活跃上报（事件 `chat_request_send`，body 为数组，事件必须含 `userId`）：

- 一条上报同时点亮 growth 连登 + 解锁 `first_buddy` 任务（领养前置）
- 每号每天 1 次即可（单时点）：日活跃奖励按天去重，重复上报无额外收益
- `conversationId` 由网关生成（`wb2api-<ms>`），无需真实会话
- 限速：账号间间隔 800ms（与旅行同口径）
- **streak 自检**：上报成功后回读连登天数（只读 oracle），日志每号一行可 grep：`activity <uid>: streak days=N`。`days=0` 记 **warn**（`report OK but streak.days=0 (silent drop?)`，对应上游「200 但静默丢弃」）；回读失败记 warn 但不影响主流程（上报按天幂等，不重试，只观测）
- 手动诊断 / 补跑用 `python3 scripts/probe_active.py`（只读探测；写操作默认 dry-run，需 `--yes`）

#### 猫猫旅行（独立排程）

对池内每个可用账号在 `travel_hours`（默认 `[9, 21]` 整点）单趟推进一次，每趟只做一个动作，不轮询不等待。默认两趟闭环：9 点领昨日到站奖励并派出，21 点领当日到站奖励（`daily_limit_reached` 自动挡住二次派出）。

| 探测结果 | 动作 |
|---|---|
| 无猫（`buddy` 为 `null`） | 先同意协议（幂等），再尝试领养；过门槛则 +300 积分并获得猫 |
| `state=idle` 且今日未派出 | 派出 `location_id=4`（古镇客栈；4 个地点收益 / 时长区间相同，无最优解） |
| `state=arrived` | 领取到站奖励（带 `record_id`） |
| `state=traveling` / 今日已达上限 / 未知状态 | 跳过 |

- 领养门槛未达标时上游返回 HTTP 400，每账号每自然日只尝试一次（跨日重试，记录仅存内存）；门槛可用活跃上报解除
- 限速：账号间间隔 800ms
- 每自然日 1 次派出：按 CST（Asia/Shanghai）自然日重置，与容器 `TZ` 无关
- 失败隔离：单账号失败只跳过该账号当趟；401 不强刷（token 刷新交保活时点）

## API 端点

### 服务端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer（`api_key` 非空时） | OpenAI 兼容补全；流式 / 非流式；请求体上限 `server.max_body_mb`（默认 8 MB） |
| `POST /v1/responses` | 同上 | Responses 文本、函数调用、流式 / 非流式、有限内存续接 |
| `GET /v1/responses/{id}` / `DELETE /v1/responses/{id}` | 同上 | 查询 / 删除未过期的已保存响应 |
| `GET /v1/models` | Bearer（`api_key` 非空时） | 模型列表（动态拉取，缓存 1h；失败回落静态表 + 5min 负缓存） |
| `GET /status` | Bearer（`api_key` 非空时） | 账号状态汇总 + 每账号详情（积分 / 冷却 / 熔断 / 在途 / 粘性；disabled 账号透出 `disabled_reason`） |
| `GET /healthz` | 无 | 健康检查：有 healthy 且未占满账号返回 200，否则 503；响应带身份标识（见下） |
| `GET /livez` | 无 | 进程存活检查，恒 200，不要求已添加账号；用于容器 HEALTHCHECK |
| `/admin/` / `/admin/api/*` | 独立管理会话 | 控制台静态登录页公开，管理数据和操作要求密码登录；修改操作校验 CSRF |

> 推理接口和 `/status` 仅当 `api_key` 非空才校验 `Authorization: Bearer <api_key>`；`api_key` 为空时直接放行。管理 API 始终要求独立管理会话，`/healthz`、`/livez` 恒无鉴权。

`/healthz` 响应示例（200 / 503 同结构，仅状态码与计数变化）：

```json
{"healthy": 2, "total": 3, "service": "workbuddy2api"}
```

响应同时带 `X-Service: workbuddy2api` 头。这两个身份标识用于区分**本网关**与同端口上可能残留的其他服务——对方即使返回 2xx 也不会带该字段 / 头，宿主探测据此避免"假成功"。

**宿主健康探测指引**：需要鉴权时用 `/status` + `api_key`；负载均衡器用 `/healthz` + `service` 字段判据（`/healthz` 恒无鉴权，`service == "workbuddy2api"` 才算命中本网关）。容器自带 `HEALTHCHECK` 使用 `/livez` 仅检查进程存活，空账号池也能启动控制台；API 流量是否可受理仍以 `/healthz` 为准。

### 流式行为细节

- 出站请求强制 `stream:true`；SSE 帧按 OpenAI 规范**白名单重建**（`reasoning_content` 保留、工具调用按 index 合并、未知字段剥离）
- 保证恰好一个 `data: [DONE]`（上游漏发时兜底补写）；空流先写一帧 `error` 再补 `[DONE]`
- 非流式请求由本地聚合完整 SSE 流为单 `chat.completion` 响应（含 `reasoning_content` / `tool_calls`）

### 上游端点

上游接口均为 CodeBuddy 官方 CLI / 插件使用的**非公开 / 逆向接口**，未见公开 API 文档；路径及 Host 以代码内常量为准（见文末出处表）。两类 base：

- **`copilot.tencent.com`**：聊天补全（SSE）、token 刷新、OAuth、模型列表、growth 域（旅行 / streak）
- **`www.codebuddy.cn`**：每日签到、余额查询、活跃上报

| 相对路径（绝对路径见出处表） | 方法 | 用途 |
|---|---|---|
| `chat/completions` | POST | 聊天补全（SSE） |
| `console/enterprises/personal/models` | GET | 动态模型列表 |
| `plugin/auth/token/refresh` | POST | token 刷新 |
| `billing/meter/daily-checkin` | POST | 每日签到 |
| `billing/meter/get-user-resource` | POST | 余额查询 |
| `report` | POST | 对话活跃上报（`chat_request_send` 事件数组，必须含 `userId`；点亮连登 / 解锁领养） |
| `plugin/auth/state?platform=CLI` | POST | OAuth 取授权 URL |
| `plugin/auth/token?state=` | GET | OAuth 轮询取 token |
| `plugin/login/account?state=` | GET | OAuth 取账号信息 |
| `activity/growth/buddy/agreement` | POST | 猫猫旅行：同意协议（幂等） |
| `activity/growth/buddy/first` | POST | 猫猫旅行：首次领养 |
| `activity/growth/buddy/info` | GET | 猫猫旅行：查询猫档案 |
| `activity/growth/buddy/travel/status` | GET | 猫猫旅行：旅行状态 |
| `activity/growth/buddy/travel/depart` | POST | 猫猫旅行：派出 |
| `activity/growth/buddy/travel/claim` | POST | 猫猫旅行：领奖 |
| `activity/growth/streak` | GET | 连登天数（只读 oracle，活跃自检用） |

出站请求统一携带 `CLI/2.63.2 CodeBuddy/2.63.2` UA（可被 `upstream.user_agent` 覆盖）；聊天请求带账号头（`X-User-Id` 等），**永不携带 `X-Refresh-Token`**（该头只出现在 token 刷新请求）。

## 请求级日志

每个 `/v1/chat/completions` 请求结束时输出一行表格日志（stdout）：

```text
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s |
```

| 字段 | 说明 |
|---|---|
| `#001` | 进程级请求序号 |
| `18:31:31` | 结束时刻 |
| `deepseek-v4` | 模型名（超 11 字符截断） |
| `stream` / `sync` | 请求模式 |
| `200` | 状态码 |
| `uid=0851ce35` | 账号 UID 前 8 位 |
| `TTFB` | 流式首帧耗时（非流式为 `-`） |
| `tok` / `tok/s` / `total` | 输出 token 数 / 速率 / 总时长 |

**敏感度**：日志不含任何 token 明文（详见[安全与合规](#安全与合规)），无落盘日志文件。

## 部署运维

### Docker 镜像

多阶段镜像（`golang:1.23-alpine` 构建 → `alpine:3.20` 运行）一次编译全部四个二进制并随镜像分发：

- **wb2api**（主服务）、**signin_bin**、**login**、**credit** + 脚本（`login.sh` / `signin.sh` / `credit.sh` / `scripts/probe_active.py`）
- 以 `app` 用户（uid 10001）运行，`app/auths` 与 `app/data` 预建
- 镜像内默认落 `config.example.json` 作为空配置（不含密钥），生产用挂载卷覆盖 `/app/config.json`
- 内置 `HEALTHCHECK`（`wget /livez`，30s 间隔）；API 流量路由请检查 `/healthz`

账号 / 数据通过 `docker-compose.yml` 命名卷 `auths` / `data` 持久化，`./config.json` 只读挂载。已有绑定目录可添加 `docker-compose.bind.yml` 保持原路径。

### 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录 → 落盘 auth → 重启容器 |
| `./signin.sh [auths_dir]` | 批量签到（过期先刷新） |
| `./credit.sh` / `./credit.sh -json` | 积分日报（美化 / 原始 JSON） |
| `python3 scripts/probe_active.py` | 活跃上报手动诊断 / 补跑（probe=只读 / report=单号上报 / unlock=单号领猫 / ALL=全池；写操作默认 dry-run，需 `--yes`） |

二进制不在 git 中：脚本首次使用自动 `go build` 对应 `cmd/*`（Docker 镜像内已预编译）。

### 账号管理

- 多账号复制 `auths/workbuddy-<uid>.json` 即可，池启动时自动对齐目录
- Session 失效账号被禁用（`disabled_reason` 透出在 `/status`）后，可用 `./login.sh` 重新登录覆盖凭证；已持久化 `disabled=true` 的账号可在源码侧调用 `Pool.ReviveDisabled(uid)` 复活（`state.json` 中清除 `disabled` 标志）
- 备份 = auths 卷（凭证）+ data 卷（池状态与 `admin-config.json`）+ 基础配置及 `.env`；配置 Upstash 后池状态另镜像至 Redis（7 天 TTL），管理配置仍在本地数据卷

## 安全与合规

### 1. 凭据管理（auths）

- **位置**：`./auths`（`auth_dir` 可配），文件名 `workbuddy-<uid>.json`
- **内容**：明文 `accessToken` / `refreshToken` + 账号元信息（`account.uid` / `enterpriseId` / `nickname`）
- **权限**：容器内以 `app` 用户（uid 10001）运行；token 刷新由 `SaveAtomic` 以 `0600` 原子写回（tmp + rename）；`login.sh` 首次落盘遵循登录 umask，建议手动 `chmod 600 auths/*.json`
- **切勿提交 git**：`.gitignore` 已排除 `auths/`、`data/`、`backups/`、`config.json`、`*.key`、`*.pem`、`*.env`、`docs/` 及除 README 外的全部 `*.md` 工作文档

### 2. 网络暴露与日志敏感度

- 默认监听 `:7863`，compose 暴露 `0.0.0.0:7863`，**无内置 TLS**；公网部署必须设置 `api_key`，建议前置反代 / 内网
- 请求日志字段：序号 / 模型 / 模式 / 状态码 / **uid 前 8 位** / TTFB / token 数——**不含** `accessToken` / `refreshToken` / `api_key` 明文（不读取 `Authorization` 头）
- 日志写 **stdout / stderr**（容器内进入 `docker logs`），代码无任何落盘日志文件

### 3. 发布来源与合规边界

- **无预编译 release**：仓库无 Release / tag，产物 = 源码自构建（Dockerfile 多阶段在本地构建时完成）
- 登录 / 签到 / 积分工具：`./login.sh` / `./signin.sh` / `./credit.sh`
- **无产物校验和**：`go.sum` 仅约束 Go 模块依赖；Docker 镜像由本地 `docker compose build` 生成，未引用第三方镜像
- 上游 CodeBuddy 属腾讯系商业产品，本项目是其**非官方 OpenAI 兼容网关**；使用其账号做 API 网关涉及目标平台服务条款与账号风险，作者不对账号封禁、条款违约或使用结果负责

### 4. 授权使用边界

- 仅限**本人授权账号**、本机 / 私有环境测试
- 不得共享、转售、违规分发，或用于违反目标平台条款的用途
- 遵守 CodeBuddy 平台服务条款与所在地法律
- 妥善保管 `auths/`（明文凭证）与网关端口

## 常见问题

### 429 code=6004（模型级限流）的冷却语义？

上游 `429` + `code 6004` 是**该模型的使用量超限**（msg 通常带「将在 YYYY-MM-DD HH:MM:SS UTC+8 重置」），**不是账号整体被限流**。网关的处理：

- **冷却到上游重置时间**：msg 带「将在 … 重置」时，账号冷却 `until` 精确等于该墙钟（按 UTC+8 解释），并封顶 `soft_rate_max`（默认 2h）
- **切模型立即可用**：冷却由 6004 触发时会记录触发模型；同一账号改用**其他模型**请求时视为可用。同模型或未记录模型的冷却回到现状
- **退回指数退避**：6004 无「将在 … 重置」文案，或非 6004 的普通软限流 → 仍是 `soft_rate`（600s 起，连续触发指数退避，封顶 `soft_rate_max`）

### 多图会话请求体超限怎么办？

请求体超过 `server.max_body_mb`（默认 8 MB）时网关直接返回 `413 request_body_too_large`：

```json
{"error":{"message":"请求体超过 8 MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试","type":"api_error","code":"request_body_too_large"}}
```

- 该错误在**网关侧**判出，**不会**打上游、**不会**罚账号、**不会**轮转
- 收到 `413` 即表示是请求体本身超限（多图 / 超长上下文场景），调大 `server.max_body_mb` 即可（`WB2A_MAX_BODY_MB` 环境变量同样生效）
- 要么放行要么明确 `413`，网关不再把半截请求体喂给上游

### 账号被 Disable 后如何恢复？

- **用 `./login.sh` 重新登录**覆盖凭证，重启后自动回池；
- 或源码侧调用 `Pool.ReviveDisabled(uid)` 清除 `disabled` 状态（`state.json` 同步刷新）。

### 系统提示词被内容策略误杀怎么办？

默认 `prompt.mode=custom` 已用网关自有提示词替换客户端 system，从源头消除大部分误报；用户 / assistant 消息中的指纹串由 `features.sanitize_blacklist_fingerprints` 清洗，两层叠加。`passthrough` 模式下首遇拦截会自动换 Degraded 中性提示词同请求重试一次。

### 如何让官网「使用端」列显示为 WorkBuddy？

官网「使用端」列按出站请求 UA 服务端归因。配置 `upstream.user_agent: "WorkBuddy/2.x.x"`（或环境变量 `WB2A_USER_AGENT`）即可改写全部出站请求的 UA；默认保持 `CLI/2.63.2 CodeBuddy/2.63.2` 现状（指纹净化考虑，可配而非改死）。

## 关键断言 ↔ 代码出处

| 断言 | 出处 |
|---|---|
| `prompt.mode` 默认 `custom` | `cmd/server/config.go:148` |
| 请求体上限默认 8 MB | `cmd/server/config.go:132`；413 判定与返回 `internal/server/handler.go:246-254` |
| 出站强制 `stream:true` | `internal/upstream/payload.go:28` |
| DeepSeek 思维链注入（`thinking.type=enabled`） | `internal/upstream/thinking.go:110` |
| 默认 `reasoning_effort` 档位 = `high` | `internal/upstream/thinking.go:32` |
| `reasoning_content` 多轮回填（assistant 消息） | `internal/upstream/thinking.go:54` |
| Degraded 中性提示词常量 | `internal/prompt/prompt.go:25` |
| 降级触发与次日 00:00 CST 重置 | `internal/server/degrade.go:30`（Trigger）、`:46`（nextMidnightCST） |
| 6004 模型级限流 code 与重置时间解析 | `internal/upstream/client.go:127`、`internal/upstream/client.go:147` |
| `11101` / Unmarshal 失败不罚号 | `internal/upstream/client.go:114-115`；处理分支 `internal/server/handler.go:489` |
| 出站 UA 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`） | `cmd/server/config.go:73`；接线 `cmd/server/main.go:96` |
| session-dead 连续阈值 3 才禁用 | `internal/pool/pool.go:249-253`（`sessionDeadThreshold`） |
| `ReviveDisabled` 人工复活 | `internal/pool/pool.go:951` |
| disabled 账号透出 `disabled_reason` | `internal/pool/pool.go:1162-1165` |
| 硬冷却至次日 04:00 | `internal/pool/pool.go:882`（`CooldownUntilTomorrow4AM`） |
| 软冷却退避封顶 2h | `internal/pool/pool.go:247`（`defaultSoftRateMax`） |
| Top-5 候选短名单 | `internal/pool/pool.go:584` |
| `activity_hours` 默认 `[10]` | `cmd/server/config.go:135` |
| 活跃自检回读 streak | `internal/scheduler/scheduler.go:227`（`checkActivityStreak`） |
| streak 端点 `activity/growth/streak` | `internal/upstream/travel.go:24`（常量）、`:139`（`GrowthStreak`） |
| Redis 粘性镜像 7 天 TTL | `internal/redisstore/redisstore.go:21` |
| 静态模型表含 `deepseek-v4-flash` 等 | `internal/server/handler.go:146` |

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发（源码或二进制形式）时，请保留原仓库的 MIT 版权声明与许可声明（如在 NOTICE 或 README 中注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`）
- 本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利；使用者仍需自行遵守上游服务条款
