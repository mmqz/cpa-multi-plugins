// task_events_test.go covers the fingerprint report layer: desktop event
// chain shape, fingerprint injection (with business-field precedence),
// header families per channel (desktop / web / mp), the mp chat event's
// activityId switch, and the mp claim fallback to the web domain.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDesktopChatSequenceShape(t *testing.T) {
	events := growthDesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	if len(events) != 6 {
		t.Fatalf("want 6 desktop chat events, got %d", len(events))
	}
	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	for i, code := range wantCodes {
		if events[i]["eventCode"] != code {
			t.Errorf("event[%d] = %v, want %s", i, events[i]["eventCode"], code)
		}
	}
	resp := events[3]
	if resp["isSuccessful"] != true || resp["conversationId"] != "conv-1" {
		t.Errorf("chat_message_response must be a successful reply JOINed to the chain: %v", resp)
	}
	if resp["rootRequestId"] != "req-1" {
		t.Errorf("chain must JOIN via rootRequestId=req-1: %v", resp["rootRequestId"])
	}
	if events[0]["has_expert"] != false {
		t.Errorf("plain chat chain must carry has_expert=false: %v", events[0]["has_expert"])
	}
}

func TestDesktopReportFingerprintAndHeaders(t *testing.T) {
	var gotUA, gotProduct, gotDomain, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotProduct = r.Header.Get("X-Product")
		gotDomain = r.Header.Get("X-Domain")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	sa.Account.UID = "uid-77"
	sa.Account.Nickname = "测试号"
	err := growthReportDesktopEvent(sa,
		growthDesktopEvent{"eventCode": "automated_task_create_suc", "extName": "override"})
	if err != nil {
		t.Fatalf("desktop report: %v", err)
	}
	if gotUA != growthDesktopUA {
		t.Errorf("UA = %q, want desktop UA", gotUA)
	}
	if gotProduct != "SaaS" || gotDomain != srv.URL {
		t.Errorf("X-Product=%q X-Domain=%q, want SaaS + chat base", gotProduct, gotDomain)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(gotBody), &arr); err != nil || len(arr) != 1 {
		t.Fatalf("body must be a 1-element array: %q (err=%v)", gotBody, err)
	}
	ev := arr[0]
	if ev["extName"] != "override" {
		t.Errorf("business fields must override fingerprint: extName=%v", ev["extName"])
	}
	if ev["ideName"] != "WorkBuddy" || ev["userId"] != "uid-77" || ev["userNickname"] != "测试号" {
		t.Errorf("fingerprint injection incomplete: ideName=%v userId=%v nickname=%v",
			ev["ideName"], ev["userId"], ev["userNickname"])
	}
	if ev["machineId"] == "" || len(ev["machineId"].(string)) != 36 {
		t.Errorf("machineId must be derived 36-hex, got %v", ev["machineId"])
	}
}

func TestDesktopDerivedIDStable(t *testing.T) {
	sa := &storedAuth{}
	sa.Account.UID = "uid-x"
	if growthDeriveID(sa, "machine") != growthDeriveID(sa, "machine") {
		t.Errorf("derive must be deterministic per (salt, uid)")
	}
	sa2 := &storedAuth{}
	sa2.Account.UID = "uid-y"
	if growthDeriveID(sa, "machine") == growthDeriveID(sa2, "machine") {
		t.Errorf("different uids must derive different ids")
	}
}

func TestBuddyAppSequenceShape(t *testing.T) {
	events := growthDesktopBuddyAppSequence("cb_x", "某应用")
	if len(events) != 5 {
		t.Fatalf("want 5 buddyapp events, got %d", len(events))
	}
	wantCodes := []string{
		"buddyapp_discover_click", "buddyapp_show", "buddyapp_enter_click",
		"buddyapp_auth_confirm_click", "buddyapp_bindaccount_skip_click",
	}
	for i, code := range wantCodes {
		if events[i]["eventCode"] != code {
			t.Errorf("event[%d] = %v, want %s", i, events[i]["eventCode"], code)
		}
		if events[i]["buddyId"] != "cb_x" || events[i]["mode"] != "LOCAL" {
			t.Errorf("event[%d] must carry buddyId+LOCAL mode: %v", i, events[i])
		}
	}
}

