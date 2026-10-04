// tag_scrub.go — issue #30 output-side guard: strip the tag-block output
// format the plugin itself taught the model.
//
// Root cause chain (issue #30, reporter Misaka09982 + friend on DSH): since
// 0.9.26 the reasoning replay (injectReasoningInPlace, issue #5) folds every
// historical assistant turn's reasoning_content into content as a
// "<thought>\n…\n</thought>" block so it survives the gateway's field
// whitelist. deepseek-v4.1-flash imitates the taught shape: answers begin
// with raw <thought>/<analysis>/<summary> blocks that clients (Pi, DSH)
// render verbatim — "Pi 长任务" because long tasks mean a longer folded
// history and thus a stronger imitation prior. Pre-absorb builds (the
// "original author" line the reporter compares against) never taught the
// pattern, hence never leaked it.
//
// Removing the replay would resurrect issue #5 (the model loses its own
// prior chain of thought across turns). The fix is therefore output-side:
// recognize exactly the leading tag-block run the fold produces — plus the
// two drift variants the reporter saw and DeepSeek's native <think> — and
// drop it from the client-visible stream, on every executor path:
//
//	pumpStreamFrames            (async pump, live streaming)
//	aggregateSSEWithCollector   (sync fallback, live streaming)
//	aggregateCompletion         (non-stream fold)
//
// Deliberately conservative:
//   - Only a LEADING run of COMPLETE blocks (whitespace between allowed) is
//     removed — the taught pattern is "answer begins with the block". The
//     first real content byte ends scrubbing for good, so legitimate HTML
//     (<details><summary>…) and code fences are never touched.
//   - Fail-open: an unterminated block, a divergence, or the 1 MB hold cap
//     releases the buffer verbatim (pre-fix behavior) instead of eating the
//     answer or stalling the stream.
//   - Mid-content blocks are left alone: a summary/thought block appearing
//     after real content is overwhelmingly real content (HTML tutorials),
//     and stripping there would require balancing risk we cannot verify
//     without the reporter's account.
package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// scrubTagNames are the tags stripped in the leading run: the three the
// reporter listed plus <think> (DeepSeek's native thinking marker — the
// most plausible drift target for the same model family).
var scrubTagNames = []string{"thought", "analysis", "summary", "think"}

// tagScrubHoldLimit caps the undecidable prefix buffer. A thinking block can
// legitimately run for tens of KB; anything past 1 MB without a decision is
// released verbatim (fail-open) so a hostile or broken upstream can neither
// stall the stream nor balloon memory.
const tagScrubHoldLimit = 1 << 20

// thoughtTagScrubber reassembles the leading region of one choice's content
// across chunk boundaries and decides byte-by-byte whether it is a run of
// strippable tag blocks or real content. Zero value is ready to use; one
// instance per choice per stream.
type thoughtTagScrubber struct {
	buf      string // held-back bytes not yet released (only while undecided)
	decided  bool   // true once real content started (or fail-open tripped) — pass-through forever
	scrubbed bool   // at least one block was removed (observability / tests)
}

func newThoughtTagScrubber() *thoughtTagScrubber { return &thoughtTagScrubber{} }

// feed consumes one content fragment and returns the text safe to emit now.
// While undecided it may return "" (everything held back); once decided it is
// a pure pass-through with zero allocation.
func (s *thoughtTagScrubber) feed(fragment string) string {
	if s.decided {
		return fragment
	}
	s.buf += fragment
	out, done := s.consume()
	if done {
		s.decided = true
		s.buf = ""
		return out
	}
	if len(s.buf) > tagScrubHoldLimit {
		out := s.buf
		s.buf = ""
		s.decided = true
		return out
	}
	return out
}

// flush ends the stream: any bytes still held are released verbatim. This is
// the fail-open path for an unterminated leading block (stream cut mid-
// thought, finish_reason=length) — the client sees the raw tail exactly as
// it would have pre-fix, instead of losing it.
func (s *thoughtTagScrubber) flush() string {
	if s.decided {
		return ""
	}
	out := s.buf
	s.buf = ""
	s.decided = true
	return out
}

