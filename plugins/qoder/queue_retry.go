// queue_retry.go — the 10605 RESOURCE_LIMIT queue envelope (v0.8.64).
//
// Observed 2026-10-20 in a Docker-deployed CPA (qfmodel, p3 queue): the
// Qoder gateway no longer only *holds* a request on the connection until the
// queue discharges (the historical behaviour chat_live_test.go documents); it
// can also reject immediately with a 403 whose body is a queue ticket asking
// the client to come back later:
//
//	HTTP 200 SSE frame, statusCodeValue=403
//	L1: {"code":403,"message":"<L2 json string>"}
//	L2: {"code":"10605","message":"<L3 json string>","subCode":"RESOURCE_LIMIT"}
//	L3: {"isQueued":true,"modelKey":"qfmodel","queueType":"p3",
//	     "retryAfterSeconds":30,"serviceAvailable":false,
//	     "subCode":"RESOURCE_LIMIT","waitTime":30}
//
// Before this file the plugin treated that as an ordinary upstream failure:
// recordUpstreamFailure + HTTP 500 with the raw JSON soup rendered by
// describeQoderEnvelopeRejection — the client saw "qoder upstream error:
// status=403 code=403 type=: {...}" and the credential was marked failed for
// what is really a transient capacity ticket.
//
// All three chat paths (async pump, synchronous collector, non-stream
// executor) now recognise the ticket and retry the request themselves after
// retryAfterSeconds, bounded by the constants below. The wait respects the
// request context, so a disconnected client cancels an in-flight queue wait.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	queueWaitBudget = queueTotalBudget
	queueBudgetMu   sync.RWMutex
)

const (
	// queueCode is the gateway's queue/RESOURCE_LIMIT business code. It has
	// been observed as a JSON string ("10605"); older gateways emit plain
	// numbers elsewhere in the family, so the matcher accepts both shapes.
	queueCode = "10605"
	// queueSubCode is the resource-limit sub-code both the L2 envelope and
	// the L3 ticket carry.
	queueSubCode = "RESOURCE_LIMIT"
	// queueMarker is the greppable prefix queueLimitLine stamps into failure
	// text; the cooldown classifier keys on it (isQueueLimitFailure).
	queueMarker = "queue_limit"

	// queueMaxAttempts bounds one chat call: the original request plus at
	// most three queue retries (30s each with the observed ticket = ~90s of
	// waiting, inside the 120s shared-client window of a single attempt).
	queueMaxAttempts = 4
	// queueWaitDefault is used when the ticket carries no usable interval.
	queueWaitDefault = 30 * time.Second
	// queueWaitCap clamps a hostile/large retryAfterSeconds per round.
	queueWaitCap = 45 * time.Second
	// queueTotalBudget caps the cumulative queue wait across all rounds of
	// one chat call, so a 60s ticket cannot pin a request for minutes.
	queueTotalBudget = 150 * time.Second
)

// queueSignal is the parsed queue ticket. detected=false means the body was
// not a 10605/RESOURCE_LIMIT envelope.
type queueSignal struct {
	detected   bool
	subCode    string
	retryAfter time.Duration
	queueType  string
	modelKey   string
	// status is the status the rejection rode in on: the HTTP status for a
	// plain 403 response, or the frame's statusCodeValue when the ticket
	// travelled inside a 200-OK SSE envelope. 0 when unknown.
	status int
}

// parseQueueSignal walks body (an upstream error payload from either the HTTP
// layer or an SSE frame) looking for the queue ticket at any nesting depth.
// The gateway nests the actionable object inside JSON-string "message" (and
// sometimes "details") fields, so both are unwrapped recursively; the walk is
// depth-bounded and collects metadata below an identified queue envelope.
func parseQueueSignal(body string) queueSignal {
	var sig queueSignal
	walkQueueBody(strings.TrimSpace(body), &sig, 0, false)
	if sig.detected {
		if sig.retryAfter <= 0 {
			sig.retryAfter = queueWaitDefault
		}
		if sig.retryAfter > queueWaitCap {
			sig.retryAfter = queueWaitCap
		}
	}
	return sig
}

