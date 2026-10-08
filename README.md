# cpa-multi-plugins

Provider plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Each plugin merges the regional variants of one provider family behind a single identifier and compiles to a C-shared library (`.so` / `.dll` / `.dylib`) for linux amd64/arm64, darwin amd64/arm64 and windows amd64.

| Plugin | Version |
| --- | --- |
| workbuddy | 0.9.56 |
| trae | 0.12.73 |
| qoder | 0.8.59 |
| zcode | 0.2.0 |
| mimo | 0.2.17 |

Tagged commits are built by CI; per-platform artifacts are attached to each [release](../../releases). Release notices are also posted to the [Telegram channel](https://t.me/+hI7SCfhLF-YwMDUx).

## Protocol Sources

The code in this repository is adapted and organized from the upstream reference repositories listed below. The protocol layer of every plugin is implemented from the code facts of these open-source projects, and each plugin marks its protocol source file paths for traceability and for following future protocol changes.

### Primary protocol sources

| Project | Language | Protocol contribution | Used by plugins |
|---|---|---|---|
| **[Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api)** | Go | Trae SOLO CN protocol layer (auth/upstream/pool/scheduler) | trae |
| **[Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin)** | Go | WorkBuddy + QoderWork full CPA plugins (v0.8.5 / v0.2.6) | workbuddy, qoder |
| **[OmniRoute](https://github.com/diegosouzapw/OmniRoute)** | TypeScript | Trae Intl Web SOLO remote protocol (trae.ts)<br>CodeBuddy CN content filter bypass (codebuddy-cn.ts)<br>CodeBuddy CN/Intl executor | trae, workbuddy |
| **[9router](https://github.com/decolua/9router)** | JavaScript | Trae three-region switching (regions: cn/sg/us)<br>Trae Intl chat_sessions/events SSE | trae |
| **[cockpit-tools](https://github.com/jlcodes99/cockpit-tools)** | Rust | Trae v2 credits pack priority (apply_usage_response)<br>Trae 4-variant differences (TraePlatformKind)<br>CodeBuddy CN check-in state machine (workbuddy_auto_checkin.rs)<br>CodeBuddy CN check-in field parsing (codebuddy_cn_oauth.rs)<br>Trae check-in API headers (x-app-type, Origin, Referer) | trae, workbuddy |
| **[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)** | Go | WorkBuddy `/v3/config` dual-path model catalog discovery (enterprise endpoint owns ordering + v3 supplements capabilities/exclusive entries)<br>nonChatModel non-chat model filtering | workbuddy |
| **[corrinehu/dsh-workbuddy-connect](https://github.com/corrinehu/dsh-workbuddy-connect)** | TypeScript | `/v3/config` per-client-identity catalog split, field-verified (CLI roster vs App roster, split is load-bearing) | workbuddy |
| **[maiphucgiang/codebuddy2api](https://github.com/maiphucgiang/codebuddy2api)** | Python | CodeBuddy intl dual-product identity (intl-cli/intl-work) catalog sharing derivation + CLI X-IDE-* identity headers + auto↔default-model intl alias | workbuddy |
| **[ThinkofRain1213/deepseek-harness-codearts](https://github.com/ThinkofRain1213/deepseek-harness-codearts)** | TypeScript | `buddy.ts` isChatModel filtering + supportsImages three-state<br>image_url/data URI image path, field-verified | workbuddy |
| **[Ttungx/trae-solo-local-api](https://github.com/Ttungx/trae-solo-local-api)** | TypeScript | Trae upstream has no native thinking param / agent-field 4023, field-tested (body whitelist basis)<br>image_url multimodal passthrough, field-verified | trae |
| **[TriDefender/zcode-api](https://github.com/TriDefender/zcode-api)** | TypeScript | ZCode GLM coding-plan proxy — OAuth relay login / V4 signing / identity headers (g6n/TV) / billing plane / model catalog | zcode |
| **[zai-org/ZCode](https://github.com/zai-org/ZCode)** | TypeScript | ZCode official open-source client (account-level wire protocol shapes only): start-plan anthropic translation layer + official system blocks + full business error code table (M2)<br>off-peak ticketing five-endpoint wire contract + queue/void decisions (M3) | zcode |
| **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** | Go | CPA plugin SDK (examples/plugin/{executor,auth}/go/)<br>pluginapi / pluginabi type definitions | all plugins |

### Per-plugin source files

#### `plugins/workbuddy` (forked from Sliverkiss/cpa-plugin/workbuddy v0.8.5)
- **Protocol layer**: all 30+ Go files of `Sliverkiss/cpa-plugin/workbuddy/` (MIT)
- **Content filter bypass**: `OmniRoute/open-sse/executors/codebuddy-cn.ts` lines 149-202 (AGENT_PATTERN + length fallback + reasoning_summary mirroring + large tool-description compression)
- **Check-in state machine**: `cockpit-tools/src-tauri/src/modules/workbuddy_auto_checkin.rs` lines 33-64, 406-754 (WorkbuddyAutoCheckinConfig + exponential backoff scheduler)
- **Check-in field parsing**: `cockpit-tools/src-tauri/src/modules/codebuddy_cn_oauth.rs` lines 1208-1258, 1285-1394, 1423-1587 (full CheckinStatusResponse fields + fallback paths)
- **Dual-path model discovery**: `linguo2625469/workbuddy2api-panel` nonChatModel + `/v3/config` probing (absorbed in v0.12.51: enterprise endpoint owns ordering, v3 supplements capabilities and exclusive entries, single-path failure degrades to the other path)
- **isChatModel filtering + supportsImages**: `ThinkofRain1213/deepseek-harness-codearts` `buddy.ts` (nes-/completion-/codewise- prefixes, maxOutput<=256, text-to-image tags excluded; capabilities surfaced to model metadata)
- **Catalog identity split (v0.9.35)**: `corrinehu/dsh-workbuddy-connect` `/v3/config` CLI/App dual-roster field tests + `maiphucgiang/codebuddy2api` intl-cli/intl-work dual-product identity and CLI X-IDE-* headers + `linguo2625469/workbuddy2api-panel` CLI UA union probing and /v2 enterprise endpoint family (absorbed in 0.9.35: union probing of both identities, IDE roster fields authoritative)

#### `plugins/codebuddy-cn` merged into `plugins/workbuddy` (v0.9.0)
- Same backend and credit pool (copilot.tencent.com); only login platform (CLI/ide) and X-IDE-* request headers differ
- After the merge, the `login_platform` config selects the new login flavor (CLI default / ide)
- Legacy `codebuddy-cn-<uid>.json` auth files are adopted as `workbuddy-<uid>.json` at startup (loginPlatform=ide)
- Reference: `cockpit-tools/src-tauri/src/modules/codebuddy_cn_oauth.rs:8` platform parameter

> ℹ️ Family merges since v0.10.0–v0.12.0: codebuddy-intl into workbuddy, qoder-cn/qoder-intl into qoder, trae-cn/trae-solo-cn/trae-intl into trae. The notes below are historical source attributions.

#### `plugins/codebuddy-intl` (adapted from workbuddy — merged into workbuddy)
- Same as workbuddy
- **Global host**: `www.codebuddy.ai` (vs CN `www.codebuddy.cn` / `copilot.tencent.com`)
- Reference: `OmniRoute/open-sse/config/providers/registry/codebuddy-intl/` (if present)

#### `plugins/trae-cn` (based on traework2api + cockpit-tools)
- **Protocol layer**: all Go files of `Sliverkiss/traework2api/internal/{auth,upstream,pool,scheduler}/` (MIT)
- **client_id**: `ono9krqynydwx5` (non-solo, aligned with cockpit-tools `trae_account_platform_storage.rs:185`)
- **function**: `inline_chat` (aligned with cockpit-tools `trae_account_platform_storage.rs`; ⚠️ deviating since v0.12.79: llm_utils_chat only accepts `solo_work_lite`, all cn/merged variants now send that value — issue #9)
- **Check-in headers**: `cockpit-tools/src-tauri/src/modules/trae_account_token_injection.rs:2761,2859` (x-app-type: trae, Origin: https://www.trae.cn, Referer: https://www.trae.cn/)
- **v2 credits pack priority**: `cockpit-tools/src-tauri/src/modules/trae_account_token_injection.rs:1807-1866` (apply_usage_response)
- **pack product_type mapping**: `cockpit-tools/src/types/trae.ts:174-189` (TRAE_PRODUCT_TYPE)

#### `plugins/trae-solo-cn` (based on traework2api)
- Same as trae-cn
- **client_id**: `en1oxy7wnw8j9n` (SOLO stable, aligned with traework2api + cockpit-tools)
- **function**: `solo_work_lite` (aligned with traework2api `internal/upstream/constants.go`)

#### `plugins/trae-intl` (based on OmniRoute + 9router)
- **Protocol layer**: `OmniRoute/open-sse/executors/trae.ts` (482-line TS → Go translation)
- **Three-region config**: `9router/open-sse/providers/registry/trae.js` (regions: {cn, sg, us}, defaultRegion: "cn")
- **Web SOLO remote protocol**: `core-normal.trae.ai/api/remote/v1/chat_sessions` + `events` SSE
- **mode/strategy parsing**: `OmniRoute/open-sse/executors/trae.ts` resolveMode ("work"/"auto"/concrete model name)
- **plan_item text accumulation**: `OmniRoute/open-sse/executors/trae.ts` renderNewText (cumulative, longest-wins per plan_item.id)
- **OAuth**: `api.marscode.com/cloudide/api/v3/trae/` + `ExchangeToken`
- **v1 pay endpoint**: `grow-normal.trae.ai/trae/api/v1/pay/ide_user_*` (CN uses v2, Intl uses v1)

#### `plugins/qoder-cn` (forked from Sliverkiss/cpa-plugin/qoderwork v0.2.6)
- **Protocol layer**: all 28 Go files of `Sliverkiss/cpa-plugin/qoderwork/` (MIT)
- **COSY signing**: `qoderwork/sign.go` (220 lines) + `encoding.go` (53 lines)
- **Check-in**: `qoderwork/checkin.go` (`openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/{status,claim}`)
- **PAT import**: `qoderwork/oauth.go` (`openapi.qoder.com.cn/api/v1/jobToken/exchange`)
- **v0.8.45 (official desktop client v0.4.3 asar forensics)**: the legacy qoderwork IDE-plugin constants (client_id `1c5e33e1-...` for CN, `e883ade2-...` for Intl, `qoder-work-cn://` / `qoder://aicoding...` redirects, `qoder.com.cn` auth host) were reverse-engineered from the OLD qoderwork IDE plugin, NOT the official desktop client. Both the CN RPM (`Qoder-CN-linux-x86_64.rpm`, asar `out/main/index.js` Vpe config block) and the Intl RPM (`Qoder-linux-x86_64.rpm`, same Vpe shape) reveal the official v0.4.3 desktop client uses ONE unified protocol across both regions:
  - **client_id** = `732aef47-9cf2-46a2-95fe-4cebb5d0d1fa` (shared CN+Intl)
  - **biz_variant** = `qoder` (wraps the `selectAccounts` URL in `/users/sign-in?biz_variant=qoder&oauth_callback=...`)
  - **authBaseUrl (CN)** = `https://qoder.cn` (NOT `qoder.com.cn` — that's the legacy qoderwork IDE domain)
  - **authBaseUrl (Intl)** = `https://qoder.com`
  - **redirect_uri (CN)** = null → OMITTED from the URL (the Sft builder's `...t.redirectUri?{redirect_uri:t.redirectUri}:{}` drops the param)
  - **redirect_uri (Intl)** = `qoder-app://`
  - The official newbie grant (14-day Pro trial + 300 credits) fires SERVER-SIDE on the desktop-client login event; the legacy qoderwork IDE login never triggers it — root cause of "qoder cn/init 无法签到也无法领取首登录奖励"
  - Both CN and Intl now build the desktop URL by default; the v0.8.40 Intl-only `login_dialect=desktop` opt-in is a deprecated no-op

#### `plugins/qoder-intl` (adapted from qoderwork)
- Same as qoder-cn
- **host**: `openapi.qoder.sh` / `api3.qoder.sh` (vs CN `openapi.qoder.com.cn` / `gateway.qoder.com.cn`); auth base `qoder.com` (vs CN `qoder.cn`)
- **client_id**: `732aef47-9cf2-46a2-95fe-4cebb5d0d1fa` (shared with CN since v0.8.45 — both regions present as the official desktop client v0.4.3)
- **redirect_uri**: `qoder-app://` (vs CN `""` — omitted from URL)
- **biz_variant wrapper**: `/users/sign-in?biz_variant=qoder&oauth_callback=<selectAccounts URL>` (same as CN since v0.8.45)
- **check-in**: ✅ via campaigns system (the v0.8.18 "Intl has no check-in surface" note was retired in v0.12.80 when CN joined Intl on the campaigns dialect)

#### `plugins/zcode` (clean-room from TriDefender/zcode-api + zai-org/ZCode)
- **Behavior baseline (closed-source imitation)**: `TriDefender/zcode-api/src/{auth/oauth.ts, proxy/identity.ts, proxy/client-signing.ts, proxy/upstream.ts, server/routes-quota.ts}` (OAuth relay login / g6n+TV identity headers / full V4 signing / billing plane)
- **KeyResolver credential chain (M1.1)**: `TriDefender/zcode-api/src/auth/resolver.ts` + the official open-source `apps/zcode-cli/.../coding-plan-api-key.ts` (the two implementations are line-by-line isomorphic = account-level API): `z/login → getCustomerInfo → api_keys → copy` terminal state `{apiKeyId}.{apiKeySecret}`
- **start-plan anthropic translation layer (M2)**: official open-source `translator/openai-to-anthropic.ts` + `translator/sse-translator.ts` + `proxy/system-prompt.ts` + `zcode_system.json` (3 official blocks verbatim) + `proxy/body-transformer.ts` (system hoisting / context_prefix / metadata / cache_control normalization) + `failure-provider-business-codes.ts` (full business code table)
- **off-peak ticketing (M3)**: official open-source `packages/services/src/session/offPeakServerClient.ts` (five-endpoint wire contract) + `offPeakRuntimeModel.ts` (dual credential headers) + `offPeakTaskService.ts` (state machine / resume) + `off-peak-types.ts` + `offpeak-retry.ts` (queue / void decisions)
- ⚠ Open-source adoption policy: the official open-source build is a reduced form (no V4 signing / captcha solving / claim chain; ultra gateway alternate channel) — only the account-level wire protocol shapes are taken, not behavior; see the zcode section in docs/PROTOCOL.md

### Protocol fact sheet

The complete protocol fact list lives in [docs/PROTOCOL.md](docs/PROTOCOL.md) (endpoints, headers, body formats, field parsing, state machines).

## License

MIT — see [LICENSE](LICENSE).

## Disclaimer

The code in this repository is adapted from the upstream reference repositories listed above and is provided for mirroring purposes only.
