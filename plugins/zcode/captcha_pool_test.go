// captcha_pool_test.go pins the v0.2.0 captcha-pool and auto-claim
// machinery: FIFO + capacity + dedupe + TTL on the pool, the challenge-gated
// retry route (token attached ONLY on a real 3007 challenge and consumed
// from the pool), the claim-target picker, the upstream scheduler's backoff
// table, and the captcha-less scheduler probe (no header on the wire).
package main

import (
        "io"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
        "time"
)

func resetCaptchaPool(t *testing.T) {
        t.Helper()
        captchaPoolState.Lock()
        captchaPoolState.entries = nil
        captchaPoolState.Unlock()
}

func TestCaptchaPoolFIFOAndCap(t *testing.T) {
        resetCaptchaPool(t)
        defer resetCaptchaPool(t)

        if captchaPoolPut("", "sgp") {
                t.Fatal("empty param must be rejected")
        }
        for i := 0; i < captchaPoolCapacity+2; i++ {
                if !captchaPoolPut("param-"+string(rune('a'+i)), "sgp") {
                        t.Fatalf("put %d rejected", i)
                }
        }
        // Capacity: the two oldest were dropped; the first survivor is param-c.
        p, _ := captchaPoolTake()
        if p != "param-c" {
                t.Fatalf("FIFO order broken: first survivor = %q (want param-c)", p)
        }
        // Drain fully.
        for i := 0; i < captchaPoolCapacity; i++ {
                captchaPoolTake()
        }
        if p, _ := captchaPoolTake(); p != "" {
                t.Fatalf("pool not dry: %q", p)
        }
}

func TestCaptchaPoolDedupeAndTTL(t *testing.T) {
        resetCaptchaPool(t)
        defer resetCaptchaPool(t)

        if !captchaPoolPut("same", "sgp") {
                t.Fatal("first put rejected")
        }
        if captchaPoolPut("same", "sgp") {
                t.Fatal("duplicate param accepted")
        }
        // Stale entries are dropped on take, not handed out.
        captchaPoolState.Lock()
        captchaPoolState.entries[0].mintedAt = time.Now().Add(-captchaPoolTokenTTL - time.Minute)
        captchaPoolState.Unlock()
        if p, _ := captchaPoolTake(); p != "" {
                t.Fatalf("stale token handed out: %q", p)
        }
}

func TestCaptchaRetryRouteAttachesPoolToken(t *testing.T) {
        resetCaptchaPool(t)
        defer resetCaptchaPool(t)

        sa := claimTestAuth()
        body := `{"model":"glm-5.3-flash"}`

        // No challenge → no retry.
        if _, ok := captchaRetryRoute(http.StatusTooManyRequests, `{"error":{"code":"1113"}}`, nil); ok {
                t.Fatal("non-challenge failure must not retry")
        }
        // Challenge but dry pool → no retry.
        if _, ok := captchaRetryRoute(http.StatusBadRequest, `{"code":3007}`, nil); ok {
                t.Fatal("dry pool must not retry")
        }
        // Challenge (in-body 3007 variant) + token → retry with the header.
        if !captchaPoolPut("pool-param-1", "sgp") {
                t.Fatal("put failed")
        }
        route, ok := captchaRetryRoute(http.StatusBadRequest, `{"code": 3007}`, nil)
        if !ok {
                t.Fatal("challenge + token must retry")
        }
        if !route.anthropic {
                t.Fatal("captcha retry must ride the anthropic gateway")
        }
        req, _ := http.NewRequest(http.MethodPost, route.endpoint, strings.NewReader(body))
        route.applyHeaders(req, sa, body)
        if got := req.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "pool-param-1" {
                t.Fatalf("captcha header = %q", got)
        }
        if got := req.Header.Get("X-Aliyun-Captcha-Verify-Region"); got != "sgp" {
                t.Fatalf("region header = %q", got)
        }
        // The token was consumed — a second challenge finds a dry pool.
        if _, ok := captchaRetryRoute(http.StatusBadRequest, `{"code":3007}`, nil); ok {
                t.Fatal("token not consumed by the first retry")
        }
        // Header-variant challenge: non-2xx with the captcha response header.
        if !captchaPoolPut("pool-param-2", "") {
                t.Fatal("put failed")
        }
        hdrs := http.Header{}
        hdrs.Set("x-aliyun-captcha-verify-param", "challenge-blob")
        route, ok = captchaRetryRoute(http.StatusForbidden, `{"error":"forbidden"}`, hdrs)
        if !ok {
                t.Fatal("header-variant challenge must retry")
        }
        req, _ = http.NewRequest(http.MethodPost, route.endpoint, strings.NewReader(body))
        route.applyHeaders(req, sa, body)
        if got := req.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "pool-param-2" {
                t.Fatalf("second retry param = %q", got)
        }
}

