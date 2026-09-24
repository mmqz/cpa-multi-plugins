# MiMo 桌面包深挖：引擎验证 · 身份与登录逻辑 · 社区代理交叉验证

> 任务：对官方桌面包（XiaomiMiMo-AI-latest-x64-setup.exe，`26.922.25448.0`）做反编译/反混淆深挖，
> 验证"推理引擎来自官方 CLI"的说法；提取桌面包自身的身份识别与登录逻辑；
> 对社区代理（LIGHTNINGWHALE/Xiaomi-Mimo-Desktop-Proxy）的每一条协议声明用官方包证据印证后落实结论。
> 方法：纯静态（app.asar 完整展开 + JS bundle 美化反混淆 + 引擎 bundle 逐段阅读），未运行任何官方二进制。
> 证据定位：`index.beauty.mjs:<行号>` 指 `out/main/index.mjs` 经 js-beautify 展开后的行号
> （美化产物与原始取证现场在 `/home/z/my-project/work/mimo-analysis/beautified/`，不入仓库）；
> `node.mjs:<行号>` 指引擎 bundle 原文件（esbuild 产物，自带 `// src/*.ts` 源码注释，可直接阅读）。
> 前置隐私分析见 docs/MIMO_PRIVACY.md（Task 62）；本文只补协议/身份事实，并回答其遗留问题。

## 1. "推理引擎来自官方 CLI"——验证成立

桌面包的推理引擎就是官方 CLI（XiaomiMiMo/MiMo-Code，OpenCode 分叉）的整包 bundle：

| 证据 | 内容 |
|---|---|
| `out/main/node.mjs`（36,815,770 B） | esbuild 打包的引擎 ES module，文件内保留 `// src/node.ts`、`// src/cli/bootstrap.ts` 等源码注释与 `debugId=9B89E22AF3B8B99564756E2164756E21`；文末 export 表 `bootstrap / Server / ModelsDev / Log / LLMServerTokens / JsonMigration / HostMcp / Database / Config / ChildProcessEnv / Auth` 与 MiMo-Code 仓库 `packages/opencode/src/node.ts` 的导出面一致 |
| 血统指纹 | 文内 `mimocode`×250、`opencode`×204；遥测事件 `model_call/tool_call/agent_request`、`MIMOCODE_ENABLE_ANALYSIS`、tracking.miui.com 客户端等与 Task 62 §3.3 逐项吻合 |
| 装载方式 | `out/main/engine-entry.mjs` = `import "./bun-shim.mjs"; import "./html-rewriter-shim.mjs"; import "./sqlite-bun-compat-shim.mjs"; export * from "./node.mjs"` —— 用 bun 运行时 polyfill 把 opencode 的 Bun 依赖垫平后**在 Electron 主进程内 in-process 运行**，非子进程 |
| 引擎无感 | 引擎 bundle 对 `xiaomi-sso-session` / `mimo-desktop.sso.invalid` / `user/xiaomi/me` / `available_models` **零命中**——引擎是纯 OpenCode 分叉，全部小米身份逻辑在桌面主进程侧包裹（§3） |

结论：桌面包 = Electron 壳（自研账号/遥测/审计层）+ 官方 CLI 整包当推理引擎。插件复刻协议时，
"官方 CLI 怎么调上游"（sk lane）与"桌面包怎么调上游"（Cookie lane）是两条都已完全可读的官方 wire。

## 2. 上游调用双通道（桌面主进程实装）

**通道解析 `Dl()`（index.beauty.mjs:2007-2023）**——每个模型请求二选一：

- `via:"proxy"`（Cookie lane，SSO 登录态）：`POST {区域基址}/route/chat/completions`，**无 Authorization**；
  `cp() = {base}/route`（1957-1959），`base` 来自区域表（1731-1733 + cn 默认）。
- `via:"key"`（sk lane，CLI 同款）：`POST {base_url}/chat/completions`，`Authorization: Bearer {sk}` +
  `X-Mimo-Source: {source}`；`base_url` 是 OAuth 下发的 `url` 字段（默认 `https://api.xiaomimimo.com/v1`，681 行）。

