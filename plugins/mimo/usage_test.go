// usage_test.go — v0.2.11 quota/note coverage. Every HTTP fixture is the real
// wire shape (user-verified captures, 2026-09): the /user/usage envelope with
// percent/resetAt/resetDate, the resetDate===null no-subscription state, and
// the subscription/self current object. Lane fingerprints are asserted on the
// wire (X-Mimo-Source / Authorization / no cross-lane leakage).
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// usageCred is a cookie-lane credential whose serviceToken row matches the
// httptest host (same trick as cookieCred in plugin_test.go).
func usageCred() *storedAuth {
	return &storedAuth{
		Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{
			{Name: "serviceToken", Value: "tok", Domain: ".127.0.0.1", Path: "/"},
			{Name: "userId", Value: "u-test", Domain: ".account.xiaomi.com", Path: "/"},
		}, Region: "sgp"},
		Account: mimoAccount{UID: "u-test"},
	}
}

func keyCred() *storedAuth {
	return &storedAuth{
		Auth:    mimoTokens{Lane: laneKey, SK: "sk-test", BaseURL: "https://api.xiaomimimo.com/v1", UID: "u1"},
		Account: mimoAccount{UID: "u1"},
	}
}

func TestFetchMimoUsageWireShape(t *testing.T) {
	var sawHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Clone()
		if !strings.HasSuffix(r.URL.Path, "/api/user/usage") {
			t.Errorf("usage path = %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("usage method = %s", r.Method)
		}
		// Real wire: user-verified 200 capture, verbatim.
		_, _ = w.Write([]byte(`{"code":0,"data":{"percent":63.4,"resetAt":1790250598,"resetDate":"2026-09-24"},"message":"success"}`))
	}))
	defer srv.Close()
	origBase := regionBases["sgp"]
	regionBases["sgp"] = srv.URL + "/api"
	defer func() { regionBases["sgp"] = origBase }()

	data, err := fetchMimoUsage(usageCred())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if data.Percent == nil || *data.Percent != 63.4 {
		t.Fatalf("percent = %v, want 63.4", data.Percent)
	}
	if data.ResetDate == nil || *data.ResetDate != "2026-09-24" {
		t.Fatalf("resetDate = %v", data.ResetDate)
	}
	// Cookie-lane fingerprint: desktop engine headers, no Authorization.
	if got := sawHeader.Get("X-Mimo-Source"); got != sourceCookieLane {
		t.Errorf("X-Mimo-Source = %q", got)
	}
	if got := sawHeader.Get("Authorization"); got != "" {
		t.Errorf("cookie lane carried Authorization: %q", got)
	}
	if got := sawHeader.Get("Cookie"); !strings.Contains(got, "serviceToken=tok") {
		t.Errorf("cookie lane lost the ticket: %q", got)
	}
}

func TestFetchMimoUsageKeyLaneFingerprint(t *testing.T) {
	var sawHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Clone()
		_, _ = w.Write([]byte(`{"code":0,"data":{"percent":100,"resetAt":1790250598,"resetDate":"2026-10-24"},"message":"success"}`))
	}))
	defer srv.Close()
	origBase := regionBases["sgp"]
	regionBases["sgp"] = srv.URL + "/api"
	defer func() { regionBases["sgp"] = origBase }()

	if _, err := fetchMimoUsage(keyCred()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := sawHeader.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := sawHeader.Get("X-Mimo-Source"); got != sourceKeyLane {
		t.Errorf("X-Mimo-Source = %q", got)
	}
}

func TestFetchMimoUsageNoSubscription(t *testing.T) {
	// resetDate === null is the bundle's "no active subscription" state —
	// the plugin must report it, not render a percent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"percent":null,"resetAt":null,"resetDate":null},"message":"success"}`))
	}))
	defer srv.Close()
	origBase := regionBases["sgp"]
	regionBases["sgp"] = srv.URL + "/api"
	defer func() { regionBases["sgp"] = origBase }()

	_, err := fetchMimoUsage(usageCred())
	if !errors.Is(err, errNoActiveSubscription) {
		t.Fatalf("err = %v, want errNoActiveSubscription", err)
	}
}

