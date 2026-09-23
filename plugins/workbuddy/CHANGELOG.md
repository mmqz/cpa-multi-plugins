# Changelog

## 0.9.34

### Opt-in async-stream head gate — `stream_head_timeout` (repo workbuddy 0.9.34)

On the async executor path the plugin returns the stream-open envelope as soon as
it holds a `stream_id`, so every failure that surfaces afterwards can only travel
as `host.stream.emit{error}` — plain text, HTTP status discarded. The host then
neither cools the credential nor fails over, and the client sees a "successful
empty answer". The two worst shapes: an upstream `>=400`, and an HTTP-200 stream
carrying an in-stream error frame (CodeBuddy's `event:error` / `{"code":...}`
quota body) **before the model ever answered**.

**Decide the hand-off before committing it (opt-in).** `stream_head_timeout`
(integer seconds, default `0` = disabled, which keeps the pre-feature pump
byte-for-byte, emission timing included) makes `handleExecStream` hold the
stream-open envelope until `pumpUpstreamStream` reports its first decisive event
through a one-time buffered-channel verdict (structure aligned with trae PR #11):

- upstream `>=400` / transport error → the existing synchronous failure path,
  now surfaced as a status-bearing failed envelope instead of an in-band text
  error;
- a `workBuddyStreamFrame` error before any answer → a failed envelope whose
  HTTP status is derived by **reusing the chat_error classifier** (`streamFaultStatus`
  synthesises the status the gateway *would* have used, then `upstreamStatusError`
  applies the account-vs-request policy): 401 / 402 / 429 and business-envelope 403
  ride the envelope; request/IP-level shapes and the model-scoped **6004 stay
  status-less on purpose** (a drained credential rotates, a healthy one never
  cools on a guess);
- the first frame that actually answers (content / reasoning — a **role-only
  opener does not count**, the trap trae documented) → release and stream the rest
  exactly as before;
- a silent window → release normally. A stalled or never-answering upstream is
  never turned into a hang, and no goroutine is left blocked (both channels are
  buffered and the hand-off always answers once).

**No rollback, no new accounting.** A post-answer error is still in-band only —
bytes already on the wire are not swallowed. `NoteSuccess`/usage semantics are
untouched: workbuddy already publishes a stream's success only after it drains
cleanly (unlike trae, there was no hand-off-time success miscount to fix).
Structural seam for tests: the frame loop is `pumpStreamFrames` over a `streamSink`
+ optional `streamHeadGate`. Tests (stream_head_gate_test.go) drive real SSE
frames through it — abort-before-handoff (zero chunks emitted, 402/6004 status
pinned), release-after-answer (in-band error preserved, answer intact), silent
timeout (released near the wall-clock window), and the `stream_head_timeout: 0`
regression.

## 0.9.33

### Static model catalogs removed — advertisement is discovery-only (repo v0.12.82)

Upstream retired the hy3 family (hy3 / hy3-preview / hy3-preview-agent) without
notice while the hand-maintained static catalogs still advertised it. The stale
ids turned into host-level `unknown provider for model hy3-preview-agent` 400s
that no plugin log could explain — the list a client sees and the models the
upstream actually serves had silently diverged, and nothing in the error path
pointed at the cause.

**Better empty than wrong.** The static catalogs (CN's twelve-entry table and
the Intl/Global hy4-preview fallback) are deleted, not refreshed:

- `model.static` advertises nothing, deliberately — ids are upstream's to
  define and retire, and a static table rots faster than any changelog can
  track;
- `model.for_auth` = config pin (`models_cn` / `models_intl` / `models_global`,
  still authoritative and still skipped discovery) > discovery (fresh answer >
  today's cache > stale cache) > nothing;
- a tokenless credential advertises nothing (no more faked list);
- discovery failure with no cache advertises nothing and logs why — the realm
  diagnostics now say `none (discovery failed)` / `none (no token in storage)`;
- the 11102 error hint lists the cached (or stale) discovery catalog and, when
  no catalog exists, points at `models_<realm>` pins instead of inventing ids.

Chat requests still pass the client's model id through verbatim. Pinned IDs
now get generic metadata (Name = ID) since the metadata table is gone —
display names for everything else come from upstream discovery itself.

## 0.9.32

### 6004 model-scoped rate limit: per-(credential, model) hold with the declared reset (repo v0.12.81)

The CN gateway answers model-level frequency limits with HTTP 429 + code 6004 and
says so in the message — "您也可以切换其他模型继续使用" (switch to another model to
continue). Attributing that 429 to the credential let one throttled model drive the
host's escalating quota backoff across every model on the credential and 503 them
all ("auth_unavailable ... last upstream error: 6004") — the opposite of what
upstream advises, and the escalating window ignored the exact reset instant the
upstream itself declares ("将在 2026-09-22 09:46:39 UTC+8 重置").

**No credential attribution.** `upstreamStatusError` keeps 6004 at status 0 on
every host version: the credential stays healthy for its other models. The
credential-wide escalating backoff can no longer be armed by a single model's
soft rate limit.

**Declared reset honored.** `parseRateLimitResetAt` extracts the "将在 <ts>
UTC+8 重置" instant; a per-(uid, upstream-model) registry (new
model_ratelimit.go) fast-fails that pair — no upstream call — until the
declared instant (1-minute default window when unparsable; never a guessed
long window). Later 6004s only extend, never shorten, an active hold.

**No more black box.** The bilingual copy states the exact reset time and
upstream's switch-models advice. The literal "429" is deliberately absent
from the copy so hosts that classify by message cannot re-read it as
credential quota.

**Credential ban = zero credits only.** 402/hard-credit keeps its
credential-level path (credits reconcile lifecycle); soft 429s never trigger
it (the pre-existing 429 guard in `isHardCreditError` is unchanged).

Wiring: fast-fail in `handleExecExecute`/`handleExecStream`; the window is
noted at all three upstream >=400 sites (execute, `collectUpstreamStream`,
`pumpUpstreamStream`). `collectUpstreamStream` gains an upstreamModel param.
Tests (model_ratelimit_test.go) pin the classifier, the reset parser
(including the 2026-09-22 09:46:39 UTC+8 production sample), the registry
lifecycle, the status-0 carve-out and the copy invariants.

## 0.9.30

### Deep-audit round: the non-stream path gets the same guards (repo v0.12.76)

A subagent code review of the 0.9.29 absorption round found the non-stream
executor path missing every guard the streaming paths already had; this
release closes the gap.

**Upstream error frames surface on the non-stream path too.**
`handleExecExecute` folded SSE chunks via `aggregateCompletion`, which fed
every line straight into the delta merger: an SSE `event:error` line was
silently skipped, and a 200-OK JSON body carrying `{"error":...}` or a
non-zero `code` produced a synthetic `chatcmpl-workbuddy` completion with
empty content and `finish_reason: "stop"` — a silent fake success.
`aggregateCompletion` now routes every line through `workBuddyStreamFrame`
and aborts on error frames.

**Empty streams rejected on the non-stream path too.** A 200 response that
ends without a single completion payload is now an explicit `empty_stream`
failure — same message and semantics as the two streaming paths.

**Formatting restored.** payload.go had picked up a tab→space regression in
0.9.29's large edit; `gofmt -w` returns it to canonical form.

## 0.9.29

### Fork audit round: stream errors surface, empty streams rejected, WorkBuddy CLI identity (repo v0.12.74)

A full audit of the libo0118 fork's qoder-custom branch (7 commits, 26
files) surfaced four defects in this tree; this release fixes the two that
belong to workbuddy (qoder's fixes ship in 0.8.17 the same day).

**Upstream error frames surface.** The stream pumps fed every data line
straight into cleanChunkJSON, so an upstream error delivered inside a
200-OK stream — an SSE `event:error` line, or a JSON frame carrying
`{"error":...}` or a non-zero `code` — was silently swallowed: the stream
ended looking successful and usage was billed against a completion that
never happened. `workBuddyStreamFrame` now inspects each line before
translation and aborts the stream with a redacted message on error frames.

**Empty streams rejected.** A 200 stream that ends without a single
completion payload (keep-alives + [DONE] only) is now an explicit
`empty_stream` failure — in both the streaming pump and the aggregating
path — instead of a silent empty success.

**WorkBuddy CLI identity on chat calls.** backendHeaders now fills
`X-IDE-Name: WorkBuddy / X-IDE-Type: WorkBuddy / X-IDE-Version: 5.5.6`
when no identity is present, so the official usage history's client column
is populated for CLI-login accounts. The CodeBuddyIDE (platform=="ide")
and Intl realm overrides keep their existing values.

**Hy3 "low" effort preserved.** The current Hy3 catalog advertises a low
level; forceMaxThinking no longer forces it up to high (hy3/hy3-x only —
hy4 keeps the historical pin).

## 0.9.28

### Issue #3 closure: system-prompt wholesale replacement retired (repo v0.12.73)

The remaining piece of issue #3 — the wholesale neutralPrompt swap for
system messages over 2000 bytes or matching agentPattern, kept in v0.9.26 as
a "WAF backstop" — is retired. The user's recheck challenged the retention,
and the evidence is on their side:

- The upstream filter blocklists VERBATIM phrases. A verbatim matcher does
  not reject by length or by broad agent-identity patterns — the success of
  the one-word-insert safe variants ("Anthropic's official CLI **tool** for
  Claude") proves the mechanism is literal matching, not length or
  heuristic scanning.
- PR #4's author removed both triggers and ran without 400s.
- Since the v0.9.18 role gate, non-system messages of any length or content
  pass unfiltered — no rejection was ever reported.

What the triggers DID do was gut every agent host's system prompt: nearly
all match "you are claude code" / "you are a coding agent" and exceed 2000
bytes, so tool-use rules, project context and behavior constraints were
silently swapped for an 18-byte generic line — a permanent invisible
degradation, strictly worse than a loud 400 that can be reported and fixed.

**Change.** `rewriteSystemContentField` now applies only
`sanitizeBlockedTemplates` (blocked phrases → safe variants; everything
else survives verbatim), on strings and per text part on arrays. The
neutralPrompt constant, agentPattern regex, maxSystemPromptBytes threshold
and flattenSystemParts helper are removed. The sanitize regex net from
v0.9.26 keeps the two known blocked phrases neutralized, and the v0.9.18
role gate (system-only) is untouched.

**Tests.** sanitize_scope_test.go rewritten to the new semantics: long
clean system prompt survives verbatim (the agent-host regression), identity
line sanitized while surrounding context survives, broad agent-identity
patterns no longer wipe anything, array structure preserved with per-part
sanitize, non-system roles still untouched, full-pipeline end-to-end.

## 0.9.27

### PR #6 recheck round: auth document merge on token refresh (repo v0.12.72)

The v0.12.71 absorb review missed one real fix: 4d5533e ("preserve unknown
top-level auth fields on token refresh") was dismissed with "the function no
longer exists" — wrong. `persistAuthTokens` is alive in keepalive.go and
still rebuilt the on-disk credential with `json.Marshal(sa)` alone, wiping
every top-level key outside the storedAuth struct on each token refresh:
panel-managed fields (proxy_url, logo, ...) AND the plugin's own
`markSessionDead` markers (disabled/note — a resurrected token would
silently un-disable a dead auth).

**Fix.** `persistAuthTokens` now goes through `mergeStoredAuthIntoDoc`
(phys.JSON, sa): the existing document is decoded, only the owned
auth/account objects are overwritten with the refreshed values, and every
other top-level key survives verbatim — the same merge pattern
`markSessionDead` already uses. An unreadable existing document is an error
(silently discarding it would reintroduce the wipeout); an empty one builds
a fresh document. The overwrite is deliberately top-level key-level: the
auth/account objects are replaced wholesale so stale object-internal
sub-fields can never contradict the refreshed tokens.

**Tests.** `authdoc_merge_test.go`: unknown top-level fields + lifecycle
markers survive (proxy_url/logo/disabled/note), fresh-document build,
bad-document error, and the owned-object-replace boundary pinned both ways.

## 0.9.26

### PR #4 / PR #6 code absorbed clean-room (repo v0.12.71)

The two open PRs carried Claude co-author trailers, so neither was merged —
the CODE was ported by hand onto the current tree with fresh tests
(`pr_absorb_test.go`); the PR branches stay out of main's history and the
contributors list stays clean.

**reasoning replay (issue #5).** `injectReasoningInPlace` folds each
historical assistant turn's `reasoning_content` (or `reasoning`) into its
content as a `<thought>` block during `prepareUpstreamBody` (step 3.5). The
Tencent gateway silently drops the non-standard field from multi-turn
history, so the model never saw its own prior chain of thought — inlined
text survives the field whitelist. Idempotent; handles string and
multimodal content (the thought becomes a new leading text part).

**SSE comment frames (from PR #6).** `cleanChunkJSON` returns "" for
leading-colon lines, so upstream `: keep-alive` / `: heartbeat` frames are
never re-emitted as `data: : heartbeat` — the malformed event that crashed
strict clients which JSON-parse every `data:` line.

**blocked-template regex net (from PR #4).** `sanitizeBlockedTemplates`
gains case/quote-drift regex variants of the two OmniRoute templates in
addition to the byte-exact `ReplaceAll` pair. PR #4's wholesale removal of
the 2000-byte system replacement is intentionally NOT adopted — the v0.9.18
scope fix already restored the role gate, and the system-only length
replacement stays as the WAF backstop.

**discovery transient guard (from PR #6).** `noteRealmError` no longer
wipes the realm's last successful discovery; the failure path serves
`cachedDynamicModelsStale` (the last good list, TTL notwithstanding) and
only falls back to the static catalog when nothing was ever discovered.
Diagnostics distinguish "last discovery (transient failure)" from "static
(discovery failed)".

## 0.9.25

### check-in status stability + Intl tier-alias display evidence (repo v0.12.70)

Three field reports from 2026-09-20, all fixed:

**已签到显示未签到 (check-in status flapping).** Credentials that already
checked in sometimes showed 未签到 in the panel. Root cause:
`fetchCheckinStatus` took the FIRST endpoint that answered
(`/v2/billing/meter/checkin-activity-status`), but that payload
intermittently arrives activity-shaped — `today_checked_in` absent
entirely — and the parser treated the missing field as `false`, which then
poisoned the account cache for a full TTL window (45 s) and every panel
refresh within it. Now a payload without today-evidence (no
`today_checked_in`-style key AND no calendar) is treated as ambiguous: the
second endpoint (`checkin-status`) is probed and both views OR-merge —
`true` can never be downgraded. A payload WITH explicit today evidence
returns immediately, so the common case stays a single upstream call. As a
belt-and-braces cross-check, when the payload carries `checkin_dates`,
today's Asia/Shanghai date in the calendar implies checked-in regardless of
the boolean field.

**Intl tier-alias display (国际版模型仍然不对).** The user panel now surfaces
SEVEN opaque codebuddy.ai tier ids: the four known aliases
(fast-model / auto-chat / balanced-model / default-model) plus
primary-model / deep-model / enhance-1.0 — all annotated
"（上游别名）" via `discoverToInfo`; genuine ids (o4-mini) stay untouched.
Beyond naming, the tier→real-model mapping that 0.9.19 concluded
"unknowable without upstream publishing it" is now LEARNED FROM EVIDENCE:
every chat response's `model` echo is captured (SSE collector +
folded-completion path) and, when the request model is a tier alias and the
echo names a concrete model, the alias→real pair is recorded
(`noteLearnedRealModel`), logged, overlaid onto every served model list as
"Fast Model（上游别名）·实测 glm-x", and exposed through the dashboard
models diagnostics row (`learned` map). The 0.9.24 growth-task chats seed
the mapping automatically.

**panel diagnostics.** `realmModelsState` gains `learned` (alias→real map);
the panel appends "· 实测 fast-model→glm-x" to the model-source row.

### 方言说明

`fetchCheckinStatus` 双端点循环从"首个成功即停"改为"有今日证据才停"；
`checkinSummary.mergeOR` 单向合并（false 永不覆写 true）。签到日期交叉
验证用固定 Asia/Shanghai 时区（cstZone），不吃宿主进程时区。

## 0.9.24

### real-conversation tasks: the remaining 6 growth tasks automated end-to-end (repo v0.12.69)

User follow-up (2026-09-20): "剩余 6 个任务没法直接做吗" — 0.9.23 automated
13/19 growth tasks via fingerprint event chains; the remaining real-
conversation tasks (Model_chat_GLM5.2, skill_1, expert_5, Expert_team_use_3,
Expert_lighthouse, black_cat) were deferred pending a chat-channel hook.

Answer: they are doable — wb2api's autotask has since shipped verified
recipes for all six (its package header claiming skill_1 "unbroken" and
Expert_lighthouse "skipped" is stale; the implementations carry per-task
multi-account light-up notes dated 2026-09-12). This release ports them:

- task_chat.go (new): the real-conversation layer, mounted as 6 new rows in
  the growthAutoActions table (13 + 6 = 19 — every growth task except
  Expert_Philanthropy, which requires a real money donation, is now
  automatable).
  - Two judging mechanisms, both requiring an actual chat round-trip with a
    tiny prompt ("hi，请回复一句话" / "1+1等于几？直接回答。" — negligible
    quota):
    1. glm-5.2 tasks (Model_chat_GLM5.2, black_cat): a real chat leaves
       server-side evidence; the follow-up /v2/report must align
       requestModelId with the chat model (report glm-5.2 after a glm-5.2
       chat — the pre-existing flash default reads as a mismatch).
       growthReportActivityModel adds the model-aligned variant;
       growthReportActivity delegates unchanged.
    2. JOIN tasks (skill_1, expert_5, Expert_team_use_3, Expert_lighthouse):
       a desktop-fingerprint SSE chat yields the SERVER-side requestId
       (first data.id matching cmb-|32hex — fabricated UUIDs do not count,
       wb2api Sunny row 2113), which the chat chain + skill_info /
       expert_actual_use events must JOIN. Expert ids must come from the
       real market list (POST /portal/operation-platform/market/expert/list).
  - growthDesktopChatID parses the SSE stream with a watermark scanner
    (chunk-straddling keys and non-matching leading ids handled; closes the
    stream once the id is found — JOIN tasks only need the id).
  - expert batches are deficit-aware (3/5 → exactly 2 rounds), per-expert
    failure continues, all-fail reports the last error.
  - black_cat only counts inside the 23:00–08:00 local window; outside it
    the action is a no-op line that names the window (quota preserved).
  - Night top-up slot: checkin.go's scheduler gained a 23:00 slot
    (growthNightTaskHours) running growthNightTaskTick for all CN accounts
    under the per-account checkin lock — black_cat completes overnight even
    if the user never runs tasks at night.
- host_bridge.go: hostStreamReader gained Close() (io.ReadCloser parity for
  drain-and-close task-chat streams).
- Tests (task_chat_test.go, 13): night-window boundaries + scheduler slot
  registration, server-id regex, summon/actual-use/skill_info shapes,
  SSE id extraction (including non-matching first id and truncated id),
  model-aligned report, table completeness (19), expert deficit, black_cat
  window gate + deficit, model-chat and lighthouse end-to-end flows against
  a scripted SSE upstream. TestNextCheckinTime updated for the new 23:00
  slot (22:00 → next 23:00 same day; 23:30 → next day 09:00).

- Quota note: a full fresh-account run now spends ~14 tiny real chats
  (1 glm-5.2 for Model_chat, up to 5+3+1 fast-model for experts/lighthouse,
  1 fast-model for skill_1, up to 3 glm-5.2 overnight for black_cat) on top
  of the zero-cost fingerprint events of 0.9.23.

## 0.9.23

### auto-light the growth task center (desktop/web/mp fingerprint event chains) + adopt-before-accept ordering (repo v0.12.68)

User follow-up (2026-09-20): "没办法自动做吗" — can the first_buddy gate and
the gated tasks be done automatically instead of sending the user to the
official client?

Answer: yes. Upstream scores growth tasks from behavior events on ONE
endpoint (POST /v2/report) distinguished by client fingerprint, not by
real client activity — a conclusion wb2api (workbuddy2api-panel) proved
with multi-account experiments on 2026-09-12 and this release ports:

- task_events.go (new): the fingerprint protocol layer —
  * desktop channel: copilot.tencent.com/v2/report + WorkBuddy/5.5.6 UA +
    per-event WorkBuddy/workbuddy-desktop fingerprint family (ideName/
    ideVersion/machineId/sessionId/extName, machineId deterministically
    derived per uid), business fields override the fingerprint;
  * web channel: www.workbuddy.cn/v2/report + x-client-platform: web with
    browser-shaped events (Library_read's library_doc_intro_click);
  * mp channel: billing domain + X-Client-Platform: mp-weixin header family
    + workbuddy-mp event fingerprint; growth-domain calls for mp-only tasks
    (school_season, Sequential_Tasks_1) carry X-Client-Platform:
    miniprogram — without it accept returns task not found;
  * event chains: the 6-event desktop chat sequence (RichMeow_Chat),
    buddyapp 5-event chain (Buddy_App + Buddy_App_QQ in one pass),
    automation single event, template ×5 group, playbook prompt group,
    design-canvas group, appearance/set API + skin-apply event.

- task_auto.go (new): the auto-light orchestration mounted as step 4.5 of
  the daily loop. 13 tasks are automatable end-to-end this release
  (chat_5 top-up by delta, first_buddy adoption, RichMeow_Chat, Buddy_App,
  Buddy_App_QQ, automation_1, Library_read, template_5, playbook_prompt,
  create_canvas, Hp_Appearance, school_season, Sequential_Tasks_1); every
  action is idempotent (claimed/scored tasks skipped), scores are read
  back with a bounded poll (upstream scores asynchronously — a single
  immediate read misjudges), and a claimable read-back auto-claims on the
  matching domain (mp tasks fall back chat → web on 400, the 200+OK-but-
  not-recorded accept shape is retried once with a read-back check).
  Remaining real-conversation tasks (Model_chat_GLM5.2, skill_1, expert
  family, black_cat) need a server-side requestId from a live chat and
  are planned on top of the chat channel — out of this release.

- taskcenter.go ordering fix: buddy adoption moved from the travel step
  (after accept) to step 1.5 (right after the activity report, BEFORE
  accept). On fresh accounts adoption completes first_buddy — the shared
  prerequisite of every other task — so acceptance can now enroll on the
  FIRST run instead of losing a full run; the travel step reports
  "尚未领养 Buddy" instead of re-attempting adoption.

- Tests: desktop chain shape + fingerprint injection precedence + header
  families per channel + mp activityId switch + mp-claim web fallback;
  auto-light table sanity, claimed-skip, delta top-up with deterministic
  async-score flip and auto-claim, silent skip for accounts without the
  tasks; and a daily-loop ordering regression that pins adoption strictly
  before acceptance.

Note: the "在官方客户端新建任务并发起对话" hint is no longer the intended
path for first_buddy — the loop reports and adopts by API. The label in
growthPrerequisiteLabel stays as the fallback explanation only for the
window where adoption itself was rejected (gate not yet lifted).


## 0.9.22

### management route table restored + run-all toast surfaces blockers (repo v0.12.67)

User report (2026-09-20): the voucher dialog failed with
"管理桥接响应异常（HTTP 404）：响应体为空", and the one-click 任务 run-all
"completely did no tasks" while newly granted quota packages stayed few.

Root cause (vouchers): the host dispatches plugin management routes by exact
key lookup over the plugin's DECLARED route table — any undeclared path is
answered by the host gin NoRoute with a 404 and an EMPTY body before the
plugin ever sees the request. 0.9.13 declared /tasks + /tasks/run (twice),
0.9.14 swapped their handler cases for /school/vouchers without declaring it,
and 0.9.18 restored the tasks cases without restoring the vouchers entry —
so since 0.9.14 the voucher dialog never reached the plugin. The original
"JSON.parse: unexpected end of data at line 1 column 1" report was the same
empty body hitting the old panel decoder; v0.12.64's scan-budget fix was
real defense but not the root cause.

Root cause (tasks): nothing hid the gate — the loop ran, but on fresh
accounts ~17 growth tasks sit behind the upstream first_buddy prerequisite
("领取一只 Buddy（在官方客户端新建任务并发起对话）"), which only an official-
client action can clear; accepts are rejected, progress never counts, so no
rewards and no new packages. The batch toast then filtered the loop lines to
+\d+ rewards only, swallowing the one line that explained everything — the
user saw "did no tasks" with no reason.

Fixes:
- management.go: declared Routes deduped (/tasks, /tasks/run once each) and
  GET /school/vouchers declared — the dialog reaches the plugin again, so
  voucher codes are queryable/copyable for the rest of the activity window.
- panel.html run-all toast: reward lines keep priority, prerequisite-gate
  lines (deduped by label) and session-dead lines (deduped per account) are
  now surfaced with a bounded total.
- panel.html api(): empty-body hint is status-aware — 404 points at an
  undeclared plugin route (update plugin + restart host) instead of the
  misleading scan-timeout copy.
- routes_table_test.go: three guards — no duplicate declarations, handler
  surface fully declared, and a dispatch smoke proving every declared route
  reaches a real handler (both drift directions pinned).

Family audit: trae (intl cases live in intl_management.go) and qoder route
tables are 1:1 with their handler switches — workbuddy was the only drift.

Verified with a headless-browser probe: mock /tasks/run carrying a
prerequisite line, a reward line and a non-cn skip — the batch toast now
reads "账号A 领取 daily_share: +50 分 +0 能；部分任务需先 领取一只 Buddy
（在官方客户端新建任务并发起对话）", zero JS errors.

## 0.9.21

### panel: selected-account button size anomaly fixed (repo v0.12.66)

User report (2026-09-20): on the account panel, the buttons of the card
showing the selected state rendered with an abnormal size — visibly taller
than the same buttons on every other card, with their labels stacked
vertically (使/用/中 one character per line).

Root cause: the CN action row holds five buttons (选用/使用中 + 刷新 +
签到/已签到 + 任务 + 券码). Their natural widths fit the card, but any
three-character label ("使用中" on the selected card, "已签到" after
check-in) pushed the row's natural width ~2px past the available content
width. `flex-shrink` then squeezed every button by a fraction of a pixel,
and 26px of CJK label content lost to 25.6px — each two-character label
wrapped onto two lines, doubling button height (34px -> 52px). Narrow
viewports widened the squeeze. The row lacked `flex-wrap`, so there was no
escape valve; trae's panel has had `flex-wrap: wrap` all along, which is
why only workbuddy exhibited the bug.

Fix (panel.html, embedded):
- `.actions` gains `flex-wrap: wrap` — a tight row wraps its last button
  to a second line instead of compressing the others.
- base `button` gains `white-space: nowrap` — a button label can never
  stack vertically again, regardless of future label changes.

Verified with a headless-browser probe (mock /accounts + /credits across
1280px and 720px viewports): every action button now measures 34px tall in
all six mock cards (selected / checked-in / disabled+exhausted / Global /
Intl / long-nickname), zero vertical stacking, zero row overflow.

## 0.9.20

### growth acceptance triage (repo v0.12.65)

Aligned with upstream growth_runner 2026-09-20 observations:

- "task does not require acceptance" is now treated as a normal answer,
  not a failure (previously it misled users into thinking tasks broke).
- "prerequisite not met: <code>" replies are merged into one summary line
  per reason (new accounts see ~17 tasks gated behind first_buddy; the
  per-line noise hid the one actionable item).
  growthPrerequisiteOf/growthPrerequisiteLabel map codes to actionable
  copy (first_buddy -> chat first to unlock), sorted for stable output.
- everything else remains a per-task real failure; blocked tasks no longer
  count toward the failure count.

## 0.9.19

### transport-error billing retries + bounded scans + readable bridge errors (repo v0.12.64)

User report (2026-09-20): the 开学季/券码 dialog failed with the cryptic
"JSON.parse: unexpected end of data at line 1 column 1 of the JSON data",
and credits queries against the Intl gateway surfaced repeated
`Post "https://www.codebuddy.ai/v2/billing/meter/get-user-resource": EOF`
as hard failures.

Root cause one (billing): isTransientBillingErr's doc comment always
promised transport retries, but the implementation only matched 5xx
prefixes — a gateway that closes the connection mid-request (Go's
`Post "...": EOF`) never got a second attempt. Connection-level failures
(EOF / connection reset / broken pipe / client timeout / TLS handshake
timeout / dial failures) are now classified transient and retried through
the existing billingRetryDelays loop; parse-failed and business-code
errors stay terminal (the pre-existing test boundary
"parse failed: unexpected EOF" is preserved — that shape means the server
DID answer, with garbage).

Root cause two (management scans): handleSchoolVouchers (券码) and
handleTasksQuery (任务) are per-account fan-outs with no deadline — three
sequential upstream calls per CN account at up to 120s each on the shared
client. On a flaky gateway the handler outran the host management bridge,
which returned an EMPTY body; the panel then threw the cryptic
JSON.parse error instead of anything actionable. Both scans now carry a
45s budget: accounts starting past the deadline are reported as
`skipped: "scan budget exceeded（扫描超时，稍后重试）"` and the envelope
always completes; individual school calls are additionally capped at 20s
via request context (honored on the direct-client path). The explicit
任务 run-all intentionally keeps unbounded semantics — it genuinely runs
the whole growth loop.

Root cause three (panel): api() decoded responses with a bare r.json().
It now reads text first and converts non-JSON/empty bodies into a
readable error carrying the HTTP status and a snippet
("管理桥接响应异常（HTTP xxx）… 响应体为空（上游扫描超时或桥接中断）").

Intl model aliases (field report: only fast-model / auto-chat /
balanced-model / default-model visible, the limited-free "deepseek
flash" nowhere to be found): codebuddy.ai's discovery endpoints return
product-TIER aliases, and the alias itself is the routable upstream id —
chat requests send it verbatim and it works. Upstream does not publish
which real model backs each tier, so no id can be invented client-side.
The four known aliases now carry display names annotating them as
upstream aliases (Fast Model（上游别名） etc.) via discoverToInfo; real
ids (o4-mini) and rows with richer upstream display names are untouched.

## 0.9.18

### neutralPrompt scope fix + 2026-09 growth contract (repo v0.12.63)

User report (2026-09-20): workbuddy conversations "reset every so often",
the model answers "You are a helpful AI assistant that helps with software
engineering tasks." when interrupted, tool output "looks truncated" (long
stdout comes back empty, .ps1/.md files unreadable) — the agent itself
started printing files in small chunks to work around it. Separately, the
task center showed far fewer tasks than expected.

Root cause one (payload): the neutralPrompt wholesale replacement was applied
to messages of EVERY role. OmniRoute codebuddy-cn.ts (the porting source)
gates it on `message.role !== "system" -> return message` verbatim; our port
lost the role check. Any user paste / tool result / assistant history entry
over maxSystemPromptBytes (2000) — or merely quoting an agent identity line —
was silently rewritten to neutralPrompt. The upstream model then saw a
history full of hollow "You are a helpful AI assistant..." messages: long
tool output appeared "truncated", long pastes disappeared, and the neutral
prompt itself leaked into answers. Fixed by scoping the replacement to
role=system messages only (rewriteSystemMessagesInPlace /
rewriteSystemContentField); the array shape now collapses into a single text
part (OmniRoute parity) instead of one neutralPrompt per part.

Root cause two (task center): the 2026-09 upstream contract changed
(Coding2API f18dc3d, "five stuck tasks piled up 650 unclaimed credits"):

- accept_status is FIVE-state (not_accepted | accepted | in_progress |
  completed | claimed); only ""/"not_accepted" tasks need enrolling. The old
  three-state guess treated non-empty statuses as accepted.
- Claimability now also trusts accept_status=="completed" (progress fields
  lag on some task types); the progress-reached test stays as fallback.
- Batched accept (20/call) with per-task results surfaced — silent
  "prerequisite not met" rejections stay visible.
- Redemption reads the GRANTED fields (credit_granted/energy_granted); a
  400 "unknown tier" retries once with the legacy day-number form; the 403
  "连续登录天数不足" tier-lock renders as a normal "not unlocked" line.
- Claims run BEFORE the lottery (task rewards grant chances — spendable the
  same run). Travel claim reads the credit field with reward_credit fallback.
- New steps: buddy box (the energy sink — energy has no other outlet) and an
  energy-balance tail. Lottery chances moved to /lottery/chances (balance)
  with the legacy summary endpoint as fallback.
- A credential-level 401/403 aborts the remaining growth-domain steps
  (growthHTTPError carries the HTTP status; tier-locked 403 exempt).

Regression tests: sanitize_scope_test.go (non-system messages preserved
verbatim, agent-identity user text preserved, system array collapse,
end-to-end pipeline), growth_contract_test.go (five-state matrix, completed
claimability, error taxonomy, per-task accept results, granted fields +
day retry, travel-claim credit priority, lottery endpoint fallback).

## 0.9.17

### Credential cooldown finally reaches the host (repo v0.12.62)

User report (2026-09-20): an account with drained credits + exhausted
free-tier quota keeps being picked for every request — the second failure in
a row still goes to the same credential.

Root cause: our RPC error envelope carried only code+message. The host's
decodeEnvelopeResult therefore built a status-less rpcError (StatusCode()=0)
and MarkResult could only apply its 1-minute transient default cooldown —
invisible in practice. The host machinery itself was always there (402 -> 30
min, 429 -> escalating quota backoff with credential-scoped model expansion,
401 -> 30 min; cooled credentials are filtered before scheduler pick, plugin
routing included).

- envelopeError gains `http_status` (mirrors pluginabi.Error); errorEnvelopeFor
  extracts StatusCode() from handler errors and serializes it across the RPC.
- statusError + upstreamStatusError wrap translated upstream chat failures in
  the execute and collect paths with an explicit pass-through matrix:
  account-level 401/402/429 and business-envelope 403 pass; 413/11115 prompt
  overflow, 11128 channel risk control, 11102 model-catalog rejection and bare
  403 WAF challenges stay status-less (request/IP-level — a credential must
  not be cooled for problems any account would hit).
- panel.html: the done-state button added a 1px border on top of border:0,
  shifting its layout size by 2px versus sibling buttons; replaced with an
  inset box-shadow ring (no layout change).
- Regression tests: envelope carries/omits http_status; the full
  upstreamStatusError policy matrix.

## 0.9.16

### Copy precision for prompt-too-long (repo v0.12.61)

Code-review follow-up on v0.12.59/0.9.15: the prompt-too-long translation
hardcoded "code 11115 prompt is too long" in its client-facing copy, but the
detection itself (v0.9.15) also matches bare 413 gateway rejections (HTML /
empty body) and the extended wording family — none of which carry code 11115.

- The code mention is now conditional: bodies containing 11115 keep the
  "code 11115 prompt is too long" detail; everything else shows
  "413/context limit exceeded" instead of pointing users at a code that
  is not in the raw response.
- Guidance core (缩短上下文 / 与账号无关 / request-level) unchanged.

## 0.9.15

### Large-input resilience (repo v0.12.59)

Same treatment qoder 0.8.11 got, ported to the CodeBuddy gateway after user
reports of "context/input too large → request just fails" on the agent side.
Upstream evidence: RobbsLuo/Coding2API (2026-09-18, same endpoints —
copilot.tencent.com / codebuddy.ai /v2/chat/completions).

- `normalizeHistoryInPlace` (payload step 2.5): OpenAI `developer` role →
  `system` (the Tencent backend rejects developer with channel risk-control
  11128); dirty tool_calls (missing function/name) dropped, emptied content-less
  assistant placeholders and dangling role=tool results dropped with them —
  oversized agent histories trimmed mid-conversation are the main orphan source.
- Error classification: `isChannelRiskControl` (code 11128 → actionable copy,
  request-shaped not account-level); prompt-too-long detection now covers bare
  413 (gateway body-limit rejections, HTML/empty, no envelope) plus an extended
  wording family (maximum context length / context window / too many tokens /
  输入过长…), aligned with qoder 0.8.11 chatSizeMarkers.
- Lifecycle guards: `reconcileAfterExecutorError` / `reconcileByUID` skip
  prompt-too-long bodies entirely — a 413 body that happens to carry
  "quota exceeded" wording could previously collide with hardCreditMarkers
  and mis-trigger the credits reconcile lifecycle.

## 0.9.14

### School-season automation + chat error-shape alignment (repo v0.12.56)

Second round of the wb2api upstream review (the panel repo absorbed
Sliverkiss/workbuddy2api master@76bb543 on 09-17 — "错误处理/WAF/调度/模型目录/
连接层" — and added the 开学季 activity family on 09-16). Only the pieces that
map onto the plugin's architecture are ported; pool scheduling / cost tiers /
static-catalog removal stay 2api-specific.

**School-season activity (`school.go`, CN only, time-boxed)** — the
开学季 loop the wb2api panel automates (activity window 2026-09-13 ~ 09-24,
gated at runtime by the API's own `in_period` flag, so post-window runs are
silent and unchanged). All endpoints live on the billing domain
(www.codebuddy.cn) under `/portal/activity/school` with the same account
Bearer + X-User-Id family as check-in (no web cookie needed):

- `GET /tasks` → task list + `in_period`; `POST /tasks/share-complete`
  {channel:"wechat"} → daily +100c +1 lottery chance (pure report, server
  does not verify a real share — same trust model as the growth activity
  report); `POST /tasks/{code}/viewed` → pending→in_progress activation
  (counting prerequisite for desktop_chat_1_time-style tasks, three-account
  verified upstream); `POST /tasks/{code}/claim` → reward + chance_granted;
  `GET /config` → chance balance; `POST /wheel/draw` {draw_uuid} → prize;
  `GET /vouchers` → third-party coupon list (KFC/瑞幸/酷狗…).
- Task-center integration: run loop gains step 9 (share → activate → claim →
  drain chances → vouchers recap); `GET /tasks` scan gains a school block
  (in_period/tasks/claimable/chances/vouchers) emitted ONLY while the
  activity is live; new read-only `GET /school/vouchers` endpoint (2 upstream
  calls per account vs the full scan's 5) backs the panel dialog.
- Panel: per-account 「券码」 button + toolbar 「开学季」 button opening a
  copy-friendly voucher dialog (prize name, monospace code with one-click
  copy + clipboard fallback, valid_to, expired highlighted).
- Deliberately NOT ported: the fabricated mini-program telemetry chain
  (chat_3_times / expert_use via forged `WorkBuddy_MP` /v2/report events) —
  forging a device fingerprint crosses our pure-API automation line.

**Chat error-shape alignment (from the 76bb543 sync)**:

- `policy.go` — 429 now precedes the balance word list (upstream fix
  "429+quota 措辞误硬冷却"): a 429 is soft throttling even when the body
  says quota/额度 (model-level limits reuse that wording); it no longer
  triggers the hard-credit disable/delete lifecycle. Real exhaustion is still
  caught by the periodic credits reconcile.
- `chat_error.go` — 11115 "prompt is too long" (400/404/413) now says
  "请求级问题，与账号无关，缩短上下文重试" instead of raw upstream JSON;
  bare-403-no-business-envelope (APISIX WAF page) says "风控拦截，降频/换网络";
  `Retry-After` / `Retry-After-Ms` / `X-RateLimit-Reset` headers are parsed
  (sanity-capped at 2h) and appended to every other chat failure as an
  upstream-suggested back-off hint. All three chat error sites (execute,
  stream pump, sync collect) now feed response headers into the translator.
- `payload.go` — `max_completion_tokens` → `max_tokens` rename in
  prepareUpstreamBody (OpenAI-newer clients; PR #116 semantics) so the
  gateway no longer sees an unknown parameter.
- `stream.go` — the non-stream aggregate drops tool calls whose arguments
  are non-empty but unparseable (stream cut mid-arguments → half a JSON
  string that would wedge the client's parser); clean calls pass through
  untouched, and finish_reason (often "length") explains the drop. The
  emit-as-you-go stream path cannot retract chunks and is unchanged.

## 0.9.13

### Growth-center task loop (repo v0.12.55)

Task 17 implementation — the CN growth center (成长中心) the wb2api panel
automates since mid-September, ported to the plugin's host-bridge plumbing
(`growth.go` client + `taskcenter.go` orchestration, endpoints verified
upstream by the wb2api project):

- **Task center API**: `GET /v0/management/plugins/workbuddy/tasks` — read-only
  scan (streak days, makeup cards, travel state, claimable tasks per account);
  `POST .../tasks/run` — run the daily loop for one account (`auth_index`) or
  every CN account (sem=4, per-account checkin lock reuse).
- **Daily loop order matters** (each step best-effort, failures logged into the
  per-account summary): activity report (lights streak + unlocks first_buddy)
  → makeup card for yesterday → gift/compensation → accept pending tasks
  (upstream counts progress only for accepted tasks) → buddy travel state
  machine (adopt / depart location 4 / claim by record_id) → streak tier
  redeem (7d/14d/28d, 403 = locked skip) → lottery drain → claim every
  claimable task reward.
- **Three upstream domains**: growth endpoints live on copilot.tencent.com
  (tasks carry a /v2 prefix, travel/streak do not); the chat-activity report
  and gift/compensation claims on www.codebuddy.cn; and the task reward claim
  ONLY on www.workbuddy.cn with web Origin/Referer + x-client-platform: web —
  the same path on the CLI domain 400s ("task not completed").
- **Realm gating**: CN accounts only (global has no growth center upstream;
  intl has never exposed one — wb2api gates global out with the same
  reasoning). Non-CN accounts show a clean skip in scan/run results.
- **Scheduler ride-along**: the loop runs after each auto check-in tick
  (09:00/21:00) when `tasks_auto` is on (default true, new plugin config
  field); manual run via the panel buttons works regardless.
- **Panel**: 全部任务 toolbar button + per-CN-account 任务 button with result
  toasts; reward lines (+N 分) aggregated across accounts in the batch toast.
- Report event shape keeps the full client telemetry field list with userId
  (missing userId = upstream 200 but silent drop, per wb2api REPORT notes);
  no retry on report (day-idempotent, blind resend would skew counters).


## 0.9.12

### /v3/config dual-probe discovery + capability surfacing + non-chat model filter (repo v0.12.51)

Third leg of the reference-implementation alignment
(`linguo2625469/workbuddy2api-panel`, `iJetLi/deepseek-harness-codearts`),
following 0.9.10 (status-0 wire fix) and 0.9.11 (modality advertisement):

- **Dual-probe discovery** (`models.go`): the enterprise endpoint
  (`/console/enterprises/personal/models`, ordering authority: cli agent
  base + promotions) now runs CONCURRENTLY with a `/v3/config` probe —
  the official IDE configuration catalog that needs the
  `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` UA plus
  `X-Domain`/`X-Product: SaaS`/`X-User-Id`/`X-CodeBuddy-Request: 1`
  headers. v3 carries the full capability table (real context windows,
  effort levels) and family models the enterprise table lacks
  (gpt-5.3-codex etc. on global). Merge key = model id: enterprise keeps
  ordering, v3 fills capability gaps and appends its own extras. Either
  probe failing alone degrades to the other (warn logged); double failure
  reports BOTH reasons in the panel's failure line.
- **Capability surfacing**: discovery entries now also populate
  `ContextLength`/`InputTokenLimit`, `MaxCompletionTokens`/
  `OutputTokenLimit` (both endpoint generations' field names:
  `contextWindow`/`maxTokens` AND `maxInputTokens`/`maxOutputTokens`),
  and `Thinking.Levels`/`Thinking.ZeroAllowed` from
  `reasoning.supportedEfforts`/`canDisableThinking`. The 0.9.11 modality
  rule is unchanged and applies to v3 entries too.
- **nonChatModel filter** (harness `isChatModel` parity): entries with id
  prefix `nes-`/`completion-`/`codewise-`, `supportsExtra:true`, output
  cap <= 256, or a `text-to-image` tag never reach the selectable list —
  selecting them dies with upstream 11102/11133. This closes the v0.9.8
  promotion hole where such an entry could be promoted into the chat list
  (the promotion loop now filters the same classes).
- **X-User-Id on the v3 probe**: the account uid is extracted from the
  stored auth blob (`extractAccountUID`) and sent as the IDE clients do;
  empty = header omitted.

Regression locks: `models_v3_test.go` (nonChatModel matrix, promotion
filter, capability mapping, merge/overlay, uid extraction, per-realm v3
endpoints/domains).

## 0.9.11

### Image-input capability advertisement + image-forwarding audit (repo v0.12.50)

User question: "workbuddy/trae 不能转发图片？是模型的问题还是我们没做转发？"
Audit conclusion across the whole chain (host → plugin → upstream), cross-checked
against the two reference implementations the user pointed at
(diegosouzapw/OmniRoute, jlcodes99/cockpit-tools):

- **workbuddy**: images were NEVER stripped. The host's claude→openai
  translation emits `image_url` parts, and the plugin's payload rewriter
  (`rewriteContentField`) only touches `text` parts — image parts pass
  through verbatim to `/v2/chat/completions`. OmniRoute's codebuddy-cn
  executor behaves identically and its catalog marks most models
  `supportsVision: true`.
- **trae intl** (`flattenQuery`): the reverse-engineered `chat_sessions`
  protocol is text-only — the `query` block has no image shape, so image
  parts are dropped during flattening. OmniRoute's TraeExecutor flattens
  the exact same way (text extraction only); this is an upstream protocol
  limitation, not a plugin regression.
- **trae CN/SOLO**: array content passes through untested ("保守透传"); the
  official client's image-block shape for `llm_utils_chat` is unknown.

What 0.9.11 ships: discovery entries with upstream's own
`supportsImages && !disabledMultimodal` flags (Tencent's registration table —
direct upstream evidence) now advertise
`SupportedInputModalities: ["text","image"]` so modality-aware clients can
offer/hide image attachments per model instead of guessing. Static catalogs
stay un-declared. Regression-locked in `TestModelsFromDiscoveryModalityFlags`.

Also: qoder's `host_bridge.go` had the same latent tag-less-wire decode bug
that 0.9.10 fixed for workbuddy (its `>=400` checks never fired on the
phantom 0, so it went unnoticed) — same dual-key decoder applied, qoder
0.8.9.

## 0.9.10

### Fix bridged HTTP status decode — dynamic discovery always saw "status 0" (repo v0.12.49)

Root cause of the user-visible `发现失败: models API status 0` on BOTH realms
with a healthy upstream: the host http bridge marshals the tag-less
`pluginapi.HTTPResponse` struct, so the status rides as PascalCase
`"StatusCode"`. The plugin decoded only `"status_code"` — Go's
case-insensitive field match rescued `Headers`/`Body` but NOT `StatusCode`
(underscore vs no underscore), so every bridged response decoded with status
0 and `callModelsAPI` treated even a genuine upstream 200 as a failure and
fell back to the static catalog. Chat (stream path) and billing (lenient
`>= 400` checks + body parsing) were unaffected, which is exactly why only
model discovery appeared broken. Confirmed against the official wire: the
console models endpoint exists on all three domains and answers Bearer auth
(401 on a bogus token, 302 login redirect without one), while the v2 gateway
has NO models route — the console endpoint remains the only dynamic source,
as also evidenced by upstream tools (OmniRoute ships a static model table;
cockpit-tools only does auth/billing).

- **Dual-shape decode** (`host_bridge.go`): `hostHTTPDo` now decodes both
  `"status_code"` (documented) and `"StatusCode"` (tag-less marshal via
  `decodeHostHTTPDoResult`). A genuine status 0 — HTTP has no such status —
  falls back to one direct request so callers see the real status or the real
  transport error instead of a bogus 0.
- **Richer discovery errors** (`models.go`): non-200 now reports
  `models API status %d from <url>: <body snippet>` (control chars stripped,
  200-byte cap) — a login redirect, auth wall, or server fault is visible
  from the panel hover instead of a bare code.
- **UA refresh** (`main.go`): `CLI/2.63.2 CodeBuddy/2.63.2` →
  `CLI/2.108.1 CodeBuddy/2.108.1`, matching the current official CLI
  (parity with OmniRoute's constant; the gateway rejects missing/stale
  client identification with 403/code 10085).
- Tests: dual-shape decode (PascalCase / lowercase / absent / malformed),
  direct-path status surfacing over httptest.

## 0.9.9

### Model-source diagnostics: the model list now explains itself (repo v0.12.48)

Follow-up to 0.9.8 (v0.12.47). Two user questions — "why didn't dynamic
discovery pick up the new model" and "why does the codebuddy intl realm only
show hy4" — had the same blind spot: the discovery-to-static-fallback
decision was completely invisible, so a realm stuck on its (thin) static
catalog was indistinguishable from discovery legitimately serving a short
list.

- **Per-realm diagnostics trail** (`models.go` / `main.go`): the realm cache
  entry now records WHERE the advertised list came from (`discovery` / `pin`
  / `static`), when it was fetched, and the last discovery failure reason.
- **Throttled failure logging**: a discovery failure logs
  `models: realm=<r> discovery failed (<reason>) — serving static catalog
  (N model(s))…` immediately and at most once per minute thereafter
  (`model.for_auth` can fire per models query; the old code swallowed the
  error entirely).
- **Panel "模型" row** (`panel.go` / `panel.html`): each account card shows
  the realm's model source — `动态发现 N 个模型 · X分钟前`,
  `配置钉住 N 个模型`, or `静态兜底 N 个模型 · 发现失败: <reason>`
  (hover for the full reason). A stale `models_cn/models_intl/models_global`
  pin — which silently overrides discovery for the realm — is now visible
  too.
- No change to model resolution itself: the
  pin → discovery (5-minute cache) → static-catalog chain is identical to
  0.9.6–0.9.8.
- Tests: discovery-failure state recording + reason surfacing; discovery
  success clearing the failure; pin short-circuit (discovery must not be
  called) + pin state recording.

## 0.9.8

### Promote new upstream models beyond the cli agent list + DeepSeek V4.1 Flash (repo v0.12.47)

- **Discovery promotion** (`models.go`): the cli agent's model IDs stay the
  ordered base, but any ENABLED `data.models` entry missing from that list is
  now PROMOTED (and logged) instead of silently dropped. Tencent adds new
  models to `data.models` while the cli agent list lags — the exact shape of
  the deepseek-v4.1-flash rollout on 2026-09-10 — and the old cli-only filter
  made the plugin trail the official client on every launch.
- **cli agent missing/renamed no longer hard-errors** discovery: enabled
  `data.models` alone still produce the list (before: error → stale static
  fallback).
- **`rawJSONI64`**: `contextWindow` / `maxTokens` now tolerate number,
  numeric-string and null shapes.
- **CN static fallback** gains `deepseek-v4.1-flash` (1M context; official
  DeepSeek launch-partner announcement, same evidence bar as hy4-preview).
  Intl/Global catalogs unchanged (no direct upstream evidence yet).
- Tests (`models_discovery_test.go`): cli base order, disabled skip,
  promotion, cli-missing resilience, rawJSONI64 matrix.

## 0.9.7

### Discovery-first model output made explicit (repo v0.12.20)

- **Config descriptions for `models_cn` / `models_global` / `models_intl`
  rewritten**: "Leave empty (recommended)" now leads. An empty pin IS the
  dynamic contract - each credential advertises exactly the model list its own
  upstream token returns (per-realm discovery endpoint, 5-minute cache,
  disabled models filtered), so upstream-unknown models are never advertised
  and can never be routed there. Pins stay as a manual override for edge
  cases; filling one with another realm's IDs is the one way to reintroduce
  the 11102 wrong-routing class, and the config UI now warns about that.
- No behavior change; model resolution is identical to 0.9.6 (repo v0.12.19).

## 0.9.6

### Per-realm static catalogs + user-pinnable credential model output (repo v0.12.19)

- **Per-realm static model catalogs** (`models.go`): the shared CN-flavored
  fallback (`wbModels`) no longer answers `model.for_auth` for Intl/Global
  credentials. A false negative (supported model missing from the fallback)
  heals via dynamic discovery or the new pins; a false positive (advertised
  but not registered upstream) is a hard `400 code 11102 "model [...]
  service info not found"`, so `staticModelsIntl` / `staticModelsGlobal`
  carry only models with direct upstream evidence (`hy4-preview` — Tencent's
  2026-08-28 Hy4 Preview launch covers CN AND international editions), and
  CN brand models (`deepseek-v4-*`, `glm-*`, `kimi-*`, `minimax-*`, `hy3*`)
  stay out of them. Community-observed intl claude/gpt/gemini IDs are
  deliberately NOT hardcoded: their exact upstream IDs could not be verified
  without a live intl token.
- **`models_cn` / `models_global` / `models_intl` config pins** (the
  "把支持哪些模型写进凭证产出" contract): comma-separated upstream model IDs
  per realm; when set, the credential's model output is exactly that list and
  dynamic discovery is skipped for the realm (deterministic, no 15s discovery
  latency). Missing key on reconfigure resets the realm to
  discovery + static fallback. Known IDs reuse static-catalog metadata;
  unknown IDs get generic metadata (the user pinned them deliberately).
- **Fallback chain** for every credential is now
  pinned config → per-realm discovery (realm-keyed cache, v0.12.18) →
  per-realm static catalog.
- **11102 error hint** (`chat_error.go`): the static-catalog branch of the
  bilingual hint now renders the account's OWN realm catalog (labeled
  "static INTL/GLOBAL/CN catalog (known-good for this realm …)") instead of
  the old CN list with a "may not match this realm" disclaimer.
- Tests (`models_realm_test.go`): realm catalogs may not contain CN-only
  models; pinned-config parsing (quoting / YAML flow lists / dedup);
  config_yaml envelope → realm pins end-to-end; pins win and skip discovery;
  discovery failure falls back to the realm's own catalog; hint rendering
  per realm.

## 0.9.5

### Realm-aware model discovery + 11102 actionable error (repo v0.12.18)

- **Intl chat 400 `code 11102 "model [...] service info not found"`** (user
  report on the intl credential): chat routing itself was correct (Intl tokens
  hit codebuddy.ai), but model DISCOVERY was not — `callModelsAPI` only
  special-cased Global and sent Intl tokens to the CN endpoint
  (copilot.tencent.com), whose answer (or the CN-flavored static fallback)
  then advertised CN-only models such as `deepseek-v4-flash` to Intl accounts.
  The Intl gateway rejects those with 11102.
  - `modelsEndpointFor(realm)`: per-realm discovery URL + Origin/Referer —
    cn → copilot.tencent.com, global → workbuddy.ai, intl → codebuddy.ai
    (with the IDE header set, parity with `applyRealmHeaders`).
  - `realmForStorage`: classifies the auth blob (nested/flat domain, region
    field, JWT-iss fallback) into cn|global|intl.
  - `dynamicModelsCache` re-keyed per realm — a single shared entry let one
    realm's answer satisfy `model.for_auth` for accounts on another realm.
- **11102 → bilingual actionable error** (`chat_error.go`): the executor
  error paths (non-stream, sync stream, async pump) now detect the 11102
  model-catalog rejection and rewrite it into a message naming the realm and
  its best-known model catalog (cached realm discovery; the static fallback is
  explicitly labeled as CN-flavored and possibly wrong). Non-11102 failures
  keep the historical `upstream <status>: <payload>` shape.
- Tests: `models_realm_test.go` (endpoint routing, storage classification,
  per-realm cache isolation, hint rendering, 11102 rewrite + passthrough
  guard). Verified live: 401 passthrough shape unchanged, `/v1/models`
  unaffected, upstream probes (codebuddy.ai answers 401 auth-first with an
  invalid token — the user's 400/11102 proves the request reached the model
  catalog layer with a valid one).

## 0.9.4

### Intl billing gateway fix + single OAuth entry (repo v0.12.10)

- **Intl 401 fix** (`billing.go`): a successful Intl (codebuddy.ai) login was
  immediately followed by `parse failed: invalid character '<' (body: <html>
  ... 401 Authorization Required ...)` in the panel. Root cause:
  `billingBaseFor` routed check-in / meter calls for Intl accounts to the CN
  gas station (`www.codebuddy.cn`), whose APISIX gateway rejects Intl Bearer
  tokens with an HTML 401 page. Intl accounts now hit
  `billingBaseIntl = https://www.codebuddy.ai` (verified: the meter endpoints
  exist there and answer with business JSON for valid tokens), and
  `billingHeaders` applies the Intl IDE client header set + drops
  `X-Requested-With` (parity with `applyRealmHeaders`).
- **Single OAuth entry restored**: the v0.12.10 per-region login-page menus
  (CN 登录 / Intl 登录) are removed per user feedback — the plugin is back to
  one OAuth entry whose realm follows the `login_region` config dropdown
  (STICKY). `startLoginWithRegion` stays as the pinned-region helper.
- Global (workbuddy.ai) accounts unchanged: panel import only (no upstream
  OAuth flow).

## 0.9.3

### Per-region self-serve login pages (repo v0.12.10)

- New `login_pages.go` — the management UI sidebar gains **CN 登录** /
  **Intl 登录** pages (plugin-declared menus, rendered in an iframe). Each
  page starts a region-PINNED login flow (`login_start?region=…`), opens the
  upstream authorization URL in a new tab and polls `login_wait` until the
  flow completes, then persists the credential via `host.auth.save`. CN and
  Intl logins can now run concurrently without touching the global
  `login_region` config — which previously allowed only one region at a
  time and therefore only one OAuth menu entry per plugin.
- `oauth.go` — `handleStartLogin` splits into a thin wrapper plus
  `startLoginWithRegion(raw, region)`: the host RPC keeps using the global
  `login_region`; the login pages pin their own realm. The poll handler is
  reused verbatim by `login_wait` (no duplicated token logic).
- Isolation: `login_wait` only consumes states it created itself
  (`selfServeStates`), so it can never race the host-driven poller; terminal
  results are cached per state and concurrent page retries are coalesced.
- Panel menu renamed `WorkBuddy` → `Dashboard` (the UI groups multiple
  plugin menus into a drawer, where the old label was redundant).
- Global (workbuddy.ai) accounts unchanged: no OAuth flow exists upstream,
  they keep entering via panel import (noted on the Intl login page).

## 0.9.2

### Foreign-credential defense (repo v0.12.9)

- `credits_handler.go` — `handleImportAuth` now rejects payloads whose
  explicit `type`/`provider` names another plugin (e.g. a qoder or trae
  auth file). parseStored only requires an accessToken, so foreign
  credentials used to import cleanly and then 401 against the wrong
  upstream forever (APISIX HTML page surfaced as 'parse failed: invalid
  character <'). The rejection names the owning plugin. Untyped flat
  exports (legacy CPA-Manager-Plus files) keep importing.
- `host_auth.go` — `hostAuthList` gains a content guard: a file with our
  filename prefix but an explicit foreign type in its body (e.g. a qoder
  auth saved under a workbuddy- name by a third-party tool) is skipped
  with a log line instead of being listed/executed by this plugin.
  Entries without a type field stay eligible — the filename prefix
  remains their only discriminator.
- `main.go` — parse ownership guard hardened: the host rewrites an empty
  req.Provider to the POLLED plugin's own identifier, so the old
  EqualFold(req.Provider, ...) check was always true while being polled —
  the first-polled plugin (qoder) claimed every type-less generic
  credential on disk. Type-less files are now claimed only by filename
  family; declared legacy types (codebuddy/codebuddy-cn/codebuddy-intl)
  remain accepted. Symmetric fixes ship in qoder 0.8.4 (same hole) and
  trae 0.12.8 (guard was missing entirely).


## 0.8.2

### Concurrency + lifecycle hardening

- `lifecycle.go` — P0-2: `reconcileOneAccount` now routes credits fetch
  through `cachedAccountDetails(force=true)` so singleflight serializes
  concurrent writers, eliminating a Load→Store race that could clobber
  newer plan/checkin values.
- `lifecycle.go` — P1-4: Global `lifecycleDelete` now requires a second
  `fetchUserResource` confirmation before deleting. Prevents transient 402
  from irreversibly removing an account.
- `checkin.go` — P1-5: after a successful checkin the credits cache is
  refreshed immediately (was only updating the checkin field). Panel now
  shows updated balance without waiting for the async reconcile pass.
- `cache.go` — P1-1 documented trade-off: force=true callers still join
  singleflight (skipping would re-introduce P0-2).
- `main.go` — P0-5: `scheduler_mode` ConfigField description now warns that
  `off + lifecycle_auto=false` leaves exhausted accounts routable.

## 0.8.1

### Bug fixes + compliance polish

- `keepalive.go` (new) — daily 22:00 access-token refresh to prevent Keycloak
  offline-session expiry; reuses `schedulerLoop`, routes via `host.http.do`,
  uses CPA native `disabled` field for session-dead auths.
- `models.go` — fix `filterExcludedModels` slice aliasing that corrupted
  `dynamicModelsCache` (P0).
- `billing.go` — route all billing API calls through `hostHTTPDo` (was missed
  in v0.7.0); improve "parse failed" error to include a redacted body snippet.
- `checkin.go` — avoid double `fetchCheckinStatus` in classify already-branch.
- `billing.go` — `performCheckinCall` now sets `success=true` as bool to avoid
  downstream type-mismatch when upstream returns a string.
- `host_auth.go` — fresh slice in `hostAuthList` to avoid aliasing RPC response.
- `oauth.go` — route `handleRefreshAuth` via `hostHTTPDo` (last path still on
  `sharedHTTPClient()`); make OAuth error messages actionable.

## 0.8.0

### Refactor — community-grade file layout

完成 v0.7.0 合规改造后的代码组织大重构，把两个超大主档拆成单一职责的
小文件，对齐 CPA 原生 plugin 案例的"一个能力一个文件"原则。

**File splits (main.go 2940 → 809, management.go 2263 → 349, lifecycle.go 980 → 535)：**

- `redact.go` (49) — redactSecrets + 4 个 regex + truncate
- `usage.go` (242) — handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector
- `payload.go` (469) — prepareUpstreamBody + 4 个 InPlace mutator + 4 个 legacy 包装
- `stream.go` (452) — streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
- `models.go` (443) — callModelsAPI + fetchDynamicModels + resolveUpstreamModel + alias 反解
- `oauth.go` (240) — handleStartLogin/PollLogin/RefreshAuth + newLoginClient + doJSON
- `host_bridge.go` (388) — hostHTTPDo/DoStream/Read/Close + hostStreamReader + Direct fallbacks
- `billing.go` (486) — billing API + fetch* + perform* + JSON helpers
- `cache.go` (183) — accountCache + accountDetailFlight singleflight + prune
- `host_auth.go` (73) — hostAuthList/Get/GetBundle (host auth-store RPC)
- `usage_config.go` (202) — configure + resolveUsageReport + probe* + config vars
- `checkin.go` (515) — handleManualCheckin + runAutoCheckin + schedulerLoop + classify/execute/summarize
- `credits_handler.go` (285) — handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery
- `panel.go` (266) — buildDashboardEx + summarizeCredits + servePanel + panelHTML
- `policy.go` (188) — lifecycleAction decisions + displayNote + labelForAuth
- `authfile.go` (299) — authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth + path safety

**保留的小文件**：`scheduler.go` (138)、`active_auth.go` (158) — 本来就够小。

**文档（社区标准）：**

- `README.md` — 英文版，Features / Quickstart / Configuration / Lifecycle / Development / License
- `README_CN.md` — 中文版
- `LICENSE` — MIT
- `Makefile` — build / test / lint / clean / release / tag 目标
- `.gitignore` — 忽略 `*.so` / `*.h` / `bin/` / `dist/`
- `docs/architecture.md` — 模块图 + 数据流 + 关键设计决策 + 与 CPA 的集成点
- `docs/development.md` — 本地构建 / 测试 / 调试 / 发布流程
- `docs/definition-of-done.md` — v0.8.0 验收标准（量化可测）

### Lint / style

- `gofmt -l .` → 0 files
- `go vet ./...` → 0 issues
- `gocritic check ./...` → 0 issues（修复 policy.go 的 ifElseChain）
- `staticcheck` 真实代码问题 0（工具链版本噪音已过滤）

### Bug Fixes (carried over from v0.6.31 / v0.7.0)

本次重构完整保留了之前所有 bug 修复：
- UID 路径穿越白名单（authfile.go sanitizeUIDForFileName）
- refresh_token 不再泄露到 chat 上游（main.go backendHeaders）
- invalidateAccountCredits 数据竞争修复（值拷贝）
- handleManualCheckin early-already merge（不丢 credits/plan）
- configure 嵌套锁修复（parse-then-lock）
- scheduler_mode off 接通（handleSchedulerPick 读取配置）
- deleteAuth 调 clearActiveAuthIfMatch
- runAutoCheckin 串行改并发（sem=4）
- cachedAccountDetails singleflight
- panel.html XSS 修复（addEventListener + dataset）
- panel.html CSRF（fetch credentials:omit）
- redactSecrets 裸 JWT 兜底
- pumpUpstreamStream context cancel
- out[:0] 共享底层数组改新 slice
- 热路径 4 次 JSON 序列化合并为 1 次
- 冒泡排序改 sort.Slice
- usageReportConfigured/buildDashboard 死代码删除
- handleManualCheckin 三段拆分（classify/execute/summarize）
- management BasePath 缓存（register 时读取宿主注入）

### Tests

- 115/115 tests pass (`go test -race`)
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为

## 0.7.0

### Compliance — CPA native patterns
本次大版本把「自建通道」全部替换为 CPA 官方提供的 RPC / 能力接口，
对齐 `sdk/pluginapi` 的设计意图。生产路径 100% 走宿主桥接，插件不再
绕过宿主审计 / request-log / transport policy。

- **所有上游 HTTP 调用走 `host.http.do` / `host.http.do_stream`**：
  - `models API`、`billing API`、`usage 上报`、`chat completions`（流式 + 非流式）
    全部从 `sharedHTTPClient().Do` 切到 `hostHTTPDo` / `hostHTTPDoStream`。
  - 宿主 request-log 现在能捕获插件的出站请求和原始响应（之前完全看不到）。
  - 宿主 transport policy（proxy、超时、连接池）对插件上游调用生效。
  - `sharedHTTPClient` 降级为 fallback 专用：仅当宿主桥不可用（单元测试 /
    老版本 CPA）时使用。新代码直接调用 `sharedHTTPClient` 视为合规 bug。
- **`hostStreamReader` 适配层**：把宿主桥的 32KB 任意字节块适配为 `io.Reader`，
  `bufio.Scanner` 的 SSE 行切分逻辑不变，pump / collect / aggregate 全部透明迁移。
- **`UsagePlugin` 能力声明 + `handleUsage` RPC handler**：
  - 注册能力 `usage_plugin: true`，宿主每次请求完成后会把规范化的
    `pluginapi.UsageRecord` 推送给插件。
  - 插件在 `handleUsage` 里把 record 转发到 CPAMP，与宿主 `DefaultManager`
    的记录并行，不再重复也不遗漏。
  - 旧路径 `publishUsage` 保留向后兼容（老版本 CPA 没接 UsagePlugin 时仍可
    上报），新路径 `handleUsage` 同步触发，CPAMP 侧基于 (timestamp + auth +
    model + total_tokens) 幂等去重。
- **`reportUsageToCPAMP` 重命名为 `forwardUsageToCPAMP` 并走 host.http.do**：
  CPAMP 上报自身也走宿主桥，宿主能看到插件的运维流量。

### Architecture notes
- `hostBridgeAvailable()` 检查 `hostAPI.call` 是否为 nil，统一决定是否
  fallback。生产环境永远为 true，单元测试永远为 false（无宿主）。
- 所有 `*Direct` 函数仅服务测试；生产路径不经过。
- 宿主侧 `sanitizePluginRequest` 会把 `ExecutorRequest.HTTPClient` 置 nil
  （跨 c-shared 边界接口无法传输），所以**插件不可能用宿主注入的
  HTTPClient**——`host.http.*` RPC 是 c-shared 插件访问宿主 transport 的
  唯一合规方式，本版本全部采用。

## 0.6.31

### Security
- **UID 路径穿越修复**：`authFileNameFor` 新增 `sanitizeUIDForFileName` 白名单
  （`[^a-zA-Z0-9_-]+` → `_`、长度 ≤64、拒绝 `.`/`..`），导入凭证的
  `workbuddy-<uid>.json` 不再可能被 `../` 注入到任意路径。
- **refresh_token 停止泄露到 chat 上游**：`backendHeaders` 移除
  `X-Refresh-Token`。refresh_token 是长期凭证，只在 refresh 端点用；之前每次
  chat completion 都附带它，上游日志一旦记录请求头即等同账号被盗。
- **插件层 management 鉴权 + 限流**：`handleManagement` 入口对所有 POST /
  写端点新增插件层防护：constant-time Bearer 比对（`crypto/subtle`），
  per-IP token-bucket 限流（容量 5、每 6s 1 个）。配置方式：
  `config_yaml management_key:` 或 env `WB_MANAGEMENT_KEY`。空则保持
  历史行为（仅依赖宿主鉴权）。
- **panel.html XSS 修复**：4 处 `onclick="...('${esc(auth_index)}',this)"`
  改为 `data-action` + `data-auth-index` + `addEventListener`。`esc()` 只
  转义 HTML 不防 JS 字符串上下文注入。
- **panel.html CSRF 缓解**：`fetch` 显式 `credentials:'omit'`，面板纯靠
  Authorization Bearer，不再隐式带 cookie。
- **redactSecrets 兜底裸 JWT**：新增 `redactREJWTLoose` 正则，匹配不带
  `Bearer` 前缀、`access_token` key 的 `eyJ…` 两段/三段 JWT。

### Bug Fixes
- `invalidateAccountCredits` 数据竞争：直接改 sync.Map 共享 entry 的字段
  （`e.credits = nil`），并发 dashboard / reconcile / chat 后置 invalidate
  会拿到撕裂状态。改为 `fresh := *e; Store(&fresh)` 值拷贝，与其他 4 处
  写法一致。
- `handleManualCheckin` "early already" 路径丢 credits/plan：直接构造
  `accountCacheEntry{checkin: ci}` 覆盖整个 entry，签到后面板积分消失。
  改为 merge prev 的 credits/plan。
- `configure` 嵌套锁：在 `checkinAutoMu` 内嵌套获取 `lifecycleAutoMu` /
  `schedulerModeMu`，未来加反向获取路径即死锁。改为两阶段：无锁解析到
  局部变量，再分别单锁写入。
- `scheduler_mode: off` 配置断链：configure 解析但 `handleSchedulerPick`
  从不读取，"off" 实际表现为 "credits"。现在 off 正确 defer 给内置 scheduler。
- 删除 Global 账号后 `activeAuthID` 残留指向已删 ID：`deleteAuth` 两个成功
  路径现在都调 `clearActiveAuthIfMatch(authID)`。
- `runAutoCheckin` 重复 `fetchCheckinStatus` + 变量 shadow：原代码内层
  `ci` shadow 外层，且第二次调用与第一次状态可能不一致。改为单次调用，
  签到成功才 refresh。
- `out[:0]` 共享底层数组：`filtered := out[:0]` 复用底层数组在 range 中
  写入，改为 `make([]wbAccount, 0, len(out))`。
- `pumpUpstreamStream` 无 context：`http.NewRequest` 无 context，客户端
  断开后 goroutine 一直读到 120s 超时。改为 `NewRequestWithContext` +
  cancel 传入 pump，所有退出路径释放。

### Performance
- **热路径 4 次 JSON 序列化合并为 1 次**：新增 `prepareUpstreamBody` 统一
  `forceStreamBody` + `normalizeToolsForUpstream` + `rewriteSystemForUpstream`
  + `ensureSystemMessage` + `rewriteModelInBody`，单次 unmarshal + 单次
  marshal。每次 chat completion 省 4-5 个 JSON 往返。
- **`runAutoCheckin` 串行改并发**：抽出 `processAutoCheckinAccount`，主循环
  `sem=4` 并发。N 账号从 3N 串行 HTTP 降到并发 4 路。
- **`cachedAccountDetails` 加 singleflight**：per-authID `sync.Map` + done
  channel。并发 dashboard / reconcile 对同一账号只跑 1 次上游 fetch，
  其他 goroutine 等结果，消除 6x upstream QPS + last-writer-wins。
- **冒泡排序改 sort.Slice**：`pruneAccountCacheSoftCap` 从 O(n²) 降到 O(n log n)。

### Refactor
- **handleManualCheckin 273 行拆分**：`classifyCheckinTargets` /
  `executeCheckinBatch` / `summarizeCheckinResults` 三段独立函数，各自
  单一职责，便于单测。
- **management BasePath 不再硬编码**：register 时缓存宿主注入的 BasePath，
  handleManagement 用 cached 值。宿主未来版本化路径不会失效。
- 死代码清理：删 `upstreamBase` legacy 常量、`usageReportConfigured` 无人
  调用、`buildDashboard` 包装函数。

### Tests
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为。
- 全套 115 tests + `-race` 通过。

## 0.6.29

### Fixed
- 修复签到后按钮不变"已签到"、套餐标记丢失的问题
  根因：handleManualCheckin/runAutoCheckin/handleClaimTrial 在签到/领取成功后
  accountCache.Delete(f.ID) 把 cache 清了，light load 时 checkin/plan 是 nil。
  handleCreditsQuery 的 cache merge 逻辑从 prev.plan（空）取值而不是用刚获取的
  fetchPaymentType(sa) 结果，导致 plan 在 light load 后丢失。
  修复：签到/领取成功后把 checkinSummary 存回 cache 而不是删除；
  handleCreditsQuery cache merge 用刚获取的 plan；runAutoCheckin/handleClaimTrial
  改为 invalidate credits（置 nil）而不是删除整个 cache entry。

## 0.6.28

### Fixed
- 修复面板选中卡片与实际路由账号不一致的根本问题
  根因：activeAuthID 存的是 auth.Index（运行时 SHA256 hash），但 scheduler
  的 SchedulerAuthCandidate.ID 是 auth.ID（持久化 UUID），两者永远不匹配，
  导致 pickActiveAuth 永远走 fallback 选第一个，面板显示选中第一个但实际
  路由到别的账号。同时 cachedCreditsScore 用 auth.ID 查 accountCache（key
  是 auth.Index）也查不到，exhausted 判断也坏了。
  修复：全链路统一用 auth.ID — activeAuthID、accountCache key、
  lifecycleState key、面板 selected 判断、/select API 返回值全部改用
  auth.ID。lifecycle 函数（reconcileOneAccount/disableAuth/reenableAuth/
  deleteAuth/syncAuthNote）加 authID 参数，resolveAuthIndex 改为
  resolveAuthIndexAndID 同时返回 index+ID。
- 修复首次加载面板时选中耗尽账号的问题
  首次 GET /accounts 不拉 credits（fetchCredits=false），所有卡片
  Exhausted=false，ensureDefaultActiveAuth 选第一个。lazyLoadCredits
  异步获取积分后发现第一个已耗尽，但选中状态不会更新。
  修复：lazyLoadCredits 全部完成后前端静默再拉一次 /accounts（此时
  cache 已有 credits，light load 能拿到正确 exhausted 和 selected），
  重新渲染卡片。

## 0.6.27

### Fixed
- ensureDefaultActiveAuth 也检查 Exhausted：面板刷新时选中账号已耗尽会同步切换
  修复 scheduler.pick 切了但面板 ensureDefaultActiveAuth 又选回去的 race
  现在 pickActiveAuth 和 ensureDefaultActiveAuth 用同一套规则，选中状态不会漂移

## 0.6.26

### Fixed
- 选中账号积分耗尽时自动切换到第一个可用账号，并同步更新选中状态
  全部耗尽时留在当前账号不 flip-flop
  修复 v0.6.25 过度 sticky 导致耗尽后一直报错的问题

## 0.6.25

### Fixed
- 选中账号 sticky：scheduler 不会因缓存过期/积分耗尽自动切换到别的账号
  只有 host 把选中账号从候选列表移除（disabled/deleted）才切换
  修复面板显示选中A但实际路由到B、静默消耗积分的问题

## 0.6.24

### Fixed
- model.static / model.for_auth 现在尊重 CPA 的 oauth-excluded-models 配置
  在 config.yaml 的 oauth-excluded-models.workbuddy 里列出的模型不再出现在 /models

## 0.6.23

### Fixed
- usage import URL 自动探测：先试 127.0.0.1:18317（裸机/Docker host），再试 Docker 服务名 cpa-manager-plus:18317
  不再写死 Docker 服务名，裸机安装也能自动找到 CPAMP

## 0.6.22

### Fixed
- ExecutorModelScope 改为 OAuth：插件只处理 workbuddy auth 绑定的模型
  不再拦截其他 openai-compatible 供应商的同名裸模型（如 deepseek-v4-flash、glm-5.2）
  修复启用 workbuddy 后自定义供应商模型请求不进监控的问题

## 0.6.21

### Fixed
- 积分懒加载改为并发：所有卡片同时请求，不再逐个排队

## 0.6.20

### Fixed
- 懒加载积分时同时拉取 plan（套餐类型），修复 plan 徽章显示「-」不更新

## 0.6.19

### Added
- 每张卡片新增「刷新」按钮：单独查询积分并即时更新该卡

## 0.6.18

### Added
- 积分懒加载：进页面先渲染骨架卡（加载中…），逐卡异步拉积分，失败自动重试一次
- 后端 `/accounts` 默认不再并发拉所有账号 credits（避免上游 500）
- `/credits?auth_index=` 单账号查询返回完整字段（region/exhausted/trial_claimed）

### Fixed
- 缓存有效时仍返回缓存的 credits，不再触发上游请求

## 0.6.17

### Fixed
- 流式路径也强制 `stream:true`：WorkBuddy API 现仅支持 stream 模式，`stream:false` 会报 "Non-stream chat request is currently not supported"

## 0.6.16

### Fixed
- 夜间模式：用量汇总卡与账号卡统一 `--card` 底色；内部指标格改用 `--surface`，避免汇总卡看起来更深/发黑

## 0.6.15

### Added
- 面板「选用」账号：默认第一张可用卡；选中卡决定 CN/Global 路由（读 domain，不解码 JWT）
- 选中账号耗尽/禁用/消失时随机切换下一张可用卡并记住

### Changed
- scheduler.pick 改为始终跟随 active 选中账号（不再依赖 credits 排行模式）

## 0.6.14

### Fixed
- Global 账号聊天 401/400 修复：JWT iss=workbuddy.ai 必须走 www.workbuddy.ai 端点（copilot.tencent.com 会对 Global token 返回 401）
- Global 请求自动注入 system message（www.workbuddy.ai 对 user-only 请求返回 code 11101）
- token 刷新和 models 发现也走域名感知端点

## 0.6.13

### Changed
- 请求监控 key 自动探测：config → env（CPAMP_ADMIN_KEY/USAGE_REPORT_KEY）→ docker secret `/run/secrets/cpamp_admin_key`，无需手写 usage_report_key


## 0.6.12

### Changed
- 删除无效 `usage.PublishRecord` 路径，请求监控仅走 CPAMP `/v0/management/usage/import`


## 0.6.11

### Fixed
- **请求监控**：c-shared 隔离导致 `usage.PublishRecord` 进不了宿主 redisqueue；改为异步 POST CPA-Manager-Plus `/v0/management/usage/import`（`usage_report_url`/`usage_report_key`）
- 补全 ExecutorType/AuthType/Source；配置字段暴露于管理面板


## 0.6.10

### Fixed
- **批量签到先过滤再操作**：Global 不参与；今日已签跳过；仅对 CN 未签账号调用 daily-checkin
- 返回 `summary{success,already,skipped_global,fail,eligible}`，面板文案不再把 Global/已签当失败
- 分类/签到并发（限流），降低「全部签到」卡到 502 context canceled

## 0.6.9

### Changed
- **Panel theme adaptive**: CSS variables now default to light (paper) theme; `[data-theme="white"]` and `[data-theme="dark"]` overrides align with CPA management panel tokens. Embedded iframe mirrors parent `data-theme` via MutationObserver; standalone page follows `prefers-color-scheme`. All hardcoded dark colors (toast, modal, input, buttons) replaced with theme-aware CSS variables.

## 0.6.3

### Fixed
- Auth identity: parse/refresh leave ID empty; regression tests (A-01)
- Stream pump: emit failure is failed usage; defer streamClose (A-06)
- No dual-write after host.auth.save (A-15)
- Scheduler skips host-disabled candidates (A-04)
- Global delete reconstructs path via peer auth dir (A-07)
- Panel IP ban wait parses upstream window (A-08)
- accountCache concurrent errs race + soft cap (A-02)
- Dashboard single host.auth.get per row (A-05)
- Instant check-in/trial button state (panel)


## 0.6.2

### Fixed
- **Credits look frozen after chat**: cache TTL 5m→45s; invalidate cache after successful chat (stream + non-stream)
- **Spend math**: package used = cycle size−remain; account total_size from package sizes; TotalDosage treated as capacity pool (not consumption)
- **Check-in packs inflate "available"**: UI labels 可用/已用/额度池 so grant vs spend is visible; note shows 余/已用/池

## 0.6.1

### Added
- WorkBuddy panel **用量汇总**：筛选范围内 剩余/已用/总量/占比 + 进度条；全部视图附 CN/Global 分项
- Dashboard API `summary` 字段：`total_remain` / `total_used` / 分区域统计

### Notes
- CPAMP Auth 页进度条仅支持内置 `codex/claude/kimi/xai/antigravity`（`QUOTA_PROVIDER_TYPES` 白名单）；workbuddy 无法靠 `note` 注入进度条，完整用量看插件面板

## 0.6.0

### Added
- **Credit lifecycle** (plugin-only, no CPA/CPAMP source changes):
  - CN exhausted → write auth file `disabled:true` (host skips scheduling)
  - Global exhausted → **delete** auth file (`os.Remove` on path from `host.auth.get`)
  - CN disabled + credits return (after check-in / refresh) → `disabled:false`
  - Executor hard credit errors → async reconcile; pure 429 does not delete Global
  - Unknown credits → no-op (safe default)
- Auth file **note** / **label** enrichment: `CN · 余 x · …` / `Global · …` / 已禁用
- Panel: CN/Global filter tags + counts; disabled badge; lifecycle toast on refresh
- Panel: management-key discipline to avoid CPA IP ban (no request without key; 401/403 backoff)
- Config field `lifecycle_auto` (default true)

### Changed
- Scheduled tick **no longer auto-claims Global trial** (one-shot; manual `/trial` / panel only)
- Tick = CN check-in (if `checkin_auto`) + lifecycle reconcile for all regions
- Import/save writes top-level `type`/`logo`/`note`/`disabled` with nested auth/account
- Force dashboard refresh runs lifecycle and may drop deleted Global rows

### Notes (CPAMP Auth page)
- Filter letter **「W」** / brand typeBadge colors cannot be fixed from the plugin (frontend static icon table)
- Plugin sets `Metadata.logo` + registration Logo; Auth cards show **note** for region/credits summary
- Full UX: WorkBuddy side panel

## 0.5.0

### Added
- International (Global) WorkBuddy account support (`www.workbuddy.ai` domain)
- Domain-aware billing API routing: CN accounts → `codebuddy.cn`, Global → `workbuddy.ai`
- Expert trial pack claim API: `POST /plugins/workbuddy/trial` (Global only, one-time 250 credits / 14 days)
- Panel region badges: light green `CN` (daily checkin) + light orange `Global` (expert trial)
- "全部领取" batch claim button for Global accounts
- Auto-scheduler region branch: CN → daily checkin, Global → claim expert trial if unclaimed
- `wbAccount.region` and `wbAccount.trial_claimed` fields in accounts API response
- `hasTrialPack()` helper detects trial pack from `get-user-resource` packages

### Changed
- `billingBase` selection is now domain-driven via `billingBaseFor(sa)`
- `backendHeaders` Origin/Referer dynamically set per account domain via `originRefererFor(sa)`
- Panel card buttons: CN → 签到, Global → 领取专家加油包 / 已领取
- "全部签到" button only triggers CN accounts (Global accounts are skipped with a message)
- `runAutoCheckin` branches by region: CN daily checkin, Global trial claim

## 0.4.3

### Changed
- Panel import modal: white surface + dark text for readable contrast (was dark-on-dark)

## 0.4.2

### Changed
- Panel: credential import is a toolbar button (left of 刷新数据) opening a modal, instead of an always-visible card

## 0.4.1

### Added
- Panel **耗尽** badge + `exhausted` field on accounts API (shared with scheduler)
- Credential **import** API `POST /plugins/workbuddy/import` + panel paste UI
- Per-account check-in lock (multi-tab safe)
- `executor.count_tokens` stub (`input_tokens:0` — upstream has no API)
- LICENSE (MIT), VERSION file, GitHub Actions multi-arch release workflow

### Changed
- SSE cleanChunk strips empty `extra_fields` / `refusal` / `reasoning_content`
- Scheduler credits mode prefers non-exhausted accounts first

## 0.4.0

### Added
- CPA **Scheduler** capability with `scheduler_mode`: `off` (default) | `credits`
- Credits-aware multi-account pick using panel credit cache

## 0.3.18

### Fixed
- ConfigFields use SDK `ConfigFieldType*` constants

## 0.3.17

### Fixed
- `FrontendAuthProvider` set false; remove dead frontend-auth handlers

## 0.3.16

### Fixed
- Panel refresh toast + busy feedback

## 0.3.15

### Fixed
- Normalize OpenAI object `tool_choice` for CodeBuddy upstream
