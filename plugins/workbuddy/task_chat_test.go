// task_chat_test.go covers the real-conversation task layer: event-builder
// shapes (expert summon / actual-use / skill_info), server-requestId
// extraction from SSE with the watermark scanner, model-aligned reporting,
// the action-table completion (13 fingerprint + 6 chat = 19), expert-batch
// deficit awareness, and the black_cat night-window gating.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGrowthInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{22, false}, {23, true}, {0, true}, {7, true}, {8, false}, {12, false},
	}
	for _, c := range cases {
		now := time.Date(2026, 9, 20, c.hour, 30, 0, 0, time.Local)
		if got := growthInNightWindow(now); got != c.want {
			t.Errorf("hour %d: night window = %v, want %v", c.hour, got, c.want)
		}
	}
}

func TestShouldRunNightTaskNow(t *testing.T) {
	// [23:00, 24:00) window; 00:30 next day is NOT (only the 23 slot fires).
	at := func(h, m int) time.Time { return time.Date(2026, 9, 20, h, m, 0, 0, time.Local) }
	if !shouldRunNightTaskNow(at(23, 0)) {
		t.Error("23:00 should fire the night slot")
	}
	if !shouldRunNightTaskNow(at(23, 59)) {
		t.Error("23:59 should fire the night slot")
	}
	if shouldRunNightTaskNow(at(22, 59)) {
		t.Error("22:59 must not fire the night slot")
	}
	if shouldRunNightTaskNow(at(0, 30)) {
		t.Error("00:30 must not fire the night slot (slot is 23:xx only)")
	}
}

func TestGrowthServerIDRe(t *testing.T) {
	yes := []string{"cmb-0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"}
	no := []string{"", "short", "0123", "wb-task-123", "0123456789ABCDEF0123456789ABCDEF",
		"cmb-0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdef0"}
	for _, s := range yes {
		if !growthServerIDRe.MatchString(s) {
			t.Errorf("%q should match server id shape", s)
		}
	}
	for _, s := range no {
		if growthServerIDRe.MatchString(s) {
			t.Errorf("%q should NOT match server id shape", s)
		}
	}
}

func TestExpertSummonSequenceShape(t *testing.T) {
	e := growthMarketExpert{
		ExpertID: "ex_abc", ExpertType: "team", DisplayNameZH: "专家团A",
		ProfessionZH: "编程助手", Version: "", Categories: []any{"dev"},
	}
	events := growthDesktopExpertSummonSequence(e)
	if len(events) != 3 {
		t.Fatalf("summon sequence = %d events, want 3", len(events))
	}
	wantCodes := []string{"web_element_click", "expert_summon_click", "expert_summoned"}
	for i, ev := range events {
		if ev["eventCode"] != wantCodes[i] {
			t.Errorf("event %d code = %v, want %s", i, ev["eventCode"], wantCodes[i])
		}
	}
	if events[1]["expertType"] != "team" {
		t.Errorf("summon_click expertType = %v, want team", events[1]["expertType"])
	}
	if events[1]["version"] != "1.0.0" {
		t.Errorf("empty version should default to 1.0.0, got %v", events[1]["version"])
	}
	if events[0]["elementId"] != "expert_summon_click" {
		t.Errorf("click elementId = %v", events[0]["elementId"])
	}
}

func TestExpertActualUseShapes(t *testing.T) {
	e := growthMarketExpert{ExpertID: "ex_1", ExpertType: "agent", DisplayNameZH: "X", ProfessionZH: "Y", Version: "2.0"}
	conv, req := "conv-1", "0123456789abcdef0123456789abcdef"
	craft := growthDesktopExpertActualUseEvent(e, conv, req)
	if craft["mode"] != "craft" || craft["conversationId"] != conv || craft["requestId"] != req {
		t.Errorf("craft use shape wrong: %v", craft)
	}
	// last 8 hex of req "…89ab cdef" → "89abcdef"
	if craft["messageId"] != "msg-89abcdef" {
		t.Errorf("messageId = %v, want msg-89abcdef", craft["messageId"])
	}
	if craft["requestModelId"] != "fast-model" {
		t.Errorf("requestModelId = %v", craft["requestModelId"])
	}
	local := growthDesktopExpertActualUseLocal(e, conv, req)
	if local["mode"] != "LOCAL" {
		t.Errorf("local mode = %v, want LOCAL", local["mode"])
	}
}

