# MiMo 插件前置隐私分析（Privacy Pre-Review）

> 任务：在为 CPA 编写 `mimo` 插件之前，对三个信息来源做隐私分析，划定"哪些协议可复刻、哪些数据路径绝不复刻"的边界。
> 方法：纯静态分析（开源仓库源码逐文件阅读 + 官方安装包脱壳与字符串级扫描），**未运行任何官方二进制、未发起任何登录或请求**。
> 结论均标注证据位置（文件/行号或提取物路径），供后续协议实现时复核。
> 本文档只谈隐私与数据流；协议 wire 格式细节在实现阶段另行补录 docs/PROTOCOL.md。

## 0. TL;DR

| 来源 | 一句话画像 | 隐私风险定级 |
|---|---|---|
| A. LIGHTNINGWHALE/Xiaomi-Mimo-Desktop-Proxy | 社区本地代理：读桌面端 Cookie 转 OpenAI 兼容 API；无第三方遥测 | **低**（1 处本地日志瑕疵：错误时可把用户消息前 400 字符落盘） |
| B. XiaomiMiMo/MiMo-Code（官方 CLI，OpenCode 分叉） | 终端编码助手；**默认开启元数据遥测**（tracking.miui.com），但事件只含计数不含内容 | **中**（默认 opt-out 需 `MIMOCODE_ENABLE_ANALYSIS=false`） |
| C. XiaomiMiMo-AI-latest-x64-setup.exe（官方桌面端） | Electron 壳 + 内嵌 MiMo Code 引擎；**账号级 OneTrack 遥测 + 用户内容服务端审计**；捆绑全区域 MIUI SDK 与广告配置域名 | **中高**（遥测绑定小米账号 UID；输入文本/图片/音频与输出均送服务端审计） |

**对插件的硬边界**：只复刻聊天/认证协议 lane（A+B 已给出完整 wire 形状），**零遥测、零审计调用、零桌面专属特性**；凭据 0600 落盘、不写内容日志、`user` 字段照 A 的做法剥离。详见 §7。

## 1. 来源清单与取证记录

| 来源 | 版本/标识 | 取证哈希/位置 |
|---|---|---|
| A. Xiaomi-Mimo-Desktop-Proxy | commit `5a65ad3`（2026-09-17） | 本地克隆 `/home/z/my-project/work/mimo-analysis/Xiaomi-Mimo-Desktop-Proxy` |
| B. MiMo-Code | commit `1579e7d`（2026-09-22，package 名 `opencode`，OpenCode 分叉） | 本地克隆 `.../mimo-analysis/MiMo-Code` |
| C. 桌面安装包 | NSIS，ProductVersion `26.922.25448.0`（2026-09-22 构建），ProductName "Xiaomi MiMo AI" | 251,566,032 字节，SHA-256 `f8e180a9ac5b98f85e44c8780ca3a3f399a08f8f0eaa3c54771c7de43eb51676`；脱壳至 `.../mimo-analysis/app/`（NSIS → `$PLUGINSDIR/app-64.7z` → Electron → `resources/app.asar` 99MB 已展开） |

## 2. 来源 A：社区本地代理（协议事实 + 隐私盘点）

### 2.1 协议事实（对插件直接可用）

