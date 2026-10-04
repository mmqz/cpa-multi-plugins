// growth_contract_test.go covers the 2026-09 growth-center contract fixes
// (Coding2API f18dc3d / growth_runner parity): five-state accept candidates,
// completed-status claimability, HTTP-status error classification
// (session-dead vs tier-locked vs unknown-tier), per-task accept results,
// granted-field redemption with the legacy day retry, travel-claim credit
// priority, and the lottery chances endpoint fallback.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGrowthAcceptCandidatesFiveState: only ""/"not_accepted" enroll.
func TestGrowthAcceptCandidatesFiveState(t *testing.T) {
	tasks := []growthTask{
		{TaskCode: "missing", AcceptStatus: ""},
		{TaskCode: "fresh"},
		{TaskCode: "not_acc", AcceptStatus: "not_accepted"},
		{TaskCode: "acc", AcceptStatus: "accepted"},
		{TaskCode: "prog", AcceptStatus: "in_progress"},
		{TaskCode: "done", AcceptStatus: "completed"},
		{TaskCode: "cl", AcceptStatus: "claimed"},
		{TaskCode: "lk", AcceptStatus: "not_accepted", Locked: true},
	}
	got := growthAcceptCandidates(tasks)
	want := []string{"missing", "fresh", "not_acc"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

// TestParseGrowthTasksClaimableCompleted: accept_status=="completed" is the
// authoritative claim signal even when the progress fields lag behind.
func TestParseGrowthTasksClaimableCompleted(t *testing.T) {
	raw := `{"tasks":[
                {"task_code":"lagged","accept_status":"completed","progress":{"current":1,"target":5}},
                {"task_code":"by_progress","accept_status":"in_progress","progress":{"current":5,"target":5}},
                {"task_code":"completed_claimed","accept_status":"claimed","progress":{"current":9,"target":9}}
        ]}`
	tasks := parseGrowthTasks([]byte(raw))
	if len(tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(tasks))
	}
	if !tasks[0].Claimable {
		t.Errorf("completed task must be claimable even with lagging progress: %+v", tasks[0])
	}
	if !tasks[1].Claimable {
		t.Errorf("progress-reached fallback must stay claimable: %+v", tasks[1])
	}
	if tasks[2].Claimable || !tasks[2].Claimed {
		t.Errorf("claimed stays non-claimable: %+v", tasks[2])
	}
}

// TestGrowthHTTPErrorClassify: the error taxonomy used by the daily loop.
func TestGrowthHTTPErrorClassify(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		status      int
		sessionDead bool
		tierLocked  bool
		unknownTier bool
	}{
		{"401 html", &growthHTTPError{status: 401, msg: "<html>denied</html>"}, 401, true, false, false},
		{"403 tier lock", &growthHTTPError{status: 403, msg: "code=1 msg=连续登录天数不足"}, 403, false, true, false},
		{"403 other", &growthHTTPError{status: 403, msg: "forbidden"}, 403, true, false, false},
		{"400 unknown tier", &growthHTTPError{status: 400, msg: "unknown tier"}, 400, false, false, true},
		{"400 invalid tier param", &growthHTTPError{status: 400, msg: "invalid tier value"}, 400, false, false, true},
		{"400 other", &growthHTTPError{status: 400, msg: "bad body"}, 400, false, false, false},
		{"429 quota", &growthHTTPError{status: 429, msg: "slow down"}, 429, false, false, false},
		{"plain error", errExample, 0, false, false, false},
	}
	for _, tc := range cases {
		if got := growthErrStatus(tc.err); got != tc.status {
			t.Errorf("%s: growthErrStatus=%d want %d", tc.name, got, tc.status)
		}
		if got := isGrowthSessionDead(tc.err); got != tc.sessionDead {
			t.Errorf("%s: isGrowthSessionDead=%v want %v", tc.name, got, tc.sessionDead)
		}
		if got := isGrowthTierLocked(tc.err); got != tc.tierLocked {
			t.Errorf("%s: isGrowthTierLocked=%v want %v", tc.name, got, tc.tierLocked)
		}
		if got := isGrowthUnknownTier(tc.err); got != tc.unknownTier {
			t.Errorf("%s: isGrowthUnknownTier=%v want %v", tc.name, got, tc.unknownTier)
		}
	}
	if growthErrStatus(nil) != 0 {
		t.Errorf("nil error must report status 0")
	}
}

