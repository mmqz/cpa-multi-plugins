// task_auto_test.go covers the auto-light orchestration: action-table
// sanity, idempotent skips, delta-top-up reporting with async-scored
// read-back + auto-claim, and the daily-loop ordering regression that
// pins buddy adoption BEFORE task acceptance (fresh accounts have every
// task gated by first_buddy — the v0.9.22-and-earlier loop lost a full
// run to this ordering).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAutoActionTableSanity(t *testing.T) {
	if len(growthAutoActions) < 10 {
		t.Fatalf("auto-light table unexpectedly small: %d actions", len(growthAutoActions))
	}
	seen := map[string]bool{}
	for _, a := range growthAutoActions {
		if a.Code == "" || a.run == nil {
			t.Fatalf("action entry incomplete: %+v", a)
		}
		if seen[a.Code] {
			t.Errorf("duplicate action code: %s", a.Code)
		}
		seen[a.Code] = true
	}
	for code := range growthMPTaskCodes {
		if !seen[code] {
			t.Errorf("mp code %s registered but has no action", code)
		}
	}
	for _, a := range growthAutoActions {
		if growthMPTaskCodes[a.Code] != a.mp {
			t.Errorf("mp flag mismatch for %s: table=%v registry=%v", a.Code, a.mp, growthMPTaskCodes[a.Code])
		}
	}
}

// autoLightSrv is a scripted upstream serving every growth/billing/web
// domain from one mux: it counts /v2/report hits, tracks the request
// order, flips chat_5 progress on the Nth list query (upstream scores
// asynchronously), and pays out rewards on the claim path.
type autoLightSrv struct {
	mu          sync.Mutex
	srv         *httptest.Server
	reports     int
	claims      int
	adopted     bool
	listQueries int
	flipAfter   int // serve the scored/claimable shape from this query on
	chat5       *growthTask
	paths       []string
}

func newAutoLightSrv(t *testing.T) *autoLightSrv {
	a := &autoLightSrv{}
	mux := http.NewServeMux()
	record := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			a.paths = append(a.paths, r.Method+" "+r.URL.Path)
			a.mu.Unlock()
			h(w, r)
		}
	}
	writeOK := func(w http.ResponseWriter, body string) {
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/v2/report", record(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.reports++
		a.mu.Unlock()
		writeOK(w, `{"code":0,"data":{}}`)
	}))
	mux.HandleFunc("/v2/activity/growth/tasks", record(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.listQueries++
		if a.chat5 == nil {
			writeOK(w, `{"code":0,"data":{"tasks":[]}}`)
			return
		}
		served := *a.chat5
		if a.flipAfter > 0 && a.listQueries >= a.flipAfter {
			served.Current, served.Target = 5, 5
			served.Claimable = !served.Claimed
			served.AcceptStatus = "completed"
		}
		c, _ := json.Marshal(served)
		writeOK(w, fmt.Sprintf(`{"code":0,"data":{"tasks":[%s]}}`, c))
	}))
	mux.HandleFunc("/v2/activity/growth/tasks/accept", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{"results":[]}}`)
	}))
	mux.HandleFunc("/activity/growth/tasks/", record(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.claims++
		a.mu.Unlock()
		writeOK(w, `{"code":0,"data":{"credit":100,"energy":0}}`)
	}))
	mux.HandleFunc("/activity/growth/buddy/info", record(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		adopted := a.adopted
		a.mu.Unlock()
		if adopted {
			writeOK(w, `{"code":0,"data":{"id":"b1"}}`)
			return
		}
		writeOK(w, `{"code":0,"data":null}`)
	}))
	mux.HandleFunc("/activity/growth/buddy/agreement", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{}}`)
	}))
	mux.HandleFunc("/activity/growth/buddy/first", record(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.adopted = true
		a.mu.Unlock()
		writeOK(w, `{"code":0,"data":{"credit":300}}`)
	}))
	mux.HandleFunc("/activity/growth/streak", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{"streak":{"days":0},"redemption_status":{},"makeup_cards":{"balance":0}}}`)
	}))
	mux.HandleFunc("/activity/growth/heatmap", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{"cells":[]}}`)
	}))
	mux.HandleFunc("/activity/growth/lottery/chances", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{"chances":0}}`)
	}))
	mux.HandleFunc("/activity/growth/buddy/travel/status", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{"state":"traveling"}}`)
	}))
	// Everything else (gift/compensation/quota/energy/school/...) answers a
	// clean empty envelope — zero values keep every step on its skip path.
	mux.HandleFunc("/", record(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, `{"code":0,"data":{}}`)
	}))
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *autoLightSrv) countReports() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reports
}

func (a *autoLightSrv) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...)
}

