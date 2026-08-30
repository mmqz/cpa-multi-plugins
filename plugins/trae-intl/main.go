// Package main implements the trae-intl CLIProxyAPI dynamic plugin.
//
// trae-intl wraps Trae Intl (api.marscode.com + core-normal.trae.ai) as a cliproxy
// api.trae.cn) as a cliproxy provider: Trae OAuth (GetLoginGuidance +
// ExchangeToken), Cloud-IDE-JWT auth, Web SOLO remote chat_sessions + events SSE
// chat executor, v1 credit API. Intl has no check-in.
//
// Protocol layer based on Sliverkiss/traework2api (MIT). Adapted to CPA
// dynamic plugin C ABI.
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

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
        stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
        if (stored_host == NULL || stored_host->call == NULL) {
                return 1;
        }
        return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
        if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
                stored_host->free_buffer(ptr, len);
        }
}
*/
import "C"

import (
        "encoding/json"
        "net/http"
        "sync"
        "unsafe"
        "fmt"

        "github.com/mmqz/cpa-multi-plugins/plugins/trae-intl/auth"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-intl/upstream"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
        providerName  = "trae-intl"
        authFileName  = "trae-intl.json"
        pluginLogoURL = "https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/Trae.png"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "0.1.0"

var hostAPI *C.cliproxy_host_api

// loginStates tracks in-flight OAuth flows keyed by state token.
var loginStates sync.Map

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
                writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
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
        // No-op: host calls this on its own exit path. Touching Go runtime state
        // here risks SIGSEGV in cgo after dlclose.
}

// -----------------------------------------------------------------------------
// Host calls
// -----------------------------------------------------------------------------

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
        rc := C.call_host_api(cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
        var out []byte
        if resp.ptr != nil && resp.len > 0 {
                out = C.GoBytes(resp.ptr, C.int(resp.len))
        }
        if resp.ptr != nil && hostAPI.free_buffer != nil {
                C.free_host_buffer(resp.ptr, resp.len)
        }
        if rc != 0 {
                return out, fmt.Errorf("host call %s returned %d", method, int(rc))
        }
        return out, nil
}

// -----------------------------------------------------------------------------
// Method dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
        switch method {
        case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
                return okEnvelope(registration())

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
                // Trae has no dedicated count_tokens API.
                return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})

        default:
                return errorEnvelope("unknown_method", "unknown method: "+method), nil
        }
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

type envelope struct {
        OK     bool            `json:"ok"`
        Result json.RawMessage `json:"result,omitempty"`
        Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
        Code    string `json:"code"`
        Message string `json:"message"`
}

type identifierResponse struct {
        Identifier string `json:"identifier"`
}

type registrationPayload struct {
        SchemaVersion uint32                 `json:"schema_version"`
        Metadata      pluginapi.Metadata     `json:"metadata"`
        Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
        AuthProvider          bool                         `json:"auth_provider"`
        Executor             bool                         `json:"executor"`
        ExecutorModelScope   pluginapi.ExecutorModelScope `json:"executor_model_scope"`
        ExecutorInputFormats []string                     `json:"executor_input_formats,omitempty"`
        ExecutorOutputFormats []string                    `json:"executor_output_formats,omitempty"`
}

func registration() registrationPayload {
        return registrationPayload{
                SchemaVersion: pluginabi.SchemaVersion,
                Metadata: pluginapi.Metadata{
                        Name:             providerName,
                        Version:          version,
                        Author:           "mmqz (based on traework2api by Sliverkiss)",
                        GitHubRepository: "https://github.com/mmqz/cpa-multi-plugins",
                        Logo:             pluginLogoURL,
                        ConfigFields:     []pluginapi.ConfigField{},
                },
                Capabilities: registrationCapability{
                        AuthProvider:          true,
                        Executor:              true,
                        ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
                        ExecutorInputFormats:  []string{"chat-completions"},
                        ExecutorOutputFormats: []string{"chat-completions"},
                },
        }
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func okEnvelope(result any) ([]byte, error) {
        return json.Marshal(envelope{OK: true, Result: mustJSON(result)})
}

func errorEnvelope(code, message string) []byte {
        raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
        return raw
}

func mustJSON(v any) json.RawMessage {
        raw, _ := json.Marshal(v)
        return raw
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

// -----------------------------------------------------------------------------
// Stubs (TODO: implement in next iteration)
// -----------------------------------------------------------------------------

func handleParseAuth(request []byte) ([]byte, error) {
        // TODO: parse trae auth file using auth.LoadDir / auth.Parse
        _ = request
        return errorEnvelope("not_implemented", "auth.parse: WIP"), nil
}

func handleStartLogin(request []byte) ([]byte, error) {
        // TODO: start Trae OAuth via upstream.Client.GetLoginGuidance + local callback server
        _ = request
        return errorEnvelope("not_implemented", "auth.login.start: WIP"), nil
}

func handlePollLogin(request []byte) ([]byte, error) {
        // TODO: poll for OAuth callback, call ExchangeToken, return Auth
        _ = request
        return errorEnvelope("not_implemented", "auth.login.poll: WIP"), nil
}

func handleRefreshAuth(request []byte) ([]byte, error) {
        // TODO: call upstream.Client.RefreshToken
        _ = request
        return errorEnvelope("not_implemented", "auth.refresh: WIP"), nil
}

func handleExecExecute(request []byte) ([]byte, error) {
        // TODO: call upstream.ChatStream with stream=false (or stream=true + aggregate)
        _ = request
        return errorEnvelope("not_implemented", "executor.execute: WIP"), nil
}

func handleExecStream(request []byte) ([]byte, error) {
        // TODO: call upstream.ChatStream with stream=true, return SSE chunks
        _ = request
        return errorEnvelope("not_implemented", "executor.execute_stream: WIP"), nil
}

// -----------------------------------------------------------------------------
// Compile-time references to sub-packages (ensures they're included in build)
// -----------------------------------------------------------------------------

var _ = auth.Auth{}
var _ = upstream.ClientID
var _ = http.StatusOK