**Cookie lane 请求指纹（引擎请求实际出网形状）**：

1. `lq()` wrappedFetch（1976-2001）：删除 `Authorization` → 注入 `X-Mimo-Source: "mimocode-cli-free"`（1988）→
   401 时调 `renewLogin()` 成功则原样重试一次，失败走 `onAuthLost`（会话过期广播）。
2. `hl()` sessionFetch（4791-4801）：再注 `X-Client-Version: <应用版本>` → 经
   `session.fromPartition("persist:xiaomi-account").fetch` 出网（4791-4793）——**Cookie 由 Chromium 分区会话自动携带**。
3. UA 不伪装：Electron 默认 Chromium UA（仅 SSO WebView 用 `miNative PC/...` UA，4949-4951）。

**模型改写只在 Cookie lane**（`S()`/`k6()`，2003-2005、1888-1892）：`mimo-auto` 与 `*​/mimo-auto` 落到
`EE(Fc,void 0)=mimo-pro`（1884-1886）；白名单 `WS()` 仅放行 `mimo-flash`/`mimo-pro`，外加 `/^mimo-/` 前缀兜底开关。
sk lane 不改写模型（2010-2022）。

**sk-lane 请求会被桌面劫持回 Cookie lane**：`eH()`（2972-2993）拦截所有发往
`https://api.xiaomimimo.com/v1`、带 `X-Mimo-Source: "mimocode-cli"`（`sq`，1961）且模型在白名单内的请求体，
URL 重写为 `{cp()}/chat/completions`（或 `/responses`），body 归一化后走 wrappedFetch——即桌面登录态下，
即使引擎按 CLI 的 sk 形状发请求，实际也从 Cookie lane 出网。接线点 27162（`tH(t(), e.mimoAutoFetch, ...)`）。

**引擎 auth.json 哨兵**（`oq()` 1934-1951）：SSO 登录态下，桌面给引擎注入的凭据是
`auth["mimo-desktop"] = {type:"api", key:"xiaomi-sso-session", metadata:{base_url:"https://mimo-desktop.sso.invalid/v1"}}`
（常量 1913-1916；保留用户已有其它 provider 条目）。`.sso.invalid` 永不可达，配合 `Dl()` 的
`mimoAutoAvailable` 分支确保流量进 Cookie lane。provider 键名两个：`mimo-desktop`（SSO lane）与
`xiaomi`（sk lane，`Ji` 1915；`Nl()` 1918-1920 选择）。

## 3. 身份识别与登录逻辑（桌面自有部分，全部可复刻依据）

### 3.1 登录入口与 WebView 流

- 登录窗：480×720 BrowserWindow，partition `persist:xiaomi-account`，sandbox+contextIsolation（4958-4971）。
- **起始 URL 是 `{base}/user/xiaomi/me`**（`FE()` 4958-5003 + `Ll()` 3784-3785）：未登录时服务端 302 到
  account.xiaomi.com SSO 页（主进程按 URL 形状分类 `login-flow`：`pass/(serviceLogin|sns|auth)`、
  `fe/service/login` 前缀，3883），登录完成后回到 me 端点返回 JSON = 登录成功。
- 窗口注入固定 Cookie：`pass_ua=pc`、`uLocale=zh_CN`、`deviceId=<pc_指纹>`（`n_()` 4922-4947）。
- Google 登录支线：`https://account.xiaomi.com/pass/sns/login/auth?appid=google`，
  Google client_id `29336734752-6tkqife044ost06kpukmv0gh3qm8ffqg.apps.googleusercontent.com`，
  state `{sid:"passport", callback, appid:"google_xiaomi_desktop_demo"}`（5801-5807、5869-5886）；
  成功后把 `passToken`/`userId` 等以 `.xiaomi.com` 域写回分区 Cookie（kG，5886+）。

### 3.2 设备指纹（deviceId）

`pc_` + md5(机器序列)（4843-4911）：Windows 依次尝试 BIOS SerialNumber（wmic/CIM）→ 注册表
`HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`；macOS 用 IOPlatformUUID；兜底 node-machine-id。
伪造/空值黑名单过滤（`du()` 4855-4860，"To be filled"/全 0/全 x 等）。该值仅作为登录 Cookie 传给 account.xiaomi.com。