- 上游基址：`https://mimo-server-cn.xiaomimimo.com/api`（可 `MIMO_PROXY_UPSTREAM` 覆盖）。
- 会话保温：`GET /api/user/xiaomi/me`，头 `User-Agent: mimocode/0.1.0`、`X-Mimo-Source: mimocode-cli-free`；非 200 即判会话失效（`proxy.py:144-158`）。
- 聊天：`POST /api/route/chat/completions`，**OpenAI 兼容 wire**（messages/tools/stream），上游恒 `stream:true` + `stream_options.include_usage`，SSE 聚合回非流式（`proxy.py:349-396`）；delta 支持 `reasoning_content`（思维链）与 `tool_calls` 增量聚合（`proxy.py:227-292`）。
- 401/403 → 重新保温会话后重试一次（`proxy.py:360-365`）。
- 请求体清洗 `normalize_body`（`proxy.py:181-212`）：模型名白名单校验；剥离 `service_tier / metadata / modalities / audio / response_format / n / logprobs / top_logprobs / logit_bias / user`（**隐私正向：`user` 字段不透传**）；系统消息强制前置 `# Memory system` MAGIC_PREFIX（桌面端引擎的持久记忆系统提示，桌面恒带此块，上游路由是否强校验未证实——开放问题 Q1）。
- 模型表（静态）：`mimo-x-pro-preview` / `mimo-x-flash-preview` / `mimo-pro` / `mimo-flash`（`proxy.py:56-61`）。
- 认证载体：**无 token，纯 Cookie 会话**——直接读桌面端 Electron 的 Chromium Cookie 库（macOS：`~/Library/Application Support/Xiaomi MiMo/Partitions/xiaomi-account/Cookies`，sqlite 只读打开，`proxy.py:108-121`）。

### 2.2 隐私盘点

| 项 | 事实 | 评价 |
|---|---|---|
| 第三方外发 | 全部流量只到 `mimo-server-cn.xiaomimimo.com`；无任何遥测/统计调用 | 干净 |
| Cookie 处理 | 本机 sqlite 只读；Cookie 仅发往小米上游；README/代码均声明不上传 | 干净 |
| 本地 API key | `mimo-local-` + 24 字节 urlsafe 随机，落 `~/.config/xiaomi-mimo-desktop-proxy/api-key`，`chmod 0600`，恒定不复用轮换 | 良好（恒不轮换是可用性取舍，非隐私问题） |
| 监听面 | 默认 `127.0.0.1:18080` | 良好 |
| **本地日志泄露** | `/v1/responses` 上游报错时 `logger.error(... body_msgs=%s)` 把**消息数组前 400 字符**写进本地 `proxy.log`（`proxy.py:630-636`） | **瑕疵**：用户消息片段落盘明文日志。我们的实现**不复制此行为**（日志只记状态码与长度，不记内容） |
| 请求最小化 | 剥离 `user` 等字段 | 正向，照抄 |

## 3. 来源 B：官方 CLI MiMo-Code

### 3.1 身份与血统

`package.json` name = **`opencode`**：MiMo Code 是 OpenCode（开源编码 agent）的小米分叉，包结构 `packages/{opencode,console,identity,enterprise,desktop,...}`。这意味着认证/遥测代码全部可读，无需黑盒猜测。

### 3.2 认证流（三条，互不混用）

**（1）小米平台浏览器 OAuth（`xiaomi` provider，插件主参考）** — `packages/opencode/src/plugin/mimo.ts`
1. 本地生成 X25519 密钥对，起 loopback callback server（随机端口，5 分钟 TTL）；
2. 打开 `https://platform.xiaomimimo.com/authorize?pk=<SPKI base64url>&redirect_uri=http://localhost:<port>/&kn=mimocode&key_name=mimo-code-cli-key-<8hex>`（key_name 持久化在 data 目录 `mimo-key-name`，复用不重生成）；
3. 浏览器回传 `?u=<密文>`：格式 `ephemeralPubKey(32B) + nonce(12B) + ciphertext + tag(16B)`，AES-256-GCM，密钥 = `SHA256(ECDH(x25519))`，明文 `{sk, uid, url}`；
4. 落盘 auth.json：`{type:"api", key:sk, metadata:{uid, base_url:url}}`；
5. 聊天头：`X-Mimo-Source: mimocode-cli`（`plugin/mimo.ts:198-201`）。

**（2）OAuth Device Code（通用，企业/自建 console）** — `packages/opencode/src/account/account.ts`
- `POST {server}/auth/device/code`（body `{client_id:"opencode-cli"}`）→ `device_code/user_code/verification_uri_complete`；轮询 `POST {server}/auth/device/token`（`urn:ietf:params:oauth:grant-type:device_code`）→ `{access_token, refresh_token, expires_in}`；`GET {server}/api/user`（`{id,email}`）、`GET {server}/api/orgs`、`GET {server}/api/config`（带 `x-org-id`）；刷新走同 token 端点 `grant_type=refresh_token`，到期前 5 分钟主动刷新（`account.ts:134-136`）。server URL 由用户在 `mimocode account login <url>` 提供，非小米默认路径。

