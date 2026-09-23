# mimo — Xiaomi MiMo provider plugin for CLIProxyAPI

Clean-room CLIProxyAPI plugin (C ABI, `-buildmode=c-shared`) that serves
Xiaomi **MiMo** chat models through two upstream lanes, both reconstructed
from official sources and documented in
[`docs/MIMO_PRIVACY.md`](../../docs/MIMO_PRIVACY.md) and
[`docs/MIMO_AUTH.md`](../../docs/MIMO_AUTH.md):

| Lane | Credential | Upstream | Fingerprint |
|---|---|---|---|
| **sk lane**（主） | platform OAuth 永久 `sk` | `{issued url}/chat/completions`（OpenAI 兼容） | `Authorization: Bearer` + `X-Mimo-Source: mimocode-cli`（官方 CLI `plugin/mimo.ts`） |
| **cookie lane**（副） | 收养桌面端 SSO 会话 + 运行时换票 | `{region}/route/chat/completions` | 无 Authorization、`X-Mimo-Source: mimocode-cli-free`、`X-Client-Version`、拼装票据 Cookie（`buildCookieHeader` 序） |

> **真机状态（26.922.222056 实测，2026-09-23）**：sk lane 可用（跑一次 sk 登录即可）；
> cookie lane **已端到端验证**：分区库的明文账号域 cookie（passToken/userId/cUserId）
> 经 `serviceLogin → /api/sts` 换出 serviceToken 后，`mimo-pro`/`mimo-flash` 均
> HTTP 200 返回真实内容。换票链规格见 [`docs/MIMO_AUTH.md` §6.2](../../docs/MIMO_AUTH.md)。

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

- Windows `%APPDATA%\Xiaomi MiMo AI\Partitions\xiaomi-account\Network\Cookies`（Electron 41 新布局优先，旧布局 `…\xiaomi-account\Cookies` 兜底；含 "Xiaomi MiMo" 变体）
- macOS `~/Library/Application Support/Xiaomi MiMo AI/Partitions/...`（**解密需 Keychain，M1 不支持** → 请用 sk lane）
- Linux `~/.config/Xiaomi MiMo AI/Partitions/...`

收养分两类行（单趟扫描，临时副本打开，绝不写桌面文件）：

- **引导行**：`.xiaomi.com`/`.account.xiaomi.com` 的 `passToken`/`userId`/`cUserId`
  （当前桌面包只落这些，且明文）——收养后立即走 `serviceLogin → /api/sts` 换票，
  收养即出可用凭据；换票失败也不丢弃引导材料，请求阶梯会再换。
- **上游行**：`*.xiaomimimo.com`（仅老构建才有）——按 M1 整 jar 语义渲染。

Windows v10 = AES-256-GCM + DPAPI(Local State)（真机验证通过）；
Linux v10/v11 = AES-128-CBC（PBKDF2 peanuts/saltysalt）。

上游 Cookie 头按 bundle `buildCookieHeader` 顺序拼装：`userId` → `serviceToken` →
`cUserId` → `<sid>_ph`（实测裸 serviceToken 会被 302）。会话失效判定由 chat 调用
本身驱动：401 / 302 / 被跟随的登录页 → 重换票 → 重试一次（"模型不在可用范围"
豁免续期）。区域 sgp/cn 实测可用，auto 按 sgp→cn 顺序自动绑定；ru/in 的 sid
未观测到，显式拒绝臆测，请用 `region` 钉死已测区域。

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
| `region` | `auto` | cookie lane 区域：auto（换票时按 sgp→cn 顺序绑定并持久）/ cn / sgp / ru / in（ru/in 未实测，换票会拒绝） |
| `x_client_version` | `26.922.222056` | cookie lane 的 X-Client-Version（桌面对齐） |
| `platform_url` | `https://platform.xiaomimimo.com` | sk lane OAuth 基址（CLI MIMO_PLATFORM_URL 等价） |
| `cookie_paths` | （自动探测） | 额外的 Chromium Cookies 文件路径（逗号分隔） |
| `exchange_user_agent` | （Go 默认） | 换票链 P1/P2 的 UA；实测可用值 `MiClaw/1.0`，仅在 passport 边缘开始拦 UA 时才需要配置 |
