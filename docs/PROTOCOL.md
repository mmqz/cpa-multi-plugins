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
| `trae-cn` | Trae Code CN | `api.trae.cn` / `trae-api-cn.mchost.guru` | `ono9krqynydwx5` | `inline_chat` | ✅ | 4/5 llm_utils_chat |
| `trae-solo-cn` | Trae Work CN / SOLO CN | 同 trae-cn | `en1oxy7wnw8j9n` | `solo_work_lite` | ✅ | 4/5 |
| `qoder-intl` | Qoder Intl | `qoder.com` / `api3.qoder.sh` | `e883ade2-...` | - | ❌ | 5/5 COSY 签名 |
| `qoder-cn` | QoderWork CN | `qoder.com.cn` / `gateway.qoder.com.cn` | `1c5e33e1-...` | - | ✅ | 5/5 |

## 协议复用

| 核心实现 | 覆盖 provider | 配置差异 |
|---|---|---|
| `codebuddy-core` | codebuddy-cn, codebuddy-intl, workbuddy | host, platform, user_agent, has_checkin |
| `trae-core` | trae-cn, trae-solo-cn | client_id, function, has_checkin |
| `trae-intl-core` | trae-intl | 独立（Web SOLO remote 协议） |
| `qoder-core` | qoder-intl, qoder-cn | openapi_base, gateway_base, client_id, redirect_uri, has_checkin, has_pat_import |

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
- Body: `{messages, function:"inline_chat", stream:true, config_name:"{model}", model:"{model}"}`
- Body 白名单 (v0.12.37): 只透传 `messages/function/stream/config_name/model` + `tools/tool_choice`（归一化后）+ 采样参数 `temperature/top_p/max_tokens/presence_penalty/frequency_penalty/seed/n/stop`；其余客户端字段（`reasoning_effort`/`thinking`/`stream_options`/`response_format`/`user`/`metadata` 等）一律丢弃——上游没有原生 thinking 参数（社区实测），agent 字段会触发 4023 "model is unknown"（参考 Ttungx/trae-solo-local-api）
- 模型命名空间: 插件对外的模型 id 带凭据变体后缀（`-solo`/`-intl`，供宿主路由），发送上游前必须剥离——`config_name` 带后缀会被 SSE 流内 `event:error biz_code=4001 "We're sorry, the param is invalid."` 拒绝，且传输层仍算成功（v0.12.37 修复的 SOLO 全模型 4001 根因）
- SSE 事件: `metadata`, `timing_cost`, `output` (含 response + reasoning_content + tool_calls), `extra_info`, `token_usage`, `done`, `error`（`event:error` 硬模型错误码: 4001=无效 config_name、4023=未知模型字段、1005=套餐限流）

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
- **function**: `solo_work_lite` (替换 `inline_chat`)
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

## 关键实现注意点

### CodeBuddy 系
- `X-Product` 必须区分: CN=`SaaS`, Intl=`cloud`
- Token 刷新用 `X-Refresh-Token` header 而非 body 字段
- UserID 从 JWT `sub` claim 解析
- 强制 `stream=true`，非流式返回 400

### Trae 系
- `IdeVersion` 选择: `0.1.52` / `20260811` 可看到 glm-5.3 等新模型
- `trae-api-cn.mchost.guru` 是社区反代，官方对应 `api.trae.cn`
- 签到鉴权（v0.12.38 定稿）: 自走 OAuth（ExchangeToken）的 token 用 `Cloud-IDE-JWT`（官方客户端实证方案，BlueChonk FINDINGS §五实测到账）；`Bearer` 对该类 token 报 biz_code=1001（cockpit-tools 的 Bearer 经验不可平移——其 token 来自官方客户端托管会话，类别不同）。v0.12.38 起双方案探测: Cloud-IDE-JWT 优先，非 9074 失败回退 Bearer 一次；9074=官方活动名额限流（先到先得），不回退
- 签到时刻（v0.12.39）: 官方按自然日重置奖励名额，主循环默认 0 点（`defaultCheckinHour=0`）+ 前密后疏退避（1m→2m→4m→8m→16m→32m→64m→2h 封顶，10 次/日），贴重置点抢签；9 点起签 + 长退避会恒抢不到名额
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