**（3）第三方 provider**：GitHub Copilot（device code）、x.ai（device code）、OpenAI/Codex OAuth、Claude Code 导入——与我们无关，仅证明框架能力。

**凭据存储**：`auth.json` 位于 Global data 目录（XDG：`~/.local/share/mimocode/`，`MIMOCODE_HOME` 可重定向；`packages/shared/src/global.ts`）。桌面端嵌入引擎时改为**进程内注入**（`auth/index.ts:16-24` 注释明言弃用 `MIMOCODE_AUTH_CONTENT` 环境变量通道的原因：bash 子进程 `echo $VAR` 即可拖库、兄弟进程可读 `/proc/<pid>/environ`）——**这是正向安全设计，插件侧同样不使用环境变量传凭据**。

### 3.3 遥测（本分析最关键发现）

**CLI 自带指标上报默认开启**（opt-out）：

- 开关：`MIMOCODE_ENABLE_ANALYSIS` **缺省为 true**，`flag.ts:106-108` 注释原文 "Defaults to true (analytics enabled). Set MIMOCODE_ENABLE_ANALYSIS=false to opt out of POSTing model_call/tool_call/agent_request metrics."
- 挂载点：`project/bootstrap.ts:67` `Metrics.subscribe()`——项目会话启动即订阅。
- 端点：`https://tracking.miui.com/track/v4/o`，`APP_ID 31000402765`（`metrics/client.ts:1-2`）。
- 事件与字段（`metrics/event.ts`）：

| 事件 | 上报字段 |
|---|---|
| `model_call` | finish_reason、ttft_ms、latency_ms、cached_read_tokens、model_id、provider、total_tokens_in/out |
| `tool_call` | tool_name、input_bytes、output_bytes（**字节数非内容**）、tool_call_id、status |
| `agent_request` | phase、task_type、surface、total_tokens_in/out、files_changed、validation_status |
| `try_best_detected` | reason(edit_repeat/bash_retry/action_streak)、provider、model_id、count、similarity、action |

- 信封头（`client.ts:6-29`）：`app_id`、`instance_id`（**每报随机新 UUID，非持久标识**）、`uid`（**session_id，会话域非账号域**）、`e_ts`。`installation_id` 落盘 `data/installation_id` 但**代码注释明确不进 wire**（`subscriber.ts:14-17`）。
- 内容面：**无消息文本、无代码、无文件路径、无用户标识**——纯计数与枚举。发送 fire-and-forget（`catch(()=>{})`，60s 超时）。

**其他数据面排查（均为阴性）**：

| 检查项 | 结果 |
|---|---|
| PostHog / Sentry / MixPanel / 友盟 | 无（源码零标记；桌面端 sentry 字符串仅为内嵌 MCP 配置示例文本） |
| OpenTelemetry | 仅 `experimental.openTelemetry` 配置显式开启才启用（`session/llm.ts:727`），默认关 |
| `getEnvInfo()`（含 hostname/username/homedir/cwd/installation_id） | **无生产调用方**（仅导出与测试），当前版本不发出去——记为"保留诊断面"，后续版本可能启用，需跟踪 |
| 外部域清单 | 全部为文档/文档化服务：mimo.xiaomi.com、platform/api.xiaomimimo.com、models.dev（provider 元数据库）、api.opencode.ai（GitHub App 集成）、registry.npmjs.org、各 provider API——无隐蔽回传域 |
| 更新检查 | 拉取 `mimo.xiaomi.com/install(.ps1)` 脚本比对版本（`installation/index.ts:154,180`） |

## 4. 来源 C：官方桌面端安装包（脱壳静态分析）

### 4.1 打包结构

