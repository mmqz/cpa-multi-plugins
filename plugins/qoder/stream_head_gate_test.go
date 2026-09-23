package main

// The stream head gate (config `stream_head_timeout`, v0.12.85).
//
// QoderWork reports account/quota failures as a 200-OK SSE response carrying an
// error envelope (statusCodeValue>=400, or {"error":...} in the inner body).
// Because the async hand-off committed the HTTP status before the pump had read
// anything, every one of those used to reach the host as in-band error *text*
// behind a successful 200 — no rotation, no cooldown, and a client that saw an
// empty answer. These tests push real gateway frames through the real
// conversion path (qoderUnwrapFrame + cleanChunkJSON + pumpUpstreamStream) and
// pin the properties the gate must keep: a failure before the first answer
// becomes a failed request carrying the frame's own status, an answered stream
// still fails in-band without losing the answer, silence releases the hand-off
// on schedule, and window 0 is byte-identical to v0.12.84.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedStream captures what a pump run actually handed the client
// (host.stream.emit payloads) and what it reported in-band.
type recordedStream struct {
	writes chan struct{} // one non-blocking token per recorded event
	mu     sync.Mutex
	chunks []string
	faults []string
}

func newRecordedStream() *recordedStream {
	return &recordedStream{writes: make(chan struct{}, 16)}
}

func (r *recordedStream) emit(_ string, payload []byte) error {
	r.mu.Lock()
	r.chunks = append(r.chunks, string(payload))
	r.mu.Unlock()
	r.signal()
	return nil
}

func (r *recordedStream) fault(_, message string) {
	r.mu.Lock()
	r.faults = append(r.faults, message)
	r.mu.Unlock()
	r.signal()
}

func (r *recordedStream) signal() {
	select {
	case r.writes <- struct{}{}:
	default:
	}
}

// waitFor blocks until the pump has recorded want sink events. It is a bounded
// wait on a condition, never a wall-clock sleep: a healthy run returns in
// microseconds, and a run that goes silent fails the test instead of hanging.
func (r *recordedStream) waitFor(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		got := len(r.chunks) + len(r.faults)
		r.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-r.writes:
		case <-deadline:
			t.Fatalf("pump stalled after %d/%d stream events (chunks=%q faults=%q)", got, want, r.delivered(), r.inBand())
		}
	}
}

func (r *recordedStream) delivered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.chunks...)
}

func (r *recordedStream) inBand() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.faults...)
}

// installStreamSinks points the pump's two host-facing sinks at a recorder for
// the duration of one test.
func installStreamSinks(t *testing.T) *recordedStream {
	t.Helper()
	rec := newRecordedStream()
	origEmit, origFault := streamEmitSink, streamErrorSink
	streamEmitSink, streamErrorSink = rec.emit, rec.fault
	t.Cleanup(func() { streamEmitSink, streamErrorSink = origEmit, origFault })
	return rec
}

// qoderFrame renders one upstream line exactly as the QoderWork gateway does:
// an SSE data line whose envelope carries the inner chunk as a JSON *string*
// (one level of escaping, the shape qoderUnwrapFrame unmarshals) plus a numeric
// statusCodeValue.
func qoderFrame(t *testing.T, status int, inner string) string {
	t.Helper()
	body, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("encode inner body: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{
		"headers":         map[string]any{"Content-Type": []string{"application/json"}},
		"body":            json.RawMessage(body),
		"statusCodeValue": status,
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return "data:" + string(envelope) + "\n\n"
}

// The three frame kinds the gate has to tell apart: a role-only opener, a frame
// that really answers, and a frame carrying an upstream failure.
func qoderOpener(t *testing.T) string {
	return qoderFrame(t, http.StatusOK, `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
}

func qoderAnswer(t *testing.T, text string) string {
	return qoderFrame(t, http.StatusOK, `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":`+qoderJSONString(text)+`}}]}`)
}

func qoderFault(t *testing.T, status int, message string) string {
	return qoderFrame(t, status, `{"error":{"message":`+qoderJSONString(message)+`}}`)
}

// qoderBodyFault is the shape with no useful status at all: an error object
// riding a 200-OK envelope.
func qoderBodyFault(t *testing.T, message string) string {
	return qoderFrame(t, http.StatusOK, `{"error":{"message":`+qoderJSONString(message)+`}}`)
}

func qoderDone() string { return `data:{"body":"[DONE]"}` + "\n\n" }

func qoderJSONString(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

// newQoderSSEServer serves one buffered SSE body, the way hostHTTPDoStream's
// test fallback consumes it.
func newQoderSSEServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runPump drives the real async pump against url. gate == nil is the disabled
// configuration (stream_head_timeout: 0).
func runPump(url string, gate *streamHeadGate) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("encoded-body"))
	if err != nil {
		panic(err)
	}
	pumpUpstreamStream(req, nil, "stream-head-gate", false,
		"qmodel_preview", "qmodel_preview", "", time.Now(), "", "", gate)
}

// startPump runs the pump in the goroutine shape handleExecStream uses, and
// hands back a channel that closes when it exits.
func startPump(url string, gate *streamHeadGate) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPump(url, gate)
	}()
	return done
}

