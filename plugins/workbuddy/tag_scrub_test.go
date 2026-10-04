// tag_scrub_test.go — issue #30: the output-side leading tag-block scrubber.
//
// The reasoning replay (injectReasoningInPlace, issue #5) folds historical
// reasoning into assistant content as <thought> blocks; deepseek-v4.1-flash
// imitates the taught shape and clients (Pi, DSH) render the raw tags. These
// tests pin the three executor paths (pumpStreamFrames,
// aggregateSSEWithCollector, aggregateCompletion) to the invariant the fix
// promises: a leading run of taught tag blocks never reaches the client, real
// content (including legitimate HTML <summary>) is never touched, and every
// degenerate input degrades fail-open to the pre-fix wire shape.
package main

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const scrubSSEHidden = "hidden reasoning"

// feedAll drives the scrubber with the given fragments and returns everything
// released during feeding plus the final flush, concatenated.
func feedAll(t *testing.T, fragments []string) string {
	t.Helper()
	s := newThoughtTagScrubber()
	var b strings.Builder
	for _, f := range fragments {
		out := s.feed(f)
		b.WriteString(out)
		if strings.Contains(out, scrubSSEHidden) {
			t.Fatalf("released text contains the block body mid-stream: %q", out)
		}
	}
	return b.String() + s.flush()
}

func TestThoughtTagScrubber_SingleBlock(t *testing.T) {
	got := feedAll(t, []string{"<thought>\n" + scrubSSEHidden + "\n</thought>\n\nThe answer."})
	if got != "\n\nThe answer." {
		t.Fatalf("block not stripped cleanly: %q", got)
	}
}

func TestThoughtTagScrubber_ConsecutiveBlocksAllTags(t *testing.T) {
	in := "<thought>a</thought>\n<analysis>b</analysis>\n\n<summary>c</summary>\n<think>d</think>\nvisible"
	got := feedAll(t, []string{in})
	if got != "\nvisible" {
		t.Fatalf("consecutive blocks not all stripped: %q", got)
	}
}

func TestThoughtTagScrubber_ByteByByte(t *testing.T) {
	t.Helper()
	full := "<thought>\n" + scrubSSEHidden + "\n</thought>\n\nvisible answer"
	var frags []string
	for i := 0; i < len(full); i++ {
		frags = append(frags, full[i:i+1])
	}
	if got := feedAll(t, frags); got != "\n\nvisible answer" {
		t.Fatalf("byte-by-byte split broke the scrubber: %q", got)
	}
}

func TestThoughtTagScrubber_ChunkedRandomSplits(t *testing.T) {
	full := "<thought>" + scrubSSEHidden + "</thought><analysis>x</analysis>  real text"
	// Deterministic pseudo-random split points (fixed seed — hermetic).
	seed := uint32(1)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	for round := 0; round < 50; round++ {
		var frags []string
		rest := full
		for len(rest) > 0 {
			n := next(len(rest)) + 1
			if n > len(rest) {
				n = len(rest)
			}
			frags = append(frags, rest[:n])
			rest = rest[n:]
		}
		if got := feedAll(t, frags); got != "  real text" {
			t.Fatalf("round %d: split feed produced %q", round, got)
		}
	}
}

func TestThoughtTagScrubber_RealContentFirst(t *testing.T) {
	// The first byte decides: once real content starts, nothing is ever
	// stripped again — a legitimate HTML summary mid-answer must survive.
	in := "Hello <summary>HTML</summary> and <thought>fake</thought>"
	if got := feedAll(t, []string{in}); got != in {
		t.Fatalf("real-content-first answer was modified: %q", got)
	}
}

func TestThoughtTagScrubber_HTMLDetailsSummary(t *testing.T) {
	in := "\n\n<details>\n<summary>More</summary>\nbody\n</details>"
	if got := feedAll(t, []string{in}); got != in {
		t.Fatalf("HTML details/summary was touched: %q", got)
	}
}

func TestThoughtTagScrubber_UnclosedBlockFailOpen(t *testing.T) {
	s := newThoughtTagScrubber()
	if out := s.feed("<thought>truncated " + scrubSSEHidden); out != "" {
		t.Fatalf("unclosed block should hold, released %q", out)
	}
	if tail := s.flush(); tail != "<thought>truncated "+scrubSSEHidden {
		t.Fatalf("flush must release the raw tail verbatim: %q", tail)
	}
}

func TestThoughtTagScrubber_CapOverflow(t *testing.T) {
	huge := "<thought>" + strings.Repeat("x", tagScrubHoldLimit+16)
	s := newThoughtTagScrubber()
	out := s.feed(huge)
	if out != huge {
		t.Fatalf("cap overflow must fail open verbatim (got %d bytes, want %d)", len(out), len(huge))
	}
}

