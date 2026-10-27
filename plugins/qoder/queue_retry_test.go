package main

// queue_retry_test.go — the 10605 RESOURCE_LIMIT queue ticket (v0.8.64).
//
// Observed in the wild on a Docker-deployed CPA (qfmodel, p3 queue): the
// gateway rejects with a three-layer envelope — code 403 wrapping a JSON
// string that wraps {"code":"10605","subCode":"RESOURCE_LIMIT"} that wraps
// the ticket {"isQueued":true,"retryAfterSeconds":30,...} — and the plugin
// used to fail the whole request on it. These tests pin the full contract:
// the parser sees through every nesting shape, the rendered failure carries
// the queue_limit marker the cooldown classifier keys on, and all retrying
// chat paths honour the ticket and recover. The waits are injected, so the
// retry loops run at unit-test speed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- fixtures: the observed three-layer ticket, plus its variants ----------

// queueTicketL3 is the innermost ticket exactly as observed 2026-10-20.
func queueTicketL3() string {
	return `{"isQueued":true,"modelKey":"qfmodel","queueType":"p3","retryAfterSeconds":30,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT","waitTime":30}`
}

// queueTicketL2 wraps the ticket behind business code 10605.
func queueTicketL2() string {
	return `{"code":"10605","message":` + strconv.Quote(queueTicketL3()) + `,"subCode":"RESOURCE_LIMIT"}`
}

// queueTicketL1 is the outer envelope the SSE frame carried: generic code 403
// with the business envelope hidden in a JSON-string message.
func queueTicketL1() string {
	return `{"code":403,"message":` + strconv.Quote(queueTicketL2()) + `}`
}

// installFastQueueWait swaps the queue wait primitive for one that returns
// immediately while recording the requested intervals.
func installFastQueueWait(t *testing.T) *[]time.Duration {
	t.Helper()
	waits := &[]time.Duration{}
	orig := queueWaitFn
	queueWaitFn = func(ctx context.Context, d time.Duration) bool {
		*waits = append(*waits, d)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return false
			default:
			}
		}
		return true
	}
	t.Cleanup(func() { queueWaitFn = orig })
	return waits
}

// --- parser ----------------------------------------------------------------

func TestParseQueueSignalObservedThreeLayerTicket(t *testing.T) {
	sig := parseQueueSignal(queueTicketL1())
	if !sig.detected {
		t.Fatal("the observed three-layer ticket must be detected")
	}
	if sig.retryAfter != 30*time.Second {
		t.Fatalf("retryAfter: got %v, want 30s", sig.retryAfter)
	}
	if sig.modelKey != "qfmodel" {
		t.Fatalf("modelKey: got %q, want qfmodel", sig.modelKey)
	}
	if sig.queueType != "p3" {
		t.Fatalf("queueType: got %q, want p3", sig.queueType)
	}
}

func TestParseQueueSignalCallCountLimitMetadata(t *testing.T) {
	// The observed CALL_COUNT_LIMIT ticket carries its metadata below the
	// 10605 envelope, without repeating that code in the innermost object.
	ticket := strings.ReplaceAll(queueTicketL3(), "RESOURCE_LIMIT", "CALL_COUNT_LIMIT")
	ticket = strings.Replace(ticket, `"retryAfterSeconds":30`, `"retryAfterSeconds":17`, 1)
	business := `{"code":"10605","message":` + strconv.Quote(ticket) + `,"subCode":"CALL_COUNT_LIMIT"}`
	for _, carrier := range []string{"message", "details"} {
		t.Run(carrier, func(t *testing.T) {
			body := `{"code":403,"` + carrier + `":` + strconv.Quote(business) + `}`
			sig := parseQueueSignal(body)
			if !sig.detected || sig.retryAfter != 17*time.Second || sig.modelKey != "qfmodel" || sig.queueType != "p3" {
				t.Fatalf("CALL_COUNT_LIMIT metadata lost: %+v", sig)
			}
			line := queueLimitLine(sig)
			if !strings.Contains(line, "subCode=CALL_COUNT_LIMIT") || strings.Contains(line, "RESOURCE_LIMIT") || strings.Contains(line, "与账号无关") {
				t.Fatalf("queue line must preserve the upstream cause: %s", line)
			}
		})
	}
}

