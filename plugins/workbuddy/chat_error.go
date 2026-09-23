// chat_error.go translates upstream chat rejections into actionable errors.
//
// v0.12.18: the CodeBuddy Intl gateway (codebuddy.ai) rejects models that are
// not registered on ITS catalog with HTTP 400
// {"code":11102,"msg":"model [X] service info not found"}. The executor used
// to pass the raw payload through ("upstream 400: {...}"), which told the
// user nothing about which models the account's realm actually serves — and
// the model list itself was unreliable for Intl accounts because discovery
// queried the CN endpoint (fixed in models.go the same release). The
// translator detects 11102 and rewrites the error into a bilingual, actionable
// message that names the realm and its best-known catalog.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// upstreamErrorShape mirrors the APISIX/business-JSON error envelope the
// WorkBuddy gateways return on chat rejections.
type upstreamErrorShape struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// isModelNotRegistered reports whether an upstream >=400 chat payload is the
// model-catalog rejection (code 11102 "service info not found"). The JSON
// path is authoritative; the substring path catches envelope variants where
// the payload is not the plain business JSON (e.g. wrapped in HTML or a
// different envelope) but still carries the same markers.
func isModelNotRegistered(statusCode int, payload string) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	var shape upstreamErrorShape
	if err := json.Unmarshal([]byte(payload), &shape); err == nil && shape.Code == 11102 {
		return true
	}
	low := strings.ToLower(payload)
	return strings.Contains(low, "11102") && strings.Contains(low, "service info not found")
}

// realmDisplayName maps a realm key to the human-readable name used in error
// copy so users can tell WHICH gateway rejected the model.
func realmDisplayName(realm string) string {
	switch realm {
	case "intl":
		return "CodeBuddy Intl (codebuddy.ai)"
	case "global":
		return "WorkBuddy Global (workbuddy.ai)"
	default:
		return "WorkBuddy CN (copilot.tencent.com)"
	}
}

// modelHintForRealm renders the best-known model catalog for a realm.
// Cached realm discovery (if fresh) is the truth; otherwise the realm's
// static catalog is shown (v0.12.19: per-realm — the CN list is no longer
// shown for Intl/Global accounts, which used to advertise models their
// gateway rejects with 11102). Network calls are deliberately NOT made here:
// the executor error path must stay fast, and a doomed 15s discovery call
// during error handling would only add latency.
func modelHintForRealm(realm string) string {
	ids := make([]string, 0, 16)
	if ms, ok := cachedDynamicModels(realm); ok {
		for _, m := range ms {
			if id := strings.TrimSpace(m.ID); id != "" {
				ids = append(ids, id)
			}
		}
	}
	label := "该区域缓存目录 / cached realm catalog"
	if len(ids) == 0 {
		// v0.9.33: the static catalogs are gone — fall back to the stale
		// cache (the last successful discovery answer) before giving up.
		if stale, ok := cachedDynamicModelsStale(realm); ok {
			for _, m := range stale {
				if id := strings.TrimSpace(m.ID); id != "" {
					ids = append(ids, id)
				}
			}
			label = "该区域最近一次成功发现的目录 / last known realm catalog"
		}
	}
	if len(ids) == 0 {
		realmTag := strings.ToLower(strings.TrimSpace(realm))
		if realmTag == "" {
			realmTag = "cn"
		}
		return fmt.Sprintf(
			"无该区域模型目录缓存（动态发现尚未成功过）；请稍后重试，或用 models_%s 配置显式指定模型 / no cached catalog for this realm (discovery has not succeeded yet); retry later, or pin models via models_%s",
			realmTag, realmTag)
	}
	if len(ids) > 20 {
		ids = ids[:20]
	}
	return fmt.Sprintf("%s: %s", label, strings.Join(ids, ", "))
}

// translateChatUpstreamError converts one upstream chat failure into the
// plugin error. 11102 rejections get a bilingual actionable message; every
// other failure keeps the historical "upstream <status>: <payload>" shape so
// existing log parsers and client behavior stay unchanged.
func translateChatUpstreamError(statusCode int, payload string, sa *storedAuth) error {
	if isModelNotRegistered(statusCode, payload) {
		realm := "cn"
		if sa != nil {
			realm = accountRegion(sa)
		}
		return fmt.Errorf(
			"模型未被该账号区域的上游注册（code 11102 service info not found），区域=%s；请改用该区域可用模型后重试，模型列表以 /models 实际返回为准。"+
				" // Model not registered on the %s upstream; pick a model from its catalog and retry. %s | raw: %s",
			realmDisplayName(realm), realmDisplayName(realm), modelHintForRealm(realm),
			truncateRedacted(payload, 200))
	}
	return fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(payload, 200))
}

