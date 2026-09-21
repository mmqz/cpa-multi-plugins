# cpa-multi-plugins 协议事实清单

> 基于 8 个项目的代码逆向，记录 4 平台 × 2 版本共 8 个 provider 的协议事实。

## 平台与版本对照

| Provider | 平台 | Host | client_id | function | 签到 | 协议复杂度 |
|---|---|---|---|---|---|---|
| `codebuddy-cn` | Tencent CodeBuddy CN | `copilot.tencent.com` / `www.codebuddy.cn` | 无（state 轮询） | - | ✅ | 1/5 OpenAI 兼容 |
| `codebuddy-intl` | CodeBuddy Intl | `www.codebuddy.ai` | 无 | - | ❌ | 1/5 |
| `workbuddy` | Tencent WorkBuddy | 同 codebuddy-cn | 无 | - | ✅ | 1/5 |

> ⚠️ v0.9.0 起 `codebuddy-cn` 与 `workbuddy` 合并为单一插件 `workbuddy`（同一后端、同一额度池），
> 通过 `login_platform` 配置选择 CLI / ide 登录方式，旧 codebuddy-cn 账号文件自动收养。下表保留两者以说明协议差异。下文 Provider 章节同样保留作协议参考。
| `trae-intl` | Trae Intl | `api.marscode.com` / `core-normal.trae.ai` | `ono9krqynydwx5` | - | ❌ | 4/5 Web SOLO remote |
| `trae-cn` | Trae Code CN | `api.trae.cn` / `trae-api-cn.mchost.guru` | `ono9krqynydwx5` | `solo_work_lite` ᵛ⁰·¹²·⁷⁹ | ✅ | 4/5 llm_utils_chat |
| `trae-solo-cn` | Trae Work CN / SOLO CN | 同 trae-cn | `en1oxy7wnw8j9n` | `solo_work_lite` | ✅ | 4/5 |
| `qoder-intl` | Qoder Intl | `qoder.com` / `api3.qoder.sh` | `e883ade2-...` | - | ❌ | 5/5 COSY 签名 |
| `qoder-cn` | QoderWork CN | `qoder.com.cn` / `gateway.qoder.com.cn` | `1c5e33e1-...` | - | ✅ | 5/5 |
| `zcode`（zcode 分支） | 智谱 GLM 编码套餐（Z.AI + BigModel） | `zcode.z.ai`（控制面/网关）/ `api.z.ai` + `open.bigmodel.cn`（LLM） | 无（poll_token 中转） | - | —（claim 需验证码侧车） | 5/5 签名 V4 + anthropic 翻译 + off-peak 票务 |

> ᵛ⁰·¹²·⁷⁹ issue #9：`llm_utils_chat` 仅接受 `function=solo_work_lite`（其余值一律流内
> 4001 "param is invalid"），trae-cn/trae-intl 的聊天与目录请求自 v0.12.79 起也发
> `solo_work_lite`（intl 实际走 Web SOLO remote 协议，不经过该端点；此处仅为映射
> 表不再产出死值）。与官方客户端的 `inline_chat` 取值刻意偏离，依据：
> trae2api-web RESEARCH.md / trae2api-more `IsModelConfigMismatch` / 报告者
> 同-JWT 仅换 function 的翻转实验（2 个 CN 账号 11 模型实测可用）。

## 协议复用

| 核心实现 | 覆盖 provider | 配置差异 |
|---|---|---|
| `codebuddy-core` | codebuddy-cn, codebuddy-intl, workbuddy | host, platform, user_agent, has_checkin |
| `trae-core` | trae-cn, trae-solo-cn | client_id, has_checkin（v0.12.79 起 function 统一为 solo_work_lite，issue #9） |
| `trae-intl-core` | trae-intl | 独立（Web SOLO remote 协议） |
| `qoder-core` | qoder-intl, qoder-cn | openapi_base, gateway_base, client_id, redirect_uri, has_checkin, has_pat_import |
| `zcode-core`（zcode 分支） | zai, bigmodel（单插件按账号路由） | llm_openai_base, provider 字段; plan 字段路由 coding/start；coding 直连 OpenAI 网关，start/off-peak 走 anthropic 翻译层 |

---

## Provider: codebuddy-cn

### OAuth 流程
- **登录入口**: `POST https://www.codebuddy.cn/v2/plugin/auth/state?platform=ide`
- 返回 `data.state` + `data.authUrl`
- 浏览器打开 `{endpoint}/login?state={state}` 完成登录
- **Token 轮询**: `GET https://www.codebuddy.cn/v2/plugin/auth/token?state={state}`
  - `code=11217` = pending
  - `code=0` = success
- **Token 刷新**: `POST https://copilot.tencent.com/v2/plugin/auth/token/refresh`
  - Headers: `X-Refresh-Token`, `X-Auth-Refresh-Source: plugin`, `Authorization: Bearer {old}`, `X-User-Id`, `X-Domain`
