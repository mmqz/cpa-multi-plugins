// executor.go builds and sends the coding-plan chat traffic: OpenAI-format
// request bodies pass through to the provider's OpenAI-compatible gateway
// (api.z.ai / open.bigmodel.cn /chat/completions) with the ZCode identity +
// trace + V4 signing headers applied.
//
// Upstream contract notes (TriDefender/zcode-api):
//   - Coding-plan OpenAI upstream auth is `Authorization: Bearer {key}` where
//     {key} is the RESOLVED plan key ("{id}.{secret}") from auth_keyres.go.
//   - The LLM User-Agent carries the `ai-sdk/anthropic/3.0.81` SDK suffix.
//   - Trace headers attribute every model request: x-request-id,
//     x-zcode-session-type: main, x-zcode-trace-id, x-query-id, x-session-id
//     (start-plan omits the last two; coding-plan sends all five).
//   - The V4 signing gate decides per (origin, credential) whether Ed25519 +
//     PoW headers ride the request (signing.go); this layer just asks.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// startPlanAnthropicEndpoint is the start-plan gateway the live ZCode desktop
// client posts Anthropic messages to (the legacy OpenAI route
// /api/v1/zcode-plan/chat/completions 404s since the 2026-08-28 server-side
// rollout). Signing is exempt on this path (see signing.go). Var (not const)
// so tests can point it at an httptest server.
var startPlanAnthropicEndpoint = "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"

// chatRoute is one plan's upstream contract: where to POST, which header
// set to apply, and whether the upstream speaks Anthropic messages (start-
// plan) or OpenAI chat completions (coding-plan). The anthropic flag drives
// the request/response/SSE translation layer (anthropic.go / anthropic_sse.go).
type chatRoute struct {
	endpoint     string
	anthropic    bool
	applyHeaders func(*http.Request, *storedAuth, string)
}

// routeFor resolves the chat route for one credential: start-plan accounts
// ride the zcode.z.ai anthropic gateway with the plan JWT; coding-plan
// accounts ride the provider's OpenAI-compatible endpoint with the resolved
// plan key.
func routeFor(sa *storedAuth) chatRoute {
	if sa.Auth.Plan == planStart {
		return chatRoute{endpoint: startPlanAnthropicEndpoint, anthropic: true, applyHeaders: applyStartPlanChatHeaders}
	}
	return chatRoute{endpoint: chatEndpointFor(sa), applyHeaders: applyChatHeaders}
}

// chatEndpointFor returns the coding-plan OpenAI-compatible chat endpoint.
func chatEndpointFor(sa *storedAuth) string {
	if authProviderFor(sa) == providerBigmodel {
		return openAIBaseBigmodel + "/chat/completions"
	}
	return openAIBaseZai + "/chat/completions"
}

// buildChatBody prepares the upstream request body from the client's
// chat-completions payload: the model field is rewritten to the bare
// upstream model id and the stream flag is pinned per path (streaming pump
// forces stream=true; the non-stream path forces it off so the upstream
// returns a single JSON completion).
func buildChatBody(payload []byte, upstreamModel string, stream bool) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("empty chat payload")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", fmt.Errorf("payload parse: %w", err)
	}
	modelJSON, _ := json.Marshal(upstreamModel)
	obj["model"] = modelJSON
	streamJSON, _ := json.Marshal(stream)
	obj["stream"] = streamJSON
	out, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// applyChatHeaders applies the full upstream header set for one coding-plan
// chat call: identity (g6n) + trace + auth + SDK UA suffix + V4 signing
// headers (via the signing manager's retry ladder in the send paths).
func applyChatHeaders(req *http.Request, sa *storedAuth, body string) {
	req.Header.Set("Content-Type", "application/json")
	for k, v := range buildLlmIdentityHeaders(zcodeIdentity()) {
		req.Header.Set(k, v)
	}
	applyTraceHeaders(req)
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	ua := req.Header.Get("User-Agent")
	if ua != "" {
		req.Header.Set("User-Agent", ua+" "+llmSDKUserAgentSuffix)
	}
}

// applyTraceHeaders mirrors the client's attribution header set for
// coding-plan traffic: five UUID-attributed headers with session-type main.
func applyTraceHeaders(req *http.Request) {
	req.Header.Set("x-request-id", uuid.NewString())
	req.Header.Set("x-zcode-session-type", "main")
	req.Header.Set("x-zcode-trace-id", uuid.NewString())
	req.Header.Set("x-query-id", uuid.NewString())
	req.Header.Set("x-session-id", uuid.NewString())
}

// sessionIdFromRequest extracts the x-session-id value — the signing message
// binds to it, so signing must read exactly the header the request carries.
func sessionIdFromRequest(req *http.Request) string {
	return strings.TrimSpace(req.Header.Get("x-session-id"))
}

// applyStartPlanChatHeaders applies the start-plan gateway's header set:
// identity (g6n) + the three-header trace subset (start-plan omits
// x-query-id/x-session-id — the client has no query/session context on this
// plane) + `Authorization: Bearer {plan JWT}` + the required
// anthropic-version + the ai-sdk/anthropic UA suffix (bundle `Cm`/`k0o`:
// real anthropic-kind LLM requests arrive as
// `ZCode/{ver} ai-sdk/anthropic/3.0.81`).
func applyStartPlanChatHeaders(req *http.Request, sa *storedAuth, body string) {
	req.Header.Set("Content-Type", "application/json")
	for k, v := range buildLlmIdentityHeaders(zcodeIdentity()) {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-request-id", uuid.NewString())
	req.Header.Set("x-zcode-session-type", "main")
	req.Header.Set("x-zcode-trace-id", uuid.NewString())
	req.Header.Set("Authorization", "Bearer "+sa.Auth.JWT)
	req.Header.Set("anthropic-version", anthropicVersion)
	ua := req.Header.Get("User-Agent")
	if ua != "" {
		req.Header.Set("User-Agent", ua+" "+llmSDKUserAgentSuffix)
	}
}