// statusError carries an upstream HTTP status across the RPC boundary. The
// host's decodeEnvelopeResult rebuilds it as rpcError (via the envelope error
// http_status field, see errorEnvelopeFor) whose StatusCode() drives
// MarkResult's per-status cooldown: 402 -> 30 min, 429 -> escalating quota
// backoff (credential-scoped across models), 401 -> 30 min. Request-level or
// IP-level failures must NOT be wrapped — they stay plain errors with status 0
// and only get the host's 1-minute transient cooldown, same as before 0.9.17.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string   { return e.err.Error() }
func (e *statusError) StatusCode() int { return e.status }
func (e *statusError) Unwrap() error   { return e.err }

// upstreamStatusError wraps a translated upstream chat failure with the HTTP
// status the host cooldown layer should attribute to the credential.
//
// Passed through (account-level): 401 dead token, 402 payment/credits,
// 429 rate/quota (free-tier exhaustion typically surfaces here — the host's
// escalating quota backoff is exactly the "stop hammering a drained
// credential" behavior requested on 2026-09-20), and business 403s that carry
// the upstream envelope (permission-class, e.g. 11140).
//
// Kept at status 0 (request/IP-level, credential stays healthy): 413/11115
// prompt overflow, 11128 channel risk control, 11102 model-catalog rejection,
// the 6004 model-scoped frequency limit (v0.9.32 — upstream itself says
// "switch to another model to continue", so the credential must stay
// healthy; the model is instead parked in the plugin's per-(uid, model)
// rate-limit registry, see model_ratelimit.go), bare 403 WAF challenges,
// and everything else (400/404/5xx keep the host default transient
// cooldown — unchanged from pre-0.9.17 behavior).
func upstreamStatusError(status int, payload string, err error) error {
	switch {
	case isPromptTooLong(status, payload),
		isChannelRiskControl(status, payload),
		isModelNotRegistered(status, payload),
		isModelScopedRateLimit(status, payload),
		isWafBlocked(status, payload):
		return err
	case status == http.StatusUnauthorized,
		status == http.StatusPaymentRequired,
		status == http.StatusTooManyRequests:
		return &statusError{status: status, err: err}
	case status == http.StatusForbidden && hasBusinessEnvelope(payload):
		return &statusError{status: status, err: err}
	}
	return err
}

// containsAny reports whether s holds any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// streamFaultError turns a pre-answer in-stream error frame (the transport
// status was 200, so there is no HTTP status on the wire) into a failure the
// async hand-off can return as a NORMAL failed envelope, so the status survives
// instead of degrading into a lossy in-band text error the host reads as a
// "successful empty answer".
//
// The status is not invented: streamFaultStatus derives the status the gateway
// *would* have used, then upstreamStatusError applies the exact account-vs-
// request policy (401/402/429/business-403 ride the envelope; request/IP-level
// shapes and the model-scoped 6004 stay status 0). payload must be the raw
// frame body (stripDataPrefix'd), NOT the redacted/truncated error string, so
// the classifiers see the real business envelope.
func streamFaultError(frameErr error, payload string, sa *storedAuth) error {
	status := streamFaultStatus(payload)
	if status == 0 {
		// Unrecognised / request-level / model-scoped 6004: keep the frame
		// error plain (status 0), matching the transient-cooldown behavior an
		// unknown stream error had before the gate existed.
		return frameErr
	}
	// Reuse the exact synchronous error surface (actionable 11102/11115/… copy
	// + Retry-After is absent here since a 200 stream carries none) so a
	// pre-answer frame and a real >=400 produce the same user-facing message,
	// then let upstreamStatusError apply the account-vs-request policy.
	return upstreamStatusError(status, payload, translateChatUpstreamErrorFull(status, payload, sa, nil))
}

