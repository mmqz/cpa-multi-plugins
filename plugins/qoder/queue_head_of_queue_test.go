package main

// queue_head_of_queue_test.go — v0.8.71: the queueCount=0 ladder.
//
// Observed 2026-10-29 on a CN deployment: every ticket of one credential
// reported `"queueCount":0` (nobody ahead) while still asking for
// retryAfterSeconds/waitTime 30, and a third consecutive ticket from the same
// credential was admitted with a full stream. Honouring the 30s estimate three
// times made one client request look like a 90s hang, and because the host
// rotates credentials on a failed execution, each rotation paid that ladder
// again — 21-31s per client request. These tests pin the replacement policy:
// an empty-queue ticket is retried on the fast ladder, exactly one credential
// runs that ladder per window, and the pair cools for one window instead of a
// minute afterward. A ticket with a real depth keeps the v0.8.64 behaviour.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- fixtures: the 2026-10-29 ticket, plus its variants ---------------------

// queueHeadTicketL3 is the innermost ticket exactly as observed 2026-10-29:
// the same shape as queueTicketL3 plus an explicit zero depth.
func queueHeadTicketL3() string {
	return `{"isQueued":true,"modelKey":"dfmodel","queueCount":0,"queueType":"p3","retryAfterSeconds":30,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT","waitTime":30}`
}

func queueHeadTicketL2() string {
	return `{"code":"10605","message":` + strconv.Quote(queueHeadTicketL3()) + `,"subCode":"RESOURCE_LIMIT"}`
}

func queueHeadTicketL1() string {
	return `{"code":403,"message":` + strconv.Quote(queueHeadTicketL2()) + `}`
}

// resetQueueFastLadderWindows clears the per-model claim table so one test's
// ladder cannot mask the next one's.
func resetQueueFastLadderWindows() {
	queueFastLadderMu.Lock()
	queueFastLadderAt = map[string]time.Time{}
	queueFastLadderMu.Unlock()
}

// --- parsing ----------------------------------------------------------------

func TestParseQueueSignalHeadOfQueueUsesFastLadder(t *testing.T) {
	sig := parseQueueSignal(queueHeadTicketL1())
	if !sig.detected {
		t.Fatal("the 2026-10-29 ticket must be recognised as a queue ticket")
	}
	if !sig.depthSeen || sig.queueCount != 0 || !sig.emptyQueue {
		t.Fatalf("the reported depth must survive parsing: %+v", sig)
	}
	if sig.retryAfter != queueFastRetryBackoff {
		t.Fatalf("a queueCount=0 ticket must retry on the fast ladder (%v), got %v",
			queueFastRetryBackoff, sig.retryAfter)
	}
	if sig.modelKey != "dfmodel" || sig.subCode != queueSubCode {
		t.Fatalf("metadata lost: %+v", sig)
	}

	line := queueLimitLine(sig)
	for _, want := range []string{queueMarker, queueHeadMarker, "queueCount=0", "dfmodel"} {
		if !strings.Contains(line, want) {
			t.Fatalf("rendered line %q must carry %q", line, want)
		}
	}
	if !isQueueLimitFailure(line) || !isHeadOfQueueFailure(line) {
		t.Fatalf("both classifiers must key on the rendered line: %q", line)
	}
}

func TestParseQueueSignalQuotedDepthStillReadsEmpty(t *testing.T) {
	// The gateway mixes number and quoted-string shapes inside these envelopes;
	// "0" must read as an empty queue, not as an unparseable field.
	body := `{"code":"10605","subCode":"RESOURCE_LIMIT","message":` +
		strconv.Quote(`{"isQueued":true,"modelKey":"qfmodel","queueCount":"0"}`) + `}`
	sig := parseQueueSignal(body)
	if !sig.detected || !sig.emptyQueue || sig.retryAfter != queueFastRetryBackoff {
		t.Fatalf("a quoted zero depth must read as head of the queue: %+v", sig)
	}
}