- **Token 字段**: `accessToken`, `refreshToken`, `expiresIn`, `refreshExpiresIn`, `tokenType`, `domain`
- JWT `sub` claim = UserID

### Chat API
- `POST https://copilot.tencent.com/v2/chat/completions`
- **强制 `stream=true`**（非流式返回 400 + code=11101）
- Headers:
  - `Authorization: Bearer {accessToken}`
  - `X-User-Id: {uid}`
  - `X-Domain: www.codebuddy.cn`
  - `User-Agent: CodeBuddyIDE/4.9.7 CodeBuddy/4.9.7`
  - `X-Product: SaaS`, `X-IDE-Type: CodeBuddyIDE`, `X-IDE-Version: 4.9.7`
- Body: 原生 OpenAI Chat Completions
- SSE: 标准 OpenAI `data: {...}\n\n` + `data: [DONE]`

### 配额查询
- `POST /v2/billing/meter/get-user-resource` — `ProductCode=p_tcaca`, `Status=[0,3]`
- 返回 `data.Response.Data.Accounts[]`，含 `CycleCapacityRemain` / `CycleCapacityUsed` / `CapacityUnit:"credits"`

### 签到
- **状态**: `POST /v2/billing/meter/checkin-activity-status`
- **领取**: `POST /v2/billing/meter/daily-checkin` (body `{}`)
- 字段: `todayCheckedIn`, `active`, `streakDays`, `dailyCredit`, `todayCredit`, `nextStreakDay`, `isStreakDay`

---

## Provider: codebuddy-intl

### 与 codebuddy-cn 差异
- Host: `www.codebuddy.ai` (替换 `www.codebuddy.cn`)
- platform: `ide` (相同)
- Headers 差异:
  - `User-Agent: CodeBuddy/1.100.0` (无 CodeBuddyIDE 前缀)
  - `X-Domain: www.codebuddy.ai`
  - `X-IDE-Type: IDE`, `X-IDE-Name: CodeBuddy`, `X-IDE-Version: 1.100.0`
  - `X-Product: cloud`, `X-Product-Version: 1.100.0`
  - 无 `X-Product: SaaS`, 无 `X-Requested-With`
- **无签到接口**
- **无 enterprise 用量接口**

---

## Provider: workbuddy

### 与 codebuddy-cn 差异
- 仅 OAuth start 时 `platform=workbuddy` (替换 `platform=ide`)
- 其他完全相同（host、token、chat、配额、签到）
- cockpit-tools `workbuddy_auto_checkin.rs:506` 直接调用 `codebuddy_cn_oauth::get_checkin_status` 和 `perform_checkin`

### 模型能力发现 (v0.12.51，参考 workbuddy2api-panel + deepseek-harness-codearts)

双路并发探测（实现 `plugins/workbuddy/models.go`，逐条从代码核实）：

