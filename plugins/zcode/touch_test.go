// touch_test.go pins the marketing/touch banner-feed wire contract (issue
// #69 / magpie #503): the URL and seq counter, the identity header set (the
// session + X-Device-Mid + X-ZCode-App-Version + X-Client-Language; the TV
// plane's X-ZCode-Agent stays off), the walk-based delivery parse with the
// capture's banner shape, the unread doctrine (a feed that doesn't read is
// an error, never "no plans"), the merged discovery order/dedupe, and the
// scheduler's multi-claim fold.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// weekendTouchFixture mirrors magpie #503's 10-30 capture shape
// (kongbai01203): data.deliveries[].banner with a claim_zcode_plan button
// and the plan's name in banner.background.args.zcode_plan.
func weekendTouchFixture(id string) string {
	return `{"code":0,"msg":"","data":{"deliveries":[` +
		`{"campaign_id":"2101141259813175296","banner":{` +
		`"background":{"type":"zcode_plan","args":{"zcode_plan":{` +
		`"plan_id":"` + id + `","name":"ZCODE WEEKEND BUILD","grant_units":500000000,"period":"one_time"}}},` +
		`"buttons":[` +
		`{"text":"领取","action":{"type":"claim_zcode_plan","args":{"plan_id":"` + id + `"}}},` +
		`{"text":"关闭","action":{"type":"close","args":{}}}]}}]}}`
}

// touchSeqRecorder serves the touch fixture while recording query params and
// headers, plus optional extra handlers.
func touchSeqRecorder(t *testing.T, fixture string, record *sync.Map) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			record.Store("path", r.URL.Path)
			record.Store("query", r.URL.RawQuery)
			record.Store("auth", r.Header.Get("Authorization"))
			record.Store("device", r.Header.Get("X-Device-Mid"))
			record.Store("version", r.Header.Get("X-ZCode-App-Version"))
			record.Store("language", r.Header.Get("X-Client-Language"))
			record.Store("agent", r.Header.Get("X-ZCode-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fixture)
	}))
}

func withTestAPIBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := zcodeAPIBase
	zcodeAPIBase = srv.URL + "/api/v1"
	t.Cleanup(func() { zcodeAPIBase = old })
}

func TestFetchTouchPlansWireContract(t *testing.T) {
	const weekend = "zcode-v3-start-plan-1030-wk"
	var record sync.Map
	srv := touchSeqRecorder(t, weekendTouchFixture(weekend), &record)
	defer srv.Close()
	withTestAPIBase(t, srv)

	plans, err := fetchTouchPlans(previewTestAuth())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, _ := record.Load("path"); got != "/api/v1/marketing/touch" {
		t.Fatalf("path = %v", got)
	}
	if q, _ := record.Load("query"); !strings.Contains(q.(string), "seq=") {
		t.Fatalf("query %q missing seq", q)
	}
	if got, _ := record.Load("auth"); got != "Bearer test-jwt" {
		t.Fatalf("Authorization = %v", got)
	}
	if got, _ := record.Load("device"); got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("X-Device-Mid = %v", got)
	}
	if got, _ := record.Load("version"); got == "" {
		t.Fatalf("X-ZCode-App-Version missing")
	}
	if got, _ := record.Load("language"); got == "" {
		t.Fatalf("X-Client-Language missing")
	}
	if got, _ := record.Load("agent"); got != "" {
		t.Fatalf("X-ZCode-Agent = %v (must stay off the TV plane)", got)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	p := plans[0]
	if p.PlanID != weekend || p.Name != "ZCODE WEEKEND BUILD" {
		t.Fatalf("plan = %+v", p)
	}
	if p.Source != previewSourceBanner {
		t.Fatalf("source = %q, want banner", p.Source)
	}
	if len(p.Entitlements) != 1 || p.Entitlements[0].Grant != 500000000 {
		t.Fatalf("grant entitlement = %+v (500M weekend build)", p.Entitlements)
	}
	// seq counts up from 0 per process: two more fetches must move it.
	first := nextTouchSeq()
	second := nextTouchSeq()
	if second != first+1 {
		t.Fatalf("seq must count up: %d then %d", first, second)
	}
}

