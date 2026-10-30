# Changelog

## 0.8.71

### A ticket that says "nobody ahead" stops costing 90 seconds (field report 2026-10-29)

A CN deployment reported that every chat on the `dfmodel`/`qfmodel` lane failed
after 21-31 seconds while a sibling model on the same credential answered in
about a second. The tickets were all the same shape, and the shape is the bug:

```json
{"isQueued":true,"modelKey":"dfmodel","queueCount":0,"queueType":"p3",
 "retryAfterSeconds":30,"serviceAvailable":false,
 "subCode":"RESOURCE_LIMIT","waitTime":30}
```

`queueCount: 0` says nobody is ahead of this request, yet the ticket still asks
for 30 seconds. v0.8.64 took the estimate at face value, so one chat call slept
30s three times (~90s, exactly what `queueMaxAttempts` documents), and because
the host rotates credentials when an execution fails, **each** rotation paid
that ladder again. Measured on that deployment: a single client request took
21-31s to fail, and the failures were instant at the CPA layer (12-26ms once the
ladder was gated off). A direct gateway probe (bypassing CPA) showed the third
consecutive ticket from one credential answered `200` with a full stream — the
30s estimate is boilerplate, not a real wait.

- `queue_retry.go`: `queueSignal` now carries the reported depth
  (`queueCount` + a `depthSeen` flag, accepting both the number and the quoted
  string shape) and marks a `queueCount == 0` ticket as `emptyQueue`.
  `parseQueueSignal` keeps the v0.8.64 default/cap logic and then, for an
  empty-queue ticket only, replaces the interval with `queueFastRetryBackoff`
  (700ms). A ticket with a real depth is untouched — its interval still stands.
- `queueRetryContinue` replaces the bare `queueRetryAllowed` check at all six
  executor call sites (async pump HTTP/frame layers, synchronous collector
  HTTP/frame layers, non-stream HTTP/frame layers). It keeps the existing
  attempt count and `queue_wait_budget` bounds — `queue_wait_budget = 0` still
  disables queue retries outright — and adds, for empty-queue tickets, the
  `queueFastAttempts` (4) ceiling plus a per-model `queueFastLadderWindow`
  (12s) claim. Only the credential that claims the window runs the ladder; the
  siblings the host rotates through fail fast with the same actionable text.
- `queueLimitLine` stamps a new `queue_head` marker beside `queue_limit` for
  those tickets, and the terminal note describes what actually happened
  (`票据自报队列已空，已快速重试N次共X.Xs仍未放行`) instead of reporting a
  30-second wait the plugin never performed.
- `cooldown.go`: `queue_head` lands on the new `cooldownReasonHeadOfQueue`
  (10s, one ladder window). The generic queue ticket — a real depth the plugin
  waited on — keeps the 60s one-ticket cadence, and every other classifier
  (plan gate, soft rate limit, empty stream) is unchanged.

Measured on the reporting deployment after the change: a failing client request
returns in ~4.9s (was 21-31s), the repeat inside the window returns in 12ms, and
`qwen3.8-flash` / `qwen3.8-max` answer normally in 1.0-4.4s.

- Tests (`queue_head_of_queue_test.go`): the 2026-10-29 ticket parses as
  empty-queue with the 700ms ladder and renders both markers; a quoted `"0"`
  depth reads the same; a ticket with 12 requests ahead keeps its own interval
  and stays out of the head-of-queue classifier; an unparseable depth is not
  mistaken for an empty queue; the ladder runs once per window and stops at its
  ceiling while a later request may claim again; a `queue_wait_budget` of 0
  disables it; the terminal note and the cooldown cadences are pinned for both
  ticket kinds; and two pump tests (HTTP layer + in-band first frame) assert the
  recorded waits are 700ms, not 30s. All pre-existing queue tests pass
  unchanged.

## 0.8.70

### CALL_COUNT_LIMIT queue metadata survives the nested envelope (PR #65, sabercomo)

Qoder can answer 10605 with the sub-code `CALL_COUNT_LIMIT` on the outer envelope while the actionable metadata (`retryAfterSeconds`, `modelKey`, `queueType`) rides the JSON-string child WITHOUT repeating `RESOURCE_LIMIT`. The queue parser recognized the outer code but only collected metadata from objects that themselves carried the identification — so a 17-second upstream hint fell back to the default 30-second wait, the model/queue fields vanished from logs, and the rendered line hard-coded `subCode=RESOURCE_LIMIT` with a "capacity limit, unrelated to the account" reading that a call-count ticket cannot support.

- `queue_retry.go`: the walk now threads an `inQueue` flag down the `message`/`details` carriers of an IDENTIFIED envelope, collecting sub-code and metadata from its descendants; an unrelated sibling hanging off an unidentified layer cannot overwrite the ticket. `queueLimitLine` renders the actual sub-code (omitting it when absent, never inventing one) and cause-neutral guidance instead of the account-unrelated claim.
- The `queue_limit` marker and the existing queue budget / wait cap / attempt limit / marker-keyed short (auth, model) cooldown are untouched.
- Tests: nested CALL_COUNT_LIMIT metadata through both carriers, unrelated-sibling isolation, no-invented-sub-code — all three red on 0.8.69, green here (discriminability verified during review).

## 0.8.69

### A oneOf of objects with no type gets type:object — kmodel 502 fix (magpie qoder 0.2.35, #378)

Absorbed from magpie-community/plugins `ebe3555` (qoder 0.2.35, 2026-10-25): Qoder CN's kmodel (Kimi K3.1 Pro) fails a chat whose tools hold a oneOf with no type — even when every branch is an object, directly or through a chain of local $refs (dsh's REA tools, a 502 here) — and takes the same request with `"type": "object"` beside each such oneOf (200 in a real dsh run). The same rule the workbuddy plugin absorbed from workbuddy 0.1.33.

- New `schema_typeunions.go`: `mendToolSchemasRaw` decodes the client's OpenAI tools payload, gives every untyped all-object oneOf inside each tool's parameters the type "object" (branches resolved through local `#/...` $ref chains, cycles broken, ≤16 hops; only schema keywords walked — SUB_ONE/SUB_LIST/SUB_MAP, depth ≤64), and re-encodes. Unions of other or mixed types, anyOf, and missing/external/circular $refs are left as they came. **Done on a decoded copy** — the caller's raw payload bytes are never touched (magpie's structuredClone discipline).
- Wired at the single tools-forwarding point in `buildQoderBody` (covers both execute and execute_stream paths); null / empty / non-array / nothing-to-mend payloads round-trip byte-for-byte.
- Tests (`schema_typeunions_test.go`): the issue's literal shapes — the minimal $ref → oneOf, the REA $ref-branch chain (with the lone-$ref-stays-untyped pin), the left-alone matrix (typed sibling / mixed / anyOf / missing / external / circular / examples — byte-for-byte round-trip), and the pass-through set. Full qoder suite green under `-race`.
