// offpeak_test.go — the off-peak ticketing lane (M3).
//
// Coverage mirrors the wire contract the official open-source client speaks:
// the four JSON ticket endpoints (bare body AND {code:0,data} envelope —
// server v2 ships bare, the zai gateway sometimes wraps), the loose field
// semantics (position nullish once ready, next_poll_after in SECONDS), the
// lane-local failure decision (3102/3001 retake; 429/3105 queue-wait clamped
// by min(Retry-After, 5min) with the 60s default probe), the acquire state
// machine (immediate ready / queued→poll→ready / expired→retake with the
// SAME task_id / budget exhaustion with the queue position in the message),
// and the executor integration (take→ready→messages→settle ordering, the
// ticketed header set, queue-ack retry and ticket-expired retake mid-round).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------------

// offPeakAuthJSON is a coding-plan credential as stored on disk (the lane's
// dual credential: the resolved plan key + the plan JWT).
func offPeakAuthJSON() []byte {
	raw, _ := json.Marshal(storedAuth{
		Auth: zcodeTokens{
			AccessToken: "resolved-key.id.secret",
			JWT:         "offpeak-test-jwt",
			Provider:    providerZai,
			Plan:        planCoding,
			DeviceMid:   "device-mid-op",
		},
		Account: zcodeAccount{UID: "user-op"},
	})
	return raw
}

// startPlanAuthForOffPeak is a start-plan credential — the server structurally
// rejects start-plan tickets (start_plan_not_supported), so the plugin must
// never route one through the lane.
func startPlanAuthForOffPeak() []byte {
	raw, _ := json.Marshal(storedAuth{
		Auth: zcodeTokens{
			AccessToken: "start-key-placeholder",
			JWT:         "start-jwt",
			Provider:    providerZai,
			Plan:        planStart,
		},
		Account: zcodeAccount{UID: "user-sp"},
	})
	return raw
}

// opResp is one scripted response for a gateway path.
type opResp struct {
	status  int
	headers map[string]string
	body    string
}

// offPeakGateway is a recording httptest server scripting every off-peak
// path. Scripted responses are consumed in order; the last one repeats.
type offPeakGateway struct {
	srv      *httptest.Server
	mu       chan struct{}
	requests []capturedRequest
	script   map[string][]opResp
	counts   map[string]int
}

func newOffPeakGateway(t *testing.T, script map[string][]opResp) *offPeakGateway {
	t.Helper()
	g := &offPeakGateway{
		mu:     make(chan struct{}, 1),
		script: script,
		counts: map[string]int{},
	}
	g.mu <- struct{}{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAll(r.Body)
		<-g.mu
		g.requests = append(g.requests, capturedRequest{path: r.URL.Path, method: r.Method, headers: r.Header.Clone(), body: string(raw)})
		idx := g.counts[r.URL.Path]
		g.counts[r.URL.Path] = idx + 1
		scripted := script[r.URL.Path]
		resp := scripted[len(scripted)-1]
		if idx < len(scripted) {
			resp = scripted[idx]
		}
		g.mu <- struct{}{}
		for k, v := range resp.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *offPeakGateway) calls() []capturedRequest {
	<-g.mu
	defer func() { g.mu <- struct{}{} }()
	out := make([]capturedRequest, len(g.requests))
	copy(out, g.requests)
	return out
}

func (g *offPeakGateway) callsFor(path string) []capturedRequest {
	var out []capturedRequest
	for _, c := range g.calls() {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// pointOffPeakAt aims the ticket plane and the messages endpoint at the
// gateway and restores both afterwards.
func pointOffPeakAt(t *testing.T, g *offPeakGateway) {
	t.Helper()
	origBase, origMsg := offPeakBase, offPeakMessagesEndpoint
	offPeakBase = g.srv.URL
	offPeakMessagesEndpoint = g.srv.URL + "/api/v1/off-peak/anthropic/v1/messages"
	t.Cleanup(func() {
		offPeakBase = origBase
		offPeakMessagesEndpoint = origMsg
	})
}

// enableOffPeak flips the lane on for a test and always restores off.
func enableOffPeak(t *testing.T, maxWait time.Duration) {
	t.Helper()
	configureOffPeak(true, maxWait)
	t.Cleanup(func() { configureOffPeak(false, 0) })
}

// shrinkPollCadence lets the acquire loop poll fast in tests.
func shrinkPollCadence(t *testing.T) {
	t.Helper()
	origMin, origMax := offPeakPollMinInterval, offPeakPollMaxInterval
	offPeakPollMinInterval = time.Millisecond
	offPeakPollMaxInterval = 2 * time.Millisecond
	t.Cleanup(func() {
		offPeakPollMinInterval = origMin
		offPeakPollMaxInterval = origMax
	})
}

const offPeakTicketQueuedBody = `{"ticket_id":"tkt-1","task_id":"offpeak-x","state":"queued","position":3,"next_poll_after":2,"queued_at":1727000000}`
const offPeakTicketReadyBody = `{"ticket_id":"tkt-1","task_id":"offpeak-x","state":"ready","position":null,"next_poll_after":null}`
const offPeakTicketReadyBody2 = `{"ticket_id":"tkt-2","task_id":"offpeak-x","state":"ready","position":0}`

func opEnvelope(data string) string {
	return `{"code":0,"msg":"success","data":` + data + `}`
}

// offPeakAuth parses the fixture into the stored credential the client funcs take.
func offPeakAuth() *storedAuth {
	sa, err := parseStored(offPeakAuthJSON())
	if err != nil {
		panic(err)
	}
	return sa
}

// -----------------------------------------------------------------------------
// Ticket client: parsing, headers, envelope compatibility
// -----------------------------------------------------------------------------

func TestOffPeakTakeTicketShapes(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		wantID          string
		wantState       offPeakTicketState
		wantPos         int // -1 = nil
		wantNextPollSec float64
	}{
		{"bare queued", offPeakTicketQueuedBody, "tkt-1", offPeakStateQueued, 3, 2},
		{"bare ready null fields", offPeakTicketReadyBody, "tkt-1", offPeakStateReady, -1, 0},
		{"envelope ready", opEnvelope(offPeakTicketReadyBody2), "tkt-2", offPeakStateReady, 0, 0},
		{"envelope queued", opEnvelope(offPeakTicketQueuedBody), "tkt-1", offPeakStateQueued, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			orig := offPeakBase
			offPeakBase = srv.URL
			defer func() { offPeakBase = orig }()

			tk, err := offPeakTakeTicket(offPeakAuth(), "offpeak-x")
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			if tk.TicketID != tc.wantID || tk.State != tc.wantState {
				t.Fatalf("got %s/%s want %s/%s", tk.TicketID, tk.State, tc.wantID, tc.wantState)
			}
			if tc.wantPos < 0 {
				if tk.Position != nil {
					t.Fatalf("position want nil got %d", *tk.Position)
				}
			} else if tk.Position == nil || *tk.Position != tc.wantPos {
				t.Fatalf("position want %d got %v", tc.wantPos, tk.Position)
			}
			if tc.wantNextPollSec == 0 {
				if tk.NextPollAfter != nil {
					t.Fatalf("next_poll_after want nil got %v", *tk.NextPollAfter)
				}
			} else if tk.NextPollAfter == nil || *tk.NextPollAfter != tc.wantNextPollSec {
				t.Fatalf("next_poll_after want %v got %v", tc.wantNextPollSec, tk.NextPollAfter)
			}
		})
	}
}

func TestOffPeakTakeTicketValidation(t *testing.T) {
	cases := []struct {
		name, body, wantSub string
	}{
		{"missing ticket_id", `{"state":"queued"}`, "ticket_id"},
		{"unknown state", `{"ticket_id":"t1","state":"teleported"}`, "unknown state"},
		{"garbage json", `not-json`, "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			orig := offPeakBase
			offPeakBase = srv.URL
			defer func() { offPeakBase = orig }()
			_, err := offPeakTakeTicket(offPeakAuth(), "offpeak-x")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q got %v", tc.wantSub, err)
			}
		})
	}
}

