// Package main implements the mimo CLIProxyAPI dynamic plugin.
//
// mimo wraps Xiaomi MiMo as a cliproxy provider with two lanes, both
// reconstructed clean-room from official sources:
//
//   - sk lane (primary): the official MiMo-Code CLI's browser OAuth
//     (platform.xiaomimimo.com) — a local X25519 key pair publishes the public
//     key in the authorize URL, the platform redirects back to a loopback
//     callback with an AES-256-GCM blob that decrypts to {sk, uid, url}; chat
//     rides the issued base_url with `Authorization: Bearer {sk}` and
//     `X-Mimo-Source: mimocode-cli` (MiMo-Code packages/opencode/src/plugin/mimo.ts).
//   - cookie lane (secondary): adopts the Xiaomi MiMo desktop app's SSO
//     session (Chromium partition persist:xiaomi-account) and speaks the same
//     wire the desktop's in-process engine lane uses: POST
//     {region-base}/route/chat/completions with NO Authorization header,
//     `X-Mimo-Source: mimocode-cli-free`, `X-Client-Version: <desktop ver>`,
//     model alias mimo-auto resolved to mimo-pro, and the 401 → renew → retry
//     once ladder (desktop out/main/index.mjs, see docs/MIMO_AUTH.md §2).
//
// Privacy boundary (docs/MIMO_PRIVACY.md §7, hard constraints): zero
// telemetry, zero audit-plane calls, zero invite/region gate probes, no
// content logging, credentials stored via the host auth store only.
// Desktop-only surfaces (computer-use, browser-bridge, evolve/memory, image
// generation) are out of scope.
//
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
static int mimo_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
        return api->call(api->host_ctx, method, request, request_len, response);
}
static void mimo_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
        api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName = "mimo"
	authFileName = "mimo.json"

	// Lane identifiers stored in credentials.
	laneKey    = "key"    // sk lane — official CLI OAuth credential
	laneCookie = "cookie" // adopted desktop SSO session

	// X-Mimo-Source values are the OFFICIAL strings extracted from the
	// desktop bundle (index.beauty.mjs:1961/1988) and the CLI
	// (plugin/mimo.ts:200). Do not invent variants.
	sourceKeyLane    = "mimocode-cli"      // sk lane (CLI parity)
	sourceCookieLane = "mimocode-cli-free" // desktop engine cookie lane

	// desktopAppVersion is the desktop build the cookie-lane fingerprint was
	// verified against; sent as X-Client-Version on the cookie lane
	// (hl() index.beauty.mjs:4797). Configurable.
	desktopAppVersion = "26.922.222056"

	// platformBase is the OAuth authorize host (CLI parity; the CLI honors
	// MIMO_PLATFORM_URL, which the plugin exposes as config instead).
	platformBase = "https://platform.xiaomimimo.com"

	// loginTTL bounds one OAuth login flow — the CLI's callback server uses a
	// hard 5-minute window (mimo.ts authorize()); the plugin adds margin.
	loginTTL = 6 * time.Minute

	// loginStatesPruneInterval bounds how often the janitor sweeps abandoned
	// login states (user started a login but never finished).
	loginStatesPruneInterval = time.Minute

	// regionCN is the fallback region base (desktop domestic default). The
	// region table and adoption live in session.go.
	regionCN = "cn"
)

// loginCtx holds one in-flight OAuth login flow. The plugin owns the loopback
// callback server the platform redirects to (the CLI does the same in-process
// via its `authorize` hook).
type loginCtx struct {
	keyName   string
	privKey   []byte // raw X25519 private scalar (32 bytes)
	callback  *loopbackServer
	result    chan oauthResult
	expires   time.Time
	startedAt int64
}

// oauthResult is the decrypted platform callback payload.
type oauthResult struct {
	SK  string `json:"sk"`
	UID string `json:"uid"`
	URL string `json:"url"`
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
	httpClientOnce sync.Once
	sharedClient   *http.Client
)

func init() {
	go func() {
		ticker := time.NewTicker(loginStatesPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			loginStates.Range(func(key, value any) bool {
				if lc, ok := value.(*loginCtx); ok && now.After(lc.expires) {
					lc.shutdown()
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
	rc := C.mimo_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.mimo_free_host_buffer(hostAPI, resp.ptr, resp.len)
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
		return okEnvelope(mimoRegistration())
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
		// No BasePath cache needed: the only management surface is a
		// resource route, and the /v0/resource/plugins/<provider> prefix
		// is fixed (same shape trae/zcode dispatch on).
		return okEnvelope(mimoManagementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleMimoManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration & envelopes
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code string `json:"code"`
	// HTTPStatus mirrors pluginabi.Error.HTTPStatus: the host's
	// decodeEnvelopeResult feeds it into rpcError.StatusCode(), which
	// drives the host's real per-status credential cooldown (401/402/429).
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

// mimoRegistration declares the plugin's capability surface. Scope is
// deliberately narrow: models + auth + chat executor. No scheduler /
// management / usage plugin — the host tracks usage from the upstream's
// include_usage terminal chunk itself.
func mimoRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          version,
			Author:           "mmqz",
			GitHubRepository: "https://github.com/mmqz/cpa-multi-plugins",
			Logo:             "",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "region", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"auto", "cn", "sgp", "ru", "in"}, Description: "Cookie-lane region base: auto (adopt from the desktop session / cn default), cn, sgp, ru or in."},
				{Name: "x_client_version", Type: pluginapi.ConfigFieldTypeString, Description: "X-Client-Version header value for the cookie lane (desktop parity, default 26.922.222056)."},
				{Name: "platform_url", Type: pluginapi.ConfigFieldTypeString, Description: "OAuth platform base for NEW sk-lane logins (default https://platform.xiaomimimo.com; the CLI's MIMO_PLATFORM_URL equivalent)."},
				{Name: "cookie_paths", Type: pluginapi.ConfigFieldTypeString, Description: "Optional comma-separated extra Chromium 'Cookies' file paths for desktop SSO adoption (auto-detect covers standard installs of both 'Xiaomi MiMo AI' and 'Xiaomi MiMo')."},
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
			Scheduler:             false,
			ManagementAPI:         true, // v0.2.5: /oauth_submit paste-to-complete fallback
			UsagePlugin:           false,
		},
	}
}

// version self-reports the plugin build. CI releases build WITHOUT any
// -ldflags -X injection (release.yml runs plain `go build`), so this default
// must stay in lockstep with the VERSION file — the same drift class that
// shipped trae v0.12.86 self-reporting 0.12.56 (repo lesson 2026-09-23).
// `make build` may still override it via -X (git describe).
var version = "0.2.5"

// -----------------------------------------------------------------------------
// Envelope helpers
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

// contextTODO is a named placeholder so executor entry points that gain
// cancellation later keep a single import site.
var _ = context.Background