func TestSkillInfoEventShape(t *testing.T) {
	ev := growthDesktopSkillInfoEvent("skill_1", "技能A", "conv-2", "0123456789abcdef0123456789abcdef", "msg-x")
	if ev["eventCode"] != "skill_info" || ev["skillId"] != "skill_1" || ev["toolStatus"] != "success" {
		t.Errorf("skill_info shape wrong: %v", ev)
	}
	if ev["source"] != "workbuddy-desktop" {
		t.Errorf("source = %v, want workbuddy-desktop", ev["source"])
	}
	if ev["requestId"] != "0123456789abcdef0123456789abcdef" || ev["conversationId"] != "conv-2" {
		t.Errorf("JOIN ids wrong: %v", ev)
	}
}

// chatTaskSrv serves the chat SSE endpoint plus task list / market / report
// endpoints from one mux; chat and report bodies are captured separately.
type chatTaskSrv struct {
	mu           sync.Mutex
	srv          *httptest.Server
	chats        int
	reports      int
	chatBodies   []string
	reportBodies []string
	expertIDs    []string // X-Expert-Id header per chat hit
	sseBody      string
}

func newChatTaskSrv(t *testing.T, taskJSON string) *chatTaskSrv {
	a := &chatTaskSrv{
		sseBody: "data: {\"id\":\"0123456789abcdef0123456789abcdef\",\"choices\":[{\"delta\":{\"content\":\"2\"}}]}\n\n" +
			"data: [DONE]\n\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		a.mu.Lock()
		a.chats++
		a.expertIDs = append(a.expertIDs, r.Header.Get("X-Expert-Id"))
		a.chatBodies = append(a.chatBodies, string(b))
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(a.sseBody))
	})
	mux.HandleFunc("/portal/operation-platform/market/expert/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[
                        {"expert_id":"ex_1","expert_type":"agent","display_name_zh":"甲","profession_zh":"P1","version":"1.1"},
                        {"expert_id":"ex_2","expert_type":"agent","display_name_zh":"乙","profession_zh":"P2","version":"1.2"},
                        {"expert_id":"ex_3","expert_type":"agent","display_name_zh":"丙","profession_zh":"P3","version":"1.3"}]}}`))
	})
	mux.HandleFunc("/v2/activity/growth/tasks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(taskJSON))
	})
	mux.HandleFunc("/v2/report", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		a.mu.Lock()
		a.reports++
		a.reportBodies = append(a.reportBodies, string(b))
		a.mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *chatTaskSrv) chatHits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chats
}

func TestGrowthDesktopChatIDParsesServerID(t *testing.T) {
	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[]}}`)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()

	conv, req, err := growthDesktopChatID(&storedAuth{Auth: storedTokens{AccessToken: "tok"}}, "ex_9", "1+1等于几？直接回答。")
	if err != nil {
		t.Fatalf("growthDesktopChatID: %v", err)
	}
	if req != "0123456789abcdef0123456789abcdef" {
		t.Errorf("server id = %q", req)
	}
	if !strings.HasPrefix(conv, "wb-task-") {
		t.Errorf("conversation id = %q, want wb-task-*", conv)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.expertIDs) != 1 || s.expertIDs[0] != "ex_9" {
		t.Errorf("X-Expert-Id headers = %v, want [ex_9]", s.expertIDs)
	}
	if !strings.Contains(s.chatBodies[0], `"model":"fast-model"`) {
		t.Errorf("chat body model wrong: %s", s.chatBodies[0])
	}
	if !strings.Contains(s.chatBodies[0], `"role":"system"`) {
		t.Errorf("desktop chat should carry the system message: %s", s.chatBodies[0])
	}
}

func TestGrowthDesktopChatIDSkipsNonMatchingID(t *testing.T) {
	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[]}}`)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()
	// First id on the wire is a non-matching conversation-style id; the
	// watermark scanner must keep looking and find the real one.
	s.mu.Lock()
	s.sseBody = "data: {\"id\":\"wb-conv-not-server\",\"x\":1}\n\ndata: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\"}\n\n"
	s.mu.Unlock()

	_, req, err := growthDesktopChatID(&storedAuth{Auth: storedTokens{AccessToken: "tok"}}, "", "1+1等于几？直接回答。")
	if err != nil {
		t.Fatalf("growthDesktopChatID: %v", err)
	}
	if req != "cmb-0123456789abcdef0123456789abcdef" {
		t.Errorf("server id = %q, want the cmb- one", req)
	}
}

func TestGrowthDesktopChatIDTruncatedIDFails(t *testing.T) {
	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[]}}`)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()
	// The id value never completes — the scanner must hit EOF and fail
	// cleanly instead of hanging or returning a bogus id.
	s.mu.Lock()
	s.sseBody = "data: {\"partial\":\"x\", \"id\":\"0123456789ab"
	s.mu.Unlock()

	_, _, err := growthDesktopChatID(&storedAuth{Auth: storedTokens{AccessToken: "tok"}}, "", "1+1等于几？直接回答。")
	if err == nil {
		t.Fatal("truncated id should end with an error")
	}
}