func TestParseQueueSignalIgnoresUnrelatedSiblingMetadata(t *testing.T) {
	business := strings.ReplaceAll(queueTicketL2(), "RESOURCE_LIMIT", "CALL_COUNT_LIMIT")
	sibling := `{"subCode":"UNRELATED","retryAfterSeconds":1,"modelKey":"other","queueType":"other"}`
	body := `{"code":403,"message":` + strconv.Quote(business) + `,"details":` + strconv.Quote(sibling) + `}`
	line := queueLimitLine(parseQueueSignal(body))
	for _, want := range []string{"subCode=CALL_COUNT_LIMIT", "model=qfmodel", "队列=p3"} {
		if !strings.Contains(line, want) {
			t.Fatalf("queue metadata missing %q: %s", want, line)
		}
	}
	if sig := parseQueueSignal(body); sig.retryAfter != 30*time.Second {
		t.Fatalf("unrelated sibling replaced queue retry interval: %v", sig.retryAfter)
	}
}

func TestQueueLimitLineDoesNotInventSubCode(t *testing.T) {
	line := queueLimitLine(parseQueueSignal(`{"code":"10605"}`))
	if strings.Contains(line, "subCode=") || strings.Contains(line, "RESOURCE_LIMIT") || strings.Contains(line, "与账号无关") {
		t.Fatalf("queue line invented a cause absent from the ticket: %s", line)
	}
}

func TestParseQueueSignalDirectTopLevelTicket(t *testing.T) {
	// The HTTP layer may hand back the business envelope without the 403
	// wrapper, and older gateways may emit the code as a number.
	for _, body := range []string{
		queueTicketL2(),
		`{"code":10605,"message":` + strconv.Quote(queueTicketL3()) + `,"subCode":"RESOURCE_LIMIT"}`,
		`{"code":"10605","subCode":"RESOURCE_LIMIT","retryAfterSeconds":45}`,
		`{"subCode":"RESOURCE_LIMIT","waitTime":20}`,
	} {
		if sig := parseQueueSignal(body); !sig.detected {
			t.Fatalf("direct ticket must be detected: %s", body)
		}
	}
	if sig := parseQueueSignal(`{"subCode":"RESOURCE_LIMIT","waitTime":20}`); sig.retryAfter != 20*time.Second {
		t.Fatalf("waitTime fallback: got %v, want 20s", sig.retryAfter)
	}
	if sig := parseQueueSignal(`{"code":"10605","subCode":"RESOURCE_LIMIT","retryAfterSeconds":45}`); sig.retryAfter != 45*time.Second {
		t.Fatalf("retryAfterSeconds: got %v, want 45s", sig.retryAfter)
	}
}

func TestParseQueueSignalDetailsCarrier(t *testing.T) {
	body := `{"code":403,"message":"busy","details":` + strconv.Quote(queueTicketL2()) + `}`
	if sig := parseQueueSignal(body); !sig.detected {
		t.Fatal("a ticket nested in details must be detected")
	}
}

func TestParseQueueSignalDefaultsAndCaps(t *testing.T) {
	// No interval anywhere: the default window applies.
	if sig := parseQueueSignal(`{"code":"10605","subCode":"RESOURCE_LIMIT"}`); sig.retryAfter != queueWaitDefault {
		t.Fatalf("missing interval: got %v, want default %v", sig.retryAfter, queueWaitDefault)
	}
	// A hostile interval is clamped to the per-round cap.
	if sig := parseQueueSignal(`{"code":"10605","subCode":"RESOURCE_LIMIT","retryAfterSeconds":3600}`); sig.retryAfter != queueWaitCap {
		t.Fatalf("hostile interval: got %v, want cap %v", sig.retryAfter, queueWaitCap)
	}
}