func TestAutoLightSkipsCompleted(t *testing.T) {
	oldGap, oldPoll := growthAutoGap, growthClaimPollGap
	growthAutoGap, growthClaimPollGap = 0, 0
	defer func() { growthAutoGap, growthClaimPollGap = oldGap, oldPoll }()

	s := newAutoLightSrv(t)
	restore := setGrowthBase(s.srv.URL)
	defer restore()
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()

	s.mu.Lock()
	s.chat5 = &growthTask{TaskCode: "chat_5", Claimed: true, Current: 5, Target: 5}
	s.mu.Unlock()

	var lines []string
	add := func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) }
	tasksAutoLightOnce(&storedAuth{}, add)

	for _, l := range lines {
		if strings.Contains(l, "点亮「chat_5」") {
			t.Errorf("claimed chat_5 must be skipped idempotently, got line: %s", l)
		}
	}
}

func TestAutoLightTopsUpAndClaims(t *testing.T) {
	oldGap, oldPoll := growthAutoGap, growthClaimPollGap
	growthAutoGap, growthClaimPollGap = 0, 0
	defer func() { growthAutoGap, growthClaimPollGap = oldGap, oldPoll }()

	s := newAutoLightSrv(t)
	restore := setGrowthBase(s.srv.URL)
	defer restore()
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()
	oldWeb := growthWebBaseCN
	growthWebBaseCN = s.srv.URL
	defer func() { growthWebBaseCN = oldWeb }()

	// 2/5 at scan time; the list endpoint flips to 5/5 claimable from the
	// 3rd query (1 = ctx scan, 2-3 = bounded read-back) — async scoring sim.
	s.mu.Lock()
	s.chat5 = &growthTask{TaskCode: "chat_5", AcceptStatus: "accepted", Current: 2, Target: 5, RewardCredit: 100}
	s.flipAfter = 3
	s.mu.Unlock()

	var lines []string
	add := func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) }
	tasksAutoLightOnce(&storedAuth{}, add)

	if reports := s.countReports(); reports < 3 {
		t.Errorf("chat_5 2/5 must top up 3 reports, got %d report calls total", reports)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "点亮「chat_5」") {
		t.Errorf("chat_5 action must be attempted and reported, lines:\n%s", joined)
	}
	if !strings.Contains(joined, "已领 +100 分") {
		t.Errorf("claimable read-back must auto-claim (+100), lines:\n%s", joined)
	}
	if s.claims != 1 {
		t.Errorf("exactly one claim call expected, got %d", s.claims)
	}
}

func TestAutoLightSkipsAbsentTasks(t *testing.T) {
	oldGap, oldPoll := growthAutoGap, growthClaimPollGap
	growthAutoGap, growthClaimPollGap = 0, 0
	defer func() { growthAutoGap, growthClaimPollGap = oldGap, oldPoll }()

	s := newAutoLightSrv(t)
	restore := setGrowthBase(s.srv.URL)
	defer restore()
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()

	var lines []string
	add := func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) }
	tasksAutoLightOnce(&storedAuth{}, add)

	if len(lines) != 0 {
		t.Errorf("an account with no tasks must light nothing silently, got:\n%s",
			strings.Join(lines, "\n"))
	}
	if reports := s.countReports(); reports != 0 {
		t.Errorf("no task → no reports, got %d", reports)
	}
}

func TestDailyBonusAdoptBeforeAccept(t *testing.T) {
	oldGap, oldPoll := growthAutoGap, growthClaimPollGap
	growthAutoGap, growthClaimPollGap = 0, 0
	defer func() { growthAutoGap, growthClaimPollGap = oldGap, oldPoll }()

	s := newAutoLightSrv(t)
	restore := setGrowthBase(s.srv.URL)
	defer restore()
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()
	oldWeb := growthWebBaseCN
	growthWebBaseCN = s.srv.URL
	defer func() { growthWebBaseCN = oldWeb }()

	// One fresh not_accepted task so step 4 actually fires an accept call.
	s.mu.Lock()
	s.chat5 = &growthTask{TaskCode: "chat_5", AcceptStatus: "not_accepted", Current: 0, Target: 5}
	s.mu.Unlock()

	sa := &storedAuth{}
	sa.Account.UID = "u1"
	res := tasksDailyBonus(sa)
	if res == nil || !res.Success {
		t.Fatalf("daily loop must complete: %+v", res)
	}

	adoptIdx, acceptIdx := -1, -1
	for i, p := range s.snapshot() {
		switch {
		case p == "POST /activity/growth/buddy/first" && adoptIdx < 0:
			adoptIdx = i
		case p == "POST /v2/activity/growth/tasks/accept" && acceptIdx < 0:
			acceptIdx = i
		}
	}
	if adoptIdx < 0 || acceptIdx < 0 {
		t.Fatalf("loop must adopt and accept: adopt=%d accept=%d\npaths:\n%s",
			adoptIdx, acceptIdx, strings.Join(s.snapshot(), "\n"))
	}
	if adoptIdx > acceptIdx {
		t.Errorf("adoption must run BEFORE accept (fresh-account gate), got adopt@%d accept@%d",
			adoptIdx, acceptIdx)
	}
}