func TestFetchTouchPlansTolerance(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		wantErr string // "" = must succeed with zero plans
	}{
		{"no deliveries key", `{"code":0,"msg":"","data":{}}`, 200, ""},
		{"null deliveries", `{"code":0,"msg":"","data":{"deliveries":null}}`, 200, ""},
		{"empty deliveries", `{"code":0,"msg":"","data":{"deliveries":[]}}`, 200, ""},
		{"feed not deployed (404)", `{"error":"not found"}`, 404, ""},
		{"deliveries not a list", `{"code":0,"msg":"","data":{"deliveries":{"a":1}}}`, 200, "not a list"},
		{"garbage json", `not json at all`, 200, "parse"},
		{"biz code refused", `{"code":3001,"msg":"parameter error","data":{"deliveries":[]}}`, 200, "code=3001"},
		{"http error", `boom`, 500, "http 500"},
		{"no deliveries on data-less body", `{"code":0,"msg":""}`, 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			withTestAPIBase(t, srv)
			plans, err := fetchTouchPlans(previewTestAuth())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("must read as empty, got %v", err)
				}
				if len(plans) != 0 {
					t.Fatalf("plans = %d, want 0", len(plans))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestTouchPlansFromDeliveryWalk(t *testing.T) {
	// Non-object deliveries contribute nothing; a claim action nested in
	// arrays is still found; actions without a plan_id are skipped; a
	// duplicate id collapses; a name-less plan falls back to its id.
	delivery := map[string]any{
		"banner": map[string]any{
			"buttons": []any{
				map[string]any{"action": map[string]any{"type": "claim_zcode_plan", "args": map[string]any{}}},
				[]any{map[string]any{"action": map[string]any{"type": "claim_zcode_plan", "args": map[string]any{"plan_id": " deep-id "}}}},
				map[string]any{"action": map[string]any{"type": "claim_zcode_plan", "args": map[string]any{"plan_id": "deep-id"}}},
			},
		},
	}
	plans := touchPlansFromDelivery(delivery)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want the one trimmed deduped id", plans)
	}
	if plans[0].PlanID != "deep-id" || plans[0].Name != "deep-id" {
		t.Fatalf("plan = %+v (trim + id fallback)", plans[0])
	}
	if touchPlansFromDelivery("not an object") != nil {
		t.Fatalf("scalar delivery must contribute nothing")
	}
	// The walk stops at the depth cap: a claim action 20 levels down is not
	// the app's shape and must not be found.
	deep := map[string]any{"action": map[string]any{"type": "claim_zcode_plan", "args": map[string]any{"plan_id": "too-deep"}}}
	for i := 0; i < 20; i++ {
		deep = map[string]any{"next": deep}
	}
	if got := touchPlansFromDelivery(deep); got != nil {
		t.Fatalf("over-deep action must be skipped: %+v", got)
	}
}

func TestFetchClaimablePlansMerge(t *testing.T) {
	const bannerID = "zcode-v3-start-plan-1030-wk"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/marketing/touch":
			_, _ = io.WriteString(w, weekendTouchFixture(bannerID))
		case "/api/v1/zcode-plan/billing/preview":
			// The same plan_id also sits on the preview list (richer meta) —
			// first sighting (the banner) must win — plus a preview-only plan.
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"plans":[`+
				`{"plan_id":"`+bannerID+`","name":"ZCODE WEEKEND BUILD (preview)","priority":9},`+
				`{"plan_id":"preview-only","name":"Trust Build","priority":1}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withTestAPIBase(t, srv)

	set := fetchClaimablePlans(previewTestAuth())
	if len(set.Unread) != 0 {
		t.Fatalf("unread = %v, want none", set.Unread)
	}
	if len(set.Plans) != 2 {
		t.Fatalf("plans = %+v, want banner+preview-only deduped to 2", set.Plans)
	}
	if set.Plans[0].PlanID != bannerID || set.Plans[0].Source != previewSourceBanner || set.Plans[0].Name != "ZCODE WEEKEND BUILD" {
		t.Fatalf("banner plan must keep first-sighting meta: %+v", set.Plans[0])
	}
	if set.Plans[1].PlanID != "preview-only" || set.Plans[1].Source != previewSourcePreview {
		t.Fatalf("preview plan = %+v", set.Plans[1])
	}
}

func TestFetchClaimablePlansUnreadDoctrine(t *testing.T) {
	// The banner feed answers the weekend plan; billing/preview 500s. The
	// plan is still claimed and the unread list is named.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/marketing/touch" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, weekendTouchFixture("wk"))
			return
		}
		http.Error(w, "preview down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	withTestAPIBase(t, srv)

	set := fetchClaimablePlans(previewTestAuth())
	if len(set.Plans) != 1 || set.Plans[0].PlanID != "wk" {
		t.Fatalf("plans = %+v, want the banner plan beside the unread list", set.Plans)
	}
	if len(set.Unread) != 1 || !strings.Contains(set.Unread[0], "billing preview") || !strings.Contains(set.Unread[0], "http 500") {
		t.Fatalf("unread = %v, want the preview failure named", set.Unread)
	}

	// Both lists unread → no plans, but unread names them: unknown, not none.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv2.Close()
	withTestAPIBase(t, srv2)
	set = fetchClaimablePlans(previewTestAuth())
	if len(set.Plans) != 0 || len(set.Unread) != 2 {
		t.Fatalf("set = %+v unread=%v, want 0 plans + 2 unread", set.Plans, set.Unread)
	}
}

func TestFoldClaimOutcomes(t *testing.T) {
	now := time.Now()
	cooldown := 10 * time.Minute

	// One success → claimed, hold to ends_at, message names it.
	st := &claimAccountState{}
	foldClaimOutcomes(st, claimableSet{}, []claimOutcome{{OK: true, Kind: "claimed", EndsAt: now.Add(48 * time.Hour).Unix()}}, []string{"ZCODE WEEKEND BUILD"}, cooldown, now)
	if st.Kind != "claimed" || st.Badge || st.Message != "claimed ZCODE WEEKEND BUILD" {
		t.Fatalf("single success fold = %+v", st)
	}
	if !st.HoldUntil.Equal(time.Unix(now.Add(48*time.Hour).Unix(), 0)) {
		t.Fatalf("hold = %v, want ends_at", st.HoldUntil)
	}

	// Success beside a captcha → the captcha wins the hold (badge +
	// cooldown) so the account comes back; the message names both.
	st = &claimAccountState{}
	foldClaimOutcomes(st, claimableSet{},
		[]claimOutcome{{OK: true, EndsAt: now.Add(48 * time.Hour).Unix()}, {Kind: "captcha", Message: "verify failed"}},
		[]string{"WEEKEND", "TRUST"}, cooldown, now)
	if !st.Badge || !st.HoldUntil.Equal(now.Add(cooldown)) {
		t.Fatalf("captcha must win the hold: %+v", st)
	}
	want := "claimed WEEKEND; TRUST: claim it in the panel (verify failed)"
	if st.Message != want {
		t.Fatalf("message = %q, want %q", st.Message, want)
	}

	// An unread list caps the hold at one hour even beside a success.
	st = &claimAccountState{}
	foldClaimOutcomes(st, claimableSet{Unread: []string{"banner feed: http 502"}},
		[]claimOutcome{{OK: true, EndsAt: now.Add(48 * time.Hour).Unix()}}, []string{"WEEKEND"}, cooldown, now)
	if !st.HoldUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("unread must cap the hold at 1h: %v", st.HoldUntil)
	}
	if !strings.Contains(st.Message, "banner feed: http 502") {
		t.Fatalf("message must name the unread list: %q", st.Message)
	}

	// already_claimed alone → the quiet long hold, no badge.
	st = &claimAccountState{}
	foldClaimOutcomes(st, claimableSet{}, []claimOutcome{{Kind: "already_claimed"}}, []string{"WEEKEND"}, cooldown, now)
	if st.Badge || st.HoldUntil.Before(now.Add(12*time.Hour)) {
		t.Fatalf("already_claimed fold = %+v", st)
	}

	// login_required outranks captcha for the hold window.
	st = &claimAccountState{}
	foldClaimOutcomes(st, claimableSet{},
		[]claimOutcome{{Kind: "captcha"}, {Kind: "login_required", Message: "expired"}},
		[]string{"A", "B"}, cooldown, now)
	if !st.Badge || !st.HoldUntil.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("login_required must win: %+v", st)
	}

	// Empty outs: nothing folded, no panic.
	st = &claimAccountState{}
	foldClaimOutcomes(st, claimableSet{}, nil, nil, cooldown, now)
	if st.Kind != "" || st.Message != "" {
		t.Fatalf("empty fold must not touch state: %+v", st)
	}
}

func TestFetchTouchPlansNoJWT(t *testing.T) {
	sa := previewTestAuth()
	sa.Auth.JWT = ""
	if _, err := fetchTouchPlans(sa); err == nil || !strings.Contains(err.Error(), "no plan JWT") {
		t.Fatalf("want no-JWT error, got %v", err)
	}
}