func TestParseQueueSignalRejectsNonQueueBodies(t *testing.T) {
	for _, body := range []string{
		``,
		`not json at all`,
		`{"code":112,"message":"{\"pricingUrl\":\"https://qoder.com.cn/pricing\"}"}`,
		`{"code":"4001","message":"tool orphan"}`,
		`{"error":{"message":"free quota exhausted"}}`,
		`{"code":403,"message":"plain forbidden text"}`,
		`{"subCode":"CALL_COUNT_LIMIT","retryAfterSeconds":17}`,
	} {
		if sig := parseQueueSignal(body); sig.detected {
			t.Fatalf("non-queue body must not be detected: %s", body)
		}
	}
}

// --- rendering -------------------------------------------------------------

func TestQueueLimitLineCarriesMarkerAndGuidance(t *testing.T) {
	sig := parseQueueSignal(queueTicketL1())
	line := queueLimitLine(sig)
	for _, want := range []string{queueMarker, "code=10605", "RESOURCE_LIMIT", "qfmodel", "p3", "上游排队或受限", "等待后重试"} {
		if !strings.Contains(line, want) {
			t.Fatalf("queue line missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "isQueued") {
		t.Fatalf("queue line must not leak the raw envelope: %s", line)
	}
	exhausted := queueExhaustedLine(sig, 3, 90*time.Second)
	if !strings.Contains(exhausted, "已自动重试3次共90s仍排队") {
		t.Fatalf("exhausted line must carry the retry history: %s", exhausted)
	}
}

func TestDescribeEnvelopeRendersQueueLine(t *testing.T) {
	line := describeQoderEnvelopeRejection(queueTicketL1(), nil, 403)
	if !strings.Contains(line, queueMarker) {
		t.Fatalf("envelope rejection must render the queue line, got: %s", line)
	}
	if !strings.Contains(line, "qfmodel") {
		t.Fatalf("model key must survive rendering, got: %s", line)
	}
}

func TestChatUpstreamErrorRendersQueueLine(t *testing.T) {
	err := chatUpstreamError(403, queueTicketL1())
	if err == nil || !strings.Contains(err.Error(), queueMarker) {
		t.Fatalf("chatUpstreamError must render the queue line, got: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream 403") {
		t.Fatalf("status prefix must survive, got: %v", err)
	}
}

// --- cooldown classification -------------------------------------------------

func TestRecordUpstreamFailureQueueLimitCoolsPairBriefly(t *testing.T) {
	recordUpstreamFailure("qauth", "qfmodel", 0, "qoder upstream error: "+queueLimitLine(parseQueueSignal(queueTicketL1())))
	until := modelCoolingUntil("qauth", "qfmodel")
	if until.IsZero() {
		t.Fatal("a queue_limit failure must cool the (auth, model) pair")
	}
	if d := time.Until(until); d > modelCooldownUnknown+5*time.Second || d <= 0 {
		t.Fatalf("queue cooldown should be the short unknown cadence (~%v), got %v", modelCooldownUnknown, d)
	}
	// Account scope: another model on the same auth stays live.
	if modelIsCooling("qauth", "othermodel") {
		t.Fatal("queue cooldown must stay model-scoped")
	}
	// And a queue ticket is never a hard-credit / soft-rate-limit signal.
	if isHardCreditError(403, queueTicketL1()) || isSoftRateLimit(403, queueTicketL1()) {
		t.Fatal("a queue ticket must not read as credit trouble")
	}
}

// --- retry policy -------------------------------------------------------------

func TestQueueRetryAllowedBounds(t *testing.T) {
	cases := []struct {
		attempt int
		waited  time.Duration
		wait    time.Duration
		want    bool
	}{
		{0, 0, 30 * time.Second, true},                 // first retry
		{2, 60 * time.Second, 30 * time.Second, true},  // third retry still fits
		{3, 90 * time.Second, 30 * time.Second, false}, // attempt budget spent
		{0, 0, 0, true}, // zero interval falls back to default
		{1, 120 * time.Second, 45 * time.Second, false}, // cumulative budget exceeded
	}
	for _, c := range cases {
		if got := queueRetryAllowed(c.attempt, c.waited, c.wait); got != c.want {
			t.Fatalf("queueRetryAllowed(attempt=%d, waited=%v, wait=%v) = %v, want %v", c.attempt, c.waited, c.wait, got, c.want)
		}
	}
}

func TestQueueWaitRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if queueWaitFn(ctx, time.Second) {
		t.Fatal("a cancelled context must interrupt the wait")
	}
}

func TestQueueWaitBudgetConfig(t *testing.T) {
	t.Cleanup(func() {
		queueBudgetMu.Lock()
		queueWaitBudget = queueTotalBudget
		queueBudgetMu.Unlock()
	})
	for _, tc := range []struct {
		yaml string
		want time.Duration
	}{
		{"queue_wait_budget: 30", 30 * time.Second},
		{`{"queue_wait_budget": "0"}`, 0},
		{"checkin_auto: false", queueTotalBudget},
	} {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(tc.yaml)})
		if err := configure(raw); err != nil {
			t.Fatal(err)
		}
		if queueWaitBudget != tc.want {
			t.Fatalf("configure(%q): budget=%v, want %v", tc.yaml, queueWaitBudget, tc.want)
		}
	}
	for _, value := range []string{"-1", "151", "30s", "null", "true"} {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("queue_wait_budget: " + value)})
		if _, err := handleMethod("plugin.reconfigure", raw); err == nil {
			t.Fatalf("invalid queue_wait_budget %q must fail reconfigure", value)
		}
		if queueWaitBudget != queueTotalBudget {
			t.Fatal("invalid configuration must not change the active budget")
		}
	}
}