func TestGrowthReportActivityModelAlignment(t *testing.T) {
	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[]}}`)
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()

	if err := growthReportActivityModel(&storedAuth{Account: storedAccount{UID: "u1"}}, "wb-glm52-x", "", "glm-5.2", "GLM-5.2"); err != nil {
		t.Fatalf("report: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reports != 1 {
		t.Fatalf("reports = %d, want 1", s.reports)
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(s.reportBodies[0]), &events); err != nil {
		t.Fatalf("report body parse: %v (%s)", err, s.reportBodies[0])
	}
	ev := events[0]
	if ev["requestModelId"] != "glm-5.2" || ev["requestModelName"] != "GLM-5.2" {
		t.Errorf("model alignment wrong: %v/%v", ev["requestModelId"], ev["requestModelName"])
	}
	if ev["requestId"] != "wb-glm52-x" {
		t.Errorf("empty requestID should fall back to conversationID, got %v", ev["requestId"])
	}
}

func TestChatTaskTableComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range growthAutoActions {
		seen[a.Code] = true
	}
	for _, code := range []string{"Model_chat_GLM5.2", "skill_1", "expert_5", "Expert_team_use_3", "Expert_lighthouse", "black_cat"} {
		if !seen[code] {
			t.Errorf("chat action %s missing from growthAutoActions", code)
		}
		if growthMPTaskCodes[code] {
			t.Errorf("%s must not be an mp task", code)
		}
	}
	if len(growthAutoActions) != 19 {
		t.Errorf("auto-light table = %d actions, want 19 (13 fingerprint + 6 chat)", len(growthAutoActions))
	}
}

func TestExpertBatchDeficitAware(t *testing.T) {
	oldGap := growthExpertSummonGap
	growthExpertSummonGap = 0
	defer func() { growthExpertSummonGap = oldGap }()

	// expert_5 at 3/5 → exactly 2 chat rounds regardless of 3 experts listed.
	taskJSON := `{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"accepted","current":3,"target":5}]}}`
	s := newChatTaskSrv(t, taskJSON)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()
	restoreG := setGrowthBase(s.srv.URL)
	defer restoreG()

	msg, err := growthExpertBatch(&storedAuth{Auth: storedTokens{AccessToken: "tok"}}, "expert_5", "agent")
	if err != nil {
		t.Fatalf("growthExpertBatch: %v", err)
	}
	if s.chatHits() != 2 {
		t.Errorf("chat hits = %d, want 2 (deficit only)", s.chatHits())
	}
	if !strings.Contains(msg, "2 位") {
		t.Errorf("summary = %q, want mention of 2 experts", msg)
	}
	// Each used expert gets a summon report and an actual_use report.
	usedUses := 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.reportBodies {
		if strings.Contains(b, "expert_actual_use") {
			usedUses++
		}
	}
	if usedUses != 2 {
		t.Errorf("expert_actual_use reports = %d, want 2", usedUses)
	}
}

func TestBlackCatWindowGateAndDeficit(t *testing.T) {
	oldGap, oldFn := growthNightChatGap, growthNightWindowFn
	growthNightChatGap = 0
	defer func() { growthNightChatGap, growthNightWindowFn = oldGap, oldFn }()

	sa := &storedAuth{Auth: storedTokens{AccessToken: "tok"}}
	taskJSON := `{"code":0,"data":{"tasks":[{"task_code":"black_cat","accept_status":"accepted","current":1,"target":3}]}}`
	s := newChatTaskSrv(t, taskJSON)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()
	restoreG := setGrowthBase(s.srv.URL)
	defer restoreG()
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()

	// Outside the window: no chat quota consumed, informative no-error line.
	growthNightWindowFn = func() bool { return false }
	msg, err := runAutoBlackCat(sa)
	if err != nil {
		t.Fatalf("outside window: %v", err)
	}
	if !strings.Contains(msg, "23:00") {
		t.Errorf("outside-window line = %q", msg)
	}
	if s.chatHits() != 0 {
		t.Errorf("outside window consumed %d chats", s.chatHits())
	}

	// Inside the window: deficit = 3-1 = 2 glm-5.2 chats + 2 aligned reports.
	growthNightWindowFn = func() bool { return true }
	if _, err = runAutoBlackCat(sa); err != nil {
		t.Fatalf("inside window: %v", err)
	}
	if s.chatHits() != 2 {
		t.Errorf("chat hits = %d, want 2 (deficit)", s.chatHits())
	}
	s.mu.Lock()
	lastChat := s.chatBodies[len(s.chatBodies)-1]
	reports := s.reports
	s.mu.Unlock()
	if !strings.Contains(lastChat, `"model":"glm-5.2"`) {
		t.Errorf("black_cat chat model wrong: %s", lastChat)
	}
	if reports < 2 {
		t.Errorf("reports = %d, want >= 2", reports)
	}
}

func TestRunAutoModelChatAlignedFlow(t *testing.T) {
	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[{"task_code":"Model_chat_GLM5.2","accept_status":"accepted","current":0,"target":1}]}}`)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()
	restoreB := setBillingBase(s.srv.URL)
	defer restoreB()

	msg, err := runAutoModelChat(&storedAuth{Auth: storedTokens{AccessToken: "tok"}})
	if err != nil {
		t.Fatalf("runAutoModelChat: %v", err)
	}
	if s.chatHits() != 1 {
		t.Errorf("chat hits = %d, want 1", s.chatHits())
	}
	s.mu.Lock()
	body := s.chatBodies[0]
	s.mu.Unlock()
	if !strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("Model_chat body model wrong: %s", body)
	}
	if strings.Contains(body, `"role":"system"`) {
		t.Errorf("plain chat should NOT carry system (wb2api runModelChat parity): %s", body)
	}
	if !strings.Contains(msg, "glm-5.2") {
		t.Errorf("summary = %q", msg)
	}
	// The follow-up report must carry the aligned model.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reportBodies) != 1 || !strings.Contains(s.reportBodies[0], `"requestModelId":"glm-5.2"`) {
		t.Errorf("aligned report missing: %v", s.reportBodies)
	}
}