// streamFaultStatus maps an in-stream error frame's body to the HTTP status the
// host cooldown layer should attribute to the credential. It is deliberately
// conservative: only unambiguous account-level shapes earn a status, every-
// thing else (and the model-scoped 6004) stays 0 so a healthy credential is
// never cooled on a guess.
func streamFaultStatus(payload string) int {
	if strings.TrimSpace(payload) == "" {
		return 0
	}
	// Model-scoped 6004 first: it parks the (credential, model) pair in the
	// plugin registry, not the credential, and is intentionally status-less on
	// every host version (see model_ratelimit.go). Checked before the wording
	// probes so its reset text can never be re-read as credential quota.
	if isModelScopedRateLimit(http.StatusTooManyRequests, payload) {
		return 0
	}
	low := strings.ToLower(payload)
	switch {
	case containsAny(low, "unauthorized", "invalid token", "token expired", "未授权", "登录已失效", "登录失效"):
		return http.StatusUnauthorized
	case isHardCreditError(0, payload): // credit / quota-exhausted wording (402 is account-level)
		return http.StatusPaymentRequired
	case isSoftRateLimit(0, payload): // throttle wording — 6004 already excluded above
		return http.StatusTooManyRequests
	case containsAny(low, "forbidden", "permission", "无权限", "没有权限"):
		// upstreamStatusError keeps a bare (no-envelope) 403 status-less; only a
		// business-envelope 403 (e.g. 11140) earns the credential-level status.
		return http.StatusForbidden
	default:
		return 0
	}
}

// inputTooLargeMarkers 过大错误文案词族（除 11115/"prompt is too long"
// 外的变体；对齐 qoder 0.8.11 chatSizeMarkers）。大小写不敏感。
var inputTooLargeMarkers = []string{
	"maximum context length",
	"context length exceeded",
	"exceeds the context",
	"context window",
	"too many tokens",
	"input too long",
	"prompt too long",
	"request entity too large",
	"输入过长",
	"上下文过长",
	"超出上限",
}

// containsInputTooLargeMarker reports whether a lowercased payload hits
// any input-oversize wording.
func containsInputTooLargeMarker(low string) bool {
	for _, m := range inputTooLargeMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// isPromptTooLong reports the 11115 prompt-overflow rejection (400/404/413 +
// code 11115 / "prompt is too long" wording). Same marker philosophy as the
// wb2api Classify: context overflow is a REQUEST-level problem — the same
// body overflows on any account — so the message must say so instead of
// implying an account failure.
func isPromptTooLong(statusCode int, payload string) bool {
	// v0.9.15: 413 语义唯一（请求体/输入超限）——网关层 HTML/空体拒绝
	//（无业务信封）也算，请求级问题一律给明确文案。
	if statusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	var shape upstreamErrorShape
	if err := json.Unmarshal([]byte(payload), &shape); err == nil && shape.Code == 11115 {
		return true
	}
	low := strings.ToLower(payload)
	if strings.Contains(low, "prompt is too long") {
		return true
	}
	return strings.Contains(low, `"code":11115`) || strings.Contains(low, `"code":"11115"`) ||
		// v0.9.15: 扩大过大词族（对齐 qoder 0.8.11 chatSizeMarkers）。
		containsInputTooLargeMarker(low)
}

// isChannelRiskControl reports the Tencent channel risk-control rejection
// (code 11128). Production evidence (RobbsLuo/Coding2API 2026-09): the
// backend rejects requests carrying the OpenAI `developer` role, missing
// official-CLI request-shape fields, or bursty frequency with this code —
// a request-shaped/transient problem, not an account failure.
func isChannelRiskControl(statusCode int, payload string) bool {
	if statusCode < 400 {
		return false
	}
	var shape upstreamErrorShape
	if err := json.Unmarshal([]byte(payload), &shape); err == nil && shape.Code == 11128 {
		return true
	}
	// 信封变体容错：JSON 解析失败但带着 "code":11128 字样（含空格变体）。
	low := strings.ReplaceAll(payload, " ", "")
	return strings.Contains(low, `"code":11128`) || strings.Contains(low, `"code":"11128"`)
}

// hasBusinessEnvelope reports whether an error body carries the upstream
// business JSON envelope (a "code": or "msg": field name hit). Envelope
// presence is all that matters — malformed JSON with a "msg": marker is
// still treated as a business response (better to miss a WAF page than to
// misfile a business 403).
func hasBusinessEnvelope(payload string) bool {
	return strings.Contains(payload, `"code":`) || strings.Contains(payload, `"msg":`)
}

// isWafBlocked reports the APISIX WAF interception shape: HTTP 403 with NO
// business envelope (HTML challenge page / empty body / plain text).
// Business 403s (11140 request illegal, etc.) keep their own classification.
func isWafBlocked(statusCode int, payload string) bool {
	return statusCode == http.StatusForbidden && !hasBusinessEnvelope(payload)
}

// retryAfterHint renders the upstream retry-guidance headers as a hint
// suffix, mirroring the wb2api Retry-After family: Retry-After (seconds) →
// Retry-After-Ms (milliseconds) → X-RateLimit-Reset (epoch seconds or
// milliseconds; relative seconds below 1e9 also accepted). Sanity-capped at
// 2h; absent/invalid values yield "" — never invent a wait time.
func retryAfterHint(h http.Header) string {
	if h == nil {
		return ""
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 7200 {
			return fmt.Sprintf("上游建议 %d 秒后重试（Retry-After）", n)
		}
	}
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 7200_000 {
			return fmt.Sprintf("上游建议 %d 毫秒后重试（retry-after-ms）", n)
		}
	}
	if v := strings.TrimSpace(h.Get("X-RateLimit-Reset")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			var wait int64
			switch {
			case n > 1e12: // epoch milliseconds
				wait = n/1000 - time.Now().Unix()
			case n > 1e9: // epoch seconds
				wait = n - time.Now().Unix()
			default: // relative seconds
				wait = n
			}
			if wait > 0 && wait <= 7200 {
				return fmt.Sprintf("上游限流窗口约 %d 秒后重置（x-ratelimit-reset）", wait)
			}
		}
	}
	return ""
}