func TestPumpConfiguredQueueBudgetStopsWithError(t *testing.T) {
	for _, budget := range []time.Duration{0, 30 * time.Second} {
		t.Run(budget.String(), func(t *testing.T) {
			queueBudgetMu.Lock()
			queueWaitBudget = budget
			queueBudgetMu.Unlock()
			t.Cleanup(func() {
				queueBudgetMu.Lock()
				queueWaitBudget = queueTotalBudget
				queueBudgetMu.Unlock()
			})
			waits := installFastQueueWait(t)
			rec := installStreamSinks(t)
			srv, hits := queueCountingServer(t, 1<<30, http.StatusForbidden, queueTicketL1())
			done := startPump(srv.URL, nil)
			rec.waitFor(t, 1)
			mustFinish(t, done)
			wantRetries := int(budget / queueWaitDefault)
			if int(atomic.LoadInt32(hits)) != wantRetries+1 || len(*waits) != wantRetries {
				t.Fatalf("budget %v: hits=%d waits=%v", budget, atomic.LoadInt32(hits), *waits)
			}
			if len(rec.inBand()) != 1 || !strings.Contains(rec.inBand()[0], queueMarker) {
				t.Fatalf("expected one queue error, got %q", rec.inBand())
			}
			if len(rec.delivered()) != 0 {
				t.Fatalf("queue failure must not emit answer/completion chunks: %q", rec.delivered())
			}
		})
	}
}

func TestWorkspaceQuotaIsNotAccountCreditExhaustion(t *testing.T) {
	body := `{"error":{"code":429,"message":"Workspace allocated quota exceeded"}}`
	for _, status := range []int{0, 200, 429} {
		if isHardCreditError(status, body) {
			t.Fatalf("workspace quota must not count as account credit exhaustion (status %d)", status)
		}
	}
	if !isHardCreditError(402, body) || !isHardCreditError(429, "Insufficient credits") {
		t.Fatal("explicit payment/credit failures must retain their classification")
	}
}