func TestFetchMimoUsage302RenewRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			// Measured session-bounce shape: 302 → account.xiaomi.com serviceLogin.
			w.Header().Set("Location", "https://account.xiaomi.com/pass/serviceLogin?callback=https%3A%2F%2Fmimo-server-sgp.xiaomimimo.com%2Fapi%2Fsts")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"percent":12.5,"resetAt":1790250598,"resetDate":"2026-09-30"},"message":"success"}`))
	}))
	defer srv.Close()
	origBase := regionBases["sgp"]
	regionBases["sgp"] = srv.URL + "/api"
	defer func() { regionBases["sgp"] = origBase }()
	origRenew := renewCookieSessionFn
	renewCookieSessionFn = func(sa *storedAuth) bool { return true }
	defer func() { renewCookieSessionFn = origRenew }()

	data, err := fetchMimoUsage(usageCred())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 302 → renew → retry (2 calls), got %d", calls)
	}
	if data.Percent == nil || *data.Percent != 12.5 {
		t.Fatalf("percent after retry = %v", data.Percent)
	}
}

func TestFetchMimoUsageCodeNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":400,"data":null,"message":"param invalid"}`))
	}))
	defer srv.Close()
	origBase := regionBases["sgp"]
	regionBases["sgp"] = srv.URL + "/api"
	defer func() { regionBases["sgp"] = origBase }()

	_, err := fetchMimoUsage(usageCred())
	if err == nil || !strings.Contains(err.Error(), "param invalid") {
		t.Fatalf("err = %v, want upstream message carried", err)
	}
}

func TestFetchMimoSubscriptionActiveOnly(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		// Real shape per the verified field list; time-field wire types were
		// not part of the capture and are ignored by the parser.
		_, _ = w.Write([]byte(`{"code":0,"data":{"current":{"bizNo":123456,"planCode":"mimo-gl-plus","title":"MiMo GL Plus","planTier":"PLUS","status":"ACTIVE","source":"trial","renewalMode":"AUTO_RENEW","startTime":"2026-09-01","endTime":"2026-10-01","nextResetTime":"2026-10-01","percent":63.4},"subscriptions":[]},"message":"success"}`))
	}))
	defer srv.Close()
	origBase := regionBases["sgp"]
	regionBases["sgp"] = srv.URL + "/api"
	defer func() { regionBases["sgp"] = origBase }()

	sub := fetchMimoSubscription(usageCred())
	if sub == nil || sub.Title != "MiMo GL Plus" || sub.PlanCode != "mimo-gl-plus" {
		t.Fatalf("sub = %+v", sub)
	}
	if !strings.HasSuffix(path, "/api/user/xiaomi/subscription/self") {
		t.Errorf("subscription path = %q", path)
	}

	// A non-ACTIVE record is not a live plan.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"current":{"planCode":"mimo-gl-plus","title":"MiMo GL Plus","status":"EXPIRED"}},"message":"success"}`))
	}))
	defer srv2.Close()
	regionBases["sgp"] = srv2.URL + "/api"
	if sub := fetchMimoSubscription(usageCred()); sub != nil {
		t.Fatalf("expired plan must not surface, got %+v", sub)
	}
}

func TestUsageNoteFor(t *testing.T) {
	pct := 63.4
	reset := "2026-09-24"
	sub := &mimoSubscription{Title: "MiMo GL Plus", PlanCode: "mimo-gl-plus", Status: "ACTIVE"}

	sa := usageCred()
	note := usageNoteFor(sa, &mimoUsageData{Percent: &pct, ResetAt: 1790250598, ResetDate: &reset}, sub)
	if note != "SGP · MiMo GL Plus · 余63.4% · 重置09-24" {
		t.Errorf("full note = %q", note)
	}
	if note = usageNoteFor(sa, &mimoUsageData{Percent: &pct, ResetDate: &reset}, nil); note != "SGP · 余63.4% · 重置09-24" {
		t.Errorf("no-plan note = %q", note)
	}
	// resetDate null ⇒ no active subscription, honestly rendered.
	if note = usageNoteFor(sa, &mimoUsageData{ResetDate: nil}, sub); note != "SGP · 无有效订阅" {
		t.Errorf("no-sub note = %q", note)
	}
	// sk lane prefix is the lane tag.
	if note = usageNoteFor(keyCred(), &mimoUsageData{Percent: &pct, ResetDate: &reset}, nil); !strings.HasPrefix(note, "KEY · ") {
		t.Errorf("key-lane note = %q", note)
	}
	// Whole-note form: percent 100 renders without decimals.
	full := 100.0
	if note = usageNoteFor(sa, &mimoUsageData{Percent: &full, ResetDate: &reset}, nil); note != "SGP · 余100% · 重置09-24" {
		t.Errorf("100%% note = %q", note)
	}
	// A bloated plan title cannot push the note past the card width.
	long := &mimoSubscription{Title: strings.Repeat("额", 40), Status: "ACTIVE"}
	if note = usageNoteFor(sa, &mimoUsageData{Percent: &pct, ResetDate: &reset}, long); len(note) > 90 {
		t.Errorf("unbounded note (%d bytes): %q", len(note), note)
	}
}