func TestOffPeakTicketRequestHeaders(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAll(r.Body)
		got = capturedRequest{path: r.URL.Path, method: r.Method, headers: r.Header.Clone(), body: string(raw)}
		w.Write([]byte(offPeakTicketReadyBody))
	}))
	defer srv.Close()
	orig := offPeakBase
	offPeakBase = srv.URL
	defer func() { offPeakBase = orig }()

	if _, err := offPeakTakeTicket(offPeakAuth(), "offpeak-abc"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if got.path != "/api/v1/off-peak/ticket" || got.method != "POST" {
		t.Fatalf("line: %s %s", got.method, got.path)
	}
	if a := got.headers.Get("Authorization"); a != "Bearer offpeak-test-jwt" {
		t.Fatalf("Authorization: %q", a)
	}
	if k := got.headers.Get("x-coding-plan-api-key"); k != "resolved-key.id.secret" {
		t.Fatalf("x-coding-plan-api-key: %q", k)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: %q", ct)
	}
	// TV (control-plane) identity shape: no X-ZCode-Agent, X-Device-Mid rides.
	if got.headers.Get("X-ZCode-Agent") != "" {
		t.Fatalf("X-ZCode-Agent must be absent on the ticket plane")
	}
	if got.headers.Get("X-Device-Mid") != "device-mid-op" {
		t.Fatalf("X-Device-Mid: %q", got.headers.Get("X-Device-Mid"))
	}
	if ua := got.headers.Get("User-Agent"); !strings.HasPrefix(ua, "ZCode/") {
		t.Fatalf("User-Agent: %q", ua)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["task_id"] != "offpeak-abc" {
		t.Fatalf("task_id: %v", body["task_id"])
	}
}

func TestOffPeakBatchStatusParsing(t *testing.T) {
	// Malformed entries are skipped, not fatal; envelope unwraps.
	body := opEnvelope(`{"next_poll_after":4,"tickets":[
		{"ticket_id":"tkt-1","state":"queued","position":7},
		{"ticket_id":"","state":"ready"},
		{"ticket_id":"tkt-3","state":"weird"},
		{"ticket_id":"tkt-2","state":"ready","active_deadline":1727003600}
	]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		ids, _ := req["ticket_ids"].([]any)
		if len(ids) != 2 {
			t.Errorf("want 2 ids got %d", len(ids))
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	orig := offPeakBase
	offPeakBase = srv.URL
	defer func() { offPeakBase = orig }()

	entries, next, err := offPeakBatchStatus(offPeakAuth(), []string{"tkt-1", "tkt-2"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 valid entries got %d: %+v", len(entries), entries)
	}
	if entries[0].State != offPeakStateQueued || entries[0].Position == nil || *entries[0].Position != 7 {
		t.Fatalf("entry0: %+v", entries[0])
	}
	if entries[1].State != offPeakStateReady || entries[1].ActiveDeadline == nil {
		t.Fatalf("entry1: %+v", entries[1])
	}
	if next == nil || *next != 4 {
		t.Fatalf("next_poll_after: %v", next)
	}
}

func TestOffPeakBatchStatusTruncates(t *testing.T) {
	var gotCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		ids, _ := req["ticket_ids"].([]any)
		gotCount = len(ids)
		w.Write([]byte(`{"tickets":[]}`))
	}))
	defer srv.Close()
	orig := offPeakBase
	offPeakBase = srv.URL
	defer func() { offPeakBase = orig }()

	many := make([]string, 137)
	for i := range many {
		many[i] = fmt.Sprintf("t-%d", i)
	}
	if _, _, err := offPeakBatchStatus(offPeakAuth(), many); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if gotCount != offPeakBatchLimit {
		t.Fatalf("want truncation to %d got %d", offPeakBatchLimit, gotCount)
	}
}

func TestOffPeakSettleIdempotent(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"2xx ack", 200, false},
		{"4xx unknown ticket also acked", 404, false},
		{"5xx propagates", 500, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var settlePath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				settlePath = r.URL.Path
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"ticket_id":"tkt-9","state":"settled"}`))
			}))
			defer srv.Close()
			orig := offPeakBase
			offPeakBase = srv.URL
			defer func() { offPeakBase = orig }()

			err := offPeakSettle(offPeakAuth(), "tkt-9/with slash")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v want %v", err, tc.wantErr)
			}
			want := "/api/v1/off-peak/ticket/tkt-9/with slash/settle" // server-side r.URL.Path is decoded
			if settlePath != want {
				t.Fatalf("settle path: %s", settlePath)
			}
		})
	}
}