// mustFinish fails the test if the pump goroutine is still alive: an abandoned
// pump (or one blocked reporting to a hand-off that gave up) is precisely the
// leak the gate must never introduce.
func mustFinish(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump goroutine outlived the hand-off — the gate leaked it")
	}
}

// TestQoderFrameBuilderMatchesTheRealUnwrapper pins the fixtures against the
// production parser: an envelope the tests build must unwrap the same way a
// captured gateway frame does, or every gate assertion below is vacuous.
func TestQoderFrameBuilderMatchesTheRealUnwrapper(t *testing.T) {
	body, meaningful, err := qoderUnwrapFrame(qoderAnswer(t, "hi"))
	if err != nil || !meaningful {
		t.Fatalf("answer envelope must unwrap clean: body=%q meaningful=%v err=%v", body, meaningful, err)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("answer envelope unwrapped to %q — the escaping is wrong", body)
	}
	if !streamChunkAnswers(cleanChunkJSON(body)) {
		t.Fatalf("the builder's answer frame is not recognised as an answer: %q", body)
	}
	if _, _, err := qoderUnwrapFrame(qoderFault(t, http.StatusTooManyRequests, "boom")); err == nil {
		t.Fatal("the builder's fault envelope must surface as a frame error")
	}
	if _, meaningful, _ := qoderUnwrapFrame(qoderOpener(t)); !meaningful {
		t.Fatal("a role-only opener still counts toward the empty-stream guard")
	}
	if _, _, err := qoderUnwrapFrame(qoderDone()); err != nil {
		t.Fatalf("[DONE] must stay clean: %v", err)
	}
}

// --- 1. failure before the first answer -> failed request, real status -------

func TestHeadGateAbortsHandoffWithFrameStatus(t *testing.T) {
	rec := installStreamSinks(t)
	srv := newQoderSSEServer(t, http.StatusOK,
		qoderOpener(t)+
			qoderFault(t, http.StatusTooManyRequests, "free quota exhausted")+
			qoderAnswer(t, "text the client must never see"))

	gate := newStreamHeadGate(2 * time.Second)
	if gate == nil {
		t.Fatal("a positive window must build a gate")
	}
	pumpDone := startPump(srv.URL, gate)

	verdict, decided := gate.awaitHandoff(2 * time.Second)
	if !decided || verdict.err == nil {
		t.Fatalf("the pre-answer frame error must come back to the hand-off, got verdict=%+v decided=%v", verdict, decided)
	}
	if verdict.status != http.StatusTooManyRequests {
		t.Fatalf("host cooldown keys on the frame's own status: got %d, want 429", verdict.status)
	}
	if !strings.Contains(verdict.err.Error(), "free quota exhausted") {
		t.Fatalf("the upstream message must survive into the envelope: %q", verdict.err)
	}
	// The status has to cross the RPC boundary, or the host cannot act on it.
	raw := errorEnvelopeFor(&statusError{status: verdict.status, err: verdict.err})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.OK || env.Error == nil {
		t.Fatalf("failure envelope malformed: %s err=%v", raw, err)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429 so the host rotates the credential", env.Error.HTTPStatus)
	}

	// Zero chunks delivered: a hand-off that fails must not have put even the
	// role opener on the wire, and the failure must not be double-reported.
	mustFinish(t, pumpDone)
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("aborted hand-off delivered %d chunk(s): %q", len(got), got)
	}
	if got := rec.inBand(); len(got) != 0 {
		t.Fatalf("the failure must be reported once, as the envelope; in-band: %q", got)
	}
}

func TestHeadGateAbortsOnUpstreamHTTPStatus(t *testing.T) {
	rec := installStreamSinks(t)
	srv := newQoderSSEServer(t, http.StatusPaymentRequired, `{"error":{"message":"credits exhausted"}}`)
	gate := newStreamHeadGate(2 * time.Second)
	pumpDone := startPump(srv.URL, gate)

	verdict, decided := gate.awaitHandoff(2 * time.Second)
	if !decided || verdict.err == nil {
		t.Fatalf("a hard status before any chunk is the cheapest pre-hand-off failure to catch, got %+v", verdict)
	}
	if verdict.status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want the upstream's own 402", verdict.status)
	}
	mustFinish(t, pumpDone)
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("nothing may reach the client: %q", got)
	}
	if got := rec.inBand(); len(got) != 0 {
		t.Fatalf("no in-band duplicate expected, got %q", got)
	}
}