// walkQueueBody is parseQueueSignal's recursion. depth guards against
// pathological nesting. Layers do not short-circuit on the first hit: the
// observed envelope splits the identification (code 10605 on L2) from the
// payload (retryAfterSeconds/modelKey on L3), so every layer gets to
// contribute what it carries. inQueue applies only to descendants of an
// identified envelope, so an unrelated sibling cannot overwrite its metadata.
func walkQueueBody(s string, sig *queueSignal, depth int, inQueue bool) {
	if depth > 4 {
		return
	}
	if !strings.HasPrefix(s, "{") {
		return
	}
	var obj struct {
		Code              json.RawMessage `json:"code"`
		SubCode           string          `json:"subCode"`
		Message           json.RawMessage `json:"message"`
		Details           json.RawMessage `json:"details"`
		RetryAfterSeconds float64         `json:"retryAfterSeconds"`
		WaitTime          float64         `json:"waitTime"`
		QueueType         string          `json:"queueType"`
		ModelKey          string          `json:"modelKey"`
	}
	if json.Unmarshal([]byte(s), &obj) != nil {
		return
	}
	if codeScalarString(obj.Code) == queueCode || strings.EqualFold(strings.TrimSpace(obj.SubCode), queueSubCode) {
		sig.detected = true
		inQueue = true
	}
	if inQueue {
		if v := strings.TrimSpace(obj.SubCode); v != "" {
			sig.subCode = v
		}
		switch {
		case obj.RetryAfterSeconds > 0:
			sig.retryAfter = time.Duration(obj.RetryAfterSeconds * float64(time.Second))
		case obj.WaitTime > 0:
			sig.retryAfter = time.Duration(obj.WaitTime * float64(time.Second))
		}
		if v := strings.TrimSpace(obj.QueueType); v != "" {
			sig.queueType = v
		}
		if v := strings.TrimSpace(obj.ModelKey); v != "" {
			sig.modelKey = v
		}
	}
	// The ticket may be one level deeper (L2 hits on code, L3 carries
	// retryAfterSeconds) — keep walking the JSON-string carriers.
	if nested := jsonObjectString(obj.Message); nested != "" {
		walkQueueBody(nested, sig, depth+1, inQueue)
	}
	if nested := jsonObjectString(obj.Details); nested != "" {
		walkQueueBody(nested, sig, depth+1, inQueue)
	}
}

// codeScalarString renders a JSON code field that is either a quoted string
// ("10605") or a number (10605) as its plain text form; anything else is "".
func codeScalarString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

// jsonObjectString returns the JSON *string* carried by raw only when its
// content itself looks like a JSON object — the gateway's nested-envelope
// convention. Plain-text messages return "" so the recursion never chases
// prose.
func jsonObjectString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") {
		return t
	}
	return ""
}

// queueLimitLine preserves the upstream sub-code without assuming every 10605
// rejection is model-capacity trouble unrelated to the account.
// The queue_limit prefix is load-bearing — recordUpstreamFailure's classifier
// keys on it to apply the short (auth, model) cooldown.
func queueLimitLine(sig queueSignal) string {
	line := queueMarker + ": code=" + queueCode
	if sig.subCode != "" {
		line += " subCode=" + sig.subCode
	}
	line += " 上游排队或受限"
	if sig.modelKey != "" {
		line += "，model=" + sig.modelKey
	}
	if sig.queueType != "" {
		line += "，队列=" + sig.queueType
	}
	line += " — 按上游建议等待后重试，或改用其他模型"
	return line
}

// queueRetryAllowed reports whether one more queue retry round fits the
// attempt count and the cumulative wait budget. attempt is the number of
// retries already performed (0 on the original request).
func queueRetryAllowed(attempt int, waited time.Duration, wait time.Duration) bool {
	if wait <= 0 {
		wait = queueWaitDefault
	}
	if wait > queueWaitCap {
		wait = queueWaitCap
	}
	queueBudgetMu.RLock()
	budget := queueWaitBudget
	queueBudgetMu.RUnlock()
	return attempt+1 < queueMaxAttempts && waited+wait <= budget
}

// queueWait sleeps for the ticket's interval, honouring ctx cancellation. It
// returns false when ctx fired first (the client is gone — abandon the call
// without recording any failure). A nil ctx waits unconditionally, matching
// the synchronous paths that have no request context today.
//
// Package var so tests can run the retry loops at full speed.
var queueWaitFn = func(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = queueWaitDefault
	}
	if d > queueWaitCap {
		d = queueWaitCap
	}
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// queueWaitFor clamps a ticket interval to the per-round cap.
func queueWaitFor(d time.Duration) time.Duration {
	if d <= 0 {
		return queueWaitDefault
	}
	if d > queueWaitCap {
		return queueWaitCap
	}
	return d
}

// queueExhaustedLine renders the terminal failure after the retry budget is
// spent: the actionable queue line plus what the plugin already did about it,
// so a user reading a Docker log understands the 90s of silence.
func queueExhaustedLine(sig queueSignal, attempts int, waited time.Duration) string {
	return fmt.Sprintf("%s（已自动重试%d次共%.0fs仍排队）", queueLimitLine(sig), attempts, waited.Seconds())
}
