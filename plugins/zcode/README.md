# zcode plugin (CPA)

把智谱 **GLM 编码套餐**（Z.AI 国际站 / BigModel 国内站）接入 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的动态库插件：一次 `/v1/chat/completions` 调用 `glm-4.5-air` ~ `glm-5.3` 全系模型，coding-plan 直连 OpenAI 兼容网关，start-plan / off-peak 走官方 anthropic 通道。

行为基线为闭源客户端仿冒（`TriDefender/zcode-api` 逆向蓝本：OAuth 登录流、身份指纹头、签名 V4、账务平面逐条对齐）；官方开源 [zai-org/ZCode](https://github.com/zai-org/ZCode) 仅作账户级线路协议参考（M2 翻译层 / M3 错峰票务），文件头注明每处代码出处。

## 功能

- **双 provider 单插件**：Z.AI（`api.z.ai`）+ BigModel（`open.bigmodel.cn`）按账号文件内 `auth.provider` 字段路由（对齐 workbuddy 三区 / qoder 双区先例），旧式单文件 `zcode.json` 启动时自动收养。
- **服务端中转 CLI 登录**（无本地回调、无深链跳转）：
  `POST zcode.z.ai/api/v1/oauth/cli/init`（自生成 32B poll_token）→ 浏览器打开服务端下发的 `authorize_url` 原样直开（其原生 redirect 即服务端 CLI 回调 `/api/v1/oauth/cli/callback/{zai,bigmodel}`，授权后由它落账并渲染结果页）→ 轮询 `/oauth/cli/poll/{flow_id}` 至 `ready` → **KeyResolver 解析** → 落盘 `{accessToken, oauthToken, plan JWT, user_id}`。
  轮询错误语义逐条对齐：4xx（除 408/429）、envelope `code!==0`、未知 status 为致命；5xx/网络/畸形 200 按 pending 重试。流程过期（HTTP 400 `invalid_flow`）直出友好文案：服务端 flow 有效期约 5 分钟，超时请重新发起登录。
  v0.1.3 修复：此前把 redirect 覆写为桌面端中间页 `zcode.z.ai/app/oauth/login`，但该页只有带 `app_version>3.9.1` 才会补发落账 fetch——插件不带版本号导致授权永不落账、轮询直至超期报错（实测复现 400/3004）。CLI 场景本无深链需求，现改为 authorize_url 直通。
- **KeyResolver 凭证链**（M1.1，两参考实现逐行同构 = 账户级 API）：poll ready 的 `access_token` 是 OAuth token 而非聊天 Key —— zai：`z/login` → Bearer bizToken → `getCustomerInfo` 默认机构/项目（子串匹配+首项兜底）→ `api_keys` 找/建 `zcode-api-key` → `copy/{apiKey}` 取 secretKey（必需），终态 `{apiKeyId}.{apiKeySecret}`；bigmodel：裸值作 authorization + copy best-effort 回落单段 Key。解析失败 = 登录失败（与两端一致）。
- **coding-plan OpenAI 兼容上游**：`{provider}/api/coding/paas/v4/chat/completions` 直连，请求体除 model/stream 钉死外原样透传；身份头按 `g6n` 形状（`X-ZCode-Agent: glm` 内联第 8 位、无 `X-Device-Mid`），LLM UA 追加 `ai-sdk/anthropic/3.0.81`，trace 头五件套全 UUID。
- **start-plan anthropic 翻译层**（M2，官方开源协议）：OpenAI 入 → anthropic 出 → OpenAI 回。旧 OpenAI 路由已下线（2026-08-28 404），翻译层对齐官方客户端 wire 形状：3 个官方 system 身份块前置（各带 ephemeral cache breakpoint，缺块网关 3012 拒绝）+ `<system-reminder>` 上下文前缀（本地日期）+ `metadata.user_id` blob + cache_control 规范化 + GLM-5.3 `output_config.effort` thinking 通道（low/high/max 预算配对）；SSE 状态机回译（usage 合并/tool 索引/截断流兜底）；业务码全表分流（1261 上下文超限 / 1312 过载可重试 / 1302-1308 限流 / 3007 验证码挑战 / 1006 鉴权 …）。
- **off-peak 错峰票务通道**（M3，官方开源协议）：配置 `offpeak: true` 后 coding-plan 账号走官方闲时优惠通道 —— 取号 `POST /off-peak/ticket` → 预算内轮询至 ready → 带票直连 `/off-peak/anthropic/v1/messages`（免签路径，双凭证 `Bearer JWT` + `X-Coding-Plan-Api-Key` + `X-Off-Peak-Ticket-ID`）→ 终态幂等 `settle`。失败决策对齐官方 offpeak-retry.ts：429/3105 排队等待 min(Retry-After, 5min)（非流式路径内重试）、3102/3001 票废同 task_id 重取号续跑、3101 无资格 / 3103 取号超限文案直出；启用时模型目录收窄至官方 idle-plan 集合（GLM-5.3 / GLM-5.3-Flash）。start-plan 账号服务端结构性拒绝，插件同规则排除。
- **签名 V4**（ZCode 3.9.1 `ClientRequestSigningV4Signer` 镜像）：门禁探测 `/api/v1/agent/configs`（`codingPlanSignature.enable`，TTL 1h + 负缓存）→ 握手 `/api/paas/c1f3a7e2/v2/client`（HMAC-SHA256(HKDF(secret, salt=`WD_CLIENT_SIGN_KDF_SALT`)) 换取 AES-GCM 加密的 PKCS8 Ed25519 私钥）→ 每请求 Ed25519 签名 + 8-bit PoW 七头组。全部消息模板**换行连接**（空格连接被上游拒绝）。重试梯：签名 → 401 `VERIFY_*` 重握手重签 → 二次 `VERIFY_*` 永久 bypass 补发一次无签名请求。任何环节失败 fail-open（与客户端一致）。start-plan / off-peak 路径永不签名。
- **模型目录**：钉死 11 个 GLM 模型（上下文 131K–1M、输出 32K–131K），start-plan 凭证按官方网关集合过滤（glm-5.3-flash / glm-5.2 / glm-5-turbo），off-peak 启用按 idle-plan 集合过滤；支持 `oauth-excluded-models` 过滤与 per-(账号, 模型) 冷却过滤。
- **配额面板**：`/api/v1/zcode-plan/billing/balance`（JWT + 稳定 `X-Device-Mid`，缺头会被活动网关以 3001 拒绝）实时余量，折入 creditsSummary 多池展示；账号卡显示 provider/plan/冷却，支持选中路由账号与 coding/start plan 切换。
- **生命周期**：配额耗尽自动禁用（`lifecycle_auto`），回补后自动恢复；401/402/429 经 envelope `http_status` 驱动宿主按状态冷却。
- **流守卫三路对称**：异步泵、同步收集、非流折叠三条路径共享空流守卫 + 200-OK 错误信封识别（`error` 字段即失败，绝不折叠成假成功）；`empty_stream` 前缀措辞兼作模型级冷却与宿主连接生命周期分类依据。
- **用量上报**：NDJSON 转发 CPA-Manager-Plus `/v0/management/usage/import`（config → env → docker secret 三级解析）。

## 限制（当前版本）

- **off-peak 流式不重试**：429 排队 / 3102 票废的重试循环仅在非流式路径透明执行；流式一旦开始吐 chunk，重试无法对客户端隐藏（取票阶段的等待/重取两条路径全通道可用）。票已 ready 时 5min TTL 内发送，遇到 429/3102 的概率极低。
- **off-peak bigmodel-team 形态未支持**：Team 账号需要 `bigmodel-organization` / `bigmodel-project` 双头（缺一不发），插件当前不存储组织/项目 ID，个人套餐（personal）不受影响。
- **试用套餐自动领取（claim）未内建**：claim 需要 Aliyun 无痕验证码 token，原实现依赖 JS 运行时 + 完整 DOM（happy-dom），无法进入 c-shared 插件。面板已展示可领取活动（billing/preview）；自动领取走可选的 Node/Bun 验证码侧车（vendored MIT 求解模块，`POST /claim {jwt, plan_id}`，插件经 localhost 调用），未部署侧车时回落官方客户端手动领取（详见仓库 worklog Task 37 设计定稿）。

## 构建

```bash
make build    # zcode.so（c-shared）
make test     # go test -race
make lint     # gofmt + vet
```

## 配置（config.yaml → plugins.configs.zcode）

| 字段 | 说明 |
|---|---|
| `login_provider` | 新登录目标：`zai`（默认）/ `bigmodel`；已有账号保留自身 provider |
| `lifecycle_auto` | 配额耗尽自动禁用（默认 true） |
| `scheduler_mode` | `off`（默认，宿主调度）/ `credits`（面板选中账号 + 耗尽回退 + 冷却过滤） |
| `offpeak` | 启用错峰票务通道（默认 false；仅对 coding-plan 账号生效，start-plan 永不路由） |
| `offpeak_max_wait` | 排队取票最长等待秒数（默认 0 = 仅接受即时 ready 的票；如 `900` = 最多等 15min；429 排队重试共用该预算） |
| `usage_report_url` / `usage_report_key` | CPAMP 用量上报覆盖（env `USAGE_REPORT_*` / `CPAMP_ADMIN_KEY` 亦可） |
| `identity_version` 等 | 身份头 appVersion/Title/Referer/Language/Timezone 覆盖（默认对齐 ZCode 3.14.0） |

## 协议出处

- **行为基线（闭源仿冒）** — [TriDefender/zcode-api](https://github.com/TriDefender/zcode-api)（2026-09 快照）：
  - OAuth：`src/auth/oauth.ts`（cli/init + cli/poll + 中间页参数）
  - 身份头：`src/proxy/identity.ts`（g6n / TV 双 builder，printable-ASCII 门控）
  - 上游构造：`src/proxy/upstream.ts`（双认证头/SDK UA 后缀/trace 头）
  - 签名 V4：`src/proxy/client-signing.ts`（门禁/握手/Ed25519/PoW/重试梯）
  - 账务平面：`src/server/routes-quota.ts` + `src/claim/client.ts`
  - 模型目录：`src/provider/models.ts`
- **账户级线路协议（官方开源参考）** — [zai-org/ZCode](https://github.com/zai-org/ZCode)（Apache-2.0）：
  - KeyResolver：`apps/zcode-cli/.../auth/coding-plan-api-key.ts`（与 zcode-api `src/auth/resolver.ts` 逐行同构）
  - start-plan 翻译层（M2）：`translator/openai-to-anthropic.ts` + `translator/sse-translator.ts` + `proxy/system-prompt.ts` + `zcode_system.json`（官方 3 system 块逐字）+ `proxy/body-transformer.ts`
  - 业务错误码全表：`.../failure-provider-business-codes.ts`
  - off-peak 票务（M3）：`packages/services/src/session/offPeakServerClient.ts`（五端点）+ `offPeakRuntimeModel.ts`（双凭证头）+ `offPeakTaskService.ts`（状态机/续跑）+ `packages/shared/src/off-peak-types.ts`（准入态）+ `apps/zcode-cli/.../offpeak-retry.ts`（排队/废票决策）

> ⚠ 官方开源版为减配形态（无签名 V4 / 验证码求解 / claim 链，改走 ultra 网关替代通道）且服务端可按客户端形态识别 —— 本插件仅取其账户级线路协议形状（服务端无法按客户端区分的部分），行为基线保持闭源仿冒。

## License

MIT