func TestParseQueueSignalQueuedTicketKeepsTicketInterval(t *testing.T) {
	// A ticket with a real depth behind it is a genuine wait: the 2026-10-20
	// behaviour (honour the interval, retry within the attempt budget) stands.
	body := `{"code":"10605","subCode":"RESOURCE_LIMIT","message":` +
		strconv.Quote(`{"isQueued":true,"modelKey":"qfmodel","queueCount":12,"queueType":"p3","waitTime":10}`) + `}`
	sig := parseQueueSignal(body)
	if sig.emptyQueue {
		t.Fatal("a ticket with 12 requests ahead is not a head-of-queue ticket")
	}
	if sig.retryAfter != 10*time.Second {
		t.Fatalf("the ticket's own interval must stand, got %v", sig.retryAfter)
	}
	if isHeadOfQueueFailure(queueLimitLine(sig)) {
		t.Fatalf("only empty-queue tickets carry %q: %q", queueHeadMarker, queueLimitLine(sig))
	}
}

func TestParseQueueSignalMalformedDepthIsNotHeadOfQueue(t *testing.T) {
	body := `{"code":"10605","subCode":"RESOURCE_LIMIT","message":` +
		strconv.Quote(`{"isQueued":true,"modelKey":"qfmodel","queueCount":"many","waitTime":30}`) + `}`
	sig := parseQueueSignal(body)
	if sig.depthSeen || sig.emptyQueue {
		t.Fatalf("an unparseable depth must not read as an empty queue: %+v", sig)
	}
	if sig.retryAfter != 30*time.Second {
		t.Fatalf("the ticket's interval must stand, got %v", sig.retryAfter)
	}
}

// --- retry policy -----------------------------------------------------------

func TestQueueRetryContinueRunsLadderOncePerWindow(t *testing.T) {
	resetQueueFastLadderWindows()
	sig := parseQueueSignal(queueHeadTicketL1())

	if !queueRetryContinue(0, 0, sig) {
		t.Fatal("the first ticket of a window must run the ladder")
	}
	// The second credential the host rotates to sees the same ticket with
	// attempt 0 — it must fail fast instead of repeating the ladder.
	if queueRetryContinue(0, 0, sig) {
		t.Fatal("a sibling credential inside the same window must not run the ladder again")
	}

	// Inside the claimed window the ladder keeps going to its own ceiling.
	for attempt := 1; attempt < queueFastAttempts-1; attempt++ {
		waited := time.Duration(attempt) * queueFastRetryBackoff
		if !queueRetryContinue(attempt, waited, sig) {
			t.Fatalf("ladder round %d must be allowed (waited=%v)", attempt, waited)
		}
	}
	spent := time.Duration(queueFastAttempts-1) * queueFastRetryBackoff
	if queueRetryContinue(queueFastAttempts-1, spent, sig) {
		t.Fatalf("the ladder must stop after %d rounds", queueFastAttempts)
	}

	// A later client request may claim again.
	if !queueFastLadderClaim(sig.modelKey, time.Now().Add(queueFastLadderWindow+time.Second)) {
		t.Fatal("the window must expire so the next request can run the ladder")
	}
}

func TestQueueRetryContinueRespectsConfiguredBudget(t *testing.T) {
	resetQueueFastLadderWindows()
	queueBudgetMu.Lock()
	orig := queueWaitBudget
	queueWaitBudget = 0
	queueBudgetMu.Unlock()
	defer func() {
		queueBudgetMu.Lock()
		queueWaitBudget = orig
		queueBudgetMu.Unlock()
	}()

	sig := parseQueueSignal(queueHeadTicketL1())
	if queueRetryContinue(0, 0, sig) {
		t.Fatal("queue_wait_budget=0 must disable queue retries outright")
	}
	if queueRetryContinue(1, queueFastRetryBackoff, sig) {
		t.Fatal("queue_wait_budget=0 must disable the ladder mid-flight too")
	}
}

