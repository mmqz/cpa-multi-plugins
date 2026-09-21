package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// The note-preservation behavior is a clean-room absorption of the failure
// mode reported against the bfSan fork (commits 68acf6f/ba7192b, re-verified
// against our tree): a cr==nil round (restart, lazy refresh, transient billing
// error) used to regress a known "余N 已用N" note back to "积分未知".

func TestCreditSegmentFromNote(t *testing.T) {
	cases := []struct {
		note string
		want string
	}{
		{"CN · 余100 已用5", "余100 已用5"},
		{"INTL · 已禁用 · 余3 已用2", "余3 已用2"},
		{"INTL · 余10 已用1 池50", "余10 已用1 池50"},
		{"CN · 积分未知", ""},
		{"CN", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := creditSegmentFromNote(c.note); got != c.want {
			t.Errorf("creditSegmentFromNote(%q) = %q, want %q", c.note, got, c.want)
		}
	}
}

func TestDisplayNoteWithPrev(t *testing.T) {
	// nil cr + known prev → keep the live numbers.
	if got := displayNoteWithPrev(nil, nil, false, "CN · 余100 已用5"); !strings.Contains(got, "余100 已用5") {
		t.Fatalf("nil cr must preserve prev credits, got %q", got)
	} else if strings.Contains(got, "积分未知") {
		t.Fatalf("nil cr must not resurrect the placeholder, got %q", got)
	}
	// nil cr + placeholder-only prev → placeholder.
	if got := displayNoteWithPrev(nil, nil, false, "CN · 积分未知"); !strings.Contains(got, "积分未知") {
		t.Fatalf("placeholder prev must not be treated as live credits, got %q", got)
	}
	// nil cr + no prev → placeholder.
	if got := displayNoteWithPrev(nil, nil, false, ""); !strings.Contains(got, "积分未知") {
		t.Fatalf("no prev must fall back to the placeholder, got %q", got)
	}
	// fresh cr wins over prev.
	cr := &creditsSummary{TotalRemain: 7, TotalUsed: 2}
	if got := displayNoteWithPrev(nil, cr, false, "CN · 余100 已用5"); strings.Contains(got, "余100") {
		t.Fatalf("fresh credits must win over prev, got %q", got)
	}
	// disabled flag still rendered from the prefix, not the credit segment.
	if got := displayNoteWithPrev(nil, nil, true, "CN · 已禁用 · 余9 已用1"); !strings.Contains(got, "已禁用") || strings.Contains(got, "已禁用 · 已禁用") {
		t.Fatalf("disabled prefix rendering broken, got %q", got)
	}
}

func TestSyncAuthNotePreservesCreditsOnNilCr(t *testing.T) {
	var persisted [][]byte
	origGet, origPersist := hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{
			Name: "qoderwork-cn-uid1.json",
			Path: "/auth/qoderwork-cn-uid1.json",
			JSON: []byte(`{"note":"CN · 余100 已用5"}`),
		}, nil
	}
	hostAuthPersistMigrateFn = func(name, path, legacyPath string, raw []byte) error {
		persisted = append(persisted, append([]byte(nil), raw...))
		return nil
	}
	defer func() { hostAuthGetPhysicalFn, hostAuthPersistMigrateFn = origGet, origPersist }()

	sa := &storedAuth{Auth: storedTokens{AccessToken: "tok", Region: "cn"}, Account: storedAccount{UID: "uid1"}}
	if err := syncAuthNote("idx-1", "auth-1", sa, nil, false); err != nil {
		t.Fatalf("syncAuthNote: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected one persist call, got %d", len(persisted))
	}
	var doc map[string]any
	if err := json.Unmarshal(persisted[0], &doc); err != nil {
		t.Fatalf("persisted doc: %v", err)
	}
	note, _ := doc["note"].(string)
	if !strings.Contains(note, "余100 已用5") || strings.Contains(note, "积分未知") {
		t.Fatalf("nil-cr sync must preserve live credits, got note %q", note)
	}

	// Fresh credits overwrite the stored segment.
	cr := &creditsSummary{TotalRemain: 7, TotalUsed: 2}
	persisted = nil
	if err := syncAuthNote("idx-1", "auth-1b", sa, cr, false); err != nil {
		t.Fatalf("syncAuthNote fresh: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected one persist call for fresh credits, got %d", len(persisted))
	}
	_ = json.Unmarshal(persisted[0], &doc)
	note, _ = doc["note"].(string)
	if !strings.Contains(note, "余7 已用2") {
		t.Fatalf("fresh credits must overwrite stored segment, got note %q", note)
	}
}

func TestStreamErrorFrameWireShape(t *testing.T) {
	raw, err := streamErrorFrame("sid-1", "upstream 429: quota exceeded")
	if err != nil {
		t.Fatalf("streamErrorFrame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("frame json: %v", err)
	}
	if m["stream_id"] != "sid-1" {
		t.Fatalf("stream_id = %v, want sid-1", m["stream_id"])
	}
	if msg, _ := m["error"].(string); msg != "upstream 429: quota exceeded" {
		t.Fatalf("top-level error = %v, want the terminal message", m["error"])
	}
	if _, ok := m["payload"]; ok {
		t.Fatal("terminal errors must never travel inside the payload field: the host treats payload bytes as a normal data chunk, so the SSE translator drops the unframed line and the classifier never sees the message")
	}
	if _, ok := m["error"].(map[string]any); ok {
		t.Fatal("error must be a plain string (rpcStreamEmitRequest.Error), not a nested object")
	}
}

func TestStreamTransportErrorWording(t *testing.T) {
	// Host lifecycle exemption keys on the "unexpected eof" substring;
	// plugin-side model cooldown keys on the "empty_stream" prefix.
	es := emptyStreamError().Error()
	if !strings.Contains(es, "empty_stream") || !strings.Contains(es, "unexpected EOF") {
		t.Fatalf("emptyStreamError wording broken: %q", es)
	}
	re := upstreamReadError(io.ErrUnexpectedEOF)
	if !strings.Contains(re.Error(), "unexpected EOF") || !strings.Contains(re.Error(), "upstream stream read error") {
		t.Fatalf("upstreamReadError wording broken: %q", re)
	}
}
