# Changelog

## 0.2.8

### The auth.parse response carries the operator fields (issue #68)

Same fix as workbuddy 0.9.82: the host rebuilds its in-memory Auth record
from the auth.parse response and does not re-read proxy_url / prefix /
priority from the file, so the panel proxy reset to "none" after restarts
and after plugin-side rewrites. `applyHostOwnedAuthFields` (new,
`host_owned_fields.go`) re-injects the #25 whitelist into the AuthData —
proxy_url/prefix ride the dedicated fields, all keys ride Metadata so the
host's persist path writes them back. Unit + end-to-end handleParseAuth
tests pin the behaviour.


## 0.2.7

### Empty first-user messages no longer collide every conversation's session-id (review catch on 0.2.6)

A targeted edge test on 0.2.6's `conversationSessionID` found the degenerate shapes: a first user message whose content is MISSING, `null`, `""`, whitespace-only, or `[]` hashed its empty key into ONE constant UUID — the same `x-session-id` for every such conversation on every account. That violates the feature's own contract ("a different conversation gets a different one") and cross-contaminates BigModel's prompt-cache key exactly where the feature was supposed to create hits.

- `session_id.go`: `isEmptyishContent` (absent / null / empty or whitespace-only string / empty array) routes those shapes to the DEVICE fallback — per-account stable, never a global constant; different accounts never collide. Image-only parts arrays and unparseable shapes keep the raw-bytes key (distinct per conversation) — their behavior is unchanged and now pinned.
- New `session_id_edge_test.go`: the five empty shapes all land on the device fallback, the literal bug shape (two different conversations, same empty spelling) no longer shares an id, cross-account isolation, image-only distinctness, and the 0.2.6 contracts (cross-turn stability, cross-conversation distinctness) re-asserted.

## 0.2.6

### Every request names its conversation in x-session-id — BigModel's prompt cache hits (magpie zcode 2b56b3b, #388)

ZCode's client names every request's conversation in `x-session-id`: a bare lower-case UUID, the same on each turn. BigModel keys its prompt cache on it — the upstream author's numbers: **3/12** turns hit the cache with a fresh per-request UUID, **24/24** with a stable one (#387). We were sending a fresh `uuid.NewString()` per request on the coding-plan trace set (and omitting the header on the start plan), so a long conversation almost never hit the cache.

- New `session_id.go`: `conversationSessionID(payload, deviceMid)` derives the conversation's stable UUID — from the conversation's FIRST USER MESSAGE (our host's ExecutorRequest carries no session context; the first user message never changes within a conversation, so the id is the same on every turn; magpie's device-derived UUID is the fallback when no user turn exists). Rendered as a lower-case UUIDv4 shape (8-4-4-4-12, version/variant bits) — exactly what the official client's `crypto.randomUUID()` produces.
- Coding plan (`applyChatHeaders`): x-session-id is the conversation UUID; the other trace headers (request/trace/query) stay per-request — they name THIS request, not the conversation. The V4 signing ladder still reads the header the request carries (`sessionIdFromRequest`), so the binding is unchanged.
- Start plan (`applyStartPlanChatHeaders`): the header now rides this plane too — magpie 2b56b3b sends it on every zcode request ("on the Start Plan and on Coding Plan or team seats alike"), superseding 0.2.4's omission; the whitelist discipline is unchanged (plugin-owned UUID, never an agent header).
- `metadata.user_id.session_id` carries the SAME UUID (in `buildAnthropicMetadataUserId`), as ZCode's own requests do — was hardcoded empty.
- Tests: `TestConversationSessionIDStability` (same conversation → same UUID across turns; different conversation → different; device fallback stable per device, distinct across devices; the anthropic `{type:"text"}` parts shape keys the same as the plain-string shape), the start-plan header assertion updated (x-query-id still omitted, x-session-id present as a UUID), and the metadata blob assertions. Full zcode suite green under `-race`.

## 0.2.5

### 405 code 3012 says ZCode's risk control blocked the account (magpie zcode 0.1.27, #348)