### 3.3 SSO 凭据体系与两阶段 serviceToken

- 账号对象缓存（`lG` 5124-5214）：`{passToken, userId, cUserId, serviceTokens:{sid→{…}}, nickName, allInfo}`，
  账号整体 7 天过期（`MAX_AGE_MS`=10080min）。注意：本构建唯一装配点 `new lG(NG())`（6716）接入的是
  **内存 Map**（`NG` 6690）——该缓存不落盘，持久面是分区 Cookie（§6 #1）；类内 "Account saved to
  secure storage" 日志措辞与实际注入的存储后端不符，属实现漂移，勿据此断言 DPAPI 落盘。
- **ServiceTokenManager（5215-5415）是小米原生 SSO SDK 的忠实 JS 移植**（日志注释直接引用原厂
  `SSO_curl.cpp` 行号）：
  1. Phase 1：`GET https://account.xiaomi.com/pass/serviceLogin`（Cookie：`userId`+`passToken`+`cUserId`，
     **不发 ssecurity/nonce**；UA `MiClaw/1.0`，5277）→ 响应体/重定向给出 Phase 2 URL（带 nonce/ssecurity 参数）；
  2. Phase 2：`GET <Phase 2 URL>` **故意不带 Cookie**（5358-5360，"matches original SDK
     SSO_curl.cpp line 728: cookies.clear()"）→ 响应 `Set-Cookie` 中的 `serviceToken` 或 `<sid>_serviceToken`
     即凭据（200 但缺 serviceToken 判失败，5372-5405，对照原厂 760-768 行）。
  3. serviceToken 按 sid 缓存/失效（5166-5190、5414-5419）；`passportapi` sid 用于用户信息 API。
- 用户信息：`GET https://api.account.xiaomi.com/pass/v2/safe/user/coreInfo`（5472），调用前确保
  `passportapi` serviceToken 在缓存（5481-5483；6302/6340 主动刷新）。
- serviceLogin 默认 `sid=passport`（5968）。

### 3.4 会话探测 / 续期 / 服务端门

- 探测：隐藏窗口加载 `{base}/user/xiaomi/me` 并轮询 `document.body.innerText`（`_()` 4147-4184、
  `k()` 4190-4227）——**me 端点即会话探针**。
- me 响应形状（`qc()` 3825-3858）：`{code:0, data:{userId, region, country, userMark, <displayName 字段>}}`；
  `code` 命中服务端拒绝码集合 `PE` 时带 `serverRejectedCode` 返回。
- 三道登录门：**邀请制** `GET {base}/user/invite/check`（6754、6806-6831，rejected → 撤销本次登录并广播
  `mimo:xiaomiLoginRejected`）；**账号区域**（`requiresCnAccount`，4110-4118）；**区域不支持**
  （4119-4126）。inconclusive 一律放行（fail-open）。
- 区域收养：me 响应 region ≠ 当前集群 → 先用隐藏窗口验证新集群 me 可认证 → 成功才 adopt
  （`w()` 4127-4145 + `nq()` 1862-1871）；区域表 `op`：cn 默认 + sgp/ru/in（1731-1733），可被 pin。
- 401 驱动续期：`renewLogin()`（89564）= 重新走一遍 me 探测；连续失败 `expireXiaomiSession()` +
  广播 `mimo:xiaomiAuthExpired`（89566）。黑名单阈值触发强制登出（89640 附近 `forceXiaomiLogout` 接线）。

### 3.5 认证日志自脱敏（正面设计，值得点名）

小米账号 SDK 的所有日志过 `GH` 正则过滤：含 `cookie|token|security|nonce|signature|...|credential|password`
字样的行只输出 `[miaccount-sdk] diagnostic details redacted`（4822-4841）。官方在凭据面上是收敛的。

## 4. 社区代理逐项交叉验证（LIGHTNINGWHALE → 官方包证据）