// --- frame plumbing -----------------------------------------------------------

func TestFrameErrorCarriesQueueSignal(t *testing.T) {
	_, _, err := qoderUnwrapFrame(qoderFrame(t, http.StatusForbidden, queueTicketL1()))
	if err == nil {
		t.Fatal("the ticket frame must surface as a frame error")
	}
	var fe *qoderFrameError
	if !asFrameError(err, &fe) {
		t.Fatalf("error type: %T", err)
	}
	if fe.queued == nil || !fe.queued.detected {
		t.Fatal("the frame error must carry the queue signal")
	}
	if fe.queued.retryAfter != 30*time.Second {
		t.Fatalf("carried signal retryAfter: got %v", fe.queued.retryAfter)
	}
	if !strings.Contains(fe.Error(), queueMarker) {
		t.Fatalf("frame error text must carry the marker (it feeds classifiers), got: %s", fe.Error())
	}

	// The error-field carrier is recognised too.
	errBody := `{"error":` + queueTicketL1() + `}`
	_, _, err = qoderUnwrapFrame(qoderFrame(t, http.StatusOK, errBody))
	if err == nil {
		t.Fatal("error-field ticket must surface as a frame error")
	}
	if !asFrameError(err, &fe) || fe.queued == nil {
		t.Fatalf("error-field ticket must carry the queue signal: %v", err)
	}

	// A non-queue failure stays queue-free.
	_, _, err = qoderUnwrapFrame(qoderFault(t, http.StatusTooManyRequests, "free quota exhausted"))
	if err == nil {
		t.Fatal("fault frame must still be a frame error")
	}
	if !asFrameError(err, &fe) {
		t.Fatalf("error type: %T", err)
	}
	if fe.queued != nil {
		t.Fatal("a non-queue fault must not carry a queue signal")
	}
}

func asFrameError(err error, out **qoderFrameError) bool {
	fe, ok := err.(*qoderFrameError)
	if !ok {
		return false
	}
	*out = fe
	return true
}

// --- the async pump ----------------------------------------------------------

// queueCountingServer answers queueTicketBody for the first ticketCount
// requests, then serves one healthy SSE conversation.
func queueCountingServer(t *testing.T, ticketCount int32, ticketStatus int, ticketBody string) (*httptest.Server, *int32) {
	t.Helper()
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(hits, 1) <= ticketCount {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(ticketStatus)
			_, _ = io.WriteString(w, ticketBody)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, qoderOpener(t)+qoderAnswer(t, "served after the queue")+qoderDone())
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestPumpRetriesHTTPLayerQueueTicketThenSucceeds(t *testing.T) {
	installFastQueueWait(t)
	rec := installStreamSinks(t)
	srv, hits := queueCountingServer(t, 1, http.StatusForbidden, queueTicketL1())

	pumpDone := startPump(srv.URL, nil)
	rec.waitFor(t, 2) // opener + answer
	mustFinish(t, pumpDone)

	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("exactly one retry must follow the ticket, hits=%d", *hits)
	}
	if len(rec.inBand()) != 0 {
		t.Fatalf("a recovered ticket must not report any fault: %q", rec.inBand())
	}
	joined := strings.Join(rec.delivered(), "")
	if !strings.Contains(joined, "served after the queue") {
		t.Fatalf("the retried round's answer must reach the client: %q", joined)
	}
}