NSIS（32 位引导）→ `$PLUGINSDIR/app-64.7z`（nsis7z）→ Electron 应用：
`Xiaomi MiMo AI.exe` + `resources/app.asar`（99MB）+ `elevate.exe`（electron-updater 提权）+ `browser-extension/browser-bridge.crx`（浏览器控制桥）+ `computer-use-windows/runtime.ps1`（键鼠/屏幕控制，PowerShell 运行时）+ `evolve-seed/`（Python 自进化 harness：bootstrap.py、GROWTH.md）+ `runtimes/win32-x64`（内嵌引擎运行时）。

**关键集成事实**：桌面端拉起内嵌 MiMo Code 引擎时强制注入环境 `MIMOCODE_ENABLE_ANALYSIS:"false"`（另有 `MIMOCODE_CODEX_MODE:"false"`、`MIMOCODE_DISABLE_CHECKPOINT:"1"`、`MIMOCODE_E_IMPORT:"1"` 等）——**CLI 的 tracking.miui.com 直发通道在桌面端被显式关闭**。桌面有自己的遥测（下节）。

### 4.2 桌面自有遥测：OneTrack（账号域，默认开）

- SDK 形态：小米 OneTrack JS SDK 内嵌主进程（`out/main/index.mjs`），**区域端点表**：`cn: tracking.miui.com`、`in: tracking.india.miui.com`、`ru: tracking.rus.miui.com`、`tjv1: tjv1.tracking.miui.com`、`auto: auto.tracking.miui.com`、默认/intl: `tracking.intl.miui.com`；intl 另拉 `https://sdkconfig.ad.intl.xiaomi.com`（**广告配置域**）。
- 应用标识：`app_id "31000402860"`（注意与 CLI 的 `31000402765` 不同）、应用名 `mimo-desktop`、key `mimo-app-505412`（`index.mjs` @2185317 附近）。
- **UID = 小米账号 user_id**：登录态变化时 `setUid(auth.user_id)`（`index.mjs` @2181038 附近）——遥测与账号身份绑定。
- 本地队列：`onetrack-queue.jsonl` + `onetrack-cc.json` 于应用数据目录（productName 分 domestic "Xiaomi MiMo" / overseas "Xiaomi MiMo AI"）。
- 观测到的事件：
  - `desktop_performance`：process_creation_time、app_ready_time、totalmem(GB)、systemversion（初始化即发）；
  - **引擎事件转发**：`{"metrics.model_call"→"model_call", "metrics.tool_call"→"tool_call", "metrics.agent_request"→"agent_request"}`（`index.mjs` @784543）——即 §3.3 的三类事件经 OneTrack 通道继续上报（`MIMOCODE_ENABLE_ANALYSIS:false` 只挡引擎直发，不挡桌面转发）；
- 跳过条件：仅 E2E 与 `MIMO_ONETRACK_TEST=1`（开发态）；**未发现用户同意门**。
- 隐私面评价：与 CLI 指标同级的"计数型"字段集（无代码/消息内容），但**关联小米账号 UID**，且**默认开、无同意门、区域路由含广告配置域**。

### 4.3 用户内容服务端审计（内容出域，桌面特有）

- 端点：`GET/POST {server}/audit/check`、`/audit/check-async`、`/audit/result`（base = 区域 mimo server，可 `MIMO_REVIEW_API_BASE_URL` 覆盖；`index.mjs` @229140）。
- 内容分型（appId 映射 @229357）：`mimo_pc_input_text` / `input_image` / `input_audio` / `input_text2image` / `output_text` / `artifact` / `image`——**用户输入（含音频）与模型输出都会送服务端审核**，由隐藏 16px BrowserWindow 执行 JS 完成（`HN()`）。
- 定性：小米一方审核平面（与聊天同域），不是第三方；但属于"用户内容出域"事实，桌面专属行为。**插件不复刻**——插件只调 `/route/chat/completions`，路由侧是否有服务端自带审核不因客户端而异（不可归避，如实披露）。

### 4.4 账号会话与其它平面