// --- 2. no evidence -> conservative status, never an invented one ------------

func TestHeadGateStatusIsConservativeWithoutEvidence(t *testing.T) {
	rec := installStreamSinks(t)
	srv := newQoderSSEServer(t, http.StatusOK, qoderBodyFault(t, "upstream exploded"))
	gate := newStreamHeadGate(2 * time.Second)
	pumpDone := startPump(srv.URL, gate)

	verdict, decided := gate.awaitHandoff(2 * time.Second)
	if !decided || verdict.err == nil {
		t.Fatalf(`a 200-OK envelope carrying {"error":...} is still a failure, got %+v`, verdict)
	}
	if verdict.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want the 502 fallback", verdict.status)
	}
	mustFinish(t, pumpDone)
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("aborted hand-off delivered chunks: %q", got)
	}
}

func TestHeadGateStatusTable(t *testing.T) {
	cases := []struct {
		name  string
		frame int
		want  int
	}{
		{"frame 429 rides through", http.StatusTooManyRequests, 429},
		{"frame 402 rides through", http.StatusPaymentRequired, 402},
		{"frame 503 rides through", http.StatusServiceUnavailable, 503},
		{"error body on 200 falls back", http.StatusOK, 502},
		{"no status at all falls back", 0, 502},
		// 401/402/403 cool the model for 30 minutes host-side and 404 for 12
		// hours, so a number we did not read off the wire must never reach the
		// table — guessing here is what the requirement forbids.
		{"out of range falls back", 9999, 502},
		{"redirect is not an error code", http.StatusFound, 502},
	}
	for _, tc := range cases {
		if got := headGateStatus(tc.frame); got != tc.want {
			t.Errorf("%s: headGateStatus(%d) = %d, want %d", tc.name, tc.frame, got, tc.want)
		}
	}
	if got := frameGateStatus(errors.New("plain failure")); got != 0 {
		t.Fatalf("frameGateStatus(non-envelope error) = %d, want 0", got)
	}
	if got := frameGateStatus(&qoderFrameError{msg: "x", status: 429}); got != 429 {
		t.Fatalf("frameGateStatus must recover the frame status, got %d", got)
	}
	// The in-band wording is unchanged by the richer error type: the gate must
	// not alter what today's deployments already send on the wire.
	frameErr := &qoderFrameError{msg: "qoder upstream error: quota"}
	if frameErr.Error() != "qoder upstream error: quota" {
		t.Fatalf("error wording changed: %q", frameErr.Error())
	}
}

// TestFrameErrorStaysStatusLessOutsideGate pins the scope of the change: the
// non-stream aggregator and the synchronous collector hand the very same frame
// error straight to the host, and there it must keep reporting no HTTP status
// at all (v0.12.84 behavior). Promoting it into an http_status would start
// cooling credentials on frames that never cooled them before, outside the
// opt-in gate.
func TestFrameErrorStaysStatusLessOutsideGate(t *testing.T) {
	stream := qoderOpener(t) + qoderFault(t, http.StatusTooManyRequests, "quota exhausted") + qoderDone()
	_, errAggregate := aggregateQoderSSE(strings.NewReader(stream), "glm-4.6")
	if errAggregate == nil {
		t.Fatal("the aggregator must still reject an error envelope")
	}
	var env envelope
	raw := errorEnvelopeFor(errAggregate)
	if err := json.Unmarshal(raw, &env); err != nil || env.OK || env.Error == nil {
		t.Fatalf("failure envelope malformed: %s err=%v", raw, err)
	}
	if env.Error.HTTPStatus != 0 {
		t.Fatalf("non-gated paths must not start reporting a status: %d (%q)", env.Error.HTTPStatus, env.Error.Message)
	}
	// Same failure, read by the gate, does yield the frame's real number.
	if got := headGateStatus(frameGateStatus(errAggregate)); got != http.StatusTooManyRequests {
		t.Fatalf("headGateStatus(frameGateStatus(err)) = %d, want 429", got)
	}
	// collectUpstreamStreamQoder surfaces the same error type on its frame path;
	// the wrapper the head gate returns is the only place a status is attached.
	var frameErr *qoderFrameError
	if !errors.As(errAggregate, &frameErr) {
		t.Fatalf("aggregator error must stay a *qoderFrameError, got %T", errAggregate)
	}
	if _, ok := any(errAggregate).(interface{ StatusCode() int }); ok {
		t.Fatal("a frame error must not implement StatusCode() — see the qoderFrameError doc")
	}
}