func TestWebReportShape(t *testing.T) {
	var gotPlatform, gotOrigin, gotReferer, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("x-client-platform")
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	old := growthWebBaseCN
	growthWebBaseCN = srv.URL
	defer func() { growthWebBaseCN = old }()

	sa := &storedAuth{}
	sa.Account.UID = "uid-w"
	page := srv.URL + "/space/d/abc"
	err := growthReportWebEvent(sa, "web_element_click", page, "library_doc_intro_click", "资料库")
	if err != nil {
		t.Fatalf("web report: %v", err)
	}
	if gotPlatform != "web" || gotOrigin != srv.URL || gotReferer != page {
		t.Errorf("web headers wrong: platform=%q origin=%q referer=%q", gotPlatform, gotOrigin, gotReferer)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(gotBody), &arr); err != nil || len(arr) != 1 {
		t.Fatalf("body must be a 1-element array: %q", gotBody)
	}
	ev := arr[0]
	if ev["elementId"] != "library_doc_intro_click" || ev["userId"] != "uid-w" || ev["pageURL"] != page {
		t.Errorf("web event payload wrong: %v", ev)
	}
	if ua, _ := ev["userAgent"].(string); !strings.Contains(ua, "Mozilla") {
		t.Errorf("web events are browser-shaped, userAgent=%v", ev["userAgent"])
	}
}

func TestMPChatEventActivityIdSwitch(t *testing.T) {
	withID := growthMPChatEvent("conv-a", true)
	noID := growthMPChatEvent("conv-b", false)
	if withID["activityId"] != growthMPOpenDayID {
		t.Errorf("school_season event must carry activityId=%s, got %v", growthMPOpenDayID, withID["activityId"])
	}
	if _, has := noID["activityId"]; has {
		t.Errorf("Sequential_Tasks_1 event must NOT carry activityId (server keys on the mp fingerprint)")
	}
	if noID["agentName"] != "mp" {
		t.Errorf("mp chat events carry the mp agent fingerprint, got %v", noID["agentName"])
	}
}

func TestMPReportHeaders(t *testing.T) {
	var gotPlatform, gotClientProduct, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("X-Client-Platform")
		gotClientProduct = r.Header.Get("X-Client-Product")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	sa.Account.UID = "uid-mp"
	if err := growthReportMPEvent(sa, growthMPChatEvent("conv-mp", true)); err != nil {
		t.Fatalf("mp report: %v", err)
	}
	if gotPlatform != "mp-weixin" || gotClientProduct != "workbuddy-mp" {
		t.Errorf("mp header family wrong: platform=%q product=%q", gotPlatform, gotClientProduct)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(gotBody), &arr); err != nil || len(arr) != 1 {
		t.Fatalf("mp body must be a 1-element array: %q", gotBody)
	}
	if arr[0]["extName"] != "workbuddy-mp" || arr[0]["platform"] != "mini_program" {
		t.Errorf("mp fingerprint not injected: %v", arr[0])
	}
}

func TestGrowthCallMPHeader(t *testing.T) {
	var gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("X-Client-Platform")
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"school_season","current":0,"target":1}]}}`))
	}))
	defer srv.Close()
	restore := setGrowthBase(srv.URL)
	defer restore()

	tk, err := growthCallMPTask(&storedAuth{}, "school_season")
	if err != nil {
		t.Fatalf("mp task lookup: %v", err)
	}
	if gotPlatform != "miniprogram" {
		t.Errorf("growth-domain mp calls must carry X-Client-Platform: miniprogram, got %q", gotPlatform)
	}
	if tk == nil || tk.TaskCode != "school_season" {
		t.Errorf("mp list must surface mp-only tasks: %+v", tk)
	}
}

func TestMPClaimFallbackToWeb(t *testing.T) {
	mpHits, webHits := 0, 0
	mpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mpHits++
		// Chat-domain claim 400s for this tenant shape (wb2api 实测)…
		if strings.HasSuffix(r.URL.Path, "/claim") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"task not completed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer mpSrv.Close()
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webHits++
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100,"energy":5}}`))
	}))
	defer webSrv.Close()
	restore := setGrowthBase(mpSrv.URL)
	defer restore()
	old := growthWebBaseCN
	growthWebBaseCN = webSrv.URL
	defer func() { growthWebBaseCN = old }()

	credit, energy, err := growthClaimRewardMP(&storedAuth{}, "school_season")
	if err != nil {
		t.Fatalf("mp claim with web fallback: %v", err)
	}
	if mpHits != 1 || webHits != 1 {
		t.Errorf("expected 1 mp attempt + 1 web fallback, got mp=%d web=%d", mpHits, webHits)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("fallback claim must parse rewards: credit=%d energy=%d", credit, energy)
	}
}

func TestPollGapInjectable(t *testing.T) {
	if growthClaimPollGap != 3*time.Second {
		t.Errorf("default poll gap should be 3s, got %v", growthClaimPollGap)
	}
}
