# mimo — Xiaomi MiMo provider plugin for CLIProxyAPI

Clean-room CLIProxyAPI plugin (C ABI, `-buildmode=c-shared`) that serves
Xiaomi **MiMo** chat models through two upstream lanes, both reconstructed
from official sources and documented in
[`docs/MIMO_PRIVACY.md`](../../docs/MIMO_PRIVACY.md) and
[`docs/MIMO_AUTH.md`](../../docs/MIMO_AUTH.md):

| Lane | Credential | Upstream | Fingerprint |
|---|---|---|---|
| **sk lane**（主） | platform OAuth 永久 `sk` | `{issued url}/chat/completions`（OpenAI 兼容） | `Authorization: Bearer` + `X-Mimo-Source: mimocode-cli`（官方 CLI `plugin/mimo.ts`） |
| **cookie lane**（副） | 收养桌面端 SSO 会话 | `{region}/route/chat/completions` | 无 Authorization、`X-Mimo-Source: mimocode-cli-free`、`X-Client-Version`、桌面 Cookie jar（桌面引擎 lane，`index.mjs`） |

> 本插件为独立再实现（clean-room），未复制任何官方/社区代码；协议形状以官方
> CLI 与桌面包静态取证为准，社区代理（LIGHTNINGWHALE）的声明仅在官方包中得到
> 印证后才被采纳（两处臆造——`mimocode/0.1.0` UA 与 `mimo-x-*` 模型名——未采纳）。

## 模型

官方核心表（桌面 bundle 实证）：`mimo-auto` / `mimo-flash` / `mimo-pro`
（context 1M / output 128k / text+image）。`mimo-auto` 仅在 cookie lane 解析为
`mimo-pro`（桌面 `EE()/k6()` 对齐）；sk lane 不改写模型。

## 登录（sk lane）

`auth.login.start` 返回 platform 授权页 URL（本机随机端口回调，6 分钟 TTL）。
浏览器完成授权后平台 302 回 `http://localhost:<port>/?u=<密文>`，插件按官方
CLI 同款参数解密：`base64url(ephemeralPub(32B) ‖ nonce(12B) ‖ ct ‖ tag(16B))`，
AES-256-GCM，key = `SHA256(ECDH(X25519))` → `{sk, uid, url}`。sk 为永久凭据，
AuthRefresh 仅回显元数据。

## 桌面会话收养（cookie lane）

插件注册后自动扫描桌面端 Chromium Cookie 库（`persist:xiaomi-account` 分区）：

- Windows `%APPDATA%\Xiaomi MiMo AI\Partitions\xiaomi-account\Cookies`（含 "Xiaomi MiMo" 变体）
- macOS `~/Library/Application Support/Xiaomi MiMo AI\Partitions\...`（**解密需 Keychain，M1 不支持** → 请用 sk lane）
- Linux `~/.config/Xiaomi MiMo AI\Partitions\...`

只收集 `*.xiaomimimo.com` 行（登录侧 `.xiaomi.com` 凭据不会被错误重放）；临时副本
打开，绝不写桌面文件。Windows v10 = AES-256-GCM + DPAPI(Local State)；
Linux v10/v11 = AES-128-CBC（PBKDF2 peanuts/saltysalt）。

Cookie lane 行为与桌面引擎 lane 对齐：401 → 重探 `/user/xiaomi/me` 续期 → 重试一次
（"模型不在可用范围"豁免续期）；区域表 cn/sgp/ru/in，auto 模式按 me 响应收养。

## 隐私边界（硬约束，docs/MIMO_PRIVACY.md §7）

1. **零遥测**：不复刻 OneTrack / tracking.miui.com 任何调用。
2. **零审计/零门探测**：不调 `/audit/*`、不调 `/user/invite/check`。
3. **请求最小化**：出站 body 剥离 `user`/`metadata`/`service_tier`/`logprobs` 族。
4. **零内容日志**：错误只记状态码与脱敏摘要；凭据只存 host auth store。

## 构建

```bash
make build   # mimo.so (c-shared)
make test    # go test -race
make lint    # gofmt + go vet
```

## 配置（plugins.configs.mimo）

| 字段 | 默认 | 说明 |
|---|---|---|
| `region` | `auto` | cookie lane 区域：auto（me 响应收养）/ cn / sgp / ru / in |
| `x_client_version` | `26.922.222056` | cookie lane 的 X-Client-Version（桌面对齐） |
| `platform_url` | `https://platform.xiaomimimo.com` | sk lane OAuth 基址（CLI MIMO_PLATFORM_URL 等价） |
| `cookie_paths` | （自动探测） | 额外的 Chromium Cookies 文件路径（逗号分隔） |
