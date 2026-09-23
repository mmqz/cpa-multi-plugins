package main

// stream_head_timeout (opt-in async-stream head gate).
//
// Upstream sends account-level failures two ways on an async stream: either a
// real HTTP >=400, or — worse — HTTP 200 + an in-stream error frame. Because the
// executor returns the stream-open envelope as soon as it has a stream_id, an
// in-band error frame can only travel as lossy TEXT: the HTTP status is gone, so
// the host never cools the drained credential and the client sees a "successful
// empty answer". The head gate holds the hand-off until the pump reports its
// first decisive event, turning a PRE-answer failure into a normal status-bearing
// failed envelope — while an already-answered stream keeps flowing and its
// post-answer errors stay in-band (bytes on the wire cannot be rolled back).
//
// These pin the four contract points, structurally aligned with the trae
// reference (#11): status mapping, abort-before-handoff, release-after-answer,
// and the silent-timeout / disabled regressions. Tests drive the real frame loop
// (pumpStreamFrames) through a recording sink, so they observe what actually
// reaches the host stream.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- real upstream SSE frames (the shapes workBuddy actually receives) ---

const (
	headRoleOpener = `data: {"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	headContent    = `data: {"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`
	headReasoning  = `data: {"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"reasoning_content":"思考中"}}]}`
	// An HTTP-200 in-stream quota frame: the account is drained, but the
	// transport status is 200, so only the body carries the signal.
	headQuotaFrame = `data: {"code":11199,"error":{"message":"insufficient credits"}}`
	// The model-scoped 6004 — deliberately status-less (it throttles one model,
	// not the credential).
	head6004Frame = `data: {"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置"}`
)

// streamRecorder captures what the pump commits to the host stream.
type streamRecorder struct {
	mu     sync.Mutex
	chunks []string
	errors []string
}

func (r *streamRecorder) emit(payload []byte) error {
	r.mu.Lock()
	r.chunks = append(r.chunks, string(payload))
	r.mu.Unlock()
	return nil
}

func (r *streamRecorder) emitError(message string) {
	r.mu.Lock()
	r.errors = append(r.errors, message)
	r.mu.Unlock()
}

func (r *streamRecorder) snapshot() (chunks, errs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.chunks...), append([]string(nil), r.errors...)
}

// blockingReader never yields a byte until released, modelling a silent / stalled
// upstream that must trip the head-window timeout rather than hang the request.
type blockingReader struct{ release chan struct{} }

func (b *blockingReader) Read([]byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

// driveStreamHead runs pumpStreamFrames in a goroutine and blocks in
// awaitStreamHead, exactly as handleExecStream's async hand-off does. Returns the
// pre-answer failure (nil to proceed) plus the sink and loop result.
func driveStreamHead(t *testing.T, reader io.Reader, gate *streamHeadGate) (error, *streamRecorder, bool) {
	t.Helper()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	rec := &streamRecorder{}
	done := make(chan bool, 1)
	go func() {
		_, handled := pumpStreamFrames(scanner, rec, gate, false, &sseUsageCollector{}, "gpt", "gpt", "uid-1", time.Now(), nil)
		done <- handled
	}()
	headErr := awaitStreamHead(gate)
	handled := <-done
	return headErr, rec, handled
}

// envelopeStatus renders err through the real RPC serializer and reports the
// HTTP status the host would see (0 = plain / status-less).
func envelopeStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var env envelope
	if e := json.Unmarshal(errorEnvelopeFor(err), &env); e != nil {
		t.Fatalf("decode envelope: %v", e)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected a failed envelope, got %+v", env)
	}
	return env.Error.HTTPStatus
}

