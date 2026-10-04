// growth_test.go covers the growth-center pure logic: task list parsing
// (both progress shapes seen in the wild), accept/claim candidate
// selection, the travel state machine, and the activity-report event
// wire shape (userId is load-bearing — missing it means silent drops).
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseGrowthTasksProgressShapes(t *testing.T) {
	// Object progress overrides flat fields; claimable derived; claimed
	// from accept_status; locked excluded from claimable.
	raw := `{"tasks":[
		{"task_code":"chat_5","title":"活跃对话","accept_status":"accepted",
		 "progress":{"current":5,"target":5},"reward_credit":100,"current":0,"target":0},
		{"task_code":"first_buddy","accept_status":"not_accepted",
		 "current":1,"target":1,"reward_credit":300},
		{"task_code":"done_thing","accept_status":"claimed","current":9,"target":9},
		{"task_code":"locked_thing","locked":true,"current":5,"target":5},
		{"task_code":"null_progress","accept_status":"accepted","progress":null,"current":2,"target":3}
	]}`
	tasks := parseGrowthTasks(json.RawMessage(raw))
	if len(tasks) != 5 {
		t.Fatalf("want 5 tasks, got %d", len(tasks))
	}
	chat5 := tasks[0]
	if chat5.Current != 5 || chat5.Target != 5 {
		t.Errorf("object progress must override flat fields: got %d/%d", chat5.Current, chat5.Target)
	}
	if !chat5.Claimable || chat5.Claimed {
		t.Errorf("chat_5 should be claimable & unclaimed: %+v", chat5)
	}
	buddy := tasks[1]
	if !buddy.Claimable {
		t.Errorf("flat progress 1/1 should be claimable: %+v", buddy)
	}
	if tasks[2].Claimable || !tasks[2].Claimed {
		t.Errorf("claimed task must not be claimable: %+v", tasks[2])
	}
	if tasks[3].Claimable {
		t.Errorf("locked task must not be claimable: %+v", tasks[3])
	}
	if tasks[4].Claimable {
		t.Errorf("2/3 with null progress must not be claimable: %+v", tasks[4])
	}
}

func TestParseGrowthTasksGarbage(t *testing.T) {
	if tasks := parseGrowthTasks(json.RawMessage(`not json`)); tasks != nil {
		t.Errorf("garbage input should return nil, got %+v", tasks)
	}
	if tasks := parseGrowthTasks(json.RawMessage(`{"tasks":[]}`)); len(tasks) != 0 {
		t.Errorf("empty list should parse to 0 tasks, got %d", len(tasks))
	}
}

func TestGrowthAcceptCandidatesFilters(t *testing.T) {
	tasks := []growthTask{
		{TaskCode: "a_not_accepted", AcceptStatus: "not_accepted"},
		{TaskCode: "b_accepted", AcceptStatus: "accepted"},
		{TaskCode: "c_completed", AcceptStatus: "completed"},
		{TaskCode: "d_claimed", Claimed: true},
		{TaskCode: "e_locked", Locked: true},
		{TaskCode: "   ", AcceptStatus: "not_accepted"}, // blank code
		{TaskCode: "f_fresh"},
	}
	got := growthAcceptCandidates(tasks)
	want := []string{"a_not_accepted", "f_fresh"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestGrowthClaimableTasksOnly(t *testing.T) {
	tasks := []growthTask{
		{TaskCode: "yes", Claimable: true},
		{TaskCode: "no"},
		{TaskCode: "yes2", Claimable: true},
	}
	got := growthClaimableTasks(tasks)
	if len(got) != 2 || got[0].TaskCode != "yes" || got[1].TaskCode != "yes2" {
		t.Fatalf("want [yes yes2], got %+v", got)
	}
}

func TestGrowthTravelActionMatrix(t *testing.T) {
	cases := []struct {
		name   string
		st     *growthTravel
		action string
	}{
		{"nil status", nil, "skip"},
		{"arrived with record", &growthTravel{State: travelStateArrived, RecordID: 42}, "claim"},
		{"arrived no record", &growthTravel{State: travelStateArrived}, "skip"},
		{"idle ready", &growthTravel{State: travelStateIdle}, "depart"},
		{"idle limit", &growthTravel{State: travelStateIdle, DailyLimitReached: true}, "skip"},
		{"traveling", &growthTravel{State: travelStateTraveling, RecordID: 7}, "skip"},
		{"unknown state", &growthTravel{State: "weird"}, "skip"},
	}
	for _, tc := range cases {
		action, _ := growthTravelAction(tc.st)
		if action != tc.action {
			t.Errorf("%s: want %q got %q", tc.name, tc.action, action)
		}
	}
}

func TestGrowthReportEventWireShape(t *testing.T) {
	sa := &storedAuth{}
	sa.Auth.AccessToken = "tok"
	sa.Account.UID = "u-123"
	cid := "wb-conv-1"
	// Marshal the same event growthReportActivity builds (duplicated here to
	// keep the test free of network plumbing) and assert the load-bearing
	// fields survive the JSON round-trip.
	ev := growthChatEvent{
		EventCode:       "chat_request_send",
		ConversationID:  cid,
		RequestID:       cid,
		RootRequestID:   cid,
		UserID:          sa.Account.UID,
		MentionContexts: []any{},
		KnowledgeID:     []any{},
	}
	raw, err := json.Marshal([]growthChatEvent{ev})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("report body must be a 1-element array, got %d", len(arr))
	}
	m := arr[0]
	if m["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode = %v", m["eventCode"])
	}
	if m["userId"] != "u-123" {
		t.Errorf("userId is required upstream (missing = silent drop), got %v", m["userId"])
	}
	for _, k := range []string{"conversationId", "requestId", "rootRequestId"} {
		if m[k] != cid {
			t.Errorf("%s = %v, want %v", k, m[k], cid)
		}
	}
}

func TestGrowthClientTokenShape(t *testing.T) {
	tok := growthClientToken()
	if len(tok) != 36 {
		t.Errorf("uuid-shaped token length, got %d (%q)", len(tok), tok)
	}
	if strings.Count(tok, "-") != 4 {
		t.Errorf("uuid-shaped token dashes, got %q", tok)
	}
	if tok == growthClientToken() {
		t.Errorf("tokens must be random")
	}
}
