// Package main implements the zcode CLIProxyAPI dynamic plugin.
//
// zcode wraps the GLM coding-plan surface (Z.AI international + BigModel
// domestic) as a cliproxy provider: a server-mediated CLI login through
// zcode.z.ai issues a permanent provider access token plus a plan JWT, and
// the plugin forwards OpenAI-format chat traffic to the coding-plan
// OpenAI-compatible gateway (api.z.ai / open.bigmodel.cn). It is a clean-room
// reimplementation reconstructed from the public protocol notes of
// TriDefender/zcode-api (ZCode Proxy, MIT): OAuth poll flow, identity
// headers, endpoint routing and the quota/billing plane. Credit for the
// original protocol reverse-engineering goes to the ZCode Proxy project.
// Built with -buildmode=c-shared and exports the cliproxy C ABI entry points.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
        void* ptr;
        size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
        uint32_t abi_version;
        void* host_ctx;
        cliproxy_host_call_fn call;
        cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
        uint32_t abi_version;
        cliproxy_plugin_call_fn call;
        cliproxy_plugin_free_fn free_buffer;
        cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
        return api->call(api->host_ctx, method, request, request_len, response);
}
static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
        api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName  = "zcode"
	authFileName  = "zcode.json"
	pluginLogoURL = ""

	// LLM upstreams (coding-plan, OpenAI-compatible gateway).
	openAIBaseZai      = "https://api.z.ai/api/coding/paas/v4"
	openAIBaseBigmodel = "https://open.bigmodel.cn/api/coding/paas/v4"

	// loginTTL bounds one CLI login flow. The authorize URL stays valid until
	// the server-issued expires_at (init response), typically 5 minutes.
	loginTTL = 10 * time.Minute
)

// Control plane: server-mediated CLI login + plan JWT + billing. Var (not
// const) so tests can point it at an httptest server.
var zcodeAPIBase = "https://zcode.z.ai/api/v1"

// loginCtx holds one in-flight CLI login flow (zcode.z.ai /oauth/cli/*).
// The plugin generates the poll token, POSTs init to obtain flow_id +
// authorize_url, the user authorizes in a browser (the interstitial records
// the grant server-side), and PollLogin polls /oauth/cli/poll/{flow_id}.
type loginCtx struct {
	flowID    string
	pollToken string // 32-byte hex Bearer used on BOTH init and poll
	provider  string // zai | bigmodel
	expires   time.Time
	startedAt int64 // unix nano, set when StartLogin creates the state
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
	httpClientOnce sync.Once
	sharedClient   *http.Client
)

// loginStatesPruneInterval bounds how often the janitor sweeps abandoned
// login states (user started a login but never finished).
const loginStatesPruneInterval = time.Minute

func init() {
	go func() {
		ticker := time.NewTicker(loginStatesPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			loginStates.Range(func(key, value any) bool {
				if lc, ok := value.(*loginCtx); ok && now.After(lc.expires) {
					loginStates.Delete(key)
				}
				return true
			})
		}
	}()
}

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelopeFor(errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// Intentionally a no-op. The host calls this on its own exit path (after
	// the host Go runtime has started tearing down) and dlclose()es this
	// library immediately afterwards. Touching any Go runtime state here —
	// mutexes, channel close, goroutine synchronization — risks a SIGSEGV in
	// cgo (observed on every docker restart in the qoder plugin: SIGSEGV in
	// _Cfunc_cliproxy_shutdown_plugin, PC near a freed runtime pointer).
}

