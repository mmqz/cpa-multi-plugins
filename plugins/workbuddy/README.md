# WorkBuddy Plugin for CLIProxyAPI

A [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) plugin that
provides **Tencent CodeBuddy** (`copilot.tencent.com` CN and `workbuddy.ai`
Global) as a native OAuth provider: dynamic model discovery, streaming executor,
credit-aware scheduling, daily check-in automation, and a built-in management
dashboard.

[中文文档 → README_CN.md](README_CN.md)

## Features

- **OAuth login** — multi-account `workbuddy-<uid>.json` auth files via the
  host's auth store. CN and Global realms share one plugin, one config block.
- **Dynamic models** — live model list from the upstream models API with a
  5-minute cache and a static fallback. Host-side `oauth-model-alias` /
  `oauth-excluded-models` config applies unchanged.
- **Executor** — OpenAI-compatible chat completions, both streaming (real SSE
  via `host.stream.emit`) and non-streaming (SSE folded into a single
  completion). `tool_choice` normalization, Claude Code template sanitization,
  and per-realm system-message injection are built in.
- **Credit lifecycle** — CN accounts auto-`disabled` when credits run out and
  re-enabled when a check-in restores them. Global accounts are deleted on
  exhaustion (one-shot trial quota). Hard credit errors from the executor
  trigger an immediate reconcile.
- **Daily check-in** — CN accounts are checked in at 09:00 and 21:00 local
  time (configurable). Manual "check in all" from the panel. Per-account
  mutex prevents duplicate claims from racing browser tabs.
- **Trial claim** — Global accounts can claim the one-time 250-credit expert
  trial pack from the panel.
- **Dashboard** — embedded panel at `/v0/resource/plugins/workbuddy/panel`
  with credits progress bars, plan badges, exhausted/disabled flags, region
  filter, and credential import.
- **Scheduler** (optional) — `scheduler_mode: credits` makes the plugin pick
  the panel-selected account; `off` (default) defers to CPA's built-in
  scheduler entirely.
- **Usage forwarding** — implements `UsagePlugin`; every request's usage
  record is forwarded to a configurable CPAMP endpoint. No record is sent
  unless a URL+key are configured.

## Quickstart

### 1. Install the plugin

Drop the compiled `workbuddy.so` into CPA's plugin directory:

```bash
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

For multi-arch deployments use the platform subdirectory convention:

```
plugins/
  linux/amd64/workbuddy.so
  linux/arm64/workbuddy.so
  darwin/arm64/workbuddy.so
```

### 2. Enable in `config.yaml`

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. Sign in

Open the WorkBuddy panel from CPA's sidebar (or hit
`/v0/resource/plugins/workbuddy/panel` directly) and click **登录** to start
the OAuth flow. Repeat for each account you want to add — the plugin writes
one `workbuddy-<uid>.json` per account to the auth store.

### 4. Use it

Call the OpenAI-compatible endpoint with any alias that maps to a workbuddy
model:

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "point/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

## Configuration

All fields are optional and live under `plugins.configs.workbuddy`.

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # Daily check-in automation for CN accounts (default true).
      # Runs at 09:00 and 21:00 local time.
      checkin_auto: true

      # Credit lifecycle: disable CN on exhaust, delete Global on exhaust,
      # re-enable CN after check-in restores credits (default true).
      lifecycle_auto: true

      # Scheduler behavior (default "off"):
      #   off     → defer to CPA's built-in scheduler entirely
      #   credits → plugin picks the panel-selected account (with fallback
      #             when that account is exhausted / disabled)
      scheduler_mode: "off"

      # CPAMP usage forwarding. Both must be set for any record to be sent.
      # Falls back to USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY env vars or docker secret files when unset here.
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # Plugin-layer management auth. When set, all mutating endpoints under
      # /v0/management/plugins/workbuddy/* require this Bearer token.
      # When empty (default) the host's management middleware is the only
      # guard. Also readable from WB_MANAGEMENT_KEY env var.
      management_key: ""

      # Opt-in async-stream head gate, in seconds (default 0 = off, which keeps
      # today's behavior byte-for-byte). When > 0 the executor waits up to this
      # long for the first upstream event before opening the host stream, so a
      # pre-answer failure (upstream >=400, or an error frame that arrives before
      # the model starts answering) returns as a normal failed request WITH an
      # HTTP status instead of a lossy in-band text error the host reads as an
      # "empty success". It never waits longer than this and never fails a stream
      # that is merely slow to open (a silent window releases normally).
      stream_head_timeout: 0
```