| # | 代理声明（proxy.py@5a65ad3） | 官方包证据 | 结论 |
|---|---|---|---|
| 1 | 上游 `https://mimo-server-cn.xiaomimimo.com/api` | 区域表 1731-1733 + `cp()` 1957-1959 | ✅ 属实（但桌面是四区域集群+收养，代理硬编码 cn） |
| 2 | 保温 `GET /user/xiaomi/me` | 桌面的登录/探测/换区全走 me（§3.4） | ✅ 同端点；桌面当会话探针用 |
| 3 | 头 `X-Mimo-Source: mimocode-cli-free` | `lq()` 1988 逐字命中——**官方 Cookie lane 的自报值** | ✅ 逐字节属实 |
| 4 | UA `mimocode/0.1.0` | 官方包零命中 | ❌ **代理虚构**。官方 Cookie lane 不设特殊 UA，加 `X-Client-Version` 头（hl 4797） |
| 5 | 聊天 `POST /route/chat/completions` | `Dl()` 2010 | ✅ 逐字属实 |
| 6 | 恒 `stream:true` + `stream_options.include_usage` | include_usage 在主进程 0 命中；引擎 bundle 19 命中（OpenCode 标准行为） | ⚠️ 引擎侧行为，非 Cookie lane 服务端要求；代理强制是为自身 SSE 聚合 |
| 7 | `reasoning_content` / `tool_calls` 增量 | 引擎标准字段（node.mjs 通传；主进程 38514/38553 同形） | ✅ 协议兼容 |
| 8 | 剥离 `user` 等字段 | 桌面无对应逻辑（自己构造请求） | ➖ 代理自主隐私加固，与官方不冲突；插件照抄 |
| 9 | 硬插 `# Memory system` MAGIC_PREFIX | 引擎侧 `buildMemoryInstructions()`（node.mjs:356749）本地生成，桌面恒带 | ✅ 来源定案：**客户端系统提示块**，服务端校验无证据——MIMO_PRIVACY Q1 就此关闭：可复刻同形内存块，但非已证实必需 |
| 10 | 静态四模型 `mimo-x-pro-preview`/`mimo-x-flash-preview`/`mimo-pro`/`mimo-flash` | **两个 `mimo-x-*` 官方包零命中**；官方核心表 = `mimo-auto`/`mimo-flash`/`mimo-pro`（1876-1878、2045-2080），另有 `mimo-v2.5-pro` 兜底默认（2079）与 sk lane 动态 `/user/available_models`（fw 2081-2103，Bearer sk，`{code:0,data:{groups:[{models:[…]}]}}`） | ❌ **半误**：`mimo-pro`/`mimo-flash` 正确，`mimo-x-*` 疑为旧版残留，且漏了 `mimo-auto` |
| 11 | 纯 Cookie 会话、无 token | Cookie lane 全程无 Authorization（1987 删除 + Dl proxy 分支无 Bearer） | ✅ 官方同源 |
| 12 | 401 → 重保温 → 重试一次 | `lq()` 1996 逐行对应（含"模型范围错误不算 401"豁免 `cq()` 1963-1974 + 正则 1962） | ✅ 逐字节属实 |
| 13 | 只读桌面 Chromium Cookie 库 | Cookie lane 宿主就是 `persist:xiaomi-account` 分区（4791-4793；Task 62 §4.4） | ✅ 同源；Windows 落 `%APPDATA%\Xiaomi MiMo AI\Partitions\xiaomi-account\Network\Cookies`（Electron 41/Chromium 146 布局；旧布局无 `Network` 段，插件两处都探测，详见 §6 #1） |

**代理没有做到、桌面有而我们应知道的**：§2 的 sk-lane 劫持（`eH`）、区域收养、模型范围错误豁免重试、
`X-Client-Version` 头、me 端点三道服务端门（invite/账号区域/区域支持）。

**代理疑似臆造、插件不采纳**：`mimocode/0.1.0` UA、`mimo-x-*` 模型名、
`token-plan-cn.xiaomimimo.com`（官方包内仅为演示夹具数据 `Ive()` 71135-71151，非真实调用）。

## 5. 对 mimo 插件（Go）的落实结论