- 企业端点：排序权威；`/v3/config`：官方 IDE 配置目录，UA 敏感——必须 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0`（CLI UA 得到削减表：flash 输出上限 128K、无 supportedEfforts）
- `/v3/config` 请求头：`Authorization: Bearer` + `Accept: application/json, text/plain, */*` + `X-Requested-With: XMLHttpRequest` + `X-Domain: <realm 域名>` + `X-Product: SaaS` + `X-CodeBuddy-Request: 1` + `X-User-Id: <账号 uid>`（空则省略）；三 realm 各自 `{upstreamBase}/v3/config`
- 响应信封 `{code, data:{models:[...]}}`；合并 key=模型 id，企业管排序、v3 补能力 + 追加 v3 独有条目；单路失败降级另一路，双失败报错并带两个原因
- 能力透出：ContextLength/InputTokenLimit、MaxCompletionTokens/OutputTokenLimit（兼容 `contextWindow/maxTokens` 与 `maxInputTokens/maxOutputTokens` 两代字段名）、Thinking.Levels/ZeroAllowed（`reasoning.supportedEfforts`/`canDisableThinking`）
- nonChatModel 过滤（镜像 harness `buddy.ts` isChatModel 与 workbuddy2api-panel nonChatModel）：id 前缀 `nes-`/`completion-`/`codewise-`（embedding/completion/code-only）、maxOutput<=256、supportsExtra、tags 含 `text-to-image`（图像生成模型）→ 一律不进可选列表

---

## Provider: trae-intl

### OAuth 流程
- **登录入口**: `GET https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance`
  - 候选 host: `api.trae.ai`, `www.trae.ai`, `api.marscode.com`
  - 返回 `data.LoginUrl`
- **client_id**: `ono9krqynydwx5`
- **本地 callback**: 监听 `/authorize`，接收 `authCode` + `codeVerifier`
- **AuthCode Token 交换**: `POST https://grow-normal.trae.ai/trae/api/v3/oauth/ExchangeToken`
  - body: `{ClientID, AuthCode, CodeVerifier, DeviceInfo, IDEVersion}`
- **Token 刷新**: `POST https://{login_host}/cloudide/api/v3/trae/oauth/ExchangeToken`
  - body: `{ClientID, RefreshToken, ClientSecret:"-", UserID:""}`
  - 返回 `Result.{Token, RefreshToken, TokenExpireAt, RefreshExpireAt, UserID, TenantID}`
- **Token 字段**: `Token` (Cloud-IDE-JWT, RS256, ~14d), `RefreshToken` (~7mo)
- **用户信息**: `POST /cloudide/api/v3/trae/GetUserInfo`

### Chat API (Web SOLO remote)
- **创建 session**: `POST https://core-normal.trae.ai/api/remote/v1/chat_sessions`
- **拉 SSE**: `GET https://core-normal.trae.ai/api/remote/v1/chat_sessions/{id}/events`
- Headers: `Authorization: Cloud-IDE-JWT {jwt}`, `X-Trae-Client-Type: web`, `x-user-region: US`
- Body: `{mode:"code"|"work", initial_message:{content:[], query, model_name, agent_type:"solo_agent_remote", ...}}`
- SSE 事件: `plan_item` (累积 thought), `token_usage`, `done`, `error`

### 配额查询
- `POST https://grow-normal.trae.ai/trae/api/v1/pay/ide_user_pay_status`
- `POST https://grow-normal.trae.ai/trae/api/v1/pay/ide_user_ent_usage`

### 签到
- **无**

---

## Provider: trae-cn

### OAuth 流程
- **登录入口**: `GET https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance`
- **client_id**: `ono9krqynydwx5` (同 Intl)
- **AuthCode Token 交换**: `POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken`
- **Token 刷新**: `POST https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken` (注意 `.com.cn`)

### Chat API (IDE SOLO)
- **URL**: `POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat`
  - 官方对应: `https://api.trae.cn` (mchost.guru 是社区反代)
- **Method**: POST, 强制 stream=true
- Headers:
  - `Authorization: Cloud-IDE-JWT {jwt}`
  - `X-Cloudide-Token: {jwt}`, `X-Ide-Token: {jwt}`, `X-Uid: {uid}`
  - `X-App-Id: 6eefa01c-1036-4c7e-9ca5-d891f63bfcd8`
  - `X-Ide-Version: 0.1.43` (建议用 0.1.52 看 glm-5.3)
  - `X-Ide-Version-Code: 20260716` (对应 0.1.52 用 `20260811`)
  - 其他 17 个 X-* 头
- Body: `{messages, function:"solo_work_lite", stream:true, config_name:"{model}", model:"{model}"}`（v0.12.79 起：该端点仅接受 solo_work_lite，issue #9）
- Body 白名单 (v0.12.37): 只透传 `messages/function/stream/config_name/model` + `tools/tool_choice`（归一化后）+ 采样参数 `temperature/top_p/max_tokens/presence_penalty/frequency_penalty/seed/n/stop`；其余客户端字段（`reasoning_effort`/`thinking`/`stream_options`/`response_format`/`user`/`metadata` 等）一律丢弃——上游没有原生 thinking 参数（社区实测），agent 字段会触发 4023 "model is unknown"（参考 Ttungx/trae-solo-local-api）
- 模型命名空间: 插件对外的模型 id 带凭据变体后缀（`-solo`/`-intl`，供宿主路由），发送上游前必须剥离——`config_name` 带后缀会被 SSE 流内 `event:error biz_code=4001 "We're sorry, the param is invalid."` 拒绝，且传输层仍算成功（v0.12.37 修复的 SOLO 全模型 4001 根因）
- SSE 事件: `metadata`, `timing_cost`, `output` (含 response + reasoning_content + tool_calls), `extra_info`, `token_usage`, `done`, `error`（`event:error` 硬模型错误码: 4001=无效 config_name / 模型在当前 function 不可用（v0.12.79 起按请求级失败处理：不冷却账号，agnes-*/deepseek-v4-* 等 solo_agent-only 名单已从目录过滤，issue #9）、4023=未知模型字段、1005=套餐限流）

### 配额查询 (v2)
- `POST https://api.trae.cn/trae/api/v2/pay/ide_user_pay_status`
- `POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage`
- `POST https://api.trae.cn/trae/api/v2/pay/user_current_entitlement_list`
- 返回 `is_credits_billing` + `user_entitlement_pack_list[].entitlement_base_info.quota.credits_limit`

### 签到
- **状态**: `GET https://api.trae.cn/trae/api/v2/ug/checkin_credits/status?did={device_id}`
- **领取**: `POST https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim` (body `{}`)
- Headers: `Authorization: Bearer {jwt}` (或 Cloud-IDE-JWT 也可), `x-app-type: trae`, `Origin/Referer: https://www.trae.cn`, `x-device-id`
- 字段: `checked_in`, `credits`, `enable`, `consecutive_days`, `total_credits`, `credits_earned_today`