func TestShortResetDateAndPercent(t *testing.T) {
	if got := shortResetDate("2026-09-24"); got != "09-24" {
		t.Errorf("shortResetDate = %q", got)
	}
	if got := shortResetDate(" 2026-09-24 "); got != "09-24" {
		t.Errorf("trim = %q", got)
	}
	if got := shortResetDate("09-24"); got != "09-24" {
		t.Errorf("passthrough = %q", got)
	}
	if got := formatPercent(63.4); got != "63.4" {
		t.Errorf("formatPercent(63.4) = %q", got)
	}
	if got := formatPercent(100); got != "100" {
		t.Errorf("formatPercent(100) = %q", got)
	}
}

func TestSyncAuthUsageNote(t *testing.T) {
	// Real adopt-written credential shape (adoption note on disk).
	raw := []byte(`{"type":"mimo","provider":"mimo","disabled":false,"note":"桌面会话收养 · 2026-09-24 10:00","auth":{"lane":"cookie","region":"sgp","sid":"mimosgp","adoptedAt":1790250598,"exchangedAt":1790250600,"cookies":[{"name":"serviceToken","value":"tok","domain":".mimo-server-sgp.xiaomimimo.com","path":"/"}]},"account":{"uid":"u-test"}}`)
	var savedNames []string
	var savedBodies [][]byte
	origGet, origPersist := hostAuthGetPhysicalFn, hostAuthPersistFn
	defer func() { hostAuthGetPhysicalFn, hostAuthPersistFn = origGet, origPersist }()
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "mimo-cookie-u-test.json", JSON: raw}, nil
	}
	hostAuthPersistFn = func(name string, out []byte) error {
		savedNames = append(savedNames, name)
		savedBodies = append(savedBodies, out)
		return nil
	}

	sa := usageCred()
	pct := 63.4
	reset := "2026-09-24"
	u := &mimoUsageData{Percent: &pct, ResetAt: 1790250598, ResetDate: &reset}
	sub := &mimoSubscription{Title: "MiMo GL Plus", PlanCode: "mimo-gl-plus", Status: "ACTIVE"}

	if err := syncAuthUsageNote("idx0", sa, u, sub); err != nil {
		t.Fatal(err)
	}
	if len(savedBodies) != 1 || savedNames[0] != "mimo-cookie-u-test.json" {
		t.Fatalf("first sync must persist once, got %v / %v", savedNames, len(savedBodies))
	}
	// Untouched fields stay byte-exact (RawMessage merge, not map[string]any).
	var base, out map[string]json.RawMessage
	if json.Unmarshal(raw, &base) != nil || json.Unmarshal(savedBodies[0], &out) != nil {
		t.Fatal("parse failed")
	}
	for _, k := range []string{"type", "provider", "disabled", "auth", "account"} {
		if string(base[k]) != string(out[k]) {
			t.Errorf("field %s mutated:\n  %s\n  %s", k, base[k], out[k])
		}
	}
	var note struct {
		Note string `json:"note"`
	}
	_ = json.Unmarshal(savedBodies[0], &note)
	if note.Note != "SGP · MiMo GL Plus · 余63.4% · 重置09-24" {
		t.Errorf("note = %q", note.Note)
	}

	// Idempotence: unchanged note → no second save.
	raw = savedBodies[0]
	if err := syncAuthUsageNote("idx0", sa, u, sub); err != nil {
		t.Fatal(err)
	}
	if len(savedBodies) != 1 {
		t.Fatalf("unchanged note must skip the save, saves=%d", len(savedBodies))
	}

	// No-subscription state overwrites the adoption note honestly.
	resetNil := "2099-01-01"
	_ = resetNil
	if err := syncAuthUsageNote("idx0", sa, &mimoUsageData{ResetDate: nil}, nil); err != nil {
		t.Fatal(err)
	}
	if len(savedBodies) != 2 {
		t.Fatalf("no-sub note must persist, saves=%d", len(savedBodies))
	}
	_ = json.Unmarshal(savedBodies[1], &note)
	if note.Note != "SGP · 无有效订阅" {
		t.Errorf("no-sub note = %q", note.Note)
	}
}

func TestStartUsageNoteLoopQuietWithoutHost(t *testing.T) {
	// No host bridge in unit tests: the loop must refuse to start (no
	// goroutine churn, no sweeps).
	orig := usageSweepFn
	called := false
	usageSweepFn = func() { called = true }
	defer func() { usageSweepFn = orig }()
	startUsageNoteLoop()
	startUsageNoteLoop() // once-guard collapses repeats
	if called {
		t.Fatal("usage sweep must not run without a host bridge")
	}
}