var errExample error = &plainErr{"boom"}

type plainErr struct{ s string }

func (e *plainErr) Error() string { return e.s }

// TestGrowthDoHTTPErrors: growthDo maps every >=400 answer onto
// growthHTTPError with the envelope msg preserved.
func TestGrowthDoHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/activity/growth/locked":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":1001,"msg":"连续登录天数不足"}`))
		case "/activity/growth/dead":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`<html>gateway</html>`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	sa := &storedAuth{}

	_, err := growthCall(sa, http.MethodGet, "/activity/growth/locked", nil)
	if err == nil || growthErrStatus(err) != 403 || !isGrowthTierLocked(err) || isGrowthSessionDead(err) {
		t.Fatalf("403 tier-lock classification failed: %v", err)
	}
	if !strings.Contains(err.Error(), "不足") {
		t.Errorf("tier-lock error should carry the upstream msg, got %q", err.Error())
	}

	_, err = growthCall(sa, http.MethodGet, "/activity/growth/dead", nil)
	if err == nil || growthErrStatus(err) != 401 || !isGrowthSessionDead(err) {
		t.Fatalf("401 session-dead classification failed: %v", err)
	}
}

// TestGrowthAcceptTasksResultsParsing: per-task results surface verbatim,
// including error entries with their upstream message.
func TestGrowthAcceptTasksResultsParsing(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"results":[
                        {"task_code":"a","status":"ok"},
                        {"task_code":"b","status":"error","message":"prerequisite not met"}
                ]}}`))
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	results, err := growthAcceptTasks(sa, []string{"a", "b"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 per-task results, got %d", len(results))
	}
	if results[1].Status != "error" || !strings.Contains(results[1].Message, "prerequisite") {
		t.Errorf("error entry must surface its message: %+v", results[1])
	}
	if !strings.Contains(string(body), `"task_codes"`) || !strings.Contains(string(body), `"a"`) {
		t.Errorf("request must use the plural array body, got %s", body)
	}
}

// TestGrowthRedeemTierGrantedAndDayRetry: granted fields are read first, and
// a 400 unknown-tier answer retries once with the legacy day-number form.
func TestGrowthRedeemTierGrantedAndDayRetry(t *testing.T) {
	var tiers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"7d"`) {
			tiers = append(tiers, "7d")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"unknown tier"}`))
			return
		}
		if strings.Contains(string(body), `"14d"`) {
			tiers = append(tiers, "14d")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit_granted":300,"energy_granted":20}}`))
			return
		}
		if strings.Contains(string(body), `7`) {
			tiers = append(tiers, "day-7")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit_granted":150,"energy_granted":10}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":1,"msg":"bad"}`))
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	sa := &storedAuth{}

	credit, energy, err := growthRedeemTier(sa, "14d")
	if err != nil || credit != 300 || energy != 20 {
		t.Fatalf("granted fields: got %d/%d err=%v", credit, energy, err)
	}

	credit, energy, err = growthRedeemTier(sa, "7d")
	if err != nil || credit != 150 || energy != 10 {
		t.Fatalf("day retry: got %d/%d err=%v", credit, energy, err)
	}
	if len(tiers) != 3 || tiers[0] != "14d" || tiers[1] != "7d" || tiers[2] != "day-7" {
		t.Fatalf("retry sequence wrong: %v", tiers)
	}
}

// TestGrowthTravelClaimCreditPriority: the 2026-09 credit field wins,
// reward_credit stays the fallback.
func TestGrowthTravelClaimCreditPriority(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit":600,"reward_credit":500}}`))
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	got, err := growthTravelClaim(sa, 42)
	if err != nil || got != 600 {
		t.Fatalf("credit field must win: got %d err=%v", got, err)
	}
}

// TestGrowthLotteryChancesFallback: /lottery/chances answers first; the
// legacy summary endpoint takes over when the new one is missing.
func TestGrowthLotteryChancesFallback(t *testing.T) {
	paths := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == growthLotteryChancesPath {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"chances":3,"module":{"enabled":true}}}`))
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	chances, err := growthLotteryChances(sa)
	if err != nil || chances != 3 {
		t.Fatalf("fallback: chances=%d err=%v", chances, err)
	}
	if len(paths) != 2 || paths[0] != growthLotteryChancesPath || paths[1] != growthLotterySummary {
		t.Fatalf("fallback order wrong: %v", paths)
	}
}