1. **主通道 = sk lane**（与官方 CLI 逐字对齐）：OAuth `X25519+AES-256-GCM` 流拿 `{sk,uid,url}` →
   `{base_url}/chat/completions` + `Bearer` + `X-Mimo-Source: mimocode-cli`。加密格式逐字节：
   `u` = base64url(`ephemeralPub(32) ‖ nonce(12) ‖ ct ‖ tag(16)`)，key = `SHA256(ECDH(x25519))`，
   ephemeralPub 以 SPKI 前缀 `302a300506032b656e032100` 重建（MiMo-Code `plugin/mimo.ts:33-64`）。
2. **副通道 = Cookie lane**（与桌面引擎 lane 逐字对齐）：收养 `persist:xiaomi-account` 分区 Cookie；
   请求 = 删 Authorization、`X-Mimo-Source: mimocode-cli-free`、`X-Client-Version: <桌面版本>`；
   401 → 重探 me → 重试一次（含模型范围豁免）；模型 `mimo-auto → mimo-pro` 改写。
3. **模型表 = mimo-auto / mimo-flash / mimo-pro**（context 1e6 / out 128e3 / text+image→text，
   官方 `uw` 模板 2045-2066）；`mimo-auto` 仅在 Cookie lane 落到 `mimo-pro`。
4. **隐私硬边界**（docs/MIMO_PRIVACY §7 全部继续有效）：零遥测、零审计调用、零邀请/区域门探测、
   剥离 `user/metadata/service_tier/logprobs 族`、日志零内容、凭据 0600 不进环境变量。
5. `# Memory system` 不硬插（Q1 定案：客户端块，非必需；上游若校验会在实测暴露，届时再对齐）。

## 6. 本地凭据存储路径（安装后落盘位置）

桌面包（`package.json`: name `xiaomi-mimo-desktop-ai` / productName **"Xiaomi MiMo AI"** /
Electron 41.7.2 + Chromium 146.0.7680.216，取自主 exe 版本资源）安装后的凭据落盘全景，
全部经反混淆源码逐点定位：

| # | 凭据内容 | Windows 路径 | macOS / Linux 路径 | 形态与加密 | 证据 |
|---|---|---|---|---|---|
| 1 | **Cookie lane 全部会话 Cookie**（passToken / userId / serviceToken / *.xiaomimimo.com 会话） | `%APPDATA%\Xiaomi MiMo AI\Partitions\xiaomi-account\Network\Cookies`（旧布局 `…\xiaomi-account\Cookies` 仍在探测之列） | `~/Library/Application Support/Xiaomi MiMo AI/Partitions/xiaomi-account/Network/Cookies`；`~/.config/Xiaomi MiMo AI/…` | Chromium `Cookies` SQLite 库；`encrypted_value` = os_crypt v10（Windows：DPAPI 包裹的 AES-256-GCM，包密钥在 userData 根 `Local State` 的 `os_crypt.encrypted_key`；Linux：PBKDF2 硬编码） | 分区常量 3778/6006/6548；`session.fromPartition` 出网 4791-4793 |
| 2 | **sk lane 凭据**（OAuth 下发 `{sk, uid, base_url}`） | `%USERPROFILE%\.local\share\mimocode\auth.json` | `~/.local/share/mimocode/auth.json`（设了 `XDG_DATA_HOME` 则在其下；`MIMOCODE_HOME` 整体改根） | **明文 JSON，0600**；provider 键 `xiaomi`（sk 态）；SSO 态桌面在引擎视图中注入哨兵 `mimo-desktop`（key=xiaomi-sso-session，base_url=sso.invalid，仅内存不落盘）。同目录伴生：`mimo-key-name`、`mimo-login-pending.json`、`mcp-auth.json` | `ks()` 585-589（XDG_DATA_HOME→`~/.local/share/mimocode`）、auth.json 读取 65817、fF 文件组 594-603、引擎侧 `resolveMimocodeHome`（MiMo-Code `packages/shared/src/global.ts`，`auth.json` 落 `Global.Path.data` 0600） |
| 3 | 自动化/插件凭据（浏览器桥、飞书等 automation secrets） | `%APPDATA%\Xiaomi MiMo AI\plugin-secrets.json` 与 `profile-credentials.json` | 同目录同名 | JSON 容器；每条值为 `base64(safeStorage.encryptString)`：Windows=DPAPI、macOS=Keychain（Electron Safe Storage）、Linux=libsecret | `bR()` 77330 及 T/I 两个实例装配 |
| 4 | 小米账号对象缓存（passToken/userId/serviceTokens/nickName，7 天 TTL） | —— 仅内存，不落盘 —— | 同左 | 本构建唯一装配 `new lG(NG())`（6716）接的是内存 Map（`NG` 6690）；类日志 "saved to secure storage" 与实际后端不符（§3.3 已勘误） | lG 5124-5214、装配 89511 |