// -----------------------------------------------------------------------------
// Host calls (async streaming + auth callbacks)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close), read the host's auth store (host.auth.list/get), and
// route upstream HTTP through the host bridge (host.http.*).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.wb_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.wb_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		startAdoption()
		return okEnvelope(wbRegistration())
	case pluginabi.MethodModelStatic:
		return handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case pluginabi.MethodExecutorCountTokens:
		// Upstream has no dedicated count_tokens API. Return an unhandled-style
		// zero estimate so clients fall back / skip.
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodManagementRegister:
		// Cache host-injected BasePath so handleManagement doesn't hardcode
		// /v0/management (tolerate future host path changes).
		var regReq pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &regReq); err == nil {
			if regReq.BasePath != "" {
				setManagementBasePath(regReq.BasePath)
			}
		}
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration & models
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code string `json:"code"`
	// HTTPStatus mirrors pluginabi.Error.HTTPStatus: the host's
	// decodeEnvelopeResult feeds it into rpcError.StatusCode(),
	// which resultErrorFromError -> MarkResult uses for per-status
	// cooldown policy (402->30m, 429->quota backoff, 401->30m).
	// Without it plugin failures get only the 1-minute transient
	// cooldown — "failed credential keeps being picked".
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	FrontendAuthProvider  bool                         `json:"frontend_auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	Scheduler             bool                         `json:"scheduler"`
	ManagementAPI         bool                         `json:"management_api"`
	UsagePlugin           bool                         `json:"usage_plugin"`
}

// version is injected at build time via -ldflags "-X main.version=...".
var version = "0.1.0"

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          version,
			Author:           "mmqz",
			GitHubRepository: "https://github.com/mmqz/cpa-multi-plugins",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "login_provider", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{providerZai, providerBigmodel}, Description: "Upstream provider for NEW logins: zai (api.z.ai, default) or bigmodel (open.bigmodel.cn). Existing accounts keep their own provider."},
				{Name: "lifecycle_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Auto disable accounts when the quota is exhausted (default true)."},
				{Name: "scheduler_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{schedulerModeOff, schedulerModeCredits}, Description: "Multi-account selection: off (defer to built-in, default) or credits (pick highest remaining). WARNING: when off + lifecycle_auto=false, exhausted accounts may still be routed."},
				{Name: "usage_report_url", Type: pluginapi.ConfigFieldTypeString, Description: "Optional override of CPAMP usage import URL (default http://cpa-manager-plus:18317/v0/management/usage/import; also env USAGE_REPORT_URL)."},
				{Name: "usage_report_key", Type: pluginapi.ConfigFieldTypeString, Description: "Optional CPAMP admin key override. Prefer auto-detect from env CPAMP_ADMIN_KEY / USAGE_REPORT_KEY or secret file /run/secrets/cpamp_admin_key."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			FrontendAuthProvider:  false,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			ManagementAPI:         true,
			Scheduler:             true,
			UsagePlugin:           true,
		},
	}
}

// -----------------------------------------------------------------------------
// Stored credential
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a zcode credential.
type storedAuth struct {
	Auth    zcodeTokens  `json:"auth"`
	Account zcodeAccount `json:"account"`
}

// zcodeTokens holds one credential. AccessToken is the resolved coding-plan
// API key: Z.AI issues a two-part "id.secret" key, BigModel a single (or
// two-part) key. JWT is the ZCode plan token (start-plan traffic + the
// billing plane); it has no exp and is never refreshed — a 401/3012 from the
// gateway means re-login.
type zcodeTokens struct {
	// AccessToken is the RESOLVED coding-plan API key ("{id}.{secret}" for
	// zai, "{id}" or "{id}.{secret}" for bigmodel) — the chat credential and
	// the V4 signer's {apiKeyId}.{apiKeySecret} input. The raw OAuth token is
	// kept alongside in OAuthToken.
	AccessToken string `json:"accessToken"`
	OAuthToken  string `json:"oauthToken,omitempty"` // login OAuth token (key-resolution input; not a chat credential)
	JWT         string `json:"jwt,omitempty"`        // zcode.z.ai plan token (no exp)
	Provider    string `json:"provider"`             // zai | bigmodel
	Plan        string `json:"plan,omitempty"`       // coding-plan (default) | start-plan
	DeviceMid   string `json:"deviceMid,omitempty"`  // stable per-account UUID (billing plane)
	ExpiresAt   int64  `json:"expiresAt,omitempty"`  // 0 = unknown (permanent key)
}

type zcodeAccount struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// apiEnvelope is the generic {code,msg,data} wrapper used by zcode.z.ai.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// Accept both shapes seen in the wild:
	//   nested: {"auth":{"accessToken":...},"account":{"uid":...}} (plugin/oauth output)
	//   flat:   {"accessToken":...,"uid":...,"nickname":...} (manager-written files)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var sa storedAuth
	if _, nested := probe["auth"]; nested {
		if err := json.Unmarshal(raw, &sa); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
	} else {
		var flat struct {
			AccessToken string `json:"accessToken"`
			JWT         string `json:"jwt"`
			Provider    string `json:"provider"`
			Plan        string `json:"plan"`
			DeviceMid   string `json:"deviceMid"`
			ExpiresAt   int64  `json:"expiresAt"`
			UID         string `json:"uid"`
			Nickname    string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		sa.Auth = zcodeTokens{AccessToken: flat.AccessToken, JWT: flat.JWT, Provider: flat.Provider, Plan: flat.Plan, DeviceMid: flat.DeviceMid, ExpiresAt: flat.ExpiresAt}
		sa.Account = zcodeAccount{UID: flat.UID, Nickname: flat.Nickname}
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	if sa.Auth.Provider == "" {
		sa.Auth.Provider = providerZai
	}
	return &sa, nil
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

// isOurFamilyFileName reports whether a type-less auth file belongs to this
// plugin by name: the canonical zcode- prefix. The filename is the only
// trustworthy discriminator for files without an explicit type (see the
// ownership note in handleParseAuth).
func isOurFamilyFileName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), providerName+"-")
}

// isOurDeclaredType reports whether an explicitly declared auth "type"
// belongs to this plugin.
func isOurDeclaredType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "zcode", "zcode-zai", "zcode-bigmodel", "zai", "bigmodel":
		return true
	}
	return false
}

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Ownership check (CPA native contract): the host routes by the file's
	// top-level "type" field. Files without a type fall back to polling every
	// plugin — first Handled=true wins. Only claim files whose declared type
	// matches us, or whose filename carries our prefix.
	var probeType struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.RawJSON, &probeType)
	declared := strings.ToLower(strings.TrimSpace(probeType.Type))
	if declared != "" && !isOurDeclaredType(declared) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if declared == "" {
		// No type declared: claim ONLY when the filename carries our family.
		// req.Provider cannot prove ownership here — the host's callParseAuths
		// rewrites an empty Provider to the POLLED plugin's own identifier.
		if !isOurFamilyFileName(req.FileName) {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	// CRITICAL: echo back the host-provided FileName AND leave ID empty —
	// CPA falls back to authIDForPath(path) which derives ID from the file
	// path and always matches the watcher's key (prevents duplicate records).
	// Preserve the credit segment already stored in the file so a reload
	// never regresses a live "余N 已用N" note to the placeholder.
	ad := toAuthDataOptsWithNote(sa, nil, false, noteCreditsFromJSON(req.RawJSON))
	ad.ID = ""
	if fn := strings.TrimSpace(req.FileName); fn != "" {
		ad.FileName = fn
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    ad,
	})
}

func toAuthData(sa *storedAuth) pluginapi.AuthData {
	return toAuthDataOpts(sa, nil, false)
}

// toAuthDataOpts builds AuthData with optional credits snapshot and disabled flag.
func toAuthDataOpts(sa *storedAuth, cr *creditsSummary, disabled bool) pluginapi.AuthData {
	return toAuthDataOptsWithNote(sa, cr, disabled, "")
}

// toAuthDataOptsWithNote is toAuthDataOpts plus a previously known credit
// segment, used by paths that must not regress a live note to "积分未知".
func toAuthDataOptsWithNote(sa *storedAuth, cr *creditsSummary, disabled bool, prevCredits string) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	id := providerName
	fileName := authFileName
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			id = uid
			fileName = "zcode-" + authProviderFor(sa) + "-" + uid + ".json"
		}
	}
	label := labelForAuth(sa)
	meta := enrichAuthMetadataWithPrev(sa, cr, disabled, prevCredits)
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		Disabled:    disabled,
		StorageJSON: storage,
		Metadata:    meta,
	}
}

// -----------------------------------------------------------------------------
// Executor
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
	if sa.Auth.Plan == planStart {
		return nil, fmt.Errorf("start-plan execution requires the Anthropic-format upstream (not yet supported by this build); use a coding-plan account")
	}
	upstreamModel := stripProviderPrefix(req.Model)
	cooldownModel := requestModelForCooldown(req.Model, req.Metadata)
	started := time.Now()
	authUID := sa.Account.UID

	body, err := buildChatBody(req.Payload, upstreamModel, false)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, fmt.Errorf("body build: %w", err)
	}
	endpoint := chatEndpointFor(sa)
	buildReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyChatHeaders(httpReq, sa, body)
		return httpReq, nil
	}

	// Compliance: route via host.http.do_stream so request-log captures the
	// outbound call. The send carries the full V4 signing retry ladder.
	// Non-streaming body: read the whole bridge stream, then decide error vs
	// completion envelope.
	stream, statusCode, _, err := sendChatWithSigning(sa, buildReq)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		payload, _ := io.ReadAll(reader)
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(payload))
		recordUpstreamFailure(req.AuthID, cooldownModel, statusCode, string(payload))
		reconcileAfterExecutorError(req.AuthID, statusCode, string(payload))
		return nil, upstreamStatusError(statusCode, chatUpstreamError(statusCode, string(payload)))
	}
	payload, readErr := io.ReadAll(reader)
	if readErr != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, readErr.Error())
		recordUpstreamFailure(req.AuthID, cooldownModel, 0, readErr.Error())
		return nil, upstreamReadError(readErr)
	}
	completion, err := decodeNonStreamCompletion(payload, req.Model)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, err.Error())
		recordUpstreamFailure(req.AuthID, cooldownModel, 0, err.Error())
		return nil, err
	}
	publishUsage(req.Model, upstreamModel, authUID, started, usageDetailFromCompletion(completion), false, 0, "")
	invalidateAccountCredits(req.AuthID, authUID)
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
	if sa.Auth.Plan == planStart {
		return nil, fmt.Errorf("start-plan execution requires the Anthropic-format upstream (not yet supported by this build); use a coding-plan account")
	}
	upstreamModel := stripProviderPrefix(req.Model)
	cooldownModel := requestModelForCooldown(req.Model, req.Metadata)
	started := time.Now()
	authUID := sa.Account.UID

	bodyRaw := req.Payload
	if len(bodyRaw) == 0 {
		bodyRaw = req.OriginalRequest
	}
	body, err := buildChatBody(bodyRaw, upstreamModel, true)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, fmt.Errorf("body build: %w", err)
	}

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		collector := &sseUsageCollector{}
		chunks, statusCode, errCollect := collectUpstreamStream(body, sa, sseFramed, collector)
		if errCollect != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, collector.detail(), true, statusCode, errCollect.Error())
			recordUpstreamFailure(req.AuthID, cooldownModel, statusCode, errCollect.Error())
			return nil, errCollect
		}
		publishUsage(req.Model, upstreamModel, authUID, started, collector.detail(), false, 0, "")
		invalidateAccountCredits(req.AuthID, authUID)
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the upstream
	// and emits each chunk via host.stream.emit so the client sees true streaming.
	// Use context.Background() (not nil) so the request can be cancelled when the
	// client disconnects — otherwise the pump keeps reading a dead upstream until
	// sharedHTTPClient's 120s timeout, holding a pool slot the whole time.
	ctx, cancel := context.WithCancel(context.Background())
	go pumpUpstreamStream(ctx, sa, chatEndpointFor(sa), body, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, cooldownModel)
	return okEnvelope(streamResponse{Headers: headers})
}

// stripProviderPrefix removes the leading "zcode/" (or any "<provider>/")
// segment from a CPA-facing model name, leaving the bare upstream model id.
func stripProviderPrefix(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[i+1:]
	}
	return model
}

// -----------------------------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// errorEnvelopeFor serializes a handler error, preserving the upstream HTTP
// status when the error carries one (statusError or any StatusCode()
// implementation). The status crosses the RPC boundary as the envelope error
// http_status field and drives the host's real per-status credential
// cooldown (see envelopeError.HTTPStatus).
func errorEnvelopeFor(err error) []byte {
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) && sc.StatusCode() > 0 {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
			Code: "plugin_error", Message: err.Error(), HTTPStatus: sc.StatusCode(),
		}})
		return raw
	}
	return errorEnvelope("plugin_error", err.Error())
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// commonHeaders applies the baseline JSON request headers used by every
// control-plane call (login, billing). Identity headers are layered on top
// by the callers that need them.
func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)
}

// sha256hex8 returns the first 8 hex chars of sha256(s) — used to derive a
// stable pseudo-UID when the login response carries no user id.
func sha256hex8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:4])
}

// contextTODO is a named placeholder so executor entry points that gain
// cancellation later keep a single import site.
var _ = context.Background