func TestPumpRetriesInBandQueueFrameThenSucceeds(t *testing.T) {
	installFastQueueWait(t)
	rec := installStreamSinks(t)
	// The ticket rides a 200-OK SSE envelope as its first frame — the exact
	// shape from the Docker deployment report.
	ticketFrame := qoderFrame(t, http.StatusForbidden, queueTicketL1())
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(hits, 1) == 1 {
			_, _ = io.WriteString(w, ticketFrame)
			return
		}
		_, _ = io.WriteString(w, qoderOpener(t)+qoderAnswer(t, "recovered in-band")+qoderDone())
	}))
	t.Cleanup(srv.Close)

	pumpDone := startPump(srv.URL, nil)
	rec.waitFor(t, 2)
	mustFinish(t, pumpDone)

	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("the in-band ticket must trigger exactly one retry, hits=%d", *hits)
	}
	if len(rec.inBand()) != 0 {
		t.Fatalf("a recovered in-band ticket must not report any fault: %q", rec.inBand())
	}
	if !strings.Contains(strings.Join(rec.delivered(), ""), "recovered in-band") {
		t.Fatalf("the retried answer must reach the client: %q", rec.delivered())
	}
}

func TestPumpQueueExhaustionReportsQueueLimit(t *testing.T) {
	waits := installFastQueueWait(t)
	rec := installStreamSinks(t)
	// Every round hands back a ticket: the pump must try the full budget
	// (original + 3 retries) and then fail once, with the actionable line.
	srv, hits := queueCountingServer(t, 1<<30, http.StatusForbidden, queueTicketL1())

	pumpDone := startPump(srv.URL, nil)
	rec.waitFor(t, 1)
	mustFinish(t, pumpDone)

	if got := atomic.LoadInt32(hits); got != queueMaxAttempts {
		t.Fatalf("the pump must stop after %d rounds, hits=%d", queueMaxAttempts, got)
	}
	if len(*waits) != queueMaxAttempts-1 {
		t.Fatalf("three waits for four rounds, got %d: %v", len(*waits), *waits)
	}
	faults := rec.inBand()
	if len(faults) != 1 {
		t.Fatalf("exactly one terminal fault, got %q", faults)
	}
	for _, want := range []string{queueMarker, "已自动重试3次", "qfmodel"} {
		if !strings.Contains(faults[0], want) {
			t.Fatalf("terminal fault missing %q: %s", want, faults[0])
		}
	}
}

func TestPumpQueueExhaustionCoolsPairWhenAuthKnown(t *testing.T) {
	installFastQueueWait(t)
	rec := installStreamSinks(t)
	srv, _ := queueCountingServer(t, 1<<30, http.StatusForbidden, queueTicketL1())

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("encoded-body"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpUpstreamStream(req, func() (*http.Request, error) {
			return http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("encoded-body"))
		}, nil, "queue-exhaust", false, "qmodel_preview", "qmodel_preview", "", time.Now(), "qex-auth", "qfmodel", nil)
	}()
	rec.waitFor(t, 1)
	mustFinish(t, done)

	// The cooldown key is the normalized model the executor passes; assert
	// through the snapshot so the test does not depend on alias plumbing.
	entries := cooldownSnapshotFor("qex-auth")
	if len(entries) == 0 {
		t.Fatal("exhaustion with a known auth must cool the (auth, model) pair")
	}
	if entries[0]["reason"] != string(cooldownReasonUnknown) {
		t.Fatalf("queue cooldown reason: got %v, want %v", entries[0]["reason"], cooldownReasonUnknown)
	}
	if len(rec.delivered()) != 0 {
		t.Fatalf("a ticket round must never deliver anything: %q", rec.delivered())
	}
}

func TestPumpGateSurvivesQueueRetry(t *testing.T) {
	installFastQueueWait(t)
	rec := installStreamSinks(t)
	srv, hits := queueCountingServer(t, 1, http.StatusForbidden, queueTicketL1())

	gate := newStreamHeadGate(2 * time.Second)
	pumpDone := startPump(srv.URL, gate)

	// The hand-off must see a clean release once the retried round answers —
	// the ticket itself must never surface as a verdict.
	verdict, decided := gate.awaitHandoff(2 * time.Second)
	if !decided {
		t.Fatal("the retried round must decide the hand-off")
	}
	if verdict.err != nil {
		t.Fatalf("the hand-off must not see the ticket as a failure: %v", verdict.err)
	}
	rec.waitFor(t, 2)
	mustFinish(t, pumpDone)
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("one retry expected, hits=%d", *hits)
	}
}

