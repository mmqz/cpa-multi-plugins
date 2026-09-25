# cpa-multi-plugins

> CPA (CLIProxyAPI) 动态库插件集合：CodeBuddy / WorkBuddy / Trae / Qoder 的 CN + Intl 版本，以及 ZCode（智谱 GLM 编码套餐，Z.AI + BigModel）
>
> 主分支 4 个插件（workbuddy / trae / qoder / zcode）覆盖 4 个平台 × 2 个版本（CodeBuddy CN 与 WorkBuddy 已合并）+ 智谱 GLM 编码套餐（Z.AI + BigModel），让 CPA 一个 `/v1/chat/completions` 接口调用所有模型。ZCode 已于 v0.12.84 并入主分支。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows-lightgrey)]()
[![Release](https://img.shields.io/badge/release-v0.12.85-blue)](../../releases)
[![Build](https://img.shields.io/badge/build-passing-brightgreen)](../../actions)

## 项目目标

为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 提供完整的国内 AI IDE 平台 provider 插件，让 CPA 一个 `/v1/chat/completions` 接口就能调用所有模型。

## 插件清单

| 插件 | 平台 | 协议 | 签到 | 配额 | 状态 |
|---|---|---|---|---|---|
| `workbuddy` | CodeBuddy / WorkBuddy 三区合并（CN + Global + Intl） | OpenAI 兼容 | ✅ 每日 | ✅ credits | ✅ functional |
| `trae` | Trae 三变体合并（Code CN + SOLO CN + Intl） | llm_utils_chat / Web SOLO | ✅ 每日 | ✅ v2 pack 优先级 | ✅ functional |
| `qoder` | Qoder 双区合并（CN + Intl） | COSY 签名 | ✅ 每日 | ✅ quota | ✅ functional |
| `zcode` | 智谱 GLM 编码套餐双 provider 合并（Z.AI + BigModel） | OpenAI 兼容 + anthropic 翻译 + 签名 V4 + off-peak 票务 | —（claim 需验证码侧车） | ✅ billing/balance | ✅ functional |


## 功能对标

基于 cockpit-tools / 9router / OmniRoute / traework2api / Sliverkiss/cpa-plugin 五个项目的最新实现，完整对标以下功能：

### ✅ OAuth 完整流程
- `GetLoginGuidance` → 浏览器登录 → `authCode` → `ExchangeToken` → `GetUserInfo`
- 本地 callback listener（随机端口，5 分钟 TTL，async poll 模型）
- 多账号同时登录（按 uid 区分）

### ✅ Token 自动刷新
- `RefreshTokenIfNeeded`（24h skew，提前刷新避免过期）
- 每天 03:00 全量刷新（防 Keycloak offline-session expiry）
- 原子写回 auth 文件（`tmp + rename`，0600 权限）

### ✅ 多账号 pool
- credit-aware scheduler（积分降序挑选）
- 4 档 cooldown 状态机：
  - `CoolPlan` 12h（1005 plan 权益不足）
  - `CoolSoft` 60s（429/404 软限流）
  - `CoolErr` 10m（连续 3 次错误）
  - `Disable`（401 session 失效，需人工重登）
- `NoteSuccess` / `NoteError` 跟踪

### ✅ 每日签到（CN 平台）
- 每天 09:00 自动触发
- Trae: `api.trae.cn/trae/api/v2/ug/checkin_credits/{status,claim}`
- CodeBuddy CN: `codebuddy.cn/v2/billing/meter/{checkin-activity-status,daily-checkin}`
- QoderWork CN: v0.12.80 起走 campaigns 领取（`/sash/api/v1/me/campaigns/{id}/claim`）——legacy `daily-check-in/{status,claim}` 已被上游全局禁用（claim 恒 409 不发积分，2026-09-21 实测），status 保留为只读统计
- **9074 限流识别**（Trae 业务码，三家参考项目都没做，cpa-multi-plugins 独有）

### ✅ 积分/配额查询
- Trae v2 积分制完整解析（对齐 cockpit-tools `apply_usage_response`）：
  - 过滤废弃 pack（`product_type == 3` PROMO_CODE）
  - 过滤隐藏/已取消 pack（`is_hide || status == 3`）
  - CN pack 优先级：`CNExpress(100) > Ultra(6) > Pro+CN(5) > Pro+(4) > Pro(1/9) > Lite(8) > Free(0)`
  - Intl pack 优先级：`Ultra(6) > Pro+(4) > Pro(1/9) > Lite(8) > Free(0)`
  - `fastRequestAvailable` / `fastRequestPerMonth` 字段（来自选中 pack）

### ✅ CodeBuddy content filter 规避（对齐 OmniRoute codebuddy-cn.ts）
- AGENT_PATTERN 正则匹配（Claude Code / Cursor / Windsurf / Cline / Aider / Copilot / Cody 身份行）→ 替换中性 prompt
- 长度兜底（system prompt > 2000 bytes → 替换）
- `reasoning_effort` 镜像（非 none → `reasoning_summary: "auto"`）
- 大工具描述压缩（tools JSON ≥ 64KB → 删 `tool.function.description`）
- 强制 `stream=true`（腾讯后端拒非流，code 11101）
- `forceMaxThinking` for hy3/hy4-family models（hy3 家族已上游退役，逻辑保留仅为兼容历史 pin）

### ✅ Executor（execute + execute_stream）
- 非流式：上游 SSE 聚合 → 单个 `chat.completion` 对象
- 流式：实时转发 OpenAI SSE chunks（`plan_item` → `delta.content`，`token_usage` → `usage`）

## 为什么是 4 个独立插件而不是合并？

CPA 的插件架构基于 `auth.identifier` + `executor.identifier`——**每个 `.so` 只能注册一个 provider name**。CPA 的 `HasAuthProvider(provider)` 按 identifier 精确匹配，所以合并家族后统一使用单一 provider key（`workbuddy` / `qoder` / `trae` / `zcode`），区域内差异（CN/Intl/SOLO 等）通过账号文件内的字段路由，旧插件名的账号文件启动时自动收养。

### 如果你想减少插件数量

**方案 1：用 `openai-compatibility` 配置替代插件**（推荐给 OpenAI 兼容协议的平台）

CodeBuddy（CN/Intl）和 Trae Intl 走 OpenAI 兼容协议，可以不装插件，直接在 CPA `config.yaml` 配置：

```yaml
openai-compatibility:
  - name: "workbuddy"
    base-url: "https://copilot.tencent.com/v2"
    api-key-entries:
      - api-key: "<你的 CodeBuddy/WorkBuddy access_token>"
    models:
      - name: "hy4-preview"
      - name: "glm-5.2"
```

**代价**：失去 OAuth 自动登录、token 自动刷新、签到、content filter 规避、多账号 pool 等插件功能。适合"只用一个账号、手动管理 token"的场景。

**方案 2：只装你需要的插件**

4 个插件互相独立，不需要全装。每个插件内部支持区域/变体选择（配置或自动收养）：

| 你的需求 | 装哪些插件 |
|---|---|
| Trae Code CN | `trae`（login_variant: "cn"，默认） |
| Trae Work CN（薅羊毛） | `trae`（login_variant: "solo"，自动收养 trae-solo-cn 账号文件） |
| Trae Intl | `trae`（login_variant: "intl"，自动收养 trae-intl 账号文件） |
| CodeBuddy CN / WorkBuddy | `workbuddy`（login_platform: CLI 或 ide；v0.9.0 起合并） |
| CodeBuddy Intl | `workbuddy`（login_region: "intl"；自动收养 codebuddy-intl 账号文件） |
| QoderWork CN | `qoder`（login_region: "cn"，默认） |
| Qoder Intl | `qoder`（login_region: "intl"，自动收养 qoder-intl 账号文件） |
| ZCode（智谱 GLM 编码套餐） | `zcode`（login_provider: "zai" 或 "bigmodel"） |
| 全都要 | 全部 4 个 |

**方案 3：等 CPA 上游支持多 provider 插件**

如果 CPA 未来支持单插件多 provider（`auth.identifier` 返回数组），可以合并。目前上游无此计划。

## 安装

### 1. 下载 release

从 [Releases](../../releases) 下载对应平台的 zip：
- `cpa-multi-plugins-linux-amd64.zip` — Linux x86_64
- `cpa-multi-plugins-linux-arm64.zip` — Linux ARM64
- `cpa-multi-plugins-darwin-arm64.zip` — macOS Apple Silicon
- `cpa-multi-plugins-windows-amd64.zip` — Windows x86_64

### 2. 解压并放到 CPA plugins 目录

```bash
unzip cpa-multi-plugins-linux-amd64.zip -d /path/to/cpa/plugins/
```

### 3. 启用插件

CPA 的 `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    workbuddy: { enabled: true, login_platform: "CLI", login_region: "cn" }  # CLI/ide；region: cn|intl（v0.11.0 起三区合一）
    trae: { enabled: true, login_variant: "cn" }  # cn|solo|intl（v0.12.0 起三合一）
    qoder: { enabled: true, login_region: "cn" }  # cn|intl（v0.10.0 起二合一）
    zcode: { enabled: true, login_provider: "zai" }  # zai|bigmodel（v0.12.84 起随主分支提供）
```

### 4. 重启 CPA，登录账号

每个插件保持**单一 OAuth 入口**（v0.12.10 起）：OAuth 登录菜单中的 Trae / WorkBuddy / Qoder 条目按插件配置的 `login_variant`（Trae: cn|solo|intl）/ `login_region`（WorkBuddy、Qoder: cn|intl）发起登录。要切换登录指向哪个区域，在管理 UI 的插件配置里改这个下拉并保存即可，下一次点 OAuth 登录就走新区域——入口只有一个，指向由配置决定。

区域登录产生的凭证落盘到 auth-dir 并被对应插件自动收养；已有账号不受登录区域影响（登录变体不劫持现有账号的分发）。

## 常见问题（FAQ）

### trae OAuth 回调 URL 提交失败：`state is required`（issue #16）

在宿主管理界面或 TUI 的「回调 URL」粘贴框里提交 trae 登录链接会必然报这个错：该粘贴框指向宿主通用端点 `POST /v0/management/oauth-callback`，只解析 OAuth 标准的 `state` / `code` 查询参数；而 Trae 的真实授权重定向**从不携带这两个参数**（回传的是 `login_trace_id` 与 `authCode`/`authCodeInfo`，插件从不向 Trae 发送 state）。加之 Trae 授权页硬性要求回调只能是 `http://127.0.0.1:<端口>/authorize`——浏览器与 CPA 服务不在同一台机器时，重定向必然失败并留下一条宿主端点无法解析的链接。

**正确做法**：把浏览器地址栏的完整链接粘贴到**插件面板的粘贴框**，它走插件自有路由、按 Trae 真实参数形态解析：

- 面板粘贴框：`<CPA 地址>/v0/resource/plugins/trae/panel`
- 或直接 GET：`<CPA 地址>/v0/resource/plugins/trae/oauth_submit?cb_url=<URL 编码后的完整回调链接>`

前提：该链接来自 15 分钟内开始的登录、且期间 CPA 服务未重启；超时或重启后请回到面板重新点「登录」并用新链接。

### trae 配了宿主模型前缀（凭据 `prefix`）后 SOLO 通道全部 4001（issue #18）

CPA 允许给凭据配 `prefix`（`PATCH /v0/management/auth-files/fields`），用于让多家 provider 的同名底模在 `/v1/models` 里可区分。该前缀只应存在于宿主的记账键里，却会随客户端请求体原样进入插件：**v0.12.58 及之前**出站 `config_name` 取自 body 的 `model` 字段，带前缀的名字上游不认，SOLO 通道每条调用都在流内报 `biz_code=4001`（流式/非流式、全部账号一致，宿主侧看不到任何异常状态，排查成本极高）。

**v0.12.59 起**出站一律使用宿主解析后的模型 id（`ExecutorRequest.Model`，宿主分发所依据的路由键，不含前缀），body 名字仅在宿主未填该字段时回退——前缀场景自动恢复，也不会误伤 `deepseek-ai/deepseek-v4-pro` 这类自带 `/` 的合法 config 名。trae 自带 `-solo`/`-intl` 命名空间后缀，与前缀功能本就互补，**建议 trae 凭据保持 `prefix` 为空**；无法升级宿主插件时，清空前缀同样可解（`PATCH {"name":…,"prefix":""}` 热生效）。插件日志出现 `trae: outbound model uses the host-resolved …` 说明前缀仍在生效，仅作提示，不影响功能。

## 构建

```bash
# 编译所有插件（当前平台）
make all  # 或 ./scripts/build.sh

# 跨平台编译
./scripts/build.sh linux amd64
./scripts/build.sh darwin arm64
./scripts/build.sh windows amd64

# 单个插件
cd plugins/trae && CGO_ENABLED=1 go build -buildmode=c-shared -o trae.so .
```

**要求**：Go 1.23+（自动下载 1.26 toolchain）、CGO 启用、C 编译器（gcc/clang/mingw）。

## 借鉴来源（Protocol Sources）

本项目的协议层基于以下开源项目的代码事实实现。**每个插件都明确标注了协议来源文件路径**，便于溯源和后续协议变更时跟进。

### 主要协议来源

| 项目 | 语言 | 协议贡献 | 用在哪些插件 |
|---|---|---|---|
| **[Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api)** | Go | Trae SOLO CN 协议层（auth/upstream/pool/scheduler） | trae-cn, trae-solo-cn |
| **[Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin)** | Go | WorkBuddy + QoderWork 完整 CPA 插件（v0.8.5 / v0.2.6） | workbuddy, codebuddy-cn, codebuddy-intl, qoder-cn, qoder-intl |
| **[OmniRoute](https://github.com/diegosouzapw/OmniRoute)** | TypeScript | Trae Intl Web SOLO remote 协议（trae.ts）<br>CodeBuddy CN content filter 规避（codebuddy-cn.ts）<br>CodeBuddy CN/intl executor | trae-intl, workbuddy, codebuddy-cn, codebuddy-intl |
| **[9router](https://github.com/decolua/9router)** | JavaScript | Trae 三区域切换（regions: cn/sg/us）<br>Trae Intl chat_sessions/events SSE | trae-intl |
| **[cockpit-tools](https://github.com/jlcodes99/cockpit-tools)** | Rust | Trae v2 积分制 pack 优先级（apply_usage_response）<br>Trae 4 变体差异（TraePlatformKind）<br>CodeBuddy CN 签到状态机（workbuddy_auto_checkin.rs）<br>CodeBuddy CN 签到字段解析（codebuddy_cn_oauth.rs）<br>Trae 签到 API headers（x-app-type, Origin, Referer） | trae-cn, trae-solo-cn, workbuddy, codebuddy-cn |
| **[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)** | Go | WorkBuddy `/v3/config` 模型目录双路发现（企业端点排序权威 + v3 补能力/独有条目）<br>nonChatModel 非对话模型过滤 | workbuddy |
| **[corrinehu/dsh-workbuddy-connect](https://github.com/corrinehu/dsh-workbuddy-connect)** | TypeScript | `/v3/config` 按客户端身份切分目录实测确认（CLI 名册 vs App 名册，split is load-bearing） | workbuddy |
| **[maiphucgiang/codebuddy2api](https://github.com/maiphucgiang/codebuddy2api)** | Python | CodeBuddy intl 双产品身份（intl-cli/intl-work）目录共享推导 + CLI X-IDE-* 身份头 + auto↔default-model intl 别名 | workbuddy |
| **[ThinkofRain1213/deepseek-harness-codearts](https://github.com/ThinkofRain1213/deepseek-harness-codearts)** | TypeScript | `buddy.ts` isChatModel 过滤 + supportsImages 三态<br>image_url/data URI 图片链路实测 | workbuddy |
| **[Ttungx/trae-solo-local-api](https://github.com/Ttungx/trae-solo-local-api)** | TypeScript | Trae 上游无原生 thinking 参数 / agent 字段 4023 实测（Body 白名单依据）<br>image_url 多模态透传实测 | trae |
| **[TriDefender/zcode-api](https://github.com/TriDefender/zcode-api)** | TypeScript | ZCode 智谱 GLM 编码套餐反代——OAuth 中转登录 / 签名 V4 / 身份头（g6n/TV）/ 账务平面 / 模型目录 | zcode |
| **[zai-org/ZCode](https://github.com/zai-org/ZCode)** | TypeScript | ZCode 官方开源客户端（仅取账户级线路协议形状）：start-plan anthropic 翻译层 + 官方 system 块 + 业务错误码全表（M2）<br>off-peak 错峰票务五端点 wire 契约 + 排队/废票决策（M3） | zcode |
| **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** | Go | CPA 插件 SDK（examples/plugin/{executor,auth}/go/）<br>pluginapi / pluginabi 类型定义 | 全部 4 个插件 |

### 各插件的具体借鉴文件

#### `plugins/workbuddy` (fork from Sliverkiss/cpa-plugin/workbuddy v0.8.5)
- **协议层**：`Sliverkiss/cpa-plugin/workbuddy/` 全部 30+ Go 文件（MIT）
- **content filter 规避**：`OmniRoute/open-sse/executors/codebuddy-cn.ts` line 149-202（AGENT_PATTERN + 长度兜底 + reasoning_summary 镜像 + 大工具描述压缩）
- **签到状态机**：`cockpit-tools/src-tauri/src/modules/workbuddy_auto_checkin.rs` line 33-64, 406-754（WorkbuddyAutoCheckinConfig + 指数退避调度器）
- **签到字段解析**：`cockpit-tools/src-tauri/src/modules/codebuddy_cn_oauth.rs` line 1208-1258, 1285-1394, 1423-1587（CheckinStatusResponse 完整字段 + fallback 路径）
- **模型双路发现**：`linguo2625469/workbuddy2api-panel` nonChatModel + `/v3/config` 探测（v0.12.51 吸收：企业端点管排序、v3 补能力与独有条目、单路失败降级另一路）
- **isChatModel 过滤 + supportsImages**：`ThinkofRain1213/deepseek-harness-codearts` `buddy.ts`（nes-/completion-/codewise- 前缀、maxOutput<=256、text-to-image tag 不进可选列表；能力透出至模型元数据）
- **目录身份切分（v0.9.35）**：`corrinehu/dsh-workbuddy-connect` `/v3/config` CLI/App 双名册实测 + `maiphucgiang/codebuddy2api` intl-cli/intl-work 双产品身份与 CLI X-IDE-* 头 + `linguo2625469/workbuddy2api-panel` CLI UA 并集探测与 /v2 企业端点家族（0.9.35 吸收：双身份并集探测，IDE 名册字段权威）

#### `plugins/codebuddy-cn` 已并入 `plugins/workbuddy`（v0.9.0）
- 两者后端、额度池完全相同（copilot.tencent.com），仅登录 platform（CLI/ide）与 X-IDE-* 请求头不同
- 合并后通过 `login_platform` 配置选择新登录方式（CLI 默认 / ide）
- 旧 `codebuddy-cn-<uid>.json` 账号文件在插件启动时自动收养为 `workbuddy-<uid>.json`（loginPlatform=ide）
- 参考：`cockpit-tools/src-tauri/src/modules/codebuddy_cn_oauth.rs:8` 的 platform 参数

> ℹ️ v0.10.0–v0.12.0 起家族合并：codebuddy-intl 并入 workbuddy、qoder-cn/qoder-intl 并入 qoder、trae-cn/trae-solo-cn/trae-intl 并入 trae。以下为历史来源说明。

#### `plugins/codebuddy-intl` (adapted from workbuddy — merged into workbuddy)
- 全部同 workbuddy
- **Global host**：`www.codebuddy.ai`（vs CN 的 `www.codebuddy.cn` / `copilot.tencent.com`）
- 参考：`OmniRoute/open-sse/config/providers/registry/codebuddy-intl/`（如果存在）

#### `plugins/trae-cn` (based on traework2api + cockpit-tools)
- **协议层**：`Sliverkiss/traework2api/internal/{auth,upstream,pool,scheduler}/` 全部 Go 文件（MIT）
- **client_id**：`ono9krqynydwx5`（non-solo，对齐 cockpit-tools `trae_account_platform_storage.rs:185`）
- **function**：`inline_chat`（对齐 cockpit-tools `trae_account_platform_storage.rs`；⚠️ v0.12.79 起 deviate：llm_utils_chat 仅接受 `solo_work_lite`，cn/合并插件全部 variant 改发该值——issue #9）
- **签到 headers**：`cockpit-tools/src-tauri/src/modules/trae_account_token_injection.rs:2761,2859`（x-app-type: trae, Origin: https://www.trae.cn, Referer: https://www.trae.cn/）
- **v2 积分 pack 优先级**：`cockpit-tools/src-tauri/src/modules/trae_account_token_injection.rs:1807-1866`（apply_usage_response）
- **pack product_type 映射**：`cockpit-tools/src/types/trae.ts:174-189`（TRAE_PRODUCT_TYPE）

#### `plugins/trae-solo-cn` (based on traework2api)
- 全部同 trae-cn
- **client_id**：`en1oxy7wnw8j9n`（SOLO stable，对齐 traework2api + cockpit-tools）
- **function**：`solo_work_lite`（对齐 traework2api `internal/upstream/constants.go`）

#### `plugins/trae-intl` (based on OmniRoute + 9router)
- **协议层**：`OmniRoute/open-sse/executors/trae.ts`（482 行 TS → Go 翻译）
- **三区域配置**：`9router/open-sse/providers/registry/trae.js`（regions: {cn, sg, us}, defaultRegion: "cn"）
- **Web SOLO remote 协议**：`core-normal.trae.ai/api/remote/v1/chat_sessions` + `events` SSE
- **mode/strategy 解析**：`OmniRoute/open-sse/executors/trae.ts` resolveMode（"work"/"auto"/具体 model name）
- **plan_item 累积文本**：`OmniRoute/open-sse/executors/trae.ts` renderNewText（cumulative, longest-wins per plan_item.id）
- **OAuth**：`api.marscode.com/cloudide/api/v3/trae/` + `ExchangeToken`
- **v1 pay 接口**：`grow-normal.trae.ai/trae/api/v1/pay/ide_user_*`（CN 用 v2，Intl 用 v1）

#### `plugins/qoder-cn` (fork from Sliverkiss/cpa-plugin/qoderwork v0.2.6)
- **协议层**：`Sliverkiss/cpa-plugin/qoderwork/` 全部 28 Go 文件（MIT）
- **COSY 签名**：`qoderwork/sign.go`（220 行）+ `encoding.go`（53 行）
- **签到**：`qoderwork/checkin.go`（`openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/{status,claim}`）
- **PAT 导入**：`qoderwork/oauth.go`（`openapi.qoder.com.cn/api/v1/jobToken/exchange`）

#### `plugins/qoder-intl` (adapted from qoderwork)
- 全部同 qoder-cn
- **host**：`openapi.qoder.sh` / `api3.qoder.sh`（vs CN 的 `openapi.qoder.com.cn` / `gateway.qoder.com.cn`）
- **client_id**：`e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb`（vs CN 的 `1c5e33e1-...`）
- **redirect_uri**：`qoder://aicoding.aicoding-agent/login-success`（vs CN 的 `qoder-work-cn://`）
- **无签到**（Intl 平台无签到机制）

#### `plugins/zcode`（clean-room from TriDefender/zcode-api + zai-org/ZCode）
- **行为基线（闭源仿冒）**：`TriDefender/zcode-api/src/{auth/oauth.ts, proxy/identity.ts, proxy/client-signing.ts, proxy/upstream.ts, server/routes-quota.ts}`（OAuth 中转登录 / g6n+TV 身份头 / 签名 V4 全套 / 账务平面）
- **KeyResolver 凭证链（M1.1）**：`TriDefender/zcode-api/src/auth/resolver.ts` + 官方开源 `apps/zcode-cli/.../coding-plan-api-key.ts`（两实现逐行同构 = 账户级 API）：`z/login → getCustomerInfo → api_keys → copy` 终态 `{apiKeyId}.{apiKeySecret}`
- **start-plan anthropic 翻译层（M2）**：官方开源 `translator/openai-to-anthropic.ts` + `translator/sse-translator.ts` + `proxy/system-prompt.ts` + `zcode_system.json`（3 官方块逐字）+ `proxy/body-transformer.ts`（system 前置/context_prefix/metadata/cache_control 规范化）+ `failure-provider-business-codes.ts`（业务码全表）
- **off-peak 错峰票务（M3）**：官方开源 `packages/services/src/session/offPeakServerClient.ts`（五端点 wire 契约）+ `offPeakRuntimeModel.ts`（双凭证头）+ `offPeakTaskService.ts`（状态机/续跑）+ `off-peak-types.ts` + `offpeak-retry.ts`（排队/废票决策）
- ⚠ 开源采用策略：官方开源版为减配形态（无签名 V4/验证码求解/claim 链，ultra 网关替代通道）——仅抄账户级线路协议，不抄行为；详见 docs/PROTOCOL.md zcode 节

### 协议事实文档

完整的协议事实清单见 [docs/PROTOCOL.md](docs/PROTOCOL.md)（含 API endpoint、headers、body 格式、字段解析、状态机）。

## License

MIT — 详见 [LICENSE](LICENSE)

## 致谢

本项目站在以下项目的肩膀上，按贡献度排序：

- **Sliverkiss** — traework2api + cpa-plugin（WorkBuddy + QoderWork）作者，提供了 Trae SOLO CN 协议层 + CodeBuddy/Qoder 完整 CPA 插件基础
- **diegosouzapw** — OmniRoute 作者，提供了 Trae Intl Web SOLO remote 协议 + CodeBuddy CN content filter 规避
- **decolua** — 9router 作者，提供了 Trae 三区域配置 + 多平台反代参考
- **jlcodes99** — cockpit-tools 作者，提供了 Trae v2 积分制 pack 优先级 + 16 平台账号管理协议事实
- **router-for-me** — CLIProxyAPI 作者，提供了 CPA 插件 SDK + C ABI 接口规范
- **lovingfish** — workbuddy-cliproxy 作者，提供了 workbuddy 单文件 clean-room 重写参考
- **linguo2625469** — workbuddy2api-panel 作者，提供了 WorkBuddy `/v3/config` 模型目录双路发现 + nonChatModel 过滤参考（v0.12.51 吸收）
- **ThinkofRain1213** — deepseek-harness-codearts 作者，提供了 isChatModel 过滤 + supportsImages 三态参考（v0.12.51 吸收）
- **Ttungx** — trae-solo-local-api 作者，提供了 Trae Body 白名单与多模态透传实测依据（v0.12.37 依据）

## 协议变更跟踪

Trae / CodeBuddy / Qoder 平台会不定期更新协议。本项目通过以下方式跟踪：

1. **协议层独立**：所有协议常量集中在 `upstream/constants.go`，变更时只改一处
2. **pack 优先级可配置**：`SelectActivePack` 支持新增 product_type
3. **content filter 正则可扩展**：`agentPattern` 在 `payload.go` 顶部，新身份行直接加
4. **参考项目监控**：定期 sync 上游参考项目（见「借鉴来源」与 docs/PROTOCOL.md「参考来源」）的最新 commit

如发现协议变更，请提 [Issue](../../issues) 报告。

## Status

✅ **4/4 plugins fully functional** — v0.12.85 released
- `workbuddy` / `trae` / `qoder`：家族合并后的主线插件（原 8 个单平台插件已按家族并入）
- `zcode`：智谱 GLM 编码套餐，v0.12.84 起并入主分支（M1–M3 完整）
- All plugins compile to .so/.dll/.dylib on 5 platforms (linux amd64/arm64, darwin amd64/arm64, windows amd64)
- GitHub Actions release workflow: multi-platform build + auto release on tag push

## Telegram 频道

插件更新、协议变更与 release 通知会在 Telegram 频道同步发布，欢迎加入：

**邀请链接**：[https://t.me/+hI7SCfhLF-YwMDUx](https://t.me/+hI7SCfhLF-YwMDUx)

> 私有频道邀请链接（`+` 后缀），点击直达，无需在 TG 内搜索频道名。