- 小米 SSO：内嵌 WebView partition `persist:xiaomi-account`，UA `miNative PC/Normal Windows_NT/<ver> SDKV/1.0.0 DEVT/PC DEVS/Windows APP/miaccount_desktop APPV/0.1.0`；关注的 Cookie 名为 `passToken` / `serviceToken` / `*_serviceToken` / `*_ph*`（SSO 清理逻辑 @`_H/kH`）。**即来源 A 所读 Cookie 的宿主**；Windows 下同样落在 `%APPDATA%/<productName>/Partitions/xiaomi-account/Cookies`（Chromium sqlite）。
- 区域路由：`mimo-server-{sgp,ru,in}.xiaomimimo.com`（intl 默认 SGP）+ CN `mimo-server-cn.xiaomimimo.com`，区域探测/缓存/收养机制齐备；`/route` 为聊天前缀（`cp()=base+"/route"`），图片生成 `{base}/route/images/generations` + `X-Mimo-Source: mimocode-desktop`。
- 自动更新：electron-updater generic provider = `https://mimocode-cdn.xiaomimimo.com/mimocode/mimodesktopai/`（与我们下载安装包同一 CDN，良性）。
- 崩溃收集：无 Sentry/Crashpad SDK（sentry 字符串为 MCP 示例文本）。

## 5. 端点与域名总清单

### 5.1 协议必需（插件会触碰）

| 域名/路径 | 用途 | 认证 |
|---|---|---|
| `mimo-server-cn.xiaomimimo.com/api`（+ sgp/ru/in 区域变体） | 桌面会话 lane：`/route/chat/completions`、`/user/xiaomi/me`、图片 `/route/images/generations` | 小米账号 Cookie |
| `api.xiaomimimo.com/v1` | CLI sk lane：OpenAI 兼容聊天（models.dev 注册的 `xiaomi` provider api）、websearch、voice | OAuth 换来的 `sk` key |
| `platform.xiaomimimo.com` | OAuth 授权（`/authorize`、`/authorize/callback`、`/authorize/code/callback`） | X25519 pk 加密回传 |
| OAuth 返回的 `url` 字段 | CLI 登录后实际 base_url（随账号/区域下发） | sk |

### 5.2 服务方自用（插件不触碰）

| 域名/路径 | 用途 | 触发方 |
|---|---|---|
| `tracking.miui.com` 等六区域 | OneTrack 遥测 | 桌面端（默认）；CLI（默认，`MIMOCODE_ENABLE_ANALYSIS` opt-out） |
| `sdkconfig.ad.intl.xiaomi.com` | 广告/SDK 配置 | 桌面 intl |
| `mimo-server-*/api/audit/*` | 用户内容审核 | 桌面端专属调用 |
| `mimo.xiaomi.com` | 官网/安装脚本/文档 schema | CLI 更新检查、文档 |

### 5.3 第三方（均非回传，仅在用户主动选用时触达）

`ofox.ai` / `zenmux.ai` / `openrouter.ai` / `pkulaw` / `semanticscholar` 等均为**内置 provider/工具预设目录**（需用户自配 API key 才会调用）；`models.dev` 为 provider 元数据库（拉取模型目录）。

## 6. 数据出境对照表（"谁把什么发到哪里"）

| 数据 | 去向 | 默认 | 含内容? | 来源 |
|---|---|---|---|---|
| 聊天消息 | `mimo-server-*`（Cookie lane）/ `api.xiaomimimo.com`（sk lane） | 必然（核心功能） | **是**（本质） | A/B/C |
| 登录凭据（Cookie / sk） | 仅小米对应主机 | 必然 | 凭据本身 | A/B/C |
| model_call/tool_call/agent_request 元数据 | `tracking.miui.com/track/v4/o`（CLI） | **开**（opt-out） | 否（计数/枚举） | B |
| 同上三类事件 + desktop_performance | 区域 tracking.*.miui.com（OneTrack），**关联小米 UID** | **开**（无同意门） | 否（计数/枚举/时序） | C |
| 用户输入 text/image/audio + 输出 | `mimo-server-*/api/audit/*` 审核平面 | 随桌面功能 | **是** | C |
| installation_id | 不进 wire（仅本地） | — | — | B |
| hostname/username/cwd（getEnvInfo） | 当前无调用方，未出境 | — | — | B（保留面） |