// --- 3. already answering -> the stream keeps flowing in-band ---------------

func TestHeadGateReleasesAfterAnswerAndKeepsFaultInBand(t *testing.T) {
	rec := installStreamSinks(t)
	srv := newQoderSSEServer(t, http.StatusOK,
		qoderOpener(t)+
			qoderAnswer(t, "the real answer")+
			qoderFault(t, http.StatusInternalServerError, "mid-stream upstream boom")+
			qoderDone())

	gate := newStreamHeadGate(2 * time.Second)
	pumpDone := startPump(srv.URL, gate)

	verdict, decided := gate.awaitHandoff(2 * time.Second)
	if !decided {
		t.Fatal("an answering stream must resolve the hand-off immediately, not on the timeout")
	}
	if verdict.err != nil {
		t.Fatalf("bytes were already on the wire, so the stream must keep going: %v", verdict.err)
	}

	// opener + answer delivered, then the fault in-band: exactly the historical
	// three events in the historical order.
	rec.waitFor(t, 3)
	mustFinish(t, pumpDone)
	delivered := rec.delivered()
	if len(delivered) != 2 {
		t.Fatalf("want opener + answer delivered, got %q", delivered)
	}
	if !strings.Contains(delivered[0], `"role":"assistant"`) {
		t.Fatalf("the role opener must still go out first: %q", delivered[0])
	}
	if !strings.Contains(delivered[1], "the real answer") {
		t.Fatalf("the answer must not be swallowed by the later fault: %q", delivered[1])
	}
	faults := rec.inBand()
	if len(faults) != 1 || !strings.Contains(faults[0], "mid-stream upstream boom") {
		t.Fatalf("a post-answer fault stays in-band, got %q", faults)
	}
}

