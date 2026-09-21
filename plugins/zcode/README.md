# zcode plugin (CPA)

把智谱 **GLM 编码套餐**（Z.AI 国际站 / BigModel 国内站）接入 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的动态库插件：一次 `/v1/chat/completions` 调用 `glm-4.5-air` ~ `glm-5.3-flash` 全系模型。

协议净室重实现自 [TriDefender/zcode-api](https://github.com/TriDefender/zcode-api)（ZCode Proxy，MIT）的公开协议实现 —— OAuth 登录流、身份指纹头、签名 V4 与账务平面逐条对齐，文件头注明每处代码出处。

## 功能

- **双 provider 单插件**：Z.AI（`api.z.ai`）+ BigModel（`open.bigmodel.cn`）按账号文件内 `auth.provider` 字段路由（对齐 workbuddy 三区 / qoder 双区先例），旧式单文件 `zcode.json` 启动时自动收养改名。
- **服务端中转 CLI 登录**（ZCode 3.12.3 桌面端同款，无本地回调）：
  `POST zcode.z.ai/api/v1/oauth/cli/init`（自生成 32B poll_token）→ 浏览器打开 `authorize_url`（带桌面端中间页参数）→ 轮询 `/oauth/cli/poll/{flow_id}` 至 `ready` → 落盘 `{access_token, plan JWT, user_id}`。
  轮询错误语义逐条对齐：4xx（除 408/429）、envelope `code!==0`、未知 status 为致命；5xx/网络/畸形 200 按 pending 重试。
- **coding-plan OpenAI 兼容上游**：`{provider}/api/coding/paas/v4/chat/completions` 直连，请求体除 model/stream 钉死外原样透传；身份头按 `g6n` 形状（`X-ZCode-Agent: glm` 内联第 8 位、无 `X-Device-Mid`），LLM UA 追加 `ai-sdk/anthropic/3.0.81`，trace 头五件套（`x-request-id`/`x-zcode-session-type: main`/`x-zcode-trace-id`/`x-query-id`/`x-session-id`）全 UUID。
- **签名 V4**（ZCode 3.9.1 `ClientRequestSigningV4Signer` 镜像）：门禁探测 `/api/v1/agent/configs`（`codingPlanSignature.enable`，TTL 1h + 负缓存）→ 握手 `/api/paas/c1f3a7e2/v2/client`（HMAC-SHA256(HKDF(secret, salt=`WD_CLIENT_SIGN_KDF_SALT`)) 换取 AES-GCM 加密的 PKCS8 Ed25519 私钥）→ 每请求 Ed25519 签名 + 8-bit PoW 七头组。全部消息模板**换行连接**（空格连接被上游拒绝）。重试梯：签名 → 401 `VERIFY_*` 重握手重签 → 二次 `VERIFY_*` 永久 bypass 补发一次无签名请求。任何环节失败 fail-open（与客户端一致）。start-plan / off-peak 路径永不签名。
- **模型目录**：钉死 11 个 GLM 模型（上下文 131K–1M、输出 32K–131K），支持 `oauth-excluded-models` 过滤与 per-(账号, 模型) 冷却过滤。
- **配额面板**：`/api/v1/zcode-plan/billing/balance`（JWT + 稳定 `X-Device-Mid`，缺头会被活动网关以 3001 拒绝）实时余量，折入 creditsSummary 多池展示；账号卡显示 provider/plan/冷却，支持选中路由账号与 coding/start plan 切换。
- **生命周期**：配额耗尽自动禁用（`lifecycle_auto`），回补后自动恢复；401/402/429 经 envelope `http_status` 驱动宿主按状态冷却。
- **流守卫三路对称**：异步泵、同步收集、非流折叠三条路径共享空流守卫 + 200-OK 错误信封识别（`error` 字段即失败，绝不折叠成假成功）；`empty_stream` 前缀措辞兼作模型级冷却与宿主连接生命周期分类依据。
- **用量上报**：NDJSON 转发 CPA-Manager-Plus `/v0/management/usage/import`（config → env → docker secret 三级解析）。

## 限制（当前版本）

- **start-plan 执行未实现**：start-plan 网关只有 Anthropic 格式端点（`/api/v1/zcode-plan/anthropic/v1/messages`），需要 OpenAI→Anthropic 翻译层，后续版本补齐。start-plan 账号可正常登录、显示配额，执行请求返回明确错误。
- **试用套餐自动领取（claim）未实现**：claim 需要 Aliyun 验证码 token，原实现依赖 in-process 浏览器环境；Go 侧无等价物。

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
| `usage_report_url` / `usage_report_key` | CPAMP 用量上报覆盖（env `USAGE_REPORT_*` / `CPAMP_ADMIN_KEY` 亦可） |
| `identity_version` 等 | 身份头 appVersion/Title/Referer/Language/Timezone 覆盖（默认对齐 ZCode 3.14.0） |

## 协议出处

协议事实全部来自 TriDefender/zcode-api 公开源码（2026-09 快照）：

- OAuth：`src/auth/oauth.ts`（cli/init + cli/poll + 中间页参数）
- 身份头：`src/proxy/identity.ts`（g6n / TV 双 builder，printable-ASCII 门控）
- 上游构造：`src/proxy/upstream.ts`（双认证头/SDK UA 后缀/trace 头）
- 签名 V4：`src/proxy/client-signing.ts`（门禁/握手/Ed25519/PoW/重试梯）
- 端点重映射：`src/proxy/endpoint-routing.ts`（M2 路线）
- 账务平面：`src/server/routes-quota.ts` + `src/claim/client.ts`
- 模型目录：`src/provider/models.ts`

## License

MIT
