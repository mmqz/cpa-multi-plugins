// plane_test.go covers the entitlement-plane routing added 2026-09-27:
// capability harvest from billing/balance rows, model→plane routing in
// routeFor, the reactive 1113→JWT-plane fallback on all three send paths,
// and the enriched 1113 error copy. Wire shapes are the live-measured ones
// (weekend bucket granting model:glm-5.3-flash; coding-plane 429+1113).
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// resetStartPlane isolates the global per-UID plane cache per test.
func resetStartPlane(t *testing.T) {
	t.Helper()
	swap := func() {
		startPlaneMu.Lock()
		startPlaneModels = map[string]*startPlaneEntry{}
		startPlaneMu.Unlock()
	}
	swap()
	t.Cleanup(swap)
}

func TestHarvestStartPlaneCapabilities(t *testing.T) {
	resetStartPlane(t)
	rows := []zcodeBalance{
		{ShowName: "GLM-5.3-Flash", Capabilities: []string{"model:glm-5.3-flash"}},
		{ShowName: "Coding Plan", Capabilities: []string{"model:glm-5.3", "meter:token"}},
	}
	harvestStartPlaneCapabilities("user-1", rows)
	set := startPlaneModelSet("user-1")
	if set == nil {
		t.Fatal("no set harvested")
	}
	if _, ok := set["glm-5.3-flash"]; !ok {
		t.Fatalf("glm-5.3-flash missing: %v", set)
	}
	if _, ok := set["glm-5.3"]; !ok {
		t.Fatalf("glm-5.3 missing: %v", set)
	}
	if _, ok := set["token"]; ok {
		t.Fatalf("non-model capability leaked: %v", set)
	}
	if startPlaneModelSet("user-2") != nil {
		t.Fatal("harvest leaked across UIDs")
	}
	// Empty UID must be a no-op, not a panic.
	harvestStartPlaneCapabilities("", rows)
}

func TestNoteStartPlaneModelMemoizes(t *testing.T) {
	resetStartPlane(t)
	noteStartPlaneModel("user-1", "glm-5.3-flash")
	noteStartPlaneModel("user-1", "glm-5.3")
	set := startPlaneModelSet("user-1")
	if len(set) != 2 {
		t.Fatalf("set = %v", set)
	}
	noteStartPlaneModel("", "glm-5.3-flash") // no-op
	noteStartPlaneModel("user-1", "")        // no-op
}

func TestModelRidesStartPlane(t *testing.T) {
	resetStartPlane(t)
	noteStartPlaneModel("user-1", "glm-5.3-flash")
	coding := &storedAuth{
		Auth:    zcodeTokens{AccessToken: "id.secret", JWT: "jwt-1", Provider: providerZai, Plan: planCoding},
		Account: zcodeAccount{UID: "user-1"},
	}
	if !modelRidesStartPlane(coding, "glm-5.3-flash") {
		t.Fatal("known JWT-plane model not routed")
	}
	if modelRidesStartPlane(coding, "glm-5.3") {
		t.Fatal("unknown model routed to JWT plane")
	}
	noJWT := *coding
	noJWT.Auth.JWT = ""
	if modelRidesStartPlane(&noJWT, "glm-5.3-flash") {
		t.Fatal("no-JWT credential must stay on the coding plane")
	}
	start := &storedAuth{
		Auth:    zcodeTokens{AccessToken: "id.secret", JWT: "jwt-1", Provider: providerZai, Plan: planStart},
		Account: zcodeAccount{UID: "user-1"},
	}
	if modelRidesStartPlane(start, "glm-5.3-flash") {
		t.Fatal("start-plan credentials route wholesale, not via the model override")
	}
}

func TestRouteForModelRouting(t *testing.T) {
	resetStartPlane(t)
	coding := &storedAuth{
		Auth:    zcodeTokens{AccessToken: "id.secret", JWT: "jwt-1", Provider: providerZai, Plan: planCoding},
		Account: zcodeAccount{UID: "user-1"},
	}
	if route := routeFor(coding, "glm-5.3"); route.anthropic {
		t.Fatal("coding-plane model must ride the coding route")
	}
	noteStartPlaneModel("user-1", "glm-5.3-flash")
	if route := routeFor(coding, "glm-5.3-flash"); !route.anthropic {
		t.Fatalf("JWT-plane model must ride the anthropic route: %+v", route)
	}
	start := &storedAuth{
		Auth:    zcodeTokens{AccessToken: "id.secret", JWT: "jwt-1", Provider: providerZai, Plan: planStart},
		Account: zcodeAccount{UID: "user-1"},
	}
	if route := routeFor(start, "glm-5.3"); !route.anthropic {
		t.Fatal("start-plan credential must ride the anthropic route")
	}
}