Model aliases and exclusions are handled natively by CPA's
`oauth-model-alias` and `oauth-excluded-models` config — no plugin-side
duplication needed.

## Lifecycle

| State | CN account | Global account |
|---|---|---|
| Credits > 0 | active | active |
| Credits = 0 | `disabled: true` (auth file kept) | auth file **deleted** |
| Check-in restores credits | re-enabled | n/a (already deleted) |
| Trial available | n/a | claimable once per account |
| Unknown credits | untouched (never mis-kill) | untouched |

Hard credit errors from the executor (status 402, "insufficient credits",
"积分不足", etc.) trigger an immediate reconcile of the failing account.
Request-level chat failures are exempted: input-oversize rejections (code
11115, bare 413, extended too-long wording) and channel risk-control (11128)
translate into actionable copy ("请求级问题，与账号无关") and never touch the
account lifecycle. Oversized agent histories are also scrubbed before the
upstream sees them (developer → system role normalization, orphan tool-result
pairing cleanup) — see CHANGELOG 0.9.15.

## FAQ

### Why did my Intl (codebuddy.ai) account only show tier aliases (fast-model, auto-chat, …) instead of real model names?

Because the gateway serves a **different catalog per client identity**, and
plugin versions before 0.9.35 only ever presented the IDE identity. The IDE
roster is the product-tier aliases (`default-model` / `fast-model` /
`balanced-model` / `primary-model` / `deep-model` / `auto-chat` /
`enhance-1.0`) plus genuine ids such as `o4-mini`. The CLI identity gets a
different, larger roster — measured on workbuddy.ai (2026-09-22): ~22 chat
models including the deepseek-v4.1 series, gpt-6-astra, kimi-k2.8-preview
and the glm/gemini families — that the IDE identity never receives. Since
0.9.35 the plugin probes both identities and advertises the union: the tier
aliases (still routable, they are the gateway's own product tiers) now sit
alongside the real model families. Enterprise discovery also tries the
`/v2` path family first on global/intl (the `/console` path is legacy).

Still true: upstream does not publish which concrete model backs each tier
alias. Chat through an alias once and the plugin learns it from the
response's `model` echo — the display name gains `·实测 <real-id>` and the
plugin panel (`/v0/resource/plugins/workbuddy/panel`) shows the learned
map (in-memory; re-learned from live traffic after a restart). Need a
specific list? Pin `models_intl` (comma-separated ids; routability of a
pinned id is decided upstream). Empty list instead of a short one? Check
the panel's realm diagnostics first (`source` / `last_error`) — discovery
only ever advertises what upstream currently serves.

## Development

Requires Go 1.26+ (matches CPA).

```bash
# Build the plugin
go build -buildmode=c-shared -o workbuddy.so .

# Run tests
go test -race ./...

# Lint
gofmt -l .
go vet ./...
```

The plugin uses CPA's host HTTP bridge (`host.http.do` / `do_stream`) for
all upstream calls so request-log captures outbound traffic and host
transport policy applies. A fallback direct HTTP client is used only when
the bridge is unavailable (unit tests, hosts older than v7.2.x).

See [docs/development.md](docs/development.md) for the full workflow and
[docs/architecture.md](docs/architecture.md) for the module map.

## License

MIT — see [LICENSE](LICENSE).
