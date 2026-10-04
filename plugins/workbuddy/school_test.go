// school_test.go covers the school-season activity layer: endpoint wire
// shapes (paths, methods, body, headers against an httptest server), envelope
// decoding, and the pure task-state predicates used by the daily loop.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func schoolTestAuth() *storedAuth {
	sa := &storedAuth{}
	sa.Auth.AccessToken = "tok-school"
	sa.Account.UID = "u-school"
	return sa
}

// TestSchoolEndpointsWireShape walks every school endpoint against one
// httptest server that records method+path+body+headers and answers with a
// canned envelope per path.
func TestSchoolEndpointsWireShape(t *testing.T) {
	type hit struct {
		method, path, body string
		auth, uid          string
	}
	var got []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, hit{
			method: r.Method, path: r.URL.Path, body: string(b),
			auth: r.Header.Get("Authorization"), uid: r.Header.Get("X-User-Id"),
		})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"share_invite","status":"completed","progress":1,"target_count":1}],"in_period":true}}`))
		case strings.HasSuffix(r.URL.Path, "/share-complete"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/viewed"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"chance_granted":1}}`))
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"chance":{"balance":2}}}`))
		case strings.HasSuffix(r.URL.Path, "/draw"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"prize_code":"credit_50","credit_amount":50}}`))
		case strings.HasSuffix(r.URL.Path, "/vouchers"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"items":[{"grant_id":7,"sku_code":"kfc_ice_cream","prize_name":"肯德基冰淇淋","code":"ABC-123","valid_to":"2026-10-24"}]}}`))
		default:
			_, _ = w.Write([]byte(`{"code":1,"msg":"unknown path"}`))
		}
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := schoolTestAuth()

	tasks, inPeriod, err := schoolTasksList(sa)
	if err != nil || !inPeriod || len(tasks) != 1 || tasks[0].TaskCode != "share_invite" {
		t.Fatalf("tasks: inPeriod=%v tasks=%+v err=%v", inPeriod, tasks, err)
	}
	if err := schoolShareComplete(sa); err != nil {
		t.Fatalf("share-complete: %v", err)
	}
	if _, err := schoolChances(sa); err != nil {
		t.Fatalf("config: %v", err)
	}
	n, err := schoolClaimTask(sa, "share_invite")
	if err != nil || n != 1 {
		t.Fatalf("claim: n=%d err=%v", n, err)
	}
	prize, err := schoolDraw(sa)
	if err != nil || prize != "credit_50 +50c" {
		t.Fatalf("draw: prize=%q err=%v", prize, err)
	}
	vs, err := schoolVouchers(sa)
	if err != nil || len(vs) != 1 || vs[0].Code != "ABC-123" || vs[0].PrizeName != "肯德基冰淇淋" {
		t.Fatalf("vouchers: %+v err=%v", vs, err)
	}
	if err := schoolTaskViewed(sa, "share_invite"); err != nil {
		t.Fatalf("viewed: %v", err)
	}

	// Wire assertions: paths under the school prefix on the billing domain,
	// correct methods, bodies, and the auth header family.
	want := []struct {
		method, path, bodyPart string
	}{
		{"GET", "/portal/activity/school/tasks", ""},
		{"POST", "/portal/activity/school/tasks/share-complete", `"channel":"wechat"`},
		{"GET", "/portal/activity/school/config", ""},
		{"POST", "/portal/activity/school/tasks/share_invite/claim", ""},
		{"POST", "/portal/activity/school/wheel/draw", `"draw_uuid"`},
		{"GET", "/portal/activity/school/vouchers", ""},
		{"POST", "/portal/activity/school/tasks/share_invite/viewed", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.method != w.method || g.path != w.path {
			t.Errorf("call %d: %s %s, want %s %s", i, g.method, g.path, w.method, w.path)
		}
		if w.bodyPart != "" && !strings.Contains(g.body, w.bodyPart) {
			t.Errorf("call %d: body %q missing %q", i, g.body, w.bodyPart)
		}
		if g.auth != "Bearer tok-school" {
			t.Errorf("call %d: Authorization = %q", i, g.auth)
		}
		if g.uid != "u-school" {
			t.Errorf("call %d: X-User-Id = %q", i, g.uid)
		}
	}
	// draw body must carry a uuid-shaped draw_uuid.
	for _, g := range got {
		if g.path == "/portal/activity/school/wheel/draw" {
			idx := strings.Index(g.body, `"draw_uuid":"`)
			rest := g.body[idx+len(`"draw_uuid":"`):]
			end := strings.Index(rest, `"`)
			tok := rest[:end]
			if len(tok) != 36 || strings.Count(tok, "-") != 4 {
				t.Errorf("draw_uuid not uuid-shaped: %q (body %s)", tok, g.body)
			}
		}
	}
}

// TestSchoolBusinessErrorSurfaces verifies code!=0 envelopes surface as
// errors (the daily loop logs and continues past them).
func TestSchoolBusinessErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今日已分享"}`))
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	if err := schoolShareComplete(schoolTestAuth()); err == nil {
		t.Fatal("expected business error, got nil")
	} else if !strings.Contains(err.Error(), "10001") {
		t.Errorf("error should carry code: %v", err)
	}
}

// TestSchoolTaskPredicates covers the loop decision helpers.
func TestSchoolTaskPredicates(t *testing.T) {
	cases := []struct {
		name      string
		task      schoolTask
		needView  bool
		claimable bool
	}{
		{"pending activation", schoolTask{TaskCode: "chat", Status: "pending", Progress: 0, TargetCount: 1}, true, false},
		{"completed by status", schoolTask{TaskCode: "chat", Status: "completed"}, false, true},
		{"progress crossed", schoolTask{TaskCode: "chat", Status: "in_progress", Progress: 3, TargetCount: 3}, false, true},
		{"claimed", schoolTask{TaskCode: "chat", Status: "claimed", Progress: 3, TargetCount: 3}, false, false},
		{"mid progress", schoolTask{TaskCode: "chat", Status: "in_progress", Progress: 1, TargetCount: 3}, false, false},
		{"blank code", schoolTask{Status: "completed"}, false, false},
		{"zero target", schoolTask{TaskCode: "x", Status: "completed", TargetCount: 0}, false, true},
	}
	for _, tc := range cases {
		if got := schoolTaskNeedsView(tc.task); got != tc.needView {
			t.Errorf("%s: needView = %v, want %v", tc.name, got, tc.needView)
		}
		if got := schoolTaskClaimable(tc.task); got != tc.claimable {
			t.Errorf("%s: claimable = %v, want %v", tc.name, got, tc.claimable)
		}
	}
}