func TestIsNoResourcePackageError(t *testing.T) {
	body1113 := `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`
	if !isNoResourcePackageError(http.StatusTooManyRequests, body1113) {
		t.Fatal("429+1113 must read as no-resource-package")
	}
	if !isNoResourcePackageError(http.StatusPaymentRequired, `{"error":{"message":"Insufficient balance"}}`) {
		t.Fatal("402+balance marker must read as no-resource-package")
	}
	if isNoResourcePackageError(http.StatusOK, body1113) {
		t.Fatal("200 must never read as no-resource-package")
	}
	if isNoResourcePackageError(http.StatusBadRequest, `{"error":{"message":"Unsupported model glm-9.9"}}`) {
		t.Fatal("plain model-support 400 is not a package error")
	}
}

func TestStartPlaneFallbackBodyDecides(t *testing.T) {
	coding := &storedAuth{
		Auth:    zcodeTokens{AccessToken: "id.secret", JWT: "jwt-1", Provider: providerZai, Plan: planCoding, DeviceMid: "mid-1"},
		Account: zcodeAccount{UID: "user-1"},
	}
	payload := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	body1113 := `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`
	codingRoute := chatRoute{endpoint: openAIBaseZai + "/chat/completions", applyHeaders: applyChatHeaders}

	route, body, ok := startPlaneFallbackBody(coding, codingRoute, http.StatusTooManyRequests, body1113, payload, "glm-5.3-flash", false)
	if !ok {
		t.Fatal("429+1113 on the coding plane must trigger the fallback")
	}
	if !route.anthropic || route.endpoint != startPlanAnthropicEndpoint {
		t.Fatalf("fallback route = %+v", route)
	}
	var sent map[string]any
	if json.Unmarshal([]byte(body), &sent) != nil {
		t.Fatalf("fallback body not JSON: %.120s", body)
	}
	if sent["model"] != "glm-5.3-flash" {
		t.Fatalf("fallback model = %v", sent["model"])
	}

	// Anthropic / off-peak routes never fall back.
	if _, _, ok := startPlaneFallbackBody(coding, startPlaneFallbackRoute(), http.StatusTooManyRequests, body1113, payload, "glm-5.3-flash", false); ok {
		t.Fatal("anthropic route must not re-fall-back")
	}
	ticketed := codingRoute
	ticketed.offPeakTicketID = "t1"
	if _, _, ok := startPlaneFallbackBody(coding, ticketed, http.StatusTooManyRequests, body1113, payload, "glm-5.3-flash", false); ok {
		t.Fatal("off-peak route must not fall back")
	}
	// Missing JWT / non-package failures / empty payload stay put.
	noJWT := *coding
	noJWT.Auth.JWT = ""
	if _, _, ok := startPlaneFallbackBody(&noJWT, codingRoute, http.StatusTooManyRequests, body1113, payload, "glm-5.3-flash", false); ok {
		t.Fatal("no-JWT credential must not fall back")
	}
	if _, _, ok := startPlaneFallbackBody(coding, codingRoute, http.StatusBadRequest, `{"error":{"message":"bad request shape"}}`, payload, "glm-5.3-flash", false); ok {
		t.Fatal("non-package failure must not fall back")
	}
	if _, _, ok := startPlaneFallbackBody(coding, codingRoute, http.StatusTooManyRequests, body1113, nil, "glm-5.3-flash", false); ok {
		t.Fatal("empty payload must not fall back")
	}
}

func codingPlanAuthJSON() []byte {
	raw, _ := json.Marshal(storedAuth{
		Auth: zcodeTokens{
			AccessToken: "68416a3903f34613a9912898df5b094e.HI7ZkG1MUUMbln9B",
			JWT:         "test-jwt",
			Provider:    providerZai,
			Plan:        planCoding,
			DeviceMid:   "device-mid-1",
		},
		Account: zcodeAccount{UID: "user-plane"},
	})
	return raw
}