Absorbed from magpie-community/plugins `28f9cbb` (zcode 0.1.27, 2026-10-24): since 2026-10-24 zcode.z.ai answers 405 `{"code":3012,"msg":"request has been blocked due to unusual activity."}` to **every** request of an account its platform risk control blocked — ZCode's own desktop app (3.16.3) gets the same 405, and ZCode's support bot calls it "3012 platform risk control on the account". The old generic rendering (and magpie's old copy) let it read as a network block of the IP.

- `startPlanBizError` now owns the classification: the 3012 JSON gets a bilingual **account-restriction** copy — ZCode's own words with the status and code, then the actionable truth: the credential rests, the pool's other credentials serve, and only ZCode support lifts it. Distinct from the 0.2.2 content-filter shape (the sanitizer already keeps the system field clean) and from the 401 re-login family.
- New `isZcodeEdgeBlockPage` (checked before the code switch): an HTML / `errors.aliyun.com` block page is the **edge firewall blocking this network's address** — a network problem with its own copy; a JSON body is never the page, so the same wording inside the 3012 JSON stays the account's.
- Tests: `TestStartPlanBizErrorMessages` pins the 3012 restriction copy (ZCode's words + code + not-network), the HTML page's edge-firewall copy, and the JSON-never-page rule.

## 0.2.4

### Start Plan rides ZCode 3.16.3's exact request shape (magpie packages/zcode 0.1.22)

magpie's zcode plugin rebuilt its start-plan header setter on 2026-10-23 (0.1.22): the Start Plan is served only to what looks like ZCode's own request — 3.16.3's headers and nothing else. Their worry was agent headers leaking through a gateway (Claude Code's anthropic-beta, x-app, x-stainless-*, ?beta=true); our request builder was already whitelist-by-construction and the endpoint a fixed const, so no leak was possible — but three concrete details of the 3.16.3 client had drifted past us.

- `zcodeAppVersion` 3.14.0 → 3.16.3: User-Agent (`ZCode/3.16.3 ai-sdk/anthropic/3.0.81`), `X-Zcode-App-Version`, the usage preview's `app_version` query param, and the V4 signing gate message all follow the constant.
- `x-zcode-query-source: main_turn` now rides every start-plan (and off-peak ticketed) messages call — ZCode 3.16's `createModelRequestAttributionHeaders`, what asked the model.
- `X-Os-Version` is per-platform `os.release()` parity: Linux keeps `/proc/sys/kernel/osrelease`; darwin builds now send `kern.osrelease` (a real macOS ZCode client always carries the header — ours used to omit it).

## 0.2.3

### Multi-plan balance pools: a spent twin no longer hides the live one (issue #53)

Field report (AkimioJR, 2026-10-23): the ZCode 1024 Dev Day gift (GLM-5.3-Flash + GLM-5.3, 500M tokens, valid to 10-26) was claimed on the website, but the plugin's pools could not tell it apart from the new-user plan's spent GLM-5.3 — same show_name, no plan linkage, the spent bucket listed first. magpie zcode 0.1.23 (yetone, 2026-10-24) pinned the same trap on their side: "two gift plans each bucket is named by its plan, and the live ones come first".

- The balance response's `plans` array is now parsed (`status` / `user_plan_id` / `plan_id` / `name` / `ends_at`); a bucket's owner is matched the way magpie's `giftOf` does — `user_plan_id` pair first, `plan_id` fallback (`sameBalancePlan`).
- When more than one live plan holds buckets, every bucket is named after its plan ("ZCode 1024 Dev Day · GLM-5.3"); one live plan keeps the bucket's own name.
- Pools a request can spend now list first, spent ones next ("（已用尽）"), buckets of an expired/inactive plan last ("（已过期）", still plan-prefixed); each group keeps the balance's own order.
- Route-plane capability harvest (`harvestStartPlaneCapabilities`) now skips spent buckets and buckets owned by a non-live plan — with two plans granting the same model, the spent twin's capability must not stand in for the live one.
- Totals still sum every row, so a live 1024 bucket keeps an account with a spent new-user twin from reading as exhausted.
- The panel's empty claim-list note now says invitation/targeted campaigns (1024 Dev Day invitation packs) come down the website channel, not billing/preview — their pools appear in the balance area after claiming there.

## 0.2.2

### start-plan 3012: the system field carries official blocks only (PR #54, ki11a-Conton)

The start-plan gateway content-inspects the `system` field and answers 405/biz 3012 ("request has been blocked due to unusual activity") as soon as it finds a block that is not part of the official client assembly — single-variable probes: a lone "You are a helpful assistant." in system is rejected (P1), the same text inside the first user turn answers 200 (P2/P4), and official 3 blocks + caller text as a fourth is rejected (P3). magpie zcode 0.1.21 (#343) pinned the same gate on the block count.

- `anthropic.go`: `applyStartPlanTransform` pins `system` to exactly the official 3 blocks and relocates the caller's own system text into the first real user turn (before the context_prefix turn is prepended, so the official prefix keeps its single-block shape).
- `sanitize_prompt.go` (new): plugin-side phrase scrub across every role — system/user/assistant text, reasoning mirrors, and tool_calls arguments — mirroring the sibling workbuddy CN/intl gate (11128) which reads the whole body. Rewrites are meaning-preserving word substitutions; a payload without a trigger phrase passes through byte-identical.