---

## Provider: trae-solo-cn (= Trae Work CN)

### 与 trae-cn 差异
- **client_id**: `en1oxy7wnw8j9n` (SOLO stable，替换 `ono9krqynydwx5`)
- **function**: `solo_work_lite`（历史差异；v0.12.79 起 trae-cn 也发同值，issue #9 —— 差异仅剩 client_id）
- 其他完全相同（host、headers、SSE、配额、签到）
- Body 中 messages content 需转为 `[{type:"text",text:"..."}]`
- `tool_choice` 归一化: `"none"` 删 tools; `"auto"/"required"` 保留; `{type:"function",function:{name}}` 提取 name
- `tools[].function.parameters` 必须序列化为 JSON 字符串
- assistant 消息 `tool_calls[].function` 重命名为 `function_call`

---

## Provider: qoder-intl

### OAuth 流程 (device authorization)
- **登录入口**: `GET https://qoder.com/device/selectAccounts?nonce={nonce}&challenge={challenge}&challenge_method=S256&client_id={client_id}&machine_id={machine_id}&redirect_uri={redirect_uri}`
- **client_id**: `e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb`
- **redirect_uri**: `qoder://aicoding.aicoding-agent/login-success`
- **Token 轮询**: `GET https://openapi.qoder.sh/api/v1/deviceToken/poll?nonce={nonce}&verifier={verifier}&challenge_method=S256`
  - HTTP 404/202 = pending
- **PAT → jobToken**: `POST https://openapi.qoder.sh/api/v1/jobToken/exchange`
  - body: `{personal_token:"pt-..."}`
  - 返回: `{token:"jt-...", refresh_token:"jrt-...", expires_in:24h, refresh_token_expires_in:48h}`
- **Token 字段**: `token`, `refresh_token`, `expires_at`(RFC3339), `expires_in`(ms)

### Chat API (COSY-signed)
- **URL**: `POST https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1`
- **Headers**: `Authorization: Bearer COSY.{payloadB64}.{md5sig}` + 17 个 `Cosy-*` 头
- **Body 编码**: Qoder 自定义 base64 (字母表 `_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!`, `=` → `$`, 三段重排)
- **Body 格式**: `{request_id, session_id, stream:true, chat_task:"FREE_INPUT", agent_id:"agent_common", system, messages, tools, parameters:{max_tokens}, chat_context:{...}}`
- **SSE**: `{statusCodeValue, body}` 信封 — `statusCodeValue=200` 时 `body` 是标准 OpenAI SSE

### 配额查询
- `GET https://openapi.qoder.sh/api/v2/quota/usage`

### 签到
- **无**

---

## Provider: qoder-cn (QoderWork CN)

### OAuth 流程
- **登录入口**: `GET https://qoder.com.cn/device/selectAccounts?...`
- **client_id**: `1c5e33e1-364d-4ce6-b02c-acaa81274a5c`
- **redirect_uri**: `qoder-work-cn://`
- **Token 轮询**: `GET https://openapi.qoder.com.cn/api/v1/deviceToken/poll?...`
- **Token 刷新**: `POST https://openapi.qoder.com.cn/api/v1/deviceToken/refresh`
  - body: `{refresh_token:"drt-..."}`
- **PAT 导入**: `POST https://openapi.qoder.com.cn/api/v1/jobToken/exchange`
- **Token 字段**: `token` (dt-, 30d), `device_token`, `refresh_token` (drt-, 1y), `expires_at`, `expires_in`(ms)

### Chat API
- **URL**: `POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1`
- Headers 和 Body 编码同 Intl
- 模型列表: `GET https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1`

### 配额查询
- `GET https://openapi.qoder.com.cn/api/v2/quota/usage`
- `GET https://openapi.qoder.com.cn/api/v2/user/plan`

### 签到
- **状态**: `GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status`
  - 返回 `{status:"CLAIMABLE"|"CLAIMED", rewardCredits, nextClaimAt, currentStreakDays, totalClaimDays, totalRewardCredits}`
- **领取**: `POST https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/claim` (body `{}`)
  - 返回 `{success:true, rewardCredits:100, ...}`
- **Pro 升级领取**: `POST https://openapi.qoder.com.cn/sash/api/v1/me/pro-upgrade/claim`

---

## Provider: zcode（zcode 分支，M1–M3）