要点：

- **持久凭据只有两处**：分区 Cookies 库（#1，os_crypt 加密，随桌面包生命周期自动滚动）与
  `mimocode/auth.json`（#2，明文 0600）。其余均为内存缓存或非账号类凭据。
- #2 与官方 CLI 的凭据文件完全同位同形——桌面把引擎 in-process 跑起来后共享这一个 `auth.json`，
  因此 CLI 登录一次、桌面与 CLI 同时受益；反之桌面 sk lane 登录后 CLI 也能读到。
- #3 的 safeStorage 是 OS 级加密，但威胁模型与 DPAPI 一致：同用户任意进程可解，不是跨用户隔离。
- 我们插件的对应落地：自有凭据写宿主 auth-dir 的 `mimo-<lane>-<uid>.json`（0600、不进环境变量）；
  Cookie 收养按 #1 两种布局探测（`adopt.go`：`Network\Cookies` 优先、旧布局兜底），Windows 解密与
  桌面同链（DPAPI + `Local State`，`cookies_windows.go` + `cookies.go userDataRootFor`），macOS
  cookie lane 维持明示不支持。

§6 补充（真机盘点）：`mimocode` 数据目录里除上述凭据外的一切都**不含登录态**——
`mimocode.db` 是引擎 SQLite 会话/轨迹库（storage/db.ts:33，drizzle+bun-sqlite，聊天历史与运行索引，
隐私敏感但非凭据）；`storage/` 是旧版 JSON 会话存储迁移遗留；`log/`、`workflow/`、`memory/`、
`orchestrator/`、`worktree/` 均为运行数据。sk lane 登录态只认 `auth.json` 的 `xiaomi` 条目
（`mimo-key-name` 仅密钥名、`mimo-login-pending.json` 仅登录中临时态、`mcp-auth.json` 仅 MCP OAuth，
皆非账号登录态）；桌面 SSO 登录态不在此目录，只在 §6 #1 的分区 Cookies 库。

### §6.1 真机实测勘误（第二轮，Windows 11 实测，2026-09-23）

上表是**反混淆静态取证**的推断口径；本节是桌面 **26.922.222056**（Electron 41 /
Chromium 146）Windows 11 真机、桌面运行中且已登录状态下的**实测勘误**（插件仓 tip
277606b，`cmd/probe` 同链路验证）。测试经本机 Clash TUN（fake-IP 段，出站走
127.0.0.1:10573 代理）；所引 HTTP 状态码为服务端响应，代理不影响结论（同环境下
`platform.xiaomimimo.com` 返回 200，网络链路正常）。

