// executor.go builds and sends the chat traffic on both lanes.
//
// Wire parity with the official clients (docs/MIMO_AUTH.md §2/§5):
//   - sk lane: POST {base_url}/chat/completions, Authorization: Bearer {sk},
//     X-Mimo-Source: mimocode-cli. Model passes through untouched (the
//     desktop rewrites models only on its own proxy lane — S() 2003-2005).
//   - cookie lane: POST {region base}/route/chat/completions, NO
//     Authorization, X-Mimo-Source: mimocode-cli-free, X-Client-Version,
//     assembled ticket cookie (exchange.go buildCookieHeader order);
//     model alias mimo-auto resolves to mimo-pro (EE/k6 1884-1892).
//     Session faults (401, or 302 to serviceLogin, or a redirect-followed
//     login page) → re-mint the service ticket (serviceLogin→STS exchange)
//     → retry ONCE with the freshly routed endpoint — except when the 401
//     body is the upstream's model-allowlist complaint (the desktop's cq()
//     exemption, 1962-1974). /user/xiaomi/me is NOT a probe on this lane:
//     it 302s even for traffic the chat endpoint accepts (measured,
//     docs/MIMO_AUTH.md §6.2).
//
// Privacy (docs/MIMO_PRIVACY.md §7): identity/tracking fields (user,
// metadata, service_tier) and the logprob family are stripped from outbound
// bodies; nothing about message content is ever logged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// chatRoute is one lane's upstream contract: where to POST and which header
// set to apply.
type chatRoute struct {
	endpoint     string
	lane         string
	applyHeaders func(*http.Request, *storedAuth, string)
}

// routeFor resolves the chat route for one credential.
func routeFor(sa *storedAuth) chatRoute {
	if authLaneFor(sa) == laneCookie {
		return chatRoute{
			endpoint:     strings.TrimRight(regionBaseFor(sa), "/") + "/route/chat/completions",
			lane:         laneCookie,
			applyHeaders: applyCookieLaneHeaders,
		}
	}
	base := strings.TrimSpace(sa.Auth.BaseURL)
	if base == "" {
		base = "https://api.xiaomimimo.com/v1"
	}
	return chatRoute{
		endpoint:     strings.TrimRight(base, "/") + "/chat/completions",
		lane:         laneKey,
		applyHeaders: applyKeyLaneHeaders,
	}
}

// privacyStripFields are removed from outbound bodies: account attribution
// (user), provider metadata, billing tier, and the logprob family. The
// community proxy's list (proxy.py normalize_body) minus response_format —
// structured output is legitimate client functionality, and the desktop
// never strips it.
var privacyStripFields = []string{
	"user", "metadata", "service_tier", "logprobs", "top_logprobs", "logit_bias",
}

// buildChatBody prepares the upstream request body: model rewritten (cookie
// lane only), stream pinned per path, privacy fields stripped.
func buildChatBody(payload []byte, upstreamModel string, stream bool, lane string) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("empty chat payload")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", fmt.Errorf("payload parse: %w", err)
	}
	model := upstreamModel
	if lane == laneCookie {
		model = resolveAutoModel(upstreamModel)
	}
	modelJSON, _ := json.Marshal(model)
	obj["model"] = modelJSON
	streamJSON, _ := json.Marshal(stream)
	obj["stream"] = streamJSON
	for _, field := range privacyStripFields {
		delete(obj, field)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// modelAllowlistRe matches the upstream's "model not in your key's allowlist"
// complaint (the desktop's aq regex, index.beauty.mjs:1962 — CN/EN shapes).
var modelAllowlistRe = regexp.MustCompile(`该模型不在[\s\S]{0,8}(Key|key|密钥)[\s\S]{0,12}可用模型范围|模型[\s\S]{0,6}不在[\s\S]{0,8}可用模型范围|model[\s\S]{0,20}not[\s\S]{0,10}(in|within)[\s\S]{0,20}(key|api[\s_-]*key)[\s\S]{0,20}(model[\s_-]*)?(range|allowlist|list)`)

// isModelAllowlistError reports whether a 401 body is the model-range
// complaint — renewing the session cannot help there, so the retry ladder
// must skip (desktop cq() parity).
func isModelAllowlistError(body []byte) bool {
	return modelAllowlistRe.Match(body)
}

// sendChat performs one upstream chat call through the host bridge.
func sendChat(buildReq func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
	req, err := buildReq()
	if err != nil {
		return nil, 0, nil, err
	}
	return hostHTTPDoStream(req)
}

// cookieLaneSessionFault reports whether a chat status means the service
// ticket is stale and worth one re-mint: a straight 401, the 302 the
// upstream answers when the ticket is missing/expired, or — when some
// transport followed that redirect for us — a 200 that is actually the
// passport login page (HTML on a JSON endpoint).
func cookieLaneSessionFault(sc int, hdrs http.Header) bool {
	if sc == http.StatusUnauthorized || sc == http.StatusFound {
		return true
	}
	if sc == http.StatusOK && hdrs != nil {
		if ct := hdrs.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "text/html") {
			return true
		}
	}
	return false
}