func TestPumpGateExhaustionCarriesTicketStatus(t *testing.T) {
	installFastQueueWait(t)
	rec := installStreamSinks(t)
	srv, _ := queueCountingServer(t, 1<<30, http.StatusForbidden, queueTicketL1())

	gate := newStreamHeadGate(2 * time.Second)
	pumpDone := startPump(srv.URL, gate)

	verdict, decided := gate.awaitHandoff(2 * time.Second)
	if !decided || verdict.err == nil {
		t.Fatalf("exhaustion must reach the hand-off as a failure, verdict=%+v decided=%v", verdict, decided)
	}
	if verdict.status != http.StatusForbidden {
		t.Fatalf("the ticket's own status must ride the verdict, got %d", verdict.status)
	}
	if !strings.Contains(verdict.err.Error(), queueMarker) {
		t.Fatalf("the verdict error must be the actionable queue line: %v", verdict.err)
	}
	mustFinish(t, pumpDone)
	if len(rec.delivered()) != 0 {
		t.Fatalf("nothing may be delivered on a ticket, got %q", rec.delivered())
	}
}

func TestPumpNonQueueFailureStillFailsImmediately(t *testing.T) {
	installFastQueueWait(t)
	rec := installStreamSinks(t)
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"free quota exhausted"}}`)
	}))
	t.Cleanup(srv.Close)

	pumpDone := startPump(srv.URL, nil)
	rec.waitFor(t, 1)
	mustFinish(t, pumpDone)

	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("a non-queue 4xx must not be retried, hits=%d", *hits)
	}
	if len(rec.inBand()) != 1 || !strings.Contains(rec.inBand()[0], "429") {
		t.Fatalf("the ordinary failure path must be preserved: %q", rec.inBand())
	}
}

// --- the synchronous collector (unit level) -----------------------------------

func TestCollectQueueTicketRetryPolicyIntegration(t *testing.T) {
	// collectUpstreamStreamQoder dials endpointChatFor (a real upstream host),
	// so the HTTP leg cannot run hermetically. What CAN run hermetically is
	// the decision table its loop consumes: a detected ticket within budget
	// retries, an exhausted one fails with the rendered line.
	waits := installFastQueueWait(t)

	// Budget arithmetic the collector loop performs: three waits, then stop.
	waited := time.Duration(0)
	rounds := 1
	for attempt := 0; queueRetryAllowed(attempt, waited, 30*time.Second); attempt++ {
		if !queueWaitFn(nil, queueWaitFor(30*time.Second)) {
			t.Fatal("nil-context wait must always complete")
		}
		waited += queueWaitFor(30 * time.Second)
		rounds++
	}
	if rounds != queueMaxAttempts {
		t.Fatalf("expected %d rounds, got %d", queueMaxAttempts, rounds)
	}
	if len(*waits) != queueMaxAttempts-1 {
		t.Fatalf("expected %d waits, got %d", queueMaxAttempts-1, len(*waits))
	}
	if waited != 90*time.Second {
		t.Fatalf("cumulative wait: got %v, want 90s", waited)
	}
	// The terminal error the collector composes on exhaustion carries both
	// the marker and the history the classifiers key on.
	base := errors.New(queueLimitLine(parseQueueSignal(queueTicketL1())))
	terminal := fmt.Errorf("collect: %v（已自动重试%d次共%.0fs仍排队）", base, queueMaxAttempts-1, waited.Seconds())
	if !isQueueLimitFailure(terminal.Error()) {
		t.Fatalf("terminal error must classify as queue_limit: %v", terminal)
	}
}