智谱 GLM 编码套餐统一 provider（Z.AI 国际 + BigModel 国内）。协议净室重实现自
[TriDefender/zcode-api](https://github.com/TriDefender/zcode-api)（ZCode Proxy, MIT，2026-09 快照），
start-plan 翻译层与 off-peak 票务通道对齐官方开源客户端 [zai-org/ZCode](https://github.com/zai-org/ZCode)
（Apache-2.0，仅取账户级线路协议形状，行为基线保持闭源仿冒），插件侧每条实现均标注源码出处。

### OAuth：服务端中转 CLI 登录（无本地回调）

`src/auth/oauth.ts`（ZCode 3.12.3 桌面端 `startOAuthWithPolling` 同款）：

1. 插件自生成 poll_token（32B hex），`POST https://zcode.z.ai/api/v1/oauth/cli/init`
   头 `Authorization: Bearer {poll_token}`，body `{"provider":"zai"|"bigmodel"}` →
   `{code:0, data:{flow_id, poll_token, authorize_url, expires_at, poll_interval_sec}}`
2. 浏览器打开 authorize_url，**客户端追加中间页参数**：zai=`redirect_uri` / bigmodel=`redirect`，
   值均为 `https://zcode.z.ai/app/oauth/login?redirect=zcode://oauth/callback&app_version=3.14.0`
   —— 授权记录在服务端完成（浏览器不回 localhost；直连 authorize 的 localhost 回调会被
   "Redirect URI not registered" 拒绝）。注意 URL 序列化两层的双重编码形态（`%253A`）。
3. `GET /api/v1/oauth/cli/poll/{flow_id}`（同 Bearer）至 `status:"ready"` →
   `{token:<plan JWT>, user:{user_id}, zai|bigmodel:{access_token}}`。
   错误语义：4xx（除 408/429）、envelope `code!==0`、未知 status = 致命；5xx/网络/畸形 200 = 按 pending 重试。

凭据形态：poll ready 返回的 `data.{provider}.access_token` 是 **OAuth token 而非聊天 Key**，须再经
KeyResolver 业务链解析（zai：`z/login` → Bearer bizToken → `getCustomerInfo` 默认机构/项目 →
`api_keys` 找/建 `zcode-api-key` → `copy/{apiKey}` 取 secretKey，终态 `{apiKeyId}.{apiKeySecret}`；
bigmodel：OAuth token 裸值作 authorization、copy 失败回落单段 Key）。解析后的 Key 永久有效；
plan JWT **无 exp 永不刷新**（8 天旧 JWT 仍可查账务），仅网关 401/3012 表示需重登。

### LLM 上游（coding-plan，OpenAI 兼容网关）

`src/proxy/upstream.ts` + `src/provider/providers.ts`：

- 端点：`https://api.z.ai/api/coding/paas/v4/chat/completions`（zai）/
  `https://open.bigmodel.cn/api/coding/paas/v4/chat/completions`（bigmodel）
- 认证：`Authorization: Bearer {access_token}`（OpenAI 格式单头；Anthropic 格式才双头
  `x-api-key` + `Authorization` 同值 + `anthropic-version: 2023-06-01`）
- 身份头（`g6n` builder，`src/proxy/identity.ts`）：HTTP-Referer、`User-Agent: ZCode/{ver}`
  （LLM 请求追加 ` ai-sdk/anthropic/3.0.81` SDK 后缀）、[X-ZCode-App-Version]、
  `X-Title: Z Code@cli`、X-Release-Channel、X-Client-Language、X-Client-Timezone（always，
  "unknown" 回退）、**`X-ZCode-Agent: glm` 内联**、[X-Platform: `{os}-{arch}`]、
  X-Os-Category（无条件）、[X-Os-Version]。**无 X-Device-Mid**。
  控制面（billing/routing）走 `TV` builder：**无 X-ZCode-Agent**、可选 X-Device-Mid 尾位。
- trace 头五件套（编码面 attribution）：`x-request-id`、`x-zcode-session-type: main`、
  `x-zcode-trace-id`、`x-query-id`、`x-session-id` —— 全 UUID，start-plan 免后两个。
- 头值统一过 printable-ASCII 门控；appVersion 非法时整体丢 X-ZCode-App-Version 且 UA 回退 `ZCode/unknown`。

### Client Request Signing V4

`src/proxy/client-signing.ts`（ZCode 3.9.1 `ClientRequestSigningV4Signer` 镜像）。
**全部消息模板换行连接**（空格连接被上游拒绝，2026-09-18 字节级实证）：

- 门禁：`GET https://zcode.z.ai/api/v1/agent/configs`（g6n 头 + `x-api-key: {credential}`）→
  `data.codingPlanSignature.enable`；TTL 1h，网络失败负缓存 60s / 不可用负缓存 30s
- 握手：`POST {origin}/api/paas/c1f3a7e2/v2/client`，头
  `Authorization: {apiKeyId}.{apiKeySecret}`，body `{apiKey, nonce, sig, ts}`；
  `sig = base64(HMAC-SHA256(HKDF-SHA256(secret, salt="WD_CLIENT_SIGN_KDF_SALT",
  info="getSignKey_hmac"), "get_sign_key\n{id}\n{ts}\n{nonce}"))`；
  应答 `code:200, data.privateCipher` = AES-256-GCM(HKDF(secret, info="ed25519_priv"))
  加密的 PKCS8 Ed25519 私钥（iv=前 12B、AAD=apiKeyId、tag 128）
- 每请求：Ed25519 签 `"{id}\n{ts}\n{appVersion}\n{sessionId}\n{nonce}"`（base64）+
  PoW：seed=`sha256("{id}\nzcode\n{sessionId}\n{ts}")` hex[:32]，找
  `sha256("{seed}\n{candidate}")` 具 8 个前导零 bit 的 candidate（12B hex nonce + 8 位 hex counter）
- 七头组：`X-Client-Ts / X-Client-Version / X-Client-Sig / X-Session-Id / X-Client-Nonce /
  X-App-Id: zcode / X-Client-Pow`
- 重试梯：签名 → 401 且 envelope 提及 `VERIFY_SIGNATURE_INVALID`/`VERIFY_APIKEY_EXPIRED`
  → 重握手重签 → 二次 VERIFY → 永久 bypass 并补发一次无签名请求
- 免签路径：`/api/v1/zcode-plan/anthropic/v1/messages`、`/api/v1/zcode-plan/chat/completions`、
  `/api/v1/off-peak/anthropic/v1/messages`；单段凭据（无 `.` 分隔）跳过签名
- 全程 fail-open（与客户端一致）：门禁关/不可达、握手失败 → 无签名发送

### 账务平面

`src/server/routes-quota.ts` + `src/claim/client.ts`：

- `GET https://zcode.z.ai/api/v1/zcode-plan/billing/balance?app_version=&platform={os}-{arch}`
  —— 头：TV 身份集（无 X-ZCode-Agent）+ `Authorization: Bearer {JWT}` + `Accept` +
  **稳定 `X-Device-Mid`**（活动网关缺头 3001 实证）；应答 balances[]
  `{show_name, remaining_units, total_units, used_units, unit_type, expires_at}`（snake/camel 双兼容）
- 试用套餐领取（后续版本）：`billing/preview`（5min 轮询）+ `billing/claim`
  （需 `X-Aliyun-Captcha-Verify-Param`，原实现靠 in-process 浏览器环境，Go 侧无等价物）

### 模型目录

`src/provider/models.ts`：glm-4.5-air(131K/96K) · glm-4.6(200K/131K) · glm-4.6v(131K/32K 视觉) ·
glm-4.7(200K/131K) · glm-5/5-turbo(200K/64K) · glm-5v-turbo(200K/131K 视觉) · glm-5.1(200K/64K) ·
glm-5.2(1M/128K，3.11.2 目录缺名保留转发) · glm-5.3(1M/128K) · glm-5.3-flash(1M/128K，trial 网关)。

### 端点重映射（参考）

`src/proxy/endpoint-routing.ts`：`/api/v1/agent/configs` 的 `proxyEndpoint.mapping` 表
（from→to 精确 URL 重写，当前把 coding-plan Anthropic 端点映射到 `zcode.z.ai/api/v1/ultra[-zai]/...`），
TTL 5min，fail-open。注：ultra 网关是开源减配形态的替代通道，插件不采用（见“行为基线”注）。

### start-plan Anthropic 翻译层（M2，官方开源协议）

旧 OpenAI 路由 `/api/v1/zcode-plan/chat/completions` 已于 2026-08-28 服务端下线（404）。
start-plan 网关只有 Anthropic 格式端点，官方客户端一律
`POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`（`Authorization: Bearer {plan JWT}` +
`anthropic-version: 2023-06-01`，UA `ZCode/{ver} ai-sdk/anthropic/3.0.81`，3 头 trace 子集）。
来源：官方开源 `translator/openai-to-anthropic.ts` + `translator/sse-translator.ts` +
`proxy/system-prompt.ts` + `zcode_system.json` + `proxy/body-transformer.ts`：

- **网关内容审查**：system 缺官方 ZCode 身份块 → biz 3012 “method not allowed”——system 前缀是准入门票：
  3 个官方块（cli_prefix / stable / dynamic+environment，各带 ephemeral cache breakpoint）前置 +
  用户 system 尾随 + `<system-reminder>` context_prefix 首插 user 轮（含本地日期）
- **metadata.user_id**：每请求必带 `JSON{device_id, account_uuid:"", session_id:""}`；
  device_id 与身份头 X-Device-Mid 同源（billing 稳定 UUID）
- **cache_control 规范化**：清非系统消息全部标记 + 最后一条非系统消息末块打 ephemeral
  （3 官方块已占 4 断点配额前 3）
- **thinking 兼容**：GLM-5.3 家族 `output_config.effort` 是唯一生效通道
  （low 8000 / high 16000 / max 32000 预算配对；`reasoning_effort` 被静默忽略）；
  缺预算默认 1024，max_tokens += 预算且钤目录顶；禁采样参数
- 请求翻译：system 提取双 \n\n 连接、连续 tool 消息合并单 user 轮 tool_result、
  tool_calls→tool_use、图片 data-url→base64/https→url/其他降级文本、stop_sequences、
  tool_choice 映射；响应回译单消息 + SSE 状态机（overwrite-usage 合并、finish 块带 usage、
  tool 索引映射、截断流 flushFinal 兜底）
- 业务码分流（官方 failure-provider-business-codes.ts 全表）：1261 上下文超限 / 1006 鉴权 /
  1312 过载可重试 / 1302|1303|1305 限流可重试 / 1304|1308|1310|1313|3008-3010 限流不可重试 /
  3007 安全校验（验证码挑战，头/体双变体）/ 3001|3005|3006 InvalidRequest；
  start-plan 401 → 重登提示

### Off-Peak 错峰票务通道（M3，官方开源协议）

官方“闲时任务”低峰优惠通道，来源：官方开源 `packages/services/src/session/offPeakServerClient.ts`
+ `offPeakRuntimeModel.ts` + `offPeakTaskService.ts`、`packages/shared/src/off-peak-types.ts`、
`apps/zcode-cli/packages/adapters/src/model/offpeak-retry.ts`（账户级 wire 契约，无客户端指纹）：

- 五端点（基座 `https://zcode.z.ai/api/v1/off-peak`）：
  `GET /ticket/availability` → `{can_take_number, next_take_at?}`（false 必带 next_take_at，
  否则脏响应报错）；`POST /ticket` `{task_id}` → `{ticket_id, state, position?, next_poll_after?}`；
  `POST /ticket/status` `{ticket_ids≤100}` → `{next_poll_after?, tickets[]}`（按 ticket_id 匹配，
  应答序不保证）；`POST /ticket/{id}/settle`（幂等，未知票亦 2xx，4xx 同作 ack）；
  messages 直连 `/anthropic/v1/messages`（免签路径）
- 服务端准入态：queued → ready（5min TTL 废票）→ active（3h 硬顶）→ expired/settled；
  next_poll_after 单位**秒**（Retry-After 惯例），轮询触发服务端晋级
- 鉴权双凭证：`Authorization: Bearer {plan JWT}` + `x-coding-plan-api-key: {plan Key}`
  （TV 身份集，messages 加 `X-Off-Peak-Ticket-ID`；bigmodel-team 加
  `bigmodel-organization/project` 双头且缺一不发——插件 v1 不支持 Team 形态）
- 业务码（lane 本地语义，禁入全局表）：3101 无资格 / 3102 票废（同 task_id 重取号 = 官方续跑语义，
  3001 旧网关兼容）/ 3103 取号超限 / 3105 排队（HTTP 429 + Retry-After）
- 失败决策（offpeak-retry.ts）：3102|3001 → 票废重取；3105 或裸 429 → 排队等待
  min(Retry-After, 5min) 钳制，无头默认 60s 探测；排队 429 豁免 maxAttempts（官方语义）
- 插件适配：宿主同步调用不支持后台任务队列，改为“票到即发”——取票 + 预算内轮询
  （`offpeak_max_wait`，默认 0 = 仅接受即时 ready）→ 带票 messages → 终态 settle；
  429 排队/3102 重取循环仅在非流式路径做（流式开 chunk 后重试不可透明），
  流式错误经 routeChatError 渲染 lane 专属文案；off-peak 模型集合 = GLM-5.3 / GLM-5.3-Flash
  （官方 idle-plan 选择器），启用时目录随之收窄；start-plan 账号服务端结构性拒绝
  （start_plan_not_supported），插件侧同规则排除

---

## 关键实现注意点

### CodeBuddy 系
- `X-Product` 必须区分: CN=`SaaS`, Intl=`cloud`
- Token 刷新用 `X-Refresh-Token` header 而非 body 字段
- UserID 从 JWT `sub` claim 解析
- 强制 `stream=true`，非流式返回 400

### Trae 系
- `IdeVersion` 选择: `0.1.52` / `20260811` 可看到 glm-5.3 等新模型
- `trae-api-cn.mchost.guru` 是社区反代，官方对应 `api.trae.cn`
- 签到鉴权（v0.12.40 定稿，反编译 TraeWork CN 2.3.81345）: `Authorization: Cloud-IDE-JWT <token>`（官方客户端统一方案）+ `x-device-id`（真实绑定 did）+ `x-device-brand/x-device-type/x-os-version/x-app-version`；`Bearer` 对自走 OAuth token 报 biz_code=1001（cockpit-tools 的 Bearer 经验不可平移——其 token 来自官方客户端托管会话，类别不同）。v0.12.38 起双方案探测: Cloud-IDE-JWT 优先，非 9074 失败回退 Bearer 一次；9074 同源不换方案，改换 req_source（v0.12.41）
- 签到契约（v0.12.41 双版反编译交叉实证，out/main.js eb()）: **status 与 claim 均 POST**，body = `{"req_source":N}`。req_source 是【客户端产品谱系】而非用户套餐：TraeCode CN 2.3.79946（deb，09-01 build）body 为 `{}`；TraeWork CN 2.3.81345（exe 装出 "TRAE SOLO CN"，09-04 build，product.json packageType="SOLO_CN"）body 为 `{req_source: Dr(P)?2:1}`，Dr(P)=packageType∈{SOLO_CN,SOLO_I18N,SOLO_CN_ENTERPRISE}→2（SOLO/TraeWork 客户端），否则 1（Trae CN IDE 客户端）。产品谱系同时决定 OAuth appId（iCubeApp.authConfig: TRAE→ono9krqynydwx5、SOLO→en1oxy7wnw8j9n）。我方 token 全部为 TRAE 谱系（ClientID=ono9krqynydwx5）→ 插件探测序列 1 优先、9074 回退 2。status 响应扁平结构 `{enable, checked_in, did_checked_in, credits, extra_credits}`；claim 响应 `{code, message}`
- 签到契约（v0.12.41 双版反编译交叉实证，见上一条）
- credits 语义（v0.12.40 定案）: `credits` = 每日签到奖励数额（官方卡片 "Daily check-in: {credits} credits"），**非钱包/可花余额**；`extra_credits` = 会员/活动加码（"Member bonus: +{extraCredits} daily"）；到账总额 = credits + extra_credits（v0.12.31 "官方给 200 面板 150" = 150 基础 + 50 加码）
- 签到去重: `checked_in` 账号维度、`did_checked_in` 设备维度（"This device has checked in today"）；官方 claim 前置 = 两者均为 false。官方用 beijingDayKey 判定签到日
- 9074 真因（v0.12.41 修正 v0.12.40 结论）: 通用活动校验拒绝码，核心是 claim 的 req_source 与 token 产品谱系错配（v0.12.40 对 TRAE 谱系 token 发 req_source=2 即例证；更早发 `{}` 同样被拒）——2026-09-04 官方收紧活动校验所致；status 只读不受校验，故面板状态可读、claim 被拒。客户端已改为 req_source 1→2 双探测；两个源均被拒才是真·活动侧限制，由调度器退避重试兑底
- 签到时刻（v0.12.39）: 官方按自然日重置奖励，主循环默认 0 点（`defaultCheckinHour=0`）+ 前密后疏退避（1m→2m→4m→8m→16m→32m→64m→2h 封顶，10 次/日）；宿主机时区应为 CN 时区
- Trae Intl 走的是另一套协议（Web SOLO remote `chat_sessions` + GET events），与 CN 不同

### Qoder 系
- COSY `tempKey` 是 16 字节 ASCII（UUID 去横线前 16 位），不是随机 16 字节
- 自定义 base64 字母表含特殊字符 `,@#&*()%^w.(kIQyXqWA!`
- `{statusCodeValue, body}` 信封必须 peek 首个事件解包
- CN PAT 导入需要用户在 `qoder.com.cn` 网页端创建 PAT（阿里云 SSO SMS）

## 参考来源

- [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) — Trae SOLO CN 协议层（Go）
- [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) — WorkBuddy + QoderWork 现有 CPA 插件
- [HanHan666666/codebuddy2openai](https://github.com/HanHan666666/codebuddy2openai) — CodeBuddy CN Python 实现
- [1416277987/proxy-hub](https://github.com/1416277987/proxy-hub) — 多平台反代（含 Trae CN/Work）
- [jlcodes99/cockpit-tools](https://github.com/jlcodes99/cockpit-tools) — 16 平台账号管理
- [decolua/9router](https://github.com/decolua/9router) — Trae Intl JS 实现
- [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute) — Trae/Qoder TS 实现
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) — CPA 插件 SDK
- [TriDefender/zcode-api](https://github.com/TriDefender/zcode-api) — 智谱 GLM 编码套餐反代（ZCode Proxy）——zcode 插件协议蓝本（OAuth 中转登录/签名 V4/身份头/账务平面，zcode 分支吸收）
- [zai-org/ZCode](https://github.com/zai-org/ZCode) — ZCode 官方开源客户端（Apache-2.0，同代 3.14.0 洗白开源）——zcode 插件 M2/M3 协议来源（start-plan anthropic 翻译层/system 块/错误码全表/off-peak 票务 wire 契约，zcode 分支吸收）。⚠ 仅取账户级线路协议形状；开源版为减配形态（无签名 V4/验证码求解/claim 链，ultra 网关替代通道），插件行为基线保持闭源仿冒
- [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) — WorkBuddy `/v3/config` 双路模型发现 + nonChatModel 过滤（v0.12.51 吸收）
- [ThinkofRain1213/deepseek-harness-codearts](https://github.com/ThinkofRain1213/deepseek-harness-codearts) — WorkBuddy isChatModel 过滤 + supportsImages 三态（v0.12.51 吸收）
- [Ttungx/trae-solo-local-api](https://github.com/Ttungx/trae-solo-local-api) — Trae Body 白名单/多模态实测（v0.12.37 依据）