// sendChatWithCookieRetry wraps sendChat with the cookie lane's renew ladder:
// session fault (not model-range) → re-mint the service ticket → retry once.
// buildReq is invoked FRESH for every attempt — a mint can move the region,
// so the endpoint and headers must be rebuilt from the mutated credential
// (routeFor(sa)), never cached from the first try. Other statuses and the
// sk lane pass through untouched (the sk is permanent; a 401 means re-login).
// When the retry itself lands on another session fault, the raw response is
// returned: callers check cookieLaneSessionFault once more and surface the
// self-heal guidance instead of feeding a login page to the body parser.
func sendChatWithCookieRetry(sa *storedAuth, route chatRoute, buildReq func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
	stream, sc, hdrs, err := sendChat(buildReq)
	if err != nil {
		return stream, sc, hdrs, err
	}
	if route.lane != laneCookie || !cookieLaneSessionFault(sc, hdrs) {
		return stream, sc, hdrs, err
	}
	// Drain the fault body before deciding — the model-range complaint is a
	// terminal answer, not a session fault.
	errBody, _ := readAllHost(stream)
	if sc == http.StatusUnauthorized && isModelAllowlistError(errBody) {
		return nil, sc, hdrs, fmt.Errorf("mimo upstream 401 (model not allowed for this account): %s", truncateRedacted(string(errBody), 200))
	}
	if !renewCookieSessionFn(sa) {
		return nil, sc, hdrs, fmt.Errorf("mimo session expired (status %d, re-mint failed): the desktop's login rows may be stale — re-login the desktop (refreshes passToken) or use the sk lane", sc)
	}
	return sendChat(buildReq)
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripProviderPrefix(req.Model)
	route := routeFor(sa)
	body, berr := buildChatBody(req.Payload, upstreamModel, false, route.lane)
	if berr != nil {
		return nil, fmt.Errorf("body build: %w", berr)
	}
	buildReq := func() (*http.Request, error) {
		r := routeFor(sa) // re-resolve per attempt — region may have moved
		req, err := http.NewRequest(http.MethodPost, r.endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.applyHeaders(req, sa, body)
		return req, nil
	}
	stream, sc, _, serr := sendChatWithCookieRetry(sa, route, buildReq)
	if serr != nil {
		return nil, fmt.Errorf("http_error: %w", serr)
	}
	payload, rerr := readAllHost(stream)
	stream.Close()
	if rerr != nil {
		return nil, upstreamReadError(rerr)
	}
	if sc >= 400 {
		return nil, upstreamStatusError(sc, chatUpstreamError(sc, string(payload)))
	}
	completion, err := decodeNonStreamCompletion(payload, req.Model)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripProviderPrefix(req.Model)
	route := routeFor(sa)
	bodyRaw := req.Payload
	if len(bodyRaw) == 0 {
		bodyRaw = req.OriginalRequest
	}
	body, berr := buildChatBody(bodyRaw, upstreamModel, true, route.lane)
	if berr != nil {
		return nil, fmt.Errorf("body build: %w", berr)
	}

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		chunks, _, errCollect := collectUpstreamStream(sa, route, body, sseFramed)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the
	// upstream and emits each chunk via host.stream.emit so the client sees
	// true streaming. context.Background() (not nil) so the request can be
	// cancelled when the client disconnects.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		pumpUpstreamStream(ctx, sa, route, body, cancel, req.StreamID, sseFramed)
	}()
	return okEnvelope(streamResponse{Headers: headers})
}

// stripProviderPrefix removes the leading "mimo/" (or any "<provider>/")
// segment from a CPA-facing model name, leaving the bare upstream model id.
func stripProviderPrefix(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[i+1:]
	}
	return model
}

// readAllHost drains a bridged stream fully (non-stream path).
func readAllHost(stream *hostHTTPStream) ([]byte, error) {
	return io.ReadAll(newHostStreamReader(stream))
}