// translateChatUpstreamErrorFull is translateChatUpstreamError with the
// response headers available: 11115 prompt-overflow and WAF-shaped bare 403s
// get dedicated actionable copy, and upstream Retry-After guidance is
// appended to every other failure so the client can back off intelligently.
// The 11102 translation inside the base function keeps its priority — a
// model-catalog rejection is never rewritten by the newer shapes.
func translateChatUpstreamErrorFull(statusCode int, payload string, sa *storedAuth, hdr http.Header) error {
	base := translateChatUpstreamError(statusCode, payload, sa)
	switch {
	case isModelNotRegistered(statusCode, payload):
		return base
	case isModelScopedRateLimit(statusCode, payload):
		// v0.9.32 (issue round 2026-09-22): model-scoped 6004 gets its own
		// copy. The historical raw shape starts with "upstream 429", which
		// hosts without the envelope status would re-read as credential
		// quota from the message alone; the dedicated copy also carries the
		// declared reset instant and upstream's switch-models advice.
		resetAt, resetKnown := parseRateLimitResetAt(payload)
		return fmt.Errorf("%s | raw: %s", modelScopedRateLimitCopy(resetAt, resetKnown),
			truncateRedacted(payload, 200))
	case isPromptTooLong(statusCode, payload):
		// v0.9.16: 11115 码提及条件化——413 裸 HTML/空体与其他词族命中时
		// body 里并没有 11115，硬编码会误导用户去查一个不存在的码。
		detail := "413/context limit exceeded"
		if strings.Contains(payload, "11115") {
			detail = "code 11115 prompt is too long"
		}
		return fmt.Errorf(
			"提示词过长（%s）——上下文超出模型上限，属于请求本身的问题，与账号无关；请缩短上下文/清理会话或开新会话后重试。"+
				" // Prompt too long for the model's context window (request-level, not account-level); shrink the context or start a new session. | raw: %s",
			detail, truncateRedacted(payload, 200))
	case isChannelRiskControl(statusCode, payload):
		return fmt.Errorf(
			"上游渠道风控（code 11128）——通常由请求特征或频率触发，与账号状态无关；请降低请求频率稍后再试，持续出现请更新插件以对齐官方客户端请求特征。"+
				" // Upstream channel risk control (11128); back off and retry — request-shaped, not account-level. | raw: %s",
			truncateRedacted(payload, 200))
	case isWafBlocked(statusCode, payload):
		return fmt.Errorf(
			"请求被上游风控拦截（HTTP 403 无业务信封，APISIX WAF）——通常由请求频率或网络环境触发，与账号状态无关；请降低频率稍后再试，持续出现请更换网络出口。"+
				" // Blocked by the upstream WAF (bare 403, no business envelope); back off and retry, switch network if it persists. | raw: %s",
			truncateRedacted(payload, 200))
	}
	if hint := retryAfterHint(hdr); hint != "" {
		return fmt.Errorf("%w；%s", base, hint)
	}
	return base
}