// TestStreamHeadFaultStatusMapping reuses the chat_error classifier and pins the
// account-vs-request decisions, including the deliberate 6004 status-less carve-
// out. payload is the raw frame body, so the classifiers see the real envelope.
func TestStreamHeadFaultStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    int
	}{
		{"credit exhausted -> 402", `{"code":11199,"msg":"insufficient credits"}`, 402},
		{"dead token -> 401", `{"code":11101,"msg":"unauthorized, token expired"}`, 401},
		{"throttle (non-6004) -> 429", `{"code":11120,"msg":"too many requests, slow down"}`, 429},
		{"business 403 -> 403", `{"code":11140,"msg":"no permission to access"}`, 403},
		{"model-scoped 6004 stays status-less", head6004Frame, 0},
		{"prompt-too-long is request-level", `{"code":11115,"msg":"prompt is too long"}`, 0},
		{"bare event:error has nothing to map", "event:error", 0},
		{"opaque body stays status-less", `{"detail":"something odd"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := streamFaultError(errors.New("workbuddy upstream error"), tc.payload, nil)
			if got := envelopeStatus(t, err); got != tc.want {
				t.Fatalf("status = %d, want %d (err=%v)", got, tc.want, err)
			}
		})
	}
}

// TestStreamChunkAnswersOnlyContentCounts locks the gate's content test: a
// role-only opener must NOT count as answered (else the gate would release
// before a pre-answer error surfaced), while content or reasoning must. The
// pump feeds this the cleaned JSON (already data:-stripped), so strip here too.
func TestStreamChunkAnswersOnlyContentCounts(t *testing.T) {
	if streamChunkAnswers([]byte(stripDataPrefix(headRoleOpener))) {
		t.Fatal("a role-only opener frame must not be treated as the start of an answer")
	}
	if !streamChunkAnswers([]byte(stripDataPrefix(headContent))) {
		t.Fatal("a content delta must count as answered")
	}
	if !streamChunkAnswers([]byte(stripDataPrefix(headReasoning))) {
		t.Fatal("a reasoning_content delta must count as answered")
	}
	if streamChunkAnswers([]byte(`{"choices":[{"delta":{"content":""}}]}`)) {
		t.Fatal("an empty content delta must not count as answered")
	}
}

// TestStreamHeadAbortsBeforeHandoffOnQuotaFrame is the core fix: an in-stream
// error frame that lands before the model answers must surface as a failed
// request carrying the HTTP status the cooldown layer needs — and NOTHING may
// already have reached the client.
func TestStreamHeadAbortsBeforeHandoffOnQuotaFrame(t *testing.T) {
	gate := newStreamHeadGate(2 * time.Second)
	body := headRoleOpener + "\n" + headQuotaFrame + "\n"

	headErr, rec, handled := driveStreamHead(t, strings.NewReader(body), gate)

	if headErr == nil {
		t.Fatal("a pre-answer quota frame must fail the hand-off, got nil")
	}
	if got := envelopeStatus(t, headErr); got != http.StatusPaymentRequired {
		t.Fatalf("host would not rotate: envelope status = %d, want 402", got)
	}
	if !handled {
		t.Fatal("the pump must terminate the stream path (handled=true) after the abort")
	}
	chunks, errs := rec.snapshot()
	if len(chunks) != 0 {
		t.Fatalf("zero chunks may reach the client before a failed envelope, got %q", chunks)
	}
	if len(errs) != 0 {
		// The failure is the envelope, not an in-band error: streamEmitError
		// must be suppressed once the hand-off accepted the verdict.
		t.Fatalf("an accepted failure must not also emit in-band, got %q", errs)
	}
}

// TestStreamHeadAbortsOnModelScopedFrameWithoutStatus proves 6004 keeps its
// credential-healthy status-less treatment even when surfaced as an envelope.
func TestStreamHeadAbortsOnModelScopedFrameWithoutStatus(t *testing.T) {
	gate := newStreamHeadGate(2 * time.Second)
	body := headRoleOpener + "\n" + head6004Frame + "\n"

	headErr, rec, _ := driveStreamHead(t, strings.NewReader(body), gate)

	if headErr == nil {
		t.Fatal("a pre-answer 6004 frame must still fail the hand-off (no fake answer)")
	}
	if got := envelopeStatus(t, headErr); got != 0 {
		t.Fatalf("6004 must stay status-less (credential stays healthy), got %d", got)
	}
	if chunks, _ := rec.snapshot(); len(chunks) != 0 {
		t.Fatalf("nothing may be delivered before the envelope, got %q", chunks)
	}
}

// TestStreamHeadLetsAnsweredStreamFailInBand is the anti-rollback guard: once the
// model has really answered, the hand-off is released, the answer reaches the
// client, and a later error frame stays in-band (the envelope is already 200).
func TestStreamHeadLetsAnsweredStreamFailInBand(t *testing.T) {
	gate := newStreamHeadGate(2 * time.Second)
	body := headRoleOpener + "\n" + headContent + "\n" + headQuotaFrame + "\n"

	headErr, rec, _ := driveStreamHead(t, strings.NewReader(body), gate)

	if headErr != nil {
		t.Fatalf("bytes were already on the wire, so the hand-off must proceed: %v", headErr)
	}
	chunks, errs := rec.snapshot()
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "你好") {
		t.Fatalf("the answer must not be swallowed: %q", joined)
	}
	// The role opener is buffered then flushed with the answer — nothing lost.
	if !strings.Contains(joined, "assistant") {
		t.Fatalf("the leading role frame must still be delivered: %q", joined)
	}
	if len(errs) == 0 {
		t.Fatal("a post-answer frame error must still be reported in-band")
	}
	if !strings.Contains(errs[0], "workbuddy upstream error") {
		t.Fatalf("in-band error wording changed: %q", errs[0])
	}
}

// TestStreamHeadReleasesOnCleanAnswerWithoutFault proves a healthy stream hands
// off normally and its frames all reach the client.
func TestStreamHeadReleasesOnCleanAnswerWithoutFault(t *testing.T) {
	gate := newStreamHeadGate(2 * time.Second)
	body := headRoleOpener + "\n" + headContent + "\n" + headReasoning + "\ndata: [DONE]\n"

	headErr, rec, handled := driveStreamHead(t, strings.NewReader(body), gate)

	if headErr != nil {
		t.Fatalf("a clean answer must not fail the hand-off: %v", headErr)
	}
	if handled {
		t.Fatal("a clean stream ending at [DONE] must fall through to the pump tail")
	}
	chunks, errs := rec.snapshot()
	if len(errs) != 0 {
		t.Fatalf("a clean stream must emit no error frames: %q", errs)
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "你好") || !strings.Contains(joined, "思考中") {
		t.Fatalf("all answer frames must be delivered: %q", joined)
	}
}

// TestStreamHeadSilentTimeoutReleasesHandoff asserts a stalled/silent upstream
// is released normally (never a hang, never a failure) after roughly the
// configured window — the wall-clock assertion is the point of the opt-in timer.
func TestStreamHeadSilentTimeoutReleasesHandoff(t *testing.T) {
	const timeout = 120 * time.Millisecond
	gate := newStreamHeadGate(timeout)
	br := &blockingReader{release: make(chan struct{})}

	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	rec := &streamRecorder{}
	done := make(chan struct{})
	go func() {
		pumpStreamFrames(scanner, rec, gate, false, &sseUsageCollector{}, "gpt", "gpt", "uid-1", time.Now(), nil)
		close(done)
	}()

	start := time.Now()
	headErr := awaitStreamHead(gate)
	elapsed := time.Since(start)

	// Unblock the pump so the goroutine exits (no leak) once the hand-off is done.
	close(br.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump goroutine leaked: it never returned after the head timeout")
	}

	if headErr != nil {
		t.Fatalf("a silent stream must not fail: %v", headErr)
	}
	if elapsed < timeout {
		t.Fatalf("returned early: waited %v, want >= %v", elapsed, timeout)
	}
	if elapsed > time.Second {
		t.Fatalf("did not release near the timeout: waited %v", elapsed)
	}
	if chunks, errs := rec.snapshot(); len(chunks) != 0 || len(errs) != 0 {
		t.Fatalf("a silent stream must emit nothing before releasing: chunks=%q errs=%q", chunks, errs)
	}
}

// TestStreamHeadDisabledKeepsOldBehavior is the regression gate for
// stream_head_timeout == 0: with no gate the pump is byte-for-byte the pre-
// feature one — a pre-answer error frame goes in-band, and the role opener that
// already streamed is NOT retroactively dropped (no buffering, no envelope).
func TestStreamHeadDisabledKeepsOldBehavior(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader(headRoleOpener + "\n" + headQuotaFrame + "\n"))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	rec := &streamRecorder{}

	_, handled := pumpStreamFrames(scanner, rec, nil, false, &sseUsageCollector{}, "gpt", "gpt", "uid-1", time.Now(), nil)

	chunks, errs := rec.snapshot()
	if len(chunks) != 1 || !strings.Contains(chunks[0], "assistant") {
		t.Fatalf("gate off must emit frames as they arrive (old timing): %q", chunks)
	}
	if !handled || len(errs) != 1 {
		t.Fatalf("gate off must report the error in-band, not as an envelope: handled=%v errs=%q", handled, errs)
	}
}

// TestStreamHeadTimeoutConfigNormalization pins the config layer: absent / 0 /
// negative all normalize to disabled (0), and a positive value round-trips.
func TestStreamHeadTimeoutConfigNormalization(t *testing.T) {
	defer configure(configYAMLEnvelope("enabled: true")) // restore default for other tests

	if got := loadedStreamHeadTimeout(); got != 0 {
		t.Fatalf("default must be 0 (disabled), got %d", got)
	}
	for _, tc := range []struct {
		yaml string
		want int
	}{
		{"stream_head_timeout: 5", 5},
		{"stream_head_timeout: \"7\"", 7},
		{"stream_head_timeout: 0", 0},
		{"stream_head_timeout: -3", 0},
		{"stream_head_timeout: notanumber", 0},
	} {
		configure(configYAMLEnvelope("enabled: true\n" + tc.yaml + "\n"))
		if got := loadedStreamHeadTimeout(); got != tc.want {
			t.Errorf("configure(%q): loadedStreamHeadTimeout() = %d, want %d", tc.yaml, got, tc.want)
		}
	}
}