| # | 静态推断 | 真机实测 |
|---|---|---|
| 1 | 分区库含 serviceToken 与 `*.xiaomimimo.com` 会话；`encrypted_value` = os_crypt v10 | 库在（`Network\Cookies`）✅ 但仅 **7 条账号域 cookie**（`.xiaomi.com`：`_ga`/`_ga_XWN774PE8J`/`cUserId`/`uLocale`；`.account.xiaomi.com`：`cUserId`/`passToken`/`userId`），**零 serviceToken、零 `*.xiaomimimo.com` 行**；且 **7 条 `encrypted_value` 全空、明文 `value` 列**——os_crypt 链路在本机/本构建上根本没有输入。整库 20480 字节原始扫描 `xiaomimimo`/`mimo-server` 零命中（含已删除页残留） |
| 2 | `mimocode/auth.json` 持久 sk 凭据 | **不存在**。目录存在（`builtin_skills/ compose/ log/ memory/ snapshot/ storage/ tool-output/ mimocode.db`），伴生文件 `mimo-key-name`、`mimo-login-pending.json`、`mcp-auth.json` 亦全部缺席；全盘扫 `auth.json` 仅命中无关应用。`mimocode.db`（202MB）全文扫 `auth.json`/`serviceToken`/`passToken`/`mimo_sk`/`xiaomimimo`/`sk-` 零命中。**auth.json 只在用户跑过一次 CLI/引擎 sk 登录后才生成；纯桌面安装不产生它** |
| 3 | `plugin-secrets.json` / `profile-credentials.json` | 两者均**不存在**（按需生成，非安装即有——自动化/浏览器桥功能从未使用） |
| 4 | 账号对象缓存仅内存 | 与实测一致，且这是全表唯一与真机吻合的行；服务票据的完整形态见下 |

**上游拒绝实验**：把 7 条 cookie 原样组装 header 打 `me` 端点（`cmd/probe` 同链路）——
`mimo-server-sgp`/`mimo-server-cn` 双双 **302** →
`account.xiaomi.com/pass/serviceLogin?callback=…mimo-server-sgp…/api/sts?sign=…&sid=mimosgp`
（cn 区 `sid=mimopc`）。即：上游要的是**绑定 `sid=mimosgp`/`sid=mimopc` 的 serviceToken**，
账号域 passToken/userId 不构成对 mimo-server 的鉴权。

**修订结论（推翻本节原"持久凭据只有两处"）**：桌面运行期的可用服务票据
（serviceToken）**不落盘**——桌面拿 passToken 走 `/sts` 换 serviceToken，结果只活在
进程内存（或为会话期 cookie，不持久化进 Cookies 库）。实测 passToken 过期时间在桌面
运行期间从 02:10:42Z 滚动刷新为 06:03:06Z，说明续期链路活跃但只回写 passToken 本身。
因此纯桌面安装的持久账号材料**只有一处**：分区库的账号域 cookie 集（本机上还是明文），
它足以**引导**（刷新登录态）但**不足以直接调用** mimo-server API。

**对插件的影响**：
- DPAPI + `Local State` 解密链在真机验证通过（`CryptUnprotectData` 成功解出 32 字节
  AES-256-GCM 密钥）——机制本身成立，保留以覆盖未来会加密 cookie 的构建/机器。
- `cookies.go` 的 `*.xiaomimimo.com` 过滤在真机上收养结果为空集（报
  "no usable *.xiaomimimo.com cookies"）——这是**正确行为**（账号域 cookie 重放会被 302 拒，
  过滤器挡住了无意义重放）。
- cookie lane 在当前桌面包上的可用路径重定义为 **M2**：收养 passToken/userId（本机构建
  上是明文，无需 DPAPI）作为引导材料，插件进程内复刻 `serviceLogin → /api/sts` 换票链，
  自行铸造 serviceToken 后再出网。M1 的 cookie lane 验收口径按此降级为"引导材料收养 +
  链路构件真机可用"，不承诺端到端调用。

### §6.2 换票链实测规格（M2 依据，2026-09-23 真机端到端验证）

§6.1 判定"可用服务票据不落盘"成立，但**补上后半句：它随时可以用账号域 cookie
现换出来**。测试者用真机活凭证实跑通了完整换票链并拿到真实模型回复
（`mimo-pro` 与 `mimo-flash` 均 HTTP 200，响应含 `mimo-v2.6-pro`/`mimo-v2.6-flash`
真实内容与 usage）。插件 M2 的 `exchange.go` 按此实现。

**换票两阶段**（静态证据 §3.3 + 实跑双确认）：