func TestOffPeakAvailabilityContract(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCan   bool
		wantNext  int64
		wantError string
	}{
		{"available bare", `{"can_take_number":true}`, true, 0, ""},
		{"unavailable with next_take_at", `{"can_take_number":false,"next_take_at":1727000100000}`, false, 1727000100000, ""},
		{"envelope", opEnvelope(`{"can_take_number":true}`), true, 0, ""},
		{"dirty: false without next_take_at", `{"can_take_number":false}`, false, 0, "missing next_take_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/off-peak/ticket/availability" {
					t.Errorf("line: %s %s", r.Method, r.URL.Path)
				}
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			orig := offPeakBase
			offPeakBase = srv.URL
			defer func() { offPeakBase = orig }()

			av, err := offPeakQueryAvailability(offPeakAuth())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error %q got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("availability: %v", err)
			}
			if av.CanTakeNumber != tc.wantCan || av.NextTakeAt != tc.wantNext {
				t.Fatalf("got %+v want can=%v next=%d", av, tc.wantCan, tc.wantNext)
			}
		})
	}
}

func TestOffPeakErrorBodyParsing(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   int
		wantMsgSub string
		wantNextAt int64
	}{
		{"plain biz error", 403, `{"code":3103,"msg":"take number quota exceeded"}`, 3103, "take number quota", 0},
		{"nested data next_take_at", 429, `{"code":3101,"msg":"not eligible","data":{"next_take_at":1727000200000}}`, 3101, "not eligible", 1727000200000},
		{"message field alias", 400, `{"code":3102,"message":"ticket expired"}`, 3102, "ticket expired", 0},
		{"html body no biz code", 502, `<html>bad gateway</html>`, 0, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			orig := offPeakBase
			offPeakBase = srv.URL
			defer func() { offPeakBase = orig }()

			_, err := offPeakTakeTicket(offPeakAuth(), "offpeak-x")
			se, ok := err.(*offPeakServerError)
			if !ok {
				t.Fatalf("want *offPeakServerError got %T: %v", err, err)
			}
			if se.HTTPStatus != tc.status || se.BizCode != tc.wantCode || se.NextTakeAt != tc.wantNextAt {
				t.Fatalf("server error: %+v", se)
			}
			if !strings.Contains(se.Error(), fmt.Sprintf("HTTP %d", tc.status)) {
				t.Fatalf("message missing HTTP status: %s", se.Error())
			}
			if tc.wantMsgSub != "" && !strings.Contains(se.Msg, tc.wantMsgSub) {
				t.Fatalf("msg %q want sub %q", se.Msg, tc.wantMsgSub)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Failure semantics (offpeak-retry.ts parity)
// -----------------------------------------------------------------------------

func TestOffPeakFailureDecision(t *testing.T) {
	const cap5min = int64(5 * 60 * 1000)
	cases := []struct {
		name       string
		status     int
		biz        int
		retryAfter int64
		wantKind   offPeakFailureKind
		wantDelay  int64
	}{
		{"3102 expired", 400, offPeakBizTicketExpired, 0, offPeakFailureTicketExpired, 0},
		{"3001 legacy expired", 400, offPeakBizTicketExpiredOld, 0, offPeakFailureTicketExpired, 0},
		{"3105 with Retry-After", 429, offPeakBizQueued, 45_000, offPeakFailureQueued, 45_000},
		{"3105 no header → 60s default", 429, offPeakBizQueued, 0, offPeakFailureQueued, 60_000},
		{"bare 429 no biz code", 429, 0, 0, offPeakFailureQueued, 60_000},
		{"429 Retry-After capped at 5min", 429, offPeakBizQueued, 900_000, offPeakFailureQueued, cap5min},
		{"429 negative header → default", 429, 0, -5, offPeakFailureQueued, 60_000},
		{"500 plain failure", 500, 0, 0, offPeakFailureNone, 0},
		{"3101 not lane failure semantics", 403, offPeakBizNotEligible, 0, offPeakFailureNone, 0},
		{"200 with 3102 body ignored (only failures classified)", 200, offPeakBizTicketExpired, 0, offPeakFailureNone, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, delay := offPeakFailureDecision(tc.status, tc.biz, tc.retryAfter)
			if kind != tc.wantKind || delay != tc.wantDelay {
				t.Fatalf("got (%d,%d) want (%d,%d)", kind, delay, tc.wantKind, tc.wantDelay)
			}
		})
	}
}

func TestRetryAfterMillis(t *testing.T) {
	h := http.Header{}
	if v := retryAfterMillis(h); v != 0 {
		t.Fatalf("empty header: %d", v)
	}
	h.Set("Retry-After", "30")
	if v := retryAfterMillis(h); v != 30_000 {
		t.Fatalf("30s: %d", v)
	}
	h.Set("Retry-After", "junk")
	if v := retryAfterMillis(h); v != 0 {
		t.Fatalf("junk: %d", v)
	}
}

func TestExtractOffPeakBizCode(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"bare code", `{"code":3102,"msg":"x"}`, 3102},
		{"error.code", `{"error":{"code":3105}}`, 3105},
		{"data.code", `{"code":0,"data":{"code":3102}}`, 3102},
		{"no code", `{"msg":"nothing"}`, 0},
		{"garbage", `<<>>`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractOffPeakBizCode([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Acquire state machine
// -----------------------------------------------------------------------------

func TestOffPeakAcquireImmediateReady(t *testing.T) {
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {{status: 200, body: offPeakTicketReadyBody}},
	})
	pointOffPeakAt(t, g)
	shrinkPollCadence(t)

	tk, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-x", 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if tk.TicketID != "tkt-1" || tk.State != offPeakStateReady {
		t.Fatalf("ticket: %+v", tk)
	}
	// budget 0 must not stop an immediately-ready ticket; no status polls.
	if n := len(g.callsFor("/api/v1/off-peak/ticket/status")); n != 0 {
		t.Fatalf("unexpected status polls: %d", n)
	}
}

func TestOffPeakAcquireQueuedToReady(t *testing.T) {
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {{status: 200, body: offPeakTicketQueuedBody}},
		"/api/v1/off-peak/ticket/status": {
			{status: 200, body: `{"next_poll_after":1,"tickets":[{"ticket_id":"tkt-1","state":"queued","position":2}]}`},
			{status: 200, body: `{"tickets":[{"ticket_id":"WRONG-id","state":"queued"},{"ticket_id":"tkt-1","state":"ready"}]}`},
		},
	})
	pointOffPeakAt(t, g)
	shrinkPollCadence(t)

	tk, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-x", 5*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if tk.State != offPeakStateReady {
		t.Fatalf("state: %s", tk.State)
	}
	// The ready entry must be matched by ticket id — WRONG-id must be skipped,
	// not picked (response order is not guaranteed).
	if n := len(g.callsFor("/api/v1/off-peak/ticket/status")); n != 2 {
		t.Fatalf("status polls: %d", n)
	}
}