func TestRunAutoLighthouseFlow(t *testing.T) {
	oldGap := growthExpertSummonGap
	growthExpertSummonGap = 0
	defer func() { growthExpertSummonGap = oldGap }()

	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[]}}`)
	restore := setGrowthChatBase(s.srv.URL)
	defer restore()
	restoreG := setGrowthBase(s.srv.URL)
	defer restoreG()

	if _, err := runAutoExpertLighthouse(&storedAuth{Auth: storedTokens{AccessToken: "tok"}}); err != nil {
		t.Fatalf("lighthouse: %v", err)
	}
	if s.chatHits() != 1 {
		t.Errorf("chat hits = %d, want 1", s.chatHits())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.expertIDs) != 1 || s.expertIDs[0] != growthLighthouseExpertID {
		t.Errorf("lighthouse chat X-Expert-Id = %v", s.expertIDs)
	}
	// Some report must carry the LOCAL actual_use with the lighthouse overrides.
	var foundLocal bool
	for _, b := range s.reportBodies {
		if strings.Contains(b, `"eventCode":"expert_actual_use"`) &&
			strings.Contains(b, `"mode":"LOCAL"`) && strings.Contains(b, `"cost":0`) {
			foundLocal = true
		}
	}
	if !foundLocal {
		t.Errorf("no LOCAL actual_use with cost=0 in reports: %d reports", len(s.reportBodies))
	}
}

func TestMarketExpertListParse(t *testing.T) {
	s := newChatTaskSrv(t, `{"code":0,"data":{"tasks":[]}}`)
	restore := setGrowthBase(s.srv.URL)
	defer restore()

	experts, err := growthMarketExpertList(&storedAuth{Auth: storedTokens{AccessToken: "tok"}}, "agent")
	if err != nil {
		t.Fatalf("market list: %v", err)
	}
	if len(experts) != 3 {
		t.Fatalf("experts = %d, want 3", len(experts))
	}
	if experts[0].ExpertID != "ex_1" || experts[0].DisplayNameZH != "甲" {
		t.Errorf("expert[0] = %+v", experts[0])
	}
}

func TestNightTickRegistrations(t *testing.T) {
	// The scheduler wakes for the earliest slot: from 20:00 that's 21:00.
	now := time.Date(2026, 9, 20, 20, 0, 0, 0, time.Local)
	next := nextCheckinTime(now)
	if h := next.Hour(); h != 21 {
		t.Errorf("nextCheckinTime from 20:00 = %02d:%02d, want 21:00 first", h, next.Minute())
	}
	// After the evening jobs the next wake must be the 23:00 night slot.
	now = time.Date(2026, 9, 20, 22, 30, 0, 0, time.Local)
	next = nextCheckinTime(now)
	if h, m := next.Hour(), next.Minute(); h != 23 || m != 0 {
		t.Errorf("nextCheckinTime from 22:30 = %02d:%02d, want 23:00", h, m)
	}
}
