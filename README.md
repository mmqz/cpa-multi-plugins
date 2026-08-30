# cpa-multi-plugins

> CPA (CLIProxyAPI) 动态库插件集合：CodeBuddy / WorkBuddy / Trae / Qoder 的 CN + Intl 版本
>
> 8 个插件覆盖 4 个平台 × 2 个版本，让 CPA 一个 `/v1/chat/completions` 接口调用所有模型。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows-lightgrey)]()
[![Release](https://img.shields.io/badge/release-v0.2.0-blue)](../../releases)
[![Build](https://img.shields.io/badge/build-passing-brightgreen)](../../actions)

## 项目目标

为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 提供完整的国内 AI IDE 平台 provider 插件，让 CPA 一个 `/v1/chat/completions` 接口就能调用所有模型。

## 插件清单

| 插件 | 平台 | 协议 | 签到 | 配额 | 状态 |
|---|---|---|---|---|---|
| `workbuddy` | CodeBuddy CN/WorkBuddy | OpenAI 兼容 | ✅ 每日 | ✅ credits | ✅ functional |
| `codebuddy-cn` | CodeBuddy CN | OpenAI 兼容 | ✅ 每日 | ✅ credits | ✅ functional |
| `codebuddy-intl` | CodeBuddy Intl | OpenAI 兼容 | ❌ | ✅ credits | ✅ functional |
| `trae-cn` | Trae Code CN | llm_utils_chat + inline_chat | ✅ 每日 | ✅ v2 pack 优先级 | ✅ functional |
| `trae-solo-cn` | Trae Work CN/SOLO CN | llm_utils_chat + solo_work_lite | ✅ 每日 | ✅ v2 pack 优先级 | ✅ functional |
| `trae-intl` | Trae Intl | Web SOLO remote | ❌ | ✅ v1 | ✅ functional |
| `qoder-cn` | QoderWork CN | COSY 签名 | ✅ 每日 | ✅ quota | ✅ functional |
| `qoder-intl` | Qoder Intl | COSY 签名 | ❌ | ✅ quota | ✅ functional |

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
- QoderWork CN: `openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/{status,claim}`
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
- `forceMaxThinking` for hy3-family models

### ✅ Executor（execute + execute_stream）
- 非流式：上游 SSE 聚合 → 单个 `chat.completion` 对象
- 流式：实时转发 OpenAI SSE chunks（`plan_item` → `delta.content`，`token_usage` → `usage`）

## 协议复用

基于代码事实，3 套核心实现覆盖 8 个 provider：

```
codebuddy-core  →  workbuddy / codebuddy-cn / codebuddy-intl
                   （仅 platform + host + has_checkin 不同）
trae-cn-core    →  trae-cn / trae-solo-cn
                   （仅 client_id + function 不同）
trae-intl-core  →  trae-intl
                   （独立 Web SOLO remote 协议）
qoder-core     →  qoder-cn / qoder-intl
                   （仅 host + client_id 不同）
```

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
    workbuddy: { enabled: true }
    codebuddy-cn: { enabled: true }
    codebuddy-intl: { enabled: true }
    trae-cn: { enabled: true }
    trae-solo-cn: { enabled: true }
    trae-intl: { enabled: true }
    qoder-cn: { enabled: true }
    qoder-intl: { enabled: true }
```

### 4. 重启 CPA，登录账号

通过 CPAMP 管理面板或 Management API 触发 OAuth 登录流程。

## 构建

```bash
# 编译所有插件（当前平台）
make all  # 或 ./scripts/build.sh

# 跨平台编译
./scripts/build.sh linux amd64
./scripts/build.sh darwin arm64
./scripts/build.sh windows amd64

# 单个插件
cd plugins/trae-cn && CGO_ENABLED=1 go build -buildmode=c-shared -o trae-cn.so .
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
| **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** | Go | CPA 插件 SDK（examples/plugin/{executor,auth}/go/）<br>pluginapi / pluginabi 类型定义 | 全部 8 个插件 |

### 各插件的具体借鉴文件

#### `plugins/workbuddy` (fork from Sliverkiss/cpa-plugin/workbuddy v0.8.5)
- **协议层**：`Sliverkiss/cpa-plugin/workbuddy/` 全部 30+ Go 文件（MIT）
- **content filter 规避**：`OmniRoute/open-sse/executors/codebuddy-cn.ts` line 149-202（AGENT_PATTERN + 长度兜底 + reasoning_summary 镜像 + 大工具描述压缩）
- **签到状态机**：`cockpit-tools/src-tauri/src/modules/workbuddy_auto_checkin.rs` line 33-64, 406-754（WorkbuddyAutoCheckinConfig + 指数退避调度器）
- **签到字段解析**：`cockpit-tools/src-tauri/src/modules/codebuddy_cn_oauth.rs` line 1208-1258, 1285-1394, 1423-1587（CheckinStatusResponse 完整字段 + fallback 路径）

#### `plugins/codebuddy-cn` (adapted from workbuddy)
- 全部同 workbuddy
- **platform=ide**（vs workbuddy 的 platform=CLI）
- 参考：`cockpit-tools/src-tauri/src/modules/codebuddy_cn_oauth.rs:8` 的 platform 参数

#### `plugins/codebuddy-intl` (adapted from workbuddy)
- 全部同 workbuddy
- **Global host**：`www.codebuddy.ai`（vs CN 的 `www.codebuddy.cn` / `copilot.tencent.com`）
- 参考：`OmniRoute/open-sse/config/providers/registry/codebuddy-intl/`（如果存在）

#### `plugins/trae-cn` (based on traework2api + cockpit-tools)
- **协议层**：`Sliverkiss/traework2api/internal/{auth,upstream,pool,scheduler}/` 全部 Go 文件（MIT）
- **client_id**：`ono9krqynydwx5`（non-solo，对齐 cockpit-tools `trae_account_platform_storage.rs:185`）
- **function**：`inline_chat`（对齐 cockpit-tools `trae_account_platform_storage.rs`）
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

## 协议变更跟踪

Trae / CodeBuddy / Qoder 平台会不定期更新协议。本项目通过以下方式跟踪：

1. **协议层独立**：所有协议常量集中在 `upstream/constants.go`，变更时只改一处
2. **pack 优先级可配置**：`SelectActivePack` 支持新增 product_type
3. **content filter 正则可扩展**：`agentPattern` 在 `payload.go` 顶部，新身份行直接加
4. **参考项目监控**：定期 sync 上游 5 个参考项目的最新 commit

如发现协议变更，请提 [Issue](../../issues) 报告。

## Status

✅ **8/8 plugins fully functional** — v0.2.0 released
- 5 functional plugins forked from Sliverkiss/cpa-plugin (workbuddy, codebuddy-cn, codebuddy-intl, qoder-cn, qoder-intl)
- 3 Trae plugins fully implemented (trae-cn, trae-solo-cn, trae-intl)
- All plugins compile to .so/.dll/.dylib on 5 platforms (linux amd64/arm64, darwin amd64/arm64, windows amd64)
- GitHub Actions release workflow: multi-platform build + auto release on tag push