func TestOffPeakAcquireExpiredRetakeSameTaskID(t *testing.T) {
	// take → queued; status → expired; retake (SAME task_id) → ready.
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {
			{status: 200, body: offPeakTicketQueuedBody},
			{status: 200, body: offPeakTicketReadyBody2},
		},
		"/api/v1/off-peak/ticket/status": {
			{status: 200, body: `{"tickets":[{"ticket_id":"tkt-1","state":"expired"}]}`},
		},
	})
	pointOffPeakAt(t, g)
	shrinkPollCadence(t)

	tk, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-same-task", 5*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if tk.TicketID != "tkt-2" {
		t.Fatalf("want retaken ticket tkt-2 got %s", tk.TicketID)
	}
	takes := g.callsFor("/api/v1/off-peak/ticket")
	if len(takes) != 2 {
		t.Fatalf("takes: %d", len(takes))
	}
	for i, take := range takes {
		var body map[string]any
		if err := json.Unmarshal([]byte(take.body), &body); err != nil {
			t.Fatalf("take %d body: %v", i, err)
		}
		if body["task_id"] != "offpeak-same-task" {
			t.Fatalf("take %d task_id: %v (resume semantics reuse the same id)", i, body["task_id"])
		}
	}
}

func TestOffPeakAcquireBudgetExhausted(t *testing.T) {
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {{status: 200, body: offPeakTicketQueuedBody}},
		"/api/v1/off-peak/ticket/status": {
			{status: 200, body: `{"tickets":[{"ticket_id":"tkt-1","state":"queued","position":11}]}`},
		},
	})
	pointOffPeakAt(t, g)
	shrinkPollCadence(t)

	_, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-x", 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("want budget error got %v", err)
	}
	if !strings.Contains(err.Error(), "#11") {
		t.Fatalf("error must carry the queue position: %v", err)
	}
}

func TestOffPeakAcquireBizErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		wantSub string
	}{
		{"3101 not eligible", `{"code":3101,"msg":"no off-peak"}`, 403, "3101"},
		{"3103 quota", `{"code":3103,"msg":"too many tickets"}`, 429, "3103"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newOffPeakGateway(t, map[string][]opResp{
				"/api/v1/off-peak/ticket": {{status: tc.status, body: tc.body}},
			})
			pointOffPeakAt(t, g)
			_, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-x", 0)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q got %v", tc.wantSub, err)
			}
			if !strings.Contains(err.Error(), "off-peak") {
				t.Fatalf("error must carry lane context: %v", err)
			}
		})
	}
}

func TestOffPeakAcquireSettledOutOfTake(t *testing.T) {
	// A ticket that lands already-settled (or expired straight from take) is
	// a dead round — surface the marker instead of spinning.
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {{status: 200, body: `{"ticket_id":"tkt-0","state":"settled"}`}},
	})
	pointOffPeakAt(t, g)
	_, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-x", 0)
	if err == nil || !strings.Contains(err.Error(), offPeakTicketExpiredMarker) {
		t.Fatalf("want expired marker got %v", err)
	}
}

func TestOffPeakAcquireStatusProbeFailureKeepsPolling(t *testing.T) {
	// A failed status probe must not kill the ticket: keep polling until the
	// budget runs out (the server advances states on poll).
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket":        {{status: 200, body: offPeakTicketQueuedBody}},
		"/api/v1/off-peak/ticket/status": {{status: 500, body: `{"msg":"boom"}`}},
	})
	pointOffPeakAt(t, g)
	shrinkPollCadence(t)

	_, err := offPeakAcquireTicket(offPeakAuth(), "offpeak-x", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("want budget error after probe failures got %v", err)
	}
	if len(g.callsFor("/api/v1/off-peak/ticket/status")) < 2 {
		t.Fatalf("expected repeated polling, got %d", len(g.callsFor("/api/v1/off-peak/ticket/status")))
	}
}

// -----------------------------------------------------------------------------
// Executor integration: take → ready → messages → settle
// -----------------------------------------------------------------------------

const offPeakAnthropicOK = `{
  "id": "msg_op_1",
  "type": "message",
  "role": "assistant",
  "model": "glm-5.3-flash",
  "content": [{"type": "text", "text": "off-peak hello"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 13, "output_tokens": 5}
}`