// TestHandleExecExecute1113FallsBackToJWTPlane is the end-to-end regression
// for the weekend glm-5.3-flash report: the coding plane answers 429+1113
// (the resolved key owns no package), the plugin retries once on the
// zcode-plan anthropic gateway with the JWT, and the client sees a normal
// completion. Success memoizes the model.
func TestHandleExecExecute1113FallsBackToJWTPlane(t *testing.T) {
	resetStartPlane(t)
	codingGW := newStartPlanGateway(t, http.StatusTooManyRequests, nil,
		`{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`)
	jwtGW := newStartPlanGateway(t, http.StatusOK, nil, sampleAnthropicMessage)

	origCoding, origStart := openAIBaseZai, startPlanAnthropicEndpoint
	openAIBaseZai = codingGW.srv.URL
	startPlanAnthropicEndpoint = jwtGW.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { openAIBaseZai, startPlanAnthropicEndpoint = origCoding, origStart }()

	req := executorRequestJSON(t, map[string]any{
		"AuthID":       "a1",
		"AuthProvider": providerName,
		"Model":        "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, codingPlanAuthJSON())
	raw, err := handleExecExecute(req)
	if err != nil {
		t.Fatalf("handleExecExecute: %v", err)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("envelope = %s", raw)
	}
	// ExecutorResponse.Payload is []byte on the wire: a base64 string.
	var resp pluginapiExecutorResponseProbe
	if json.Unmarshal(env.Result, &resp) != nil || len(resp.Payload) == 0 {
		t.Fatalf("result = %s", env.Result)
	}
	payloadBytes, decErr := base64.StdEncoding.DecodeString(strings.Trim(string(resp.Payload), `"`))
	if decErr != nil {
		t.Fatalf("payload not base64: %v (%s)", decErr, resp.Payload)
	}
	if !strings.Contains(string(payloadBytes), "Hello from start-plan") {
		t.Fatalf("payload missing translated content: %s", payloadBytes)
	}

	// Coding plane: exactly one call with the resolved plan key.
	coding := codingGW.calls()
	if len(coding) != 1 || coding[0].path != "/chat/completions" {
		t.Fatalf("coding calls = %+v", coding)
	}
	if auth := coding[0].headers.Get("Authorization"); auth != "Bearer 68416a3903f34613a9912898df5b094e.HI7ZkG1MUUMbln9B" {
		t.Fatalf("coding auth = %q", auth)
	}
	// JWT plane: exactly one call with the plan JWT and the anthropic body.
	jwt := jwtGW.calls()
	if len(jwt) != 1 || jwt[0].path != "/api/v1/zcode-plan/anthropic/v1/messages" {
		t.Fatalf("jwt calls = %+v", jwt)
	}
	if auth := jwt[0].headers.Get("Authorization"); auth != "Bearer test-jwt" {
		t.Fatalf("jwt auth = %q", auth)
	}
	var sent map[string]any
	if json.Unmarshal([]byte(jwt[0].body), &sent) != nil || sent["model"] != "glm-5.3-flash" {
		t.Fatalf("jwt body = %.200s", jwt[0].body)
	}
	// The success memoizes the model so later calls pre-route.
	if set := startPlaneModelSet("user-plane"); set == nil {
		t.Fatal("model not memoized")
	} else if _, ok := set["glm-5.3-flash"]; !ok {
		t.Fatalf("memoized set = %v", set)
	}
}

// TestHandleExecExecuteMemoizedModelPreRoutes: after the memoize, the coding
// gateway must not see the request at all.
func TestHandleExecExecuteMemoizedModelPreRoutes(t *testing.T) {
	resetStartPlane(t)
	noteStartPlaneModel("user-plane", "glm-5.3-flash")
	codingGW := newStartPlanGateway(t, http.StatusOK, nil, `{"choices":[]}`)
	jwtGW := newStartPlanGateway(t, http.StatusOK, nil, sampleAnthropicMessage)

	origCoding, origStart := openAIBaseZai, startPlanAnthropicEndpoint
	openAIBaseZai = codingGW.srv.URL
	startPlanAnthropicEndpoint = jwtGW.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { openAIBaseZai, startPlanAnthropicEndpoint = origCoding, origStart }()

	req := executorRequestJSON(t, map[string]any{
		"AuthID":       "a1",
		"AuthProvider": providerName,
		"Model":        "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, codingPlanAuthJSON())
	if _, err := handleExecExecute(req); err != nil {
		t.Fatalf("handleExecExecute: %v", err)
	}
	if calls := codingGW.calls(); len(calls) != 0 {
		t.Fatalf("coding plane saw %d calls after memoize", len(calls))
	}
	if calls := jwtGW.calls(); len(calls) != 1 {
		t.Fatalf("jwt calls = %d", len(calls))
	}
}

// TestHandleExecExecuteCodingPlaneHealthy: a served model must not touch the
// JWT plane (no behavior change for real coding-plan accounts).
func TestHandleExecExecuteCodingPlaneHealthy(t *testing.T) {
	resetStartPlane(t)
	codingGW := newStartPlanGateway(t, http.StatusOK, nil,
		`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	jwtGW := newStartPlanGateway(t, http.StatusOK, nil, sampleAnthropicMessage)

	origCoding, origStart := openAIBaseZai, startPlanAnthropicEndpoint
	openAIBaseZai = codingGW.srv.URL
	startPlanAnthropicEndpoint = jwtGW.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { openAIBaseZai, startPlanAnthropicEndpoint = origCoding, origStart }()

	req := executorRequestJSON(t, map[string]any{
		"AuthID":       "a1",
		"AuthProvider": providerName,
		"Model":        "zcode/glm-5.3",
	}, map[string]any{
		"model":    "zcode/glm-5.3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, codingPlanAuthJSON())
	if _, err := handleExecExecute(req); err != nil {
		t.Fatalf("handleExecExecute: %v", err)
	}
	if calls := codingGW.calls(); len(calls) != 1 {
		t.Fatalf("coding calls = %d", len(calls))
	}
	if calls := jwtGW.calls(); len(calls) != 0 {
		t.Fatalf("jwt plane saw %d calls on a healthy coding response", len(calls))
	}
}

// TestHandleExecExecuteFallbackExhausted: when the JWT plane also rejects
// (risk-control 3007), the client gets the JWT-plane error and nothing is
// memoized.
func TestHandleExecExecuteFallbackExhausted(t *testing.T) {
	resetStartPlane(t)
	codingGW := newStartPlanGateway(t, http.StatusTooManyRequests, nil,
		`{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`)
	jwtGW := newStartPlanGateway(t, http.StatusBadRequest, nil, `{"code":3007,"msg":"captcha verify failed"}`)

	origCoding, origStart := openAIBaseZai, startPlanAnthropicEndpoint
	openAIBaseZai = codingGW.srv.URL
	startPlanAnthropicEndpoint = jwtGW.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { openAIBaseZai, startPlanAnthropicEndpoint = origCoding, origStart }()

	req := executorRequestJSON(t, map[string]any{
		"AuthID":       "a1",
		"AuthProvider": providerName,
		"Model":        "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, codingPlanAuthJSON())
	_, err := handleExecExecute(req)
	if err == nil {
		t.Fatal("expected the JWT-plane rejection to surface")
	}
	if !strings.Contains(err.Error(), "3007") && !strings.Contains(err.Error(), "captcha") {
		t.Fatalf("error should carry the JWT-plane rejection: %v", err)
	}
	if startPlaneModelSet("user-plane") != nil {
		t.Fatal("failed fallback must not memoize")
	}
}

// TestCollectUpstreamStreamFallsBack exercises the sync-collect path's
// fallback window (stream=true body rebuild).
func TestCollectUpstreamStreamFallsBack(t *testing.T) {
	resetStartPlane(t)
	codingGW := newStartPlanGateway(t, http.StatusTooManyRequests, nil,
		`{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`)
	jwtGW := newStartPlanGateway(t, http.StatusOK, nil, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	origCoding, origStart := openAIBaseZai, startPlanAnthropicEndpoint
	openAIBaseZai = codingGW.srv.URL
	startPlanAnthropicEndpoint = jwtGW.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { openAIBaseZai, startPlanAnthropicEndpoint = origCoding, origStart }()

	sa := &storedAuth{}
	if json.Unmarshal(codingPlanAuthJSON(), sa) != nil {
		t.Fatal("auth json")
	}
	body := `{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	route := routeFor(sa, "glm-5.3-flash")
	if route.anthropic {
		t.Fatal("cold cache must route coding first")
	}
	fb := func(status int, failBody string, _ http.Header) (chatRoute, string, bool) {
		return startPlaneFallbackBody(sa, route, status, failBody, []byte(body), "glm-5.3-flash", true)
	}
	_, statusCode, err := collectUpstreamStream(body, sa, route, false, nil, "glm-5.3-flash", fb)
	if err != nil {
		t.Fatalf("collect with fallback: %v", err)
	}
	if statusCode != 0 { // success return carries no status (collect contract)
		t.Fatalf("status = %d", statusCode)
	}
	if calls := codingGW.calls(); len(calls) != 1 {
		t.Fatalf("coding calls = %d", len(calls))
	}
	if calls := jwtGW.calls(); len(calls) != 1 {
		t.Fatalf("jwt calls = %d", len(calls))
	}
	if sent := jwtGW.calls()[0]; !strings.Contains(sent.body, `"stream":true`) {
		t.Fatalf("jwt body must keep stream=true: %.200s", sent.body)
	}
}

// TestRouteChatError1113Hint pins the enriched 1113 copy (coding route only).
func TestRouteChatError1113Hint(t *testing.T) {
	body1113 := `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`
	codingRoute := chatRoute{endpoint: openAIBaseZai + "/chat/completions", applyHeaders: applyChatHeaders}
	err := routeChatError(codingRoute, nil, http.StatusTooManyRequests, nil, body1113)
	if !strings.Contains(err.Error(), "1113") || !strings.Contains(err.Error(), "JWT") {
		t.Fatalf("1113 copy missing plane hint: %v", err)
	}
	plain := `{"error":{"code":"1302","message":"invalid request"}}`
	err = routeChatError(codingRoute, nil, http.StatusBadRequest, nil, plain)
	if strings.Contains(err.Error(), "JWT") {
		t.Fatalf("plain failures must keep the generic copy: %v", err)
	}
}