## 7. 对 `mimo` 插件的隐私边界（设计约束）

**做（复刻协议，与 A/B/C 的既有行为对齐）**：
1. sk lane（主推，来源 B §3.2-(1)）：platform OAuth X25519 流 → `{sk, uid, url}` → OpenAI 兼容调用；`X-Mimo-Source: mimocode-cli` 与官方 CLI 同值（客户端指纹对齐是本仓库既有方法论）。
2. Cookie 收养 lane（次选，来源 A）：读本机桌面端 Chromium Cookie 库（Windows 下 `persist:xiaomi-account` 分区）复用会话；只读打开、仅发小米主机。
3. 请求最小化照抄 A：剥离 `user`/`metadata`/`logprobs` 等字段；`stream_options.include_usage`。
4. 凭据落盘 auth-dir、0600、不进环境变量（对齐 B 桌面注入注释的安全理由）。

**不做（隐私边界，硬约束）**：
1. **零遥测**：不复刻 tracking.miui.com / OneTrack 任何调用；不发 installation_id / uid 之外的任何统计。与仓库既有插件一致（cpa-multi-plugins 全家无遥测）。
2. **不调审计平面**：`/audit/*` 是桌面端的内容出域行为，插件不复制；路由侧服务端审核（若存在）属小米服务端策略，客户端无法增减，如实披露。
3. **不落内容日志**：不复制 A 的错误日志含消息片段行为（只记状态码/长度/错误码）；日志不含凭据。
4. **不碰桌面专属面**：computer-use（键鼠/屏幕）、browser-bridge、evolve/memory 系统、图片生成暂不进插件范围。

**风险与开放问题**：
- **Q1（协议）**：`# Memory system` MAGIC_PREFIX 是否为上游必需？实现时做有/无对照实测（A 注入它 mimic 桌面，但未证实服务端校验）。
- **Q2（实现风险）**：Windows 下 Chromium Cookie 值为加密存储（DPAPI/AES-GCM，密钥在 Local State）——Cookie lane 能否直读要在 Windows 真机验证；sk lane 不受此影响，这也是把 sk lane 定为主路线的原因。
- **Q3（模型表）**：A 为静态四模型，桌面端代码另有 `mimo-auto` 路由与区域默认（`Fc="mimo-auto"`）；插件模型目录策略（静态 vs 探测）在实现阶段定。
- **Q4（跟踪面变化）**：B 的 `getEnvInfo()` 目前无调用方但已具备 hostname/username 采集能力，后续版本可能启用；桌面端 OneTrack 事件集可能扩展。本文档记录的是 2026-09-22 时点（mimocode `1579e7d` / 安装包 `26.922.25448.0`），重大版本升级后应复核 §3.3/§4.2。

## 8. 取证附录

- 安装包 SHA-256：`f8e180a9ac5b98f85e44c8780ca3a3f399a08f8f0eaa3c54771c7de43eb51676`（251,566,032 字节，2026-09-23 自 mimocode-cdn 下载）。
- 关键代码定位：CLI 遥测 `MiMo-Code/packages/opencode/src/metrics/{client,event,subscriber,installation}.ts` + `flag/flag.ts:106-108` + `project/bootstrap.ts:67`；OAuth `plugin/mimo.ts` / `account/account.ts` / `auth/index.ts`；桌面遥测 `app_asar/out/main/index.mjs`（@2185317 OneTrack init、@784543 事件转发、@229140 审计端点、@27798 区域表）；引擎强制 env 见 `index.mjs` MIMOCODE_ENABLE_ANALYSIS 注入点。
- 中间产物（脱壳目录、克隆）位于 `/home/z/my-project/work/mimo-analysis/`，不入仓库。