func TestHandleExecExecuteOffPeakHappyPath(t *testing.T) {
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket":                {{status: 200, body: offPeakTicketReadyBody}},
		"/api/v1/off-peak/anthropic/v1/messages": {{status: 200, body: offPeakAnthropicOK}},
		"/api/v1/off-peak/ticket/tkt-1/settle":   {{status: 200, body: `{"state":"settled"}`}},
	})
	pointOffPeakAt(t, g)
	enableOffPeak(t, 0)

	raw, err := handleExecExecute(executorRequestJSON(t, map[string]any{
		"AuthID":       "a-op",
		"AuthProvider": providerName,
		"Model":        "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, offPeakAuthJSON()))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Ordering: take → messages → settle (the settle rides the deferred best-effort).
	calls := g.calls()
	if len(calls) != 3 {
		t.Fatalf("want take+messages+settle got %d: %v", len(calls), calls)
	}
	if !strings.HasSuffix(calls[0].path, "/ticket") || !strings.HasSuffix(calls[1].path, "/messages") || !strings.HasSuffix(calls[2].path, "/settle") {
		t.Fatalf("order: %s %s %s", calls[0].path, calls[1].path, calls[2].path)
	}

	// Messages headers: dual credential + ticket id + anthropic version + SDK UA.
	msg := calls[1]
	if a := msg.headers.Get("Authorization"); a != "Bearer offpeak-test-jwt" {
		t.Fatalf("messages Authorization: %q", a)
	}
	if k := msg.headers.Get("X-Coding-Plan-Api-Key"); k != "resolved-key.id.secret" {
		t.Fatalf("messages X-Coding-Plan-Api-Key: %q", k)
	}
	if tk := msg.headers.Get("X-Off-Peak-Ticket-ID"); tk != "tkt-1" {
		t.Fatalf("ticket header: %q", tk)
	}
	if v := msg.headers.Get("anthropic-version"); v != anthropicVersion {
		t.Fatalf("anthropic-version: %q", v)
	}
	if ua := msg.headers.Get("User-Agent"); !strings.HasSuffix(ua, llmSDKUserAgentSuffix) {
		t.Fatalf("UA: %q", ua)
	}

	// Body: the start-plan translation rides the lane (official system blocks
	// + context prefix + metadata blob — the server proxies the admitted body).
	if !strings.Contains(msg.body, "You are ZCode") {
		t.Fatalf("body missing official system block: %.200s", msg.body)
	}
	if !strings.Contains(msg.body, "system-reminder") {
		t.Fatalf("body missing context prefix: %.200s", msg.body)
	}
	if !strings.Contains(msg.body, `"metadata"`) || !strings.Contains(msg.body, "device_id") {
		t.Fatalf("body missing metadata blob: %.200s", msg.body)
	}

	// The response came back translated into an OpenAI completion.
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("envelope = %s", raw)
	}
	var resp pluginapiExecutorResponseProbe
	if json.Unmarshal(env.Result, &resp) != nil || len(resp.Payload) == 0 {
		t.Fatalf("result = %s", env.Result)
	}
	// ExecutorResponse.Payload is []byte on the wire: a base64 string.
	payloadBytes, decErr := base64.StdEncoding.DecodeString(strings.Trim(string(resp.Payload), `"`))
	if decErr != nil {
		t.Fatalf("payload not base64: %v (%s)", decErr, resp.Payload)
	}
	var completion map[string]any
	if json.Unmarshal(payloadBytes, &completion) != nil {
		t.Fatalf("payload not JSON: %s", payloadBytes)
	}
	if completion["object"] != "chat.completion" {
		t.Fatalf("want translated completion got %v", completion["object"])
	}
}

func TestHandleExecExecuteOffPeakQueueRetry(t *testing.T) {
	// messages 429 (queue ack) → wait within budget → retry on the SAME ticket → 200.
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {{status: 200, body: offPeakTicketReadyBody}},
		"/api/v1/off-peak/anthropic/v1/messages": {
			{status: 429, headers: map[string]string{"Retry-After": "0"}, body: `{"error":{"type":"queue","code":3105}}`},
			{status: 200, body: offPeakAnthropicOK},
		},
		"/api/v1/off-peak/ticket/tkt-1/settle": {{status: 200, body: `{}`}},
	})
	pointOffPeakAt(t, g)
	// The default queue wait is 60s; the budget clamps it so the test stays fast.
	enableOffPeak(t, 150*time.Millisecond)

	raw, err := handleExecExecute(executorRequestJSON(t, map[string]any{
		"AuthID": "a-op", "AuthProvider": providerName, "Model": "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, offPeakAuthJSON()))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	_ = raw

	msgs := g.callsFor("/api/v1/off-peak/anthropic/v1/messages")
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages sends got %d", len(msgs))
	}
	for i, m := range msgs {
		if tk := m.headers.Get("X-Off-Peak-Ticket-ID"); tk != "tkt-1" {
			t.Fatalf("send %d must reuse the same ticket, got %q", i, tk)
		}
	}
	if settles := g.callsFor("/api/v1/off-peak/ticket/tkt-1/settle"); len(settles) != 1 {
		t.Fatalf("settles: %d", len(settles))
	}
}

func TestHandleExecExecuteOffPeakTicketExpiredRetake(t *testing.T) {
	// messages 3102 (ticket expired) → retake with the SAME task_id → new
	// ticket ready → messages 200 → BOTH tickets settled.
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {
			{status: 200, body: offPeakTicketReadyBody},  // initial take: tkt-1
			{status: 200, body: offPeakTicketReadyBody2}, // retake: tkt-2
		},
		"/api/v1/off-peak/anthropic/v1/messages": {
			{status: 400, body: `{"code":3102,"msg":"ticket expired"}`},
			{status: 200, body: offPeakAnthropicOK},
		},
		"/api/v1/off-peak/ticket/tkt-1/settle": {{status: 200, body: `{}`}},
		"/api/v1/off-peak/ticket/tkt-2/settle": {{status: 200, body: `{}`}},
	})
	pointOffPeakAt(t, g)
	enableOffPeak(t, 0)

	if _, err := handleExecExecute(executorRequestJSON(t, map[string]any{
		"AuthID": "a-op", "AuthProvider": providerName, "Model": "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, offPeakAuthJSON())); err != nil {
		t.Fatalf("execute: %v", err)
	}

	takes := g.callsFor("/api/v1/off-peak/ticket")
	if len(takes) != 2 {
		t.Fatalf("want take+retake got %d", len(takes))
	}
	var firstTask, secondTask string
	for i, take := range takes {
		var body map[string]any
		_ = json.Unmarshal([]byte(take.body), &body)
		if i == 0 {
			firstTask, _ = body["task_id"].(string)
		} else {
			secondTask, _ = body["task_id"].(string)
		}
	}
	if firstTask == "" || firstTask != secondTask {
		t.Fatalf("retake must reuse the same task_id: %q vs %q", firstTask, secondTask)
	}

	msgs := g.callsFor("/api/v1/off-peak/anthropic/v1/messages")
	if len(msgs) != 2 {
		t.Fatalf("messages sends: %d", len(msgs))
	}
	if tk := msgs[0].headers.Get("X-Off-Peak-Ticket-ID"); tk != "tkt-1" {
		t.Fatalf("first send ticket: %q", tk)
	}
	if tk := msgs[1].headers.Get("X-Off-Peak-Ticket-ID"); tk != "tkt-2" {
		t.Fatalf("second send ticket: %q", tk)
	}
	if settles := g.callsFor("/api/v1/off-peak/ticket/tkt-1/settle"); len(settles) != 1 {
		t.Fatalf("old ticket settles: %d", len(settles))
	}
	if settles := g.callsFor("/api/v1/off-peak/ticket/tkt-2/settle"); len(settles) != 1 {
		t.Fatalf("new ticket settles: %d", len(settles))
	}
}

func TestHandleExecExecuteOffPeakDisabledOrStartPlan(t *testing.T) {
	// Off-peak disabled: the coding-plan account rides its normal route and
	// no ticket traffic happens. start-plan: never eligible even when on.
	run := func(t *testing.T, storage []byte, on bool) *offPeakGateway {
		t.Helper()
		g := newOffPeakGateway(t, map[string][]opResp{})
		pointOffPeakAt(t, g)
		if on {
			enableOffPeak(t, 0)
		}
		_, err := handleExecExecute(executorRequestJSON(t, map[string]any{
			"AuthID": "a-x", "AuthProvider": providerName, "Model": "zcode/glm-5.2",
		}, map[string]any{
			"model":    "zcode/glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, storage))
		// The message call itself goes nowhere reachable — an error is fine;
		// what must NOT happen is ticket traffic on the off-peak plane.
		if err != nil && strings.Contains(err.Error(), "off-peak acquire") {
			t.Fatalf("must not attempt ticketing: %v", err)
		}
		if n := len(g.calls()); n != 0 {
			t.Fatalf("off-peak plane must stay silent, got %d calls", n)
		}
		return g
	}
	t.Run("lane disabled", func(t *testing.T) {
		run(t, offPeakAuthJSON(), false)
	})
	t.Run("start-plan never eligible", func(t *testing.T) {
		run(t, startPlanAuthForOffPeak(), true)
	})
}

func TestHandleExecExecuteOffPeakAcquireFails(t *testing.T) {
	// Acquire failure (3103 quota) surfaces the lane hint, not a generic 500.
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket": {{status: 429, body: `{"code":3103,"msg":"quota"}`}},
	})
	pointOffPeakAt(t, g)
	enableOffPeak(t, 0)

	_, err := handleExecExecute(executorRequestJSON(t, map[string]any{
		"AuthID": "a-op", "AuthProvider": providerName, "Model": "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, offPeakAuthJSON()))
	if err == nil || !strings.Contains(err.Error(), "3103") {
		t.Fatalf("want lane-hinted error got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Stream integration (sync collection + async pump settle)
// -----------------------------------------------------------------------------

func TestHandleExecStreamOffPeakSyncCollect(t *testing.T) {
	sse := sampleAnthropicSSE
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket":                {{status: 200, body: offPeakTicketReadyBody}},
		"/api/v1/off-peak/anthropic/v1/messages": {{status: 200, body: sse}},
		"/api/v1/off-peak/ticket/tkt-1/settle":   {{status: 200, body: `{}`}},
	})
	pointOffPeakAt(t, g)
	enableOffPeak(t, 0)

	raw, err := handleExecStream(executorStreamRequestJSON(t, "", map[string]any{
		"AuthID": "a-op", "AuthProvider": providerName, "Model": "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, offPeakAuthJSON()))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("envelope = %s", raw)
	}
	var resp struct {
		Headers map[string][]string `json:"headers"`
		Chunks  []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"chunks"`
	}
	if json.Unmarshal(env.Result, &resp) != nil {
		t.Fatalf("result = %s", env.Result)
	}
	if len(resp.Chunks) == 0 {
		t.Fatalf("want collected chunks")
	}
	for _, ch := range resp.Chunks {
		chunkBytes, decErr := base64.StdEncoding.DecodeString(strings.Trim(string(ch.Payload), `"`))
		if decErr != nil {
			t.Fatalf("chunk payload not base64: %v (%s)", decErr, ch.Payload)
		}
		if !strings.Contains(string(chunkBytes), "chat.completion.chunk") {
			t.Fatalf("chunk not an OpenAI chunk: %s", chunkBytes)
		}
	}
	msgs := g.callsFor("/api/v1/off-peak/anthropic/v1/messages")
	if len(msgs) != 1 {
		t.Fatalf("messages: %d", len(msgs))
	}
	if tk := msgs[0].headers.Get("X-Off-Peak-Ticket-ID"); tk != "tkt-1" {
		t.Fatalf("ticket header: %q", tk)
	}
	if !strings.Contains(msgs[0].body, `"stream":true`) {
		t.Fatalf("streaming body must pin stream=true: %.100s", msgs[0].body)
	}
	// Sync path settles before returning.
	if settles := g.callsFor("/api/v1/off-peak/ticket/tkt-1/settle"); len(settles) != 1 {
		t.Fatalf("sync settle missing")
	}
}

// executorStreamRequestJSON builds an executor_stream RPC request (mirrors
// the executor_request helper plus stream_id; StreamID "" = sync collect).
func executorStreamRequestJSON(t *testing.T, streamID string, fields map[string]any, payload map[string]any, storage []byte) []byte {
	t.Helper()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["stream_id"] = streamID
	return executorRequestJSON(t, fields, payload, storage)
}

func TestHandleExecStreamOffPeakAsyncPumpSettles(t *testing.T) {
	sse := sampleAnthropicSSE
	g := newOffPeakGateway(t, map[string][]opResp{
		"/api/v1/off-peak/ticket":                {{status: 200, body: offPeakTicketReadyBody}},
		"/api/v1/off-peak/anthropic/v1/messages": {{status: 200, body: sse}},
		"/api/v1/off-peak/ticket/tkt-1/settle":   {{status: 200, body: `{}`}},
	})
	pointOffPeakAt(t, g)
	enableOffPeak(t, 0)

	raw, err := handleExecStream(executorStreamRequestJSON(t, "stream-op-1", map[string]any{
		"AuthID": "a-op", "AuthProvider": providerName, "Model": "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, offPeakAuthJSON()))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("envelope = %s", raw)
	}
	var resp struct {
		Headers map[string][]string `json:"headers"`
		Chunks  []json.RawMessage   `json:"chunks"`
	}
	if json.Unmarshal(env.Result, &resp) != nil {
		t.Fatalf("result = %s", env.Result)
	}
	if len(resp.Chunks) != 0 {
		t.Fatalf("async response must carry no chunks")
	}
	// The pump goroutine settles when the round ends — wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(g.callsFor("/api/v1/off-peak/ticket/tkt-1/settle")) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("async pump never settled the ticket")
}

// -----------------------------------------------------------------------------
// Model filter, config, error rendering
// -----------------------------------------------------------------------------

func TestFilterOffPeakModels(t *testing.T) {
	all := zcodeModels()
	if len(all) <= len(offPeakModelIDs) {
		t.Fatalf("catalog unexpectedly small: %d", len(all))
	}
	narrowed := filterOffPeakModels(all)
	if len(narrowed) != len(offPeakModelIDs) {
		t.Fatalf("want %d models got %d", len(offPeakModelIDs), len(narrowed))
	}
	for _, m := range narrowed {
		if _, ok := offPeakModelIDs[strings.ToLower(m.ID)]; !ok {
			t.Fatalf("unexpected model %q", m.ID)
		}
	}
	// Case-insensitive on the incoming id.
	upper := []pluginapi.ModelInfo{{ID: "GLM-5.3-Flash"}}
	if got := filterOffPeakModels(upper); len(got) != 1 {
		t.Fatalf("case-insensitive match failed: %v", got)
	}
}

func TestConfigureOffPeakParsing(t *testing.T) {
	t.Cleanup(func() { configureOffPeak(false, 0) })
	cfg := "offpeak: true\noffpeak_max_wait: \"900\"\n"
	configure(cfgEnvelope(cfg))
	if !offPeakActive() {
		t.Fatalf("offpeak should be on")
	}
	if got := offPeakWaitBudget(); got != 900*time.Second {
		t.Fatalf("budget: %v", got)
	}
	// Reconfigure without the keys resets both (reset-to-default semantics).
	configure(cfgEnvelope("login_provider: zai\n"))
	if offPeakActive() {
		t.Fatalf("reconfigure must reset offpeak")
	}
	if got := offPeakWaitBudget(); got != 0 {
		t.Fatalf("budget must reset: %v", got)
	}
	// Junk numbers keep the default 0.
	configure(cfgEnvelope("offpeak: on\noffpeak_max_wait: banana\n"))
	if !offPeakActive() {
		t.Fatalf("offpeak: on should enable")
	}
	if got := offPeakWaitBudget(); got != 0 {
		t.Fatalf("junk wait must stay 0: %v", got)
	}
}

func TestRouteChatErrorOffPeak(t *testing.T) {
	sa := &storedAuth{}
	route := chatRoute{endpoint: offPeakMessagesEndpoint, anthropic: true, offPeakTicketID: "tkt-1"}

	err := routeChatError(route, sa, 400, nil, `{"code":3102,"msg":"ticket expired"}`)
	if !strings.Contains(err.Error(), offPeakTicketExpiredMarker) {
		t.Fatalf("3102 must carry the stable marker: %v", err)
	}
	err = routeChatError(route, sa, 429, nil, `{"code":3105,"msg":"queued"}`)
	if !strings.Contains(err.Error(), "3105") || !strings.Contains(err.Error(), "queue") {
		t.Fatalf("3105 must carry the queue hint: %v", err)
	}
	err = routeChatError(route, sa, 403, nil, `{"code":3101,"msg":"no"}`)
	if !strings.Contains(err.Error(), "3101") || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("3101 must carry the eligibility hint: %v", err)
	}
	err = routeChatError(route, sa, 401, nil, `{"msg":"jwt"}`)
	if !strings.Contains(err.Error(), "re-login") {
		t.Fatalf("off-peak 401 must hint re-login: %v", err)
	}
	// Untouched business codes fall through to the anthropic rendering.
	err = routeChatError(route, sa, 400, nil, `{"error":{"type":"invalid_request"}}`)
	if strings.Contains(err.Error(), "off-peak request rejected") {
		t.Fatalf("non-lane codes must keep the generic rendering: %v", err)
	}
}

// cfgB64 mirrors the RPC convention: config_yaml travels base64-encoded
// (a Go []byte field marshals to a base64 string, so marshaling the envelope
// map produces exactly the wire shape configure() unmarshals).
func cfgB64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func cfgEnvelope(yaml string) []byte {
	raw, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(yaml)})
	return raw
}
