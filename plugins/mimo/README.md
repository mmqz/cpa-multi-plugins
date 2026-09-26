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

`auth.login.start` **直通小米 OAuth 授权页** URL `https://platform.xiaomimimo.com/authorize?pk=…&redirect_uri=…&key_name=…`（v0.2.9，zcode 0.1.3 同款直通；URL 为平台站绝对地址，任何面板源下 `window.open` 都直达真登录页）：

1. 点「登录」→ 浏览器直接打开平台 OAuth 授权页，完成小米账号授权；
2. 授权后平台 302 回 `http://localhost:<port>/auth?u=<密文>`——本机部署直达回环服务器自动完成；远程/Docker 部署该页打不开，复制地址栏完整链接；
3. 到插件菜单「Mimo」页粘贴提交（同源转发 `/oauth_submit`）——**粘贴后凭证直接保存**进宿主凭据库（v0.2.10 起经 `host.auth.save`，文件名 `mimo-key-<uid>.json` 与轮询路径同一份，不会重复）；无需保持 CPA 登录窗口开启。

解密与官方 CLI 同款：`base64url(ephemeralPub(32B) ‖ nonce(12B) ‖ ct ‖ tag(16B))`，
AES-256-GCM，key = `SHA256(ECDH(X25519))` → `{sk, uid, url}`。sk 为永久凭据，
AuthRefresh 仅回显元数据。登录 TTL 6 分钟；**单活策略**：再次点「登录」会关闭旧回环服务器并作废旧会话。

兜底入口：插件菜单「Mimo」（`/v0/resource/plugins/mimo/oauth_submit`，资源路由免管理鉴权；面板经 apiBase 前缀 iframe 渲染，任何部署形态都可达）。v0.2.8 起该页**状态感知**：有进行中的登录时显示冗余授权按钮 + 粘贴框，空闲时显示粘贴指引并每 5 秒自动刷新。直开时也支持 `GET ?cb_url=<完整失败链接>`。粘贴提交后凭证直接保存（v0.2.10），不再依赖登录窗口的轮询——窗口已关也能完成登录。

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

## 诊断探针（cmd/probe）

`mimo-cookie-probe.exe` 是独立于插件主包的孪生诊断工具，在已登录桌面的 Windows
机器上一键回答"这台机器的 cookie lane 能不能用"：分区库盘点 → DPAPI/v10 解密 →
收集引导行 → 按 sgp→cn 跑与插件同款的 `serviceLogin → STS` 换票，`Set-Cookie`
收到 `serviceToken` 即判可用。me 端点判据已随 §6.2 坑 1 退役（me 对有效请求也
302）。Cookie/票据明文只进内存，输出只有结构信息。构建：

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
  -o mimo-cookie-probe.exe ./cmd/probe
```

## 真机回归清单（0.2.0 起的 M2 验收口径）

1. **探针预检**：真机跑 `mimo-cookie-probe.exe`（默认 auto），预期"换票成功
   （区域 sgp 或 cn）"、总结论为 cookie lane 可用；若报 passToken 被拒，先在
   桌面重新登录再测。
2. **插件载入**：宿主载入 mimo 插件（c-shared：linux `.so` / windows `.dll`
   需 mingw 工具链），`plugins.configs.mimo` 留默认（region=auto）。
3. **收养即换票**：注册/重启触发收养后，auth store 里 mimo 凭据应同时含引导行
   与换出的 serviceToken 行（`<sid>_ph`/`<sid>_slh` 在场），region/sid 已盖章。
4. **端到端调用**：经插件调 `mimo-pro`/`mimo-flash`，预期 200 真实回复
   （拼装序 Cookie + `X-Mimo-Source: mimocode-cli-free`、无 Authorization）。
5. **自愈阶梯**：把 auth store 中 serviceToken 行的值改坏 → 再次调用应触发
   401/302 → 重换票 → 重试成功（"模型不在可用范围"类 401 豁免续期）。
6. **区域钉死**（可选）：`region=cn` 时全程只试 `mimopc`，不回落 sgp。