- **P1** `GET https://account.xiaomi.com/pass/serviceLogin?_locale=zh_CN&_snsNone=true&sid=<sid>&_json=true`
  Cookie 发分区库的明文账号域行 `passToken`/`userId`/`cUserId`（`uLocale` 在场，带上无害）。
  响应为 `&&&START&&&{json}&&&END&&&` 包裹，取 `code=0`、`ssecurity`、`nonce`、`location`。
  **`nonce` 真机线型是裸 JSON 数字**（2026-09-24 真机回归实测，如
  `4341996316119746560`，19 位超出 float64 尾数精度）——解组必须逐字保留字面量
  （插件与 probe 的 `nonceT` 自定义 UnmarshalJSON，数字/字符串双形态兼容；
  `Nonce string` 直接解组失败，`Nonce any` 会静默丢精度毁掉 clientSign 签名）。
- **P2** `GET <location>&clientSign=<urlencode(base64(sha1("nonce="+nonce+"&"+ssecurity)))>`
  **不发 Cookie**。HTTP 200，从 `Set-Cookie` 收 `serviceToken`(364B)、`userId`、
  `<sid>_ph`、`<sid>_slh`（全部保存）。

**三处只有实跑才暴露的坑**（均已写进插件实现与测试）：

1. **上游鉴权不是裸 `Cookie: serviceToken=`**。必须按 bundle `buildCookieHeader`
   的顺序拼 `userId` → `serviceToken` → `cUserId` → `<sid>_ph`。只发 serviceToken
   全是 302。`/user/xiaomi/me` 同样 302——**me 不能当探针**（对有效 cookie lane
   请求也返回 302），插件的 me 探针已退役，会话健康由 chat 调用本身判定
   （401/302/被跟随的登录页 HTML → 重换票 → 重试一次）。
2. **每个上游请求必须注入 `X-Mimo-Source: mimocode-cli-free`**（bundle `lq()` 同时
   主动删除 `Authorization`）——插件 M1 即已按 §2 实现，实测确认正确。
3. **`mimo-auto` 必须在客户端改写成 `mimo-pro`**，否则上游
   `chat_model_not_public`/biz_code 41105（`region=SGP, country=CL` 字样说明
   集群按账号国籍放行）。插件 `resolveAutoModel`（EE/k6 对齐）已实现，实测确认。

**区域与 sid 映射**：`sgp→mimosgp`、`cn→mimopc` 实测；`ru/in` 的 sid 从未观测到，
插件拒绝臆测（显式报错）。auto 模式按 sgp→cn 顺序尝试（sgp 为实测可用集群），
已在凭据上绑定的 sid 失败后自动跳过；`region` 配置可钉死。

**UA 备注**：实测请求 UA 为 `MiClaw/1.0`（测试者自选值）——passport 端点大概率不校验
UA，但这是单样本证据。插件默认不发伪装 UA（与 §4 拒绝 `mimocode/0.1.0` 臆造 UA
同一立场），留 `exchange_user_agent` 配置位；若某天 passport 边缘开始拦 UA，把实测
值填进去即可。

**落盘语义修订（最终版）**：分区库 = 引导材料（passToken/userId/cUserId，本机构建
明文）；serviceToken = 运行时现换、桌面自身不落盘；插件将其缓存在宿主 auth store
（随 jar 持久、0600），401/302 时自动重换——与桌面"每次运行现换"同生命周期，
多一层缓存。

**probe 判据对齐（插件 0.2.0 起）**：`cmd/probe` 诊断探针随插件同步换代——M1
时代的 me 端点判据退役（本节坑 1），健康判定改为与插件同款换票链：分区库盘点 →
DPAPI/v10 解密 → 收引导行（`pickBootstrap` 同序去重）→ `serviceLogin → STS` 按
sgp→cn 试换，`Set-Cookie` 收到 `serviceToken` 即判 cookie lane 可用；passport
拒绝（code 非 0）则判 passToken 已死、需桌面重登。旧版 probe 在"分区内无
`*.xiaomimimo.com` 行"时直接误判未登录退出——当前构建本来只落账号域行
（§6.1 #1），新版视为正常形态并在换票诊断里给结论。总结论三分类：**可用** /
**passToken 已死（需桌面重登）** / **响应形态异常（非网络问题，重试/重登无益）**
——2026-09-24 真机回归的教训：nonce 裸数字解组失败曾被误报成"网络/边缘原因，
稍后重试"，把排查引向错误方向（解组失败必须单列）。
