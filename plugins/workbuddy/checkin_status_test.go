package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// checkinStatusServer routes per-path canned envelopes and counts hits per
// path — the fetchCheckinStatus probing policy (early return vs second
// endpoint) is asserted through the counters. Counters are registered for
// every canned path PLUS both canonical checkin paths, so 404-fallback
// probes are counted too.
func checkinStatusServer(t *testing.T, bodies map[string]string) (*httptest.Server, map[string]*int32) {
	t.Helper()
	counted := map[string]struct{}{}
	for path := range bodies {
		counted[path] = struct{}{}
	}
	counted["/v2/billing/meter/checkin-activity-status"] = struct{}{}
	counted["/v2/billing/meter/checkin-status"] = struct{}{}
	hits := map[string]*int32{}
	for path := range counted {
		var n int32
		hits[path] = &n
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := hits[r.URL.Path]; ok {
			atomic.AddInt32(p, 1)
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return srv, hits
}

func hitCount(p *int32) int32 {
	if p == nil {
		return 0
	}
	return atomic.LoadInt32(p)
}

// Regression for the 2026-09-20 "已签到显示未签到" field report: the first
// endpoint intermittently answers with an activity-shaped object that has NO
// today_checked_in field. The old parser treated the missing field as false
// and poisoned the cache; now the ambiguous payload must trigger the second
// endpoint and both views must OR-merge into "checked in".
func TestFetchCheckinStatus_AmbiguousFirstPayloadProbesSecond(t *testing.T) {
	const p1 = "/v2/billing/meter/checkin-activity-status"
	const p2 = "/v2/billing/meter/checkin-status"
	srv, hits := checkinStatusServer(t, map[string]string{
		p1: `{"code":0,"msg":"ok","data":{"active":true,"activity_name":"九月签到","season":3}}`,
		p2: `{"code":0,"msg":"ok","data":{"today_checked_in":true,"streak_days":3,"week_checkin_days":2}}`,
	})
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sum, err := fetchCheckinStatus(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if !sum.TodayCheckedIn {
		t.Fatalf("ambiguous first payload + true second payload must merge to TodayCheckedIn=true, got %+v", sum)
	}
	if !sum.Active || sum.ActivityName != "九月签到" || sum.Season != 3 {
		t.Fatalf("first endpoint's richer view must be preserved, got %+v", sum)
	}
	if sum.StreakDays != 3 || sum.WeekCheckinDays != 2 {
		t.Fatalf("second endpoint's summary fields must fill in, got %+v", sum)
	}
	if hitCount(hits[p1]) != 1 || hitCount(hits[p2]) != 1 {
		t.Fatalf("expected exactly one probe per endpoint, got p1=%d p2=%d", hitCount(hits[p1]), hitCount(hits[p2]))
	}
}

// The common case must stay a single upstream call: a payload that explicitly
// carries today_checked_in is authoritative and the second endpoint is never
// consulted (false stays false — no pointless OR-flip).
func TestFetchCheckinStatus_AuthoritativeFirstPayloadSingleCall(t *testing.T) {
	const p1 = "/v2/billing/meter/checkin-activity-status"
	const p2 = "/v2/billing/meter/checkin-status"
	srv, hits := checkinStatusServer(t, map[string]string{
		p1: `{"code":0,"msg":"ok","data":{"active":true,"today_checked_in":false}}`,
		p2: `{"code":0,"msg":"ok","data":{"today_checked_in":true}}`,
	})
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sum, err := fetchCheckinStatus(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if sum.TodayCheckedIn {
		t.Fatal("explicit today_checked_in=false must stay false")
	}
	if hitCount(hits[p1]) != 1 || hitCount(hits[p2]) != 0 {
		t.Fatalf("authoritative payload must short-circuit, got p1=%d p2=%d", hitCount(hits[p1]), hitCount(hits[p2]))
	}
}

// The check-in calendar outranks a missing/false today field: when
// checkin_dates contains today's Asia/Shanghai date the account HAS checked
// in — and the answer comes from the single already-authoritative call.
func TestFetchCheckinStatus_DatesCrossCheck(t *testing.T) {
	today := time.Now().In(cstZone).Format("2006-01-02")
	const p1 = "/v2/billing/meter/checkin-activity-status"
	srv, hits := checkinStatusServer(t, map[string]string{
		p1: `{"code":0,"msg":"ok","data":{"active":true,"checkin_dates":["` + today + `","2026-09-19"]}}`,
	})
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sum, err := fetchCheckinStatus(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if !sum.TodayCheckedIn {
		t.Fatalf("today in checkin_dates must imply TodayCheckedIn=true, got %+v", sum)
	}
	if hitCount(hits[p1]) != 1 {
		t.Fatalf("calendar cross-check must be authoritative, got %d calls", hitCount(hits[p1]))
	}
}

// Endpoint failure must degrade to the other endpoint, not fail the call.
func TestFetchCheckinStatus_FirstEndpointFails(t *testing.T) {
	const p1 = "/v2/billing/meter/checkin-activity-status"
	const p2 = "/v2/billing/meter/checkin-status"
	srv, hits := checkinStatusServer(t, map[string]string{
		p2: `{"code":0,"msg":"ok","data":{"active":true,"today_checked_in":true,"streak_days":7}}`,
	})
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sum, err := fetchCheckinStatus(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if !sum.TodayCheckedIn || sum.StreakDays != 7 {
		t.Fatalf("second endpoint must win when first fails, got %+v", sum)
	}
	if hitCount(hits[p1]) != 1 || hitCount(hits[p2]) != 1 {
		t.Fatalf("unexpected probe pattern p1=%d p2=%d", hitCount(hits[p1]), hitCount(hits[p2]))
	}
}

// Both endpoints failing surfaces the error (callers keep the stale-while-
// error cache value).
func TestFetchCheckinStatus_BothFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	if _, err := fetchCheckinStatus(&storedAuth{}); err == nil {
		t.Fatal("both endpoints failing must return an error")
	}
}

// parseCheckinStatusPayload: hasTodayKey must reflect the field's presence,
// not its value — the ambiguity signal that drives the second probe.
func TestParseCheckinStatusPayload_HasTodayKey(t *testing.T) {
	if _, has, err := parseCheckinStatusPayload([]byte(`{"today_checked_in":false}`)); err != nil || !has {
		t.Fatalf("explicit false must set hasTodayKey, got has=%v err=%v", has, err)
	}
	if _, has, err := parseCheckinStatusPayload([]byte(`{"todayCheckedIn":true}`)); err != nil || !has {
		t.Fatalf("camelCase key must set hasTodayKey, got has=%v err=%v", has, err)
	}
	if _, has, err := parseCheckinStatusPayload([]byte(`{"active":true}`)); err != nil || has {
		t.Fatalf("activity-shaped payload must leave hasTodayKey=false, got has=%v err=%v", has, err)
	}
}

// mergeOR is intentionally one-directional: a false never overrides a true.
func TestMergeOR_NeverUnChecks(t *testing.T) {
	base := &checkinSummary{Active: true, TodayCheckedIn: true, StreakDays: 5}
	base.mergeOR(&checkinSummary{Active: false, TodayCheckedIn: false, StreakDays: 1})
	if !base.TodayCheckedIn || !base.Active || base.StreakDays != 5 {
		t.Fatalf("mergeOR must not downgrade, got %+v", base)
	}
}