// consume strips complete leading tag blocks from s.buf and reports whether
// a decision was reached. Releasable bytes are returned only on decision —
// while undecided nothing is emitted, so the wire never shows a block we
// later decide to strip (SSE has no recall).
func (s *thoughtTagScrubber) consume() (string, bool) {
	b := s.buf
	for {
		j := 0
		for j < len(b) && isScrubSpace(b[j]) {
			j++
		}
		if j == len(b) {
			s.buf = b // compact: stripped blocks are gone for good
			// Whitespace so far — a tag may still open in the next chunk.
			return "", false
		}
		if b[j] != '<' {
			// Real content begins: the run is over, release everything
			// (including the leading whitespace) verbatim.
			return b, true
		}
		name, hdrLen, ambiguous := matchScrubTagOpen(b[j:])
		if ambiguous {
			s.buf = b // compact, then wait for more bytes
			return "", false
		}
		if name == "" {
			return b, true // '<' that opens none of our tags: real content
		}
		rest := b[j+hdrLen:]
		end := strings.Index(rest, "</"+name+">")
		if end < 0 {
			s.buf = b // inside an open block; keep buffering (cap guarded)
			return "", false
		}
		b = rest[end+len(name)+3:]
		s.scrubbed = true
		// Loop: consecutive blocks (whitespace-separated) are also stripped.
	}
}

func isScrubSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// matchScrubTagOpen decides whether s (guaranteed to start with '<') opens
// one of scrubTagNames.
//   - name set, hdrLen = len("<name>"): exact open tag matched
//   - name "", ambiguous=false: diverged — real content
//   - ambiguous=true: more bytes needed ("<thou", "<thought" without '>', …)
func matchScrubTagOpen(s string) (name string, hdrLen int, ambiguous bool) {
	if len(s) < 2 {
		return "", 0, true // "<" alone — need one more byte to diverge
	}
	body := s[1:]
	for _, t := range scrubTagNames {
		if strings.HasPrefix(body, t) {
			if len(body) > len(t) {
				if body[len(t)] == '>' {
					return t, len(t) + 2, false // exact match
				}
				continue // "<thoughtX…" can never be "<thought>" — not it
			}
			return "", 0, true // "<thought" with the '>' byte pending
		}
		// Proper-prefix case: "<tho" is a prefix of "thought" — still ambiguous.
		if len(body) < len(t) && strings.HasPrefix(t, body) {
			return "", 0, true
		}
	}
	return "", 0, false
}

// scrubTagBlocksOneShot is the non-stream variant: scrub a complete content
// string (aggregateCompletion). An unterminated leading block survives
// verbatim, mirroring the streaming fail-open policy.
func scrubTagBlocksOneShot(content string) string {
	if content == "" {
		return content
	}
	s := newThoughtTagScrubber()
	out := s.feed(content)
	return out + s.flush()
}

// scrubTagBlocksInChunk rewrites every choice's delta.content in one SSE
// frame through the per-choice scrubber held in state (created on demand,
// keyed by choice index). Returns the frame unchanged when no byte changed,
// so the common post-first-token case costs one decode + no re-encode.
func scrubTagBlocksInChunk(frame string, state map[int]*thoughtTagScrubber) string {
	var obj map[string]any
	if json.Unmarshal([]byte(frame), &obj) != nil {
		return frame
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return frame
	}
	changed := false
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := choice["index"].(float64); ok {
			idx = int(v)
		}
		st := state[idx]
		if st == nil {
			st = newThoughtTagScrubber()
			state[idx] = st
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := delta["content"].(string)
		if !ok {
			continue
		}
		out := st.feed(content)
		if out == content {
			continue
		}
		if out == "" {
			delete(delta, "content") // lets cleanChunkJSON drop a now-empty delta
		} else {
			delta["content"] = out
		}
		changed = true
	}
	if !changed {
		return frame
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return frame
	}
	return string(encoded)
}

// syntheticContentFrameJSON renders a minimal completion-chunk frame carrying
// text for one choice — used to release scrubber tails (fail-open bytes) in
// the same wire shape the upstream used.
func syntheticContentFrameJSON(index int, text string) string {
	encoded, err := json.Marshal(map[string]any{
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": index,
			"delta": map[string]any{"content": text},
		}},
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// scrubTailChunks drains every scrubber still holding bytes (stream ended
// mid-decision) and returns one cleaned, optionally SSE-framed chunk per
// choice, in index order. Empty when nothing was held.
func scrubTailChunks(state map[int]*thoughtTagScrubber, sseFramed bool) [][]byte {
	if len(state) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(state))
	for i := range state {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	var out [][]byte
	for _, i := range indexes {
		tail := state[i].flush()
		if tail == "" {
			continue
		}
		frame := syntheticContentFrameJSON(i, tail)
		if frame == "" {
			continue
		}
		cleaned := cleanChunkJSON(frame)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		out = append(out, []byte(cleaned))
	}
	return out
}