func TestStreamChunkAnswersIgnoresRoleOnlyOpener(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
		want  bool
	}{
		{"role opener", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{"empty delta", `{"choices":[{"delta":{}}]}`, false},
		{"role plus empty reasoning", `{"choices":[{"delta":{"role":"assistant","reasoning_content":""}}]}`, false},
		{"content", `{"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"reasoning content", `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, true},
		{"tool call delta", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}`, true},
		{"no choices", `{"usage":{"total_tokens":3}}`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		if got := streamChunkAnswers(tc.chunk); got != tc.want {
			t.Errorf("%s: streamChunkAnswers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- 4. silence -> the window expires and the hand-off proceeds -------------

func TestHeadGateWindowReleasesSilentStream(t *testing.T) {
	rec := installStreamSinks(t)
	// The upstream accepts the request, sends a keep-alive, then says nothing at
	// all until the test lets it finish — so the pump has no verdict to offer.
	var once sync.Once
	upstreamDone := make(chan struct{})
	unblock := func() { once.Do(func() { close(upstreamDone) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": keep-alive\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-upstreamDone
		_, _ = io.WriteString(w, qoderFault(t, http.StatusBadGateway, "late failure"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)

	gate := newStreamHeadGate(80 * time.Millisecond)
	pumpDone := startPump(srv.URL, gate)

	// 80ms window: the hand-off must give up on its own — never hang on a
	// silent upstream, never report a failure it has no evidence for.
	start := time.Now()
	verdict, decided := gate.awaitHandoff(80 * time.Millisecond)
	if decided {
		t.Fatalf("a silent upstream produced a verdict: %+v", verdict)
	}
	if verdict.err != nil {
		t.Fatalf("timeout must release, not fail: %v", verdict.err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the window is not honoured: waited %s", elapsed)
	} else if elapsed < 30*time.Millisecond {
		// Without this floor the assertion would also pass on a gate that never
		// waited at all, which is the other half of "waits up to the window".
		t.Fatalf("the hand-off did not wait for the stream head: %s", elapsed)
	}
	// Claiming the hand-off also closes the gate: a late pump failure belongs
	// in-band only, because the 200 is already committed.
	if gate.abort(http.StatusBadGateway, errors.New("late")) {
		t.Fatal("abort must be refused after the hand-off was claimed")
	}

	unblock()
	rec.waitFor(t, 1)
	mustFinish(t, pumpDone)
	if got := rec.inBand(); len(got) != 1 || !strings.Contains(got[0], "late failure") {
		t.Fatalf("the late failure must stay in-band after release, got %q", got)
	}
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("nothing was answered, so nothing should have been delivered: %q", got)
	}
}

// --- 5. stream_head_timeout: 0 -> byte-identical to v0.12.84 ----------------

func TestHeadGateDisabledKeepsHistoricalBehaviour(t *testing.T) {
	if got := newStreamHeadGate(0); got != nil {
		t.Fatal("the default window must build no gate")
	}
	var absent *streamHeadGate
	if _, decided := absent.awaitHandoff(time.Millisecond); decided {
		t.Fatal("a missing gate must never produce a verdict")
	}
	if absent.abort(http.StatusBadGateway, errors.New("x")) {
		t.Fatal("a missing gate must never take a failure back")
	}
	absent.release() // must be nil-safe

	body := qoderOpener(t) +
		qoderAnswer(t, "the real answer") +
		qoderFault(t, http.StatusInternalServerError, "mid-stream upstream boom") +
		qoderDone()

	// Gate off: the pump runs blind and reports the frame error in-band, with
	// the opener and the answer both delivered (the pre-v0.12.85 behavior).
	ungatedSrv := newQoderSSEServer(t, http.StatusOK, body)
	ungated := installStreamSinks(t)
	runPump(ungatedSrv.URL, nil) // synchronous: there is no hand-off to wait for
	if _, decided := (*streamHeadGate)(nil).awaitHandoff(time.Millisecond); decided {
		t.Fatal("disabled config must never wait")
	}

	// Gate on, same frames: the client-visible bytes and the in-band wording
	// must come out identical, because releasing early only removes latency.
	gatedSrv := newQoderSSEServer(t, http.StatusOK, body)
	gated := installStreamSinks(t)
	gate := newStreamHeadGate(2 * time.Second)
	pumpDone := startPump(gatedSrv.URL, gate)
	if verdict, decided := gate.awaitHandoff(2 * time.Second); !decided || verdict.err != nil {
		t.Fatalf("gated run must release on the answer, got %+v", verdict)
	}

	ungated.waitFor(t, 3)
	gated.waitFor(t, 3)
	mustFinish(t, pumpDone)
	if a, b := ungated.delivered(), gated.delivered(); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("the gate changed the bytes on the wire:\n off=%q\n on =%q", a, b)
	}
	if a, b := ungated.inBand(), gated.inBand(); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("the gate changed the in-band report:\n off=%q\n on =%q", a, b)
	}
	if got := ungated.delivered(); len(got) != 2 || !strings.Contains(got[1], "the real answer") {
		t.Fatalf("regression baseline changed: %q", got)
	}
	if got := ungated.inBand(); len(got) != 1 || !strings.HasPrefix(got[0], "qoder upstream error: ") {
		t.Fatalf("the historical envelope wording changed: %q", got)
	}
}

// --- 6. config plumbing ------------------------------------------------------

func TestStreamHeadTimeoutConfig(t *testing.T) {
	t.Cleanup(func() { setStreamHeadTimeout(0) })

	if got := streamHeadTimeout(); got != 0 {
		// The default must be opt-in: nobody gets a new hand-off delay just by
		// upgrading the plugin.
		t.Fatalf("default streamHeadTimeout = %s, want 0 (disabled)", got)
	}
	setStreamHeadTimeout(-1)
	if got := streamHeadTimeout(); got != 0 {
		t.Fatalf("negative window = %s, want clamped to 0", got)
	}
	setStreamHeadTimeout(3)
	if got := streamHeadTimeout(); got != 3*time.Second {
		t.Fatalf("streamHeadTimeout = %s, want 3s", got)
	}
	if newStreamHeadGate(streamHeadTimeout()) == nil {
		t.Fatal("a configured window must enable the gate")
	}

	// configure() decodes plugin config line by line: pin the key, the negative
	// clamp and the "a typo leaves it off" rule. config_yaml travels as bytes,
	// so the request has to carry it the way encoding/json renders []byte.
	cases := []struct {
		yaml string
		want time.Duration
	}{
		{"stream_head_timeout: 2", 2 * time.Second},
		{"stream_head_timeout: 5s", 0},
		{"stream_head_timeout: -7", 0},
		{`stream_head_timeout: "4"`, 4 * time.Second},
		{"checkin_auto: true", 0},
	}
	for _, tc := range cases {
		request, err := json.Marshal(map[string]any{"config_yaml": []byte(tc.yaml)})
		if err != nil {
			t.Fatalf("encode config request: %v", err)
		}
		configure(request)
		if got := streamHeadTimeout(); got != tc.want {
			t.Errorf("configure(%q): streamHeadTimeout = %s, want %s", tc.yaml, got, tc.want)
		}
	}
}