func TestThoughtTagScrubber_Divergence(t *testing.T) {
	for _, in := range []string{
		"<thx not our tag",
		"</thought> stray closer",
		"< summary with space",
		"<t>",
		"<s>",
	} {
		if got := feedAll(t, []string{in}); got != in {
			t.Fatalf("divergent input %q was modified: %q", in, got)
		}
	}
}

func TestThoughtTagScrubber_WhitespaceOnly(t *testing.T) {
	s := newThoughtTagScrubber()
	if out := s.feed("\n\n  "); out != "" {
		t.Fatalf("whitespace-only prefix must hold: %q", out)
	}
	if out := s.feed("answer"); out != "\n\n  answer" {
		t.Fatalf("leading whitespace must ride along verbatim: %q", out)
	}
	// And a whitespace-only stream ends by flushing it back out.
	s2 := newThoughtTagScrubber()
	_ = s2.feed("  ")
	if tail := s2.flush(); tail != "  " {
		t.Fatalf("whitespace-only tail must flush verbatim: %q", tail)
	}
}

func TestScrubTagBlocksOneShot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<thought>a</thought>\n\nanswer", "\n\nanswer"},
		{"<summary>a</summary><analysis>b</analysis>ans", "ans"},
		{"<thought>never closed", "<thought>never closed"}, // fail-open
		{"plain answer", "plain answer"},
		{"<thought>a</thought>\n", "\n"}, // blocks-only → whitespace remains
		{"", ""},
	}
	for _, c := range cases {
		if got := scrubTagBlocksOneShot(c.in); got != c.want {
			t.Fatalf("oneShot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// frameWithContent builds a minimal upstream SSE data line.
func frameWithContent(content string) string {
	return `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":` + quoteJSON(content) + `}}]}`
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestScrubTagBlocksInChunk_FrameRewrite(t *testing.T) {
	state := map[int]*thoughtTagScrubber{}

	// Frame 1: role + a partial tag opening — content key is removed.
	f1 := scrubTagBlocksInChunk(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"<thou"}}]}`, state)
	if strings.Contains(f1, "thou") {
		t.Fatalf("partial tag must not survive the frame: %s", f1)
	}
	var o1 map[string]any
	_ = json.Unmarshal([]byte(f1), &o1)
	d1 := o1["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, has := d1["content"]; has {
		t.Fatalf("emptied content key must be deleted: %s", f1)
	}
	if d1["role"] != "assistant" {
		t.Fatalf("role must be preserved: %s", f1)
	}

	// Frame 2: closing the block + real content — content rewritten.
	f2raw := `{"choices":[{"index":0,"delta":{"content":` + quoteJSON("ght>"+scrubSSEHidden+"</thought>\n\nvisible") + `},"finish_reason":null}]}`
	f2 := scrubTagBlocksInChunk(f2raw, state)
	if strings.Contains(f2, scrubSSEHidden) || strings.Contains(f2, "thought") {
		t.Fatalf("block leaked into the frame: %s", f2)
	}
	var o2 map[string]any
	_ = json.Unmarshal([]byte(f2), &o2)
	ch2 := o2["choices"].([]any)[0].(map[string]any)
	if ch2["finish_reason"] != nil {
		t.Fatalf("finish_reason must survive the rewrite: %s", f2)
	}
	d2 := ch2["delta"].(map[string]any)
	if d2["content"] != "\n\nvisible" {
		t.Fatalf("rewritten content wrong: %s", f2)
	}

	// Post-decision frames pass through byte-identical (no re-encode).
	f3raw := `{"choices":[{"index":0,"delta":{"content":"more <summary>x</summary>"}}]}`
	if f3 := scrubTagBlocksInChunk(f3raw, state); f3 != f3raw {
		t.Fatalf("decided scrubber must be a pass-through: %s", f3)
	}

	// Non-JSON and choice-less frames are untouched.
	for _, passthrough := range []string{`not json`, `{"choices":"nope"}`} {
		if got := scrubTagBlocksInChunk(passthrough, state); got != passthrough {
			t.Fatalf("passthrough frame modified: %s", got)
		}
	}
}

func TestScrubTagBlocksInChunk_PerChoice(t *testing.T) {
	state := map[int]*thoughtTagScrubber{}
	frame := `{"choices":[` +
		`{"index":0,"delta":{"content":"plain"}},` +
		`{"index":1,"delta":{"content":` + quoteJSON("<summary>"+scrubSSEHidden+"</summary>ans") + `}}]}`
	got := scrubTagBlocksInChunk(frame, state)
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("frame still JSON: %v", err)
	}
	choices := obj["choices"].([]any)
	if c0 := choices[0].(map[string]any)["delta"].(map[string]any)["content"]; c0 != "plain" {
		t.Fatalf("choice 0 must be untouched: %s", got)
	}
	if c1 := choices[1].(map[string]any)["delta"].(map[string]any)["content"]; c1 != "ans" {
		t.Fatalf("choice 1 block not stripped: %s", got)
	}
}

// --- executor-path level tests ---

func TestPumpStreamFrames_ScrubsTaggedStream(t *testing.T) {
	input := strings.Join([]string{
		frameWithContent("<thou"),
		frameWithContent("ght>" + scrubSSEHidden + "</thou"),
		frameWithContent("ght>\n\nvisible answer"),
		"data: [DONE]",
	}, "\n") + "\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	rec := &streamRecorder{}
	seen, handled := pumpStreamFrames(scanner, rec, nil, false, &sseUsageCollector{}, "m", "m", "uid", time.Now(), nil)
	if handled || !seen {
		t.Fatalf("unexpected loop state: handled=%v seen=%v", handled, seen)
	}
	chunks, _ := rec.snapshot()
	joined := strings.Join(chunks, "\n")
	if strings.Contains(joined, scrubSSEHidden) || strings.Contains(joined, "thought") {
		t.Fatalf("tagged block reached the client: %s", joined)
	}
	if !strings.Contains(joined, "visible answer") {
		t.Fatalf("answer lost: %s", joined)
	}
}

func TestPumpStreamFrames_FailOpenTail(t *testing.T) {
	input := strings.Join([]string{
		frameWithContent("<thought>truncated " + scrubSSEHidden),
		"data: [DONE]",
	}, "\n") + "\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	rec := &streamRecorder{}
	pumpStreamFrames(scanner, rec, nil, false, &sseUsageCollector{}, "m", "m", "uid", time.Now(), nil)
	chunks, _ := rec.snapshot()
	joined := strings.Join(chunks, "\n")
	// Fail-open: an unterminated block degrades to the pre-fix wire shape —
	// the raw text must still reach the client exactly once.
	if !strings.Contains(joined, "truncated "+scrubSSEHidden) {
		t.Fatalf("fail-open tail lost: %s", joined)
	}
}

func TestAggregateSSEWithCollector_Scrubs(t *testing.T) {
	input := strings.Join([]string{
		frameWithContent("<summary>a</summary>"),
		frameWithContent("<analysis>b</analysis>\n\nreal"),
		"data: [DONE]",
	}, "\n") + "\n"
	chunks, err := aggregateSSEWithCollector(strings.NewReader(input), false, &sseUsageCollector{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var joined strings.Builder
	for _, c := range chunks {
		joined.Write(c.Payload)
	}
	out := joined.String()
	if strings.Contains(out, "<summary>") || strings.Contains(out, "<analysis>") {
		t.Fatalf("tag blocks reached the chunk list: %s", out)
	}
	if !strings.Contains(out, "real") {
		t.Fatalf("answer lost: %s", out)
	}
}

func TestAggregateCompletion_ScrubsOneShot(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"quiet think"}}]}`,
		frameWithContent("<analysis>" + scrubSSEHidden + "</analysis>\n\nFinal answer"),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
	}, "\n") + "\n"
	out, err := aggregateCompletion(strings.NewReader(input), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var obj struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("folded completion: %v", err)
	}
	if len(obj.Choices) != 1 {
		t.Fatalf("choices: %d", len(obj.Choices))
	}
	msg := obj.Choices[0].Message
	if msg["content"] != "\n\nFinal answer" {
		t.Fatalf("content not scrubbed: %v", msg["content"])
	}
	if msg["reasoning_content"] != "quiet think" {
		t.Fatalf("reasoning_content must survive: %v", msg["reasoning_content"])
	}
}

// TestIssue30_TaughtTagRunNeverReachesClient is the release gate in one line:
// whatever the model emits in the taught shape, the client-visible stream on
// every executor path carries no leading tag block.
func TestIssue30_TaughtTagRunNeverReachesClient(t *testing.T) {
	tagged := "<thought>x</thought><analysis>y</analysis><summary>z</summary><think>w</think>"
	for _, tag := range []string{"thought", "analysis", "summary", "think"} {
		stream := "data: " + frameWithContent(tagged+"real answer") + "\ndata: [DONE]\n"
		chunks, err := aggregateSSEWithCollector(strings.NewReader(stream), false, &sseUsageCollector{})
		if err != nil {
			t.Fatalf("%s: collect: %v", tag, err)
		}
		var joined strings.Builder
		for _, c := range chunks {
			joined.Write(c.Payload)
		}
		out := joined.String()
		if strings.Contains(out, "<"+tag+">") {
			t.Fatalf("tag <%s> survived the pipeline: %s", tag, out)
		}
		if !strings.Contains(out, "real answer") {
			t.Fatalf("answer lost for <%s>: %s", tag, out)
		}
	}
}