func TestQueueExhaustedNoteHeadOfQueue(t *testing.T) {
	sig := parseQueueSignal(queueHeadTicketL1())
	note := queueExhaustedNote(3, 2100*time.Millisecond, sig)
	if !strings.Contains(note, "快速重试3次") || !strings.Contains(note, "2.1s") {
		t.Fatalf("the note must describe the fast ladder, got %q", note)
	}
	if strings.Contains(note, "已自动重试") {
		t.Fatalf("an empty-queue ticket was not slept on: %q", note)
	}
	line := queueExhaustedLine(sig, 3, 2100*time.Millisecond)
	if !strings.Contains(line, queueHeadMarker) || !strings.Contains(line, "快速重试3次") {
		t.Fatalf("terminal line must carry the marker and the ladder history: %q", line)
	}

	// A ticket with a real depth keeps the v0.8.64 wording the Docker logs
	// already document.
	depth := parseQueueSignal(queueTicketL1())
	if got := queueExhaustedNote(3, 90*time.Second, depth); !strings.Contains(got, "已自动重试3次共90s仍排队") {
		t.Fatalf("the depth ticket's wording must not change: %q", got)
	}
}

func TestRecordUpstreamFailureHeadOfQueueCoolsPairWindow(t *testing.T) {
	recordUpstreamFailure("qauth-head", "dfmodel", 0,
		"qoder upstream error: "+queueLimitLine(parseQueueSignal(queueHeadTicketL1())))
	until := modelCoolingUntil("qauth-head", "dfmodel")
	if until.IsZero() {
		t.Fatal("a head-of-queue ticket must still bench the pair")
	}
	if d := time.Until(until); d > modelCooldownHeadOfQueue+3*time.Second || d <= 0 {
		t.Fatalf("head-of-queue cools for one window (~%v), got %v", modelCooldownHeadOfQueue, d)
	}
	if modelIsCooling("qauth-head", "othermodel") {
		t.Fatal("the head-of-queue cooldown must stay model-scoped")
	}

	// The generic queue ticket (real depth, slept on) keeps the longer cadence.
	recordUpstreamFailure("qauth-head-depth", "dfmodel", 0,
		"qoder upstream error: "+queueLimitLine(parseQueueSignal(queueTicketL1())))
	if d := time.Until(modelCoolingUntil("qauth-head-depth", "dfmodel")); d <= modelCooldownHeadOfQueue+3*time.Second {
		t.Fatalf("a waited-on ticket must keep the one-ticket cadence, got %v", d)
	}
}

// --- the retrying chat paths ------------------------------------------------

func TestPumpHeadOfQueueTicketRetriesOnFastLadder(t *testing.T) {
	resetQueueFastLadderWindows()
	waits := installFastQueueWait(t)
	rec := installStreamSinks(t)
	srv, hits := queueCountingServer(t, 2, http.StatusForbidden, queueHeadTicketL1())

	pumpDone := startPump(srv.URL, nil)
	rec.waitFor(t, 2)
	mustFinish(t, pumpDone)

	if got := atomic.LoadInt32(hits); got != 3 {
		t.Fatalf("two tickets then the answer: hits=%d", got)
	}
	if len(rec.inBand()) != 0 {
		t.Fatalf("a recovered ticket must not report any fault: %q", rec.inBand())
	}
	if !strings.Contains(strings.Join(rec.delivered(), ""), "served after the queue") {
		t.Fatalf("the retried round's answer must reach the client: %q", rec.delivered())
	}
	if len(*waits) != 2 {
		t.Fatalf("two ladder rounds expected, got %v", *waits)
	}
	for _, d := range *waits {
		if d != queueFastRetryBackoff {
			t.Fatalf("the ladder must not sleep the ticket's 30s estimate: %v", *waits)
		}
	}
}

func TestPumpHeadOfQueueTicketInBandFrameRetriesFast(t *testing.T) {
	resetQueueFastLadderWindows()
	waits := installFastQueueWait(t)
	rec := installStreamSinks(t)
	ticketFrame := qoderFrame(t, http.StatusForbidden, queueHeadTicketL1())
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
		t.Fatalf("hits=%d, want 2", *hits)
	}
	if len(*waits) != 1 || (*waits)[0] != queueFastRetryBackoff {
		t.Fatalf("the in-band empty-queue ticket must retry on the fast ladder, waits=%v", *waits)
	}
}