func TestPickClaimTarget(t *testing.T) {
        plans := []previewPlan{
                {PlanID: "p-low", Priority: 1},
                {PlanID: "p-top", Priority: 10},
                {PlanID: "p-mid", Priority: 5},
        }
        if got := pickClaimTarget(plans, ""); got == nil || got.PlanID != "p-top" {
                t.Fatalf("highest priority not picked: %+v", got)
        }
        if got := pickClaimTarget(plans, "p-mid"); got == nil || got.PlanID != "p-mid" {
                t.Fatalf("explicit plan_id not honored: %+v", got)
        }
        if got := pickClaimTarget(plans, "p-missing"); got != nil {
                t.Fatalf("missing plan_id must yield nil: %+v", got)
        }
        if got := pickClaimTarget(nil, ""); got != nil {
                t.Fatalf("empty preview list must yield nil: %+v", got)
        }
}

func TestApplyClaimOutcomeSemantics(t *testing.T) {
        now := time.Now()
        cooldown := 10 * time.Minute

        // Claimed → hold until the plan's ends_at.
        st := &claimAccountState{}
        applyClaimOutcome(st, claimOutcome{OK: true, PlanID: "p", EndsAt: now.Add(48 * time.Hour).Unix()}, cooldown, now)
        if st.Kind != "claimed" || !st.HoldUntil.Equal(time.Unix(now.Add(48*time.Hour).Unix(), 0)) || st.Badge {
                t.Fatalf("claimed semantics wrong: %+v", st)
        }
        // already_claimed → hold too, no badge.
        st = &claimAccountState{}
        applyClaimOutcome(st, claimOutcome{Kind: "already_claimed"}, cooldown, now)
        if st.HoldUntil.Before(now.Add(12*time.Hour)) || st.Badge {
                t.Fatalf("already_claimed semantics wrong: %+v", st)
        }
        // captcha → badge + cooldown.
        st = &claimAccountState{}
        applyClaimOutcome(st, claimOutcome{Kind: "captcha", Message: "challenge"}, cooldown, now)
        if !st.Badge || !st.HoldUntil.Equal(now.Add(cooldown)) {
                t.Fatalf("captcha semantics wrong: %+v", st)
        }
        // quota_exhausted → 1h hold, no badge.
        st = &claimAccountState{}
        applyClaimOutcome(st, claimOutcome{Kind: "quota_exhausted"}, cooldown, now)
        if st.Badge || !st.HoldUntil.Equal(now.Add(time.Hour)) {
                t.Fatalf("quota_exhausted semantics wrong: %+v", st)
        }
        // login_required → badge + 30 min.
        st = &claimAccountState{}
        applyClaimOutcome(st, claimOutcome{Kind: "login_required"}, cooldown, now)
        if !st.Badge || !st.HoldUntil.Equal(now.Add(30*time.Minute)) {
                t.Fatalf("login_required semantics wrong: %+v", st)
        }
        // ineligible → plain cooldown, no badge.
        st = &claimAccountState{}
        applyClaimOutcome(st, claimOutcome{Kind: "ineligible"}, cooldown, now)
        if st.Badge || !st.HoldUntil.Equal(now.Add(cooldown)) {
                t.Fatalf("ineligible semantics wrong: %+v", st)
        }
}

func TestPostClaimProbeOmitsCaptchaHeader(t *testing.T) {
        var gotCap, gotRegion string
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                gotCap = r.Header.Get("X-Aliyun-Captcha-Verify-Param")
                gotRegion = r.Header.Get("X-Aliyun-Captcha-Verify-Region")
                w.Header().Set("Content-Type", "application/json")
                _, _ = io.WriteString(w, `{"code":3007,"msg":"captcha required"}`)
        }))
        defer srv.Close()
        old := zcodeAPIBase
        zcodeAPIBase = srv.URL + "/api/v1"
        t.Cleanup(func() { zcodeAPIBase = old })

        // Probe: empty captcha param → the request goes out WITHOUT the captcha
        // headers; the upstream 3007 is the definitive "campaign is gated" answer.
        out := postClaim(claimTestAuth(), "weekend-free-1024", "", "")
        if gotCap != "" || gotRegion != "" {
                t.Fatalf("probe must not carry captcha headers: cap=%q region=%q", gotCap, gotRegion)
        }
        if out.Kind != "captcha" {
                t.Fatalf("probe 3007 must classify as captcha: %+v", out)
        }
}

func TestClaimSchedulerTickSmoke(t *testing.T) {
        // Without a host bridge hostAuthList fails — the tick must record the
        // error and survive (no panic, no goroutine leak from this path). Whether
        // lastError lands depends on bridge presence; both outcomes are fine.
        claimSchedulerTick(time.Now())
        claimSchedulerState.Lock()
        lastError := claimSchedulerState.lastError
        claimSchedulerState.Unlock()
        t.Logf("tick smoke: lastError=%q", lastError)
}
