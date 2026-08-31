// Package main implements the trae-solo-cn CLIProxyAPI dynamic plugin.
//
// trae-solo-cn wraps TRAE Work CN / SOLO CN (trae-api-cn.mchost.guru +
// api.trae.cn) as a cliproxy provider: Trae OAuth (GetLoginGuidance +
// AuthCode ExchangeToken), Cloud-IDE-JWT auth, llm_utils_chat +
// function=solo_work_lite chat executor, daily check-in via checkin_credits,
// v2 credit API query, multi-account pool with credit-aware scheduler.
//
// Protocol layer based on Sliverkiss/traework2api (MIT). Adapted to CPA
// dynamic plugin C ABI with OAuth login flow, executor, scheduler, checkin,
// token keepalive, and credit lifecycle.
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
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "net/http"
        "net/url"
        "strings"
        "sync"
        "time"
        "unsafe"

        "github.com/mmqz/cpa-multi-plugins/plugins/trae-solo-cn/auth"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-solo-cn/pool"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-solo-cn/scheduler"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-solo-cn/upstream"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
        providerName  = "trae-solo-cn"
        authFileName  = "trae-solo-cn.json"
        pluginLogoURL = ""

        // OAuth login flow timeout (5 min).
        loginTTL = 5 * time.Minute

        // Scheduler defaults.
        defaultCheckinHour = 9
        defaultRefreshSkew = 24 * time.Hour

        // Account cache TTL for credits/checkin status.
        accountCacheTTL = 5 * time.Minute

        // ---- cockpit-tools aligned OAuth constants (SOLO CN platform) ----
        // GetLoginGuidance URL — first entry of TRAE_CN_LOGIN_GUIDANCE_URLS.
        oauthLoginGuidanceURL = "https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance"
        // ExchangeToken base URL — /trae/api/v3/oauth/ExchangeToken (NOT /cloudide/api/v3/trae/).
        oauthExchangeTokenURL = "https://api.trae.cn/trae/api/v3/oauth/ExchangeToken"
        // Fallback ExchangeToken host if callback does not return loginHost.
        oauthDefaultHost = "https://api.trae.cn"

        // Platform-specific flags (SOLO vs non-SOLO).
        oauthAuthFrom       = "solo"   // SOLO uses "solo"; non-SOLO uses "trae".
        oauthPlatformCode   = "SOLO_PC" // SOLO uses SOLO_PC; non-SOLO uses IDE_PC.
        oauthHideSaasLogin  = true     // SOLO only; non-SOLO omits this param.

        // Default device fingerprint values (mirrors cockpit-tools defaults).
        oauthPluginVersion = "1.0.0"
        oauthDeviceName    = "DESKTOP-CPASOLO"
        oauthDeviceType    = "windows"
        oauthDeviceBrand   = "83DG"
        oauthOSVersion     = "Windows 11 Pro"
        oauthEnv           = "prod"
        oauthAppType       = "trae"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "0.1.0"

var (
        hostAPI *C.cliproxy_host_api

        // loginStates tracks in-flight OAuth flows keyed by state token.
        loginStates sync.Map

        // upstreamClient is the shared SOLO upstream client.
        upstreamClient *upstream.Client

        // accountPool is the multi-account pool with cooldown/disable state.
        accountPool *pool.Pool

        // sched is the daily check-in + token refresh scheduler.
        sched *scheduler.Scheduler

        // schedulerCtx cancels the scheduler on shutdown.
        schedulerCtx context.Context
        schedulerCancel context.CancelFunc

        // accountCache caches credits/checkin status per auth_index.
        accountCache sync.Map // auth_index → *accountCacheEntry
)

type accountCacheEntry struct {
        credits   int64
        checkin   *checkinStatus
        fetched   time.Time
}

type checkinStatus struct {
        CheckedIn bool
        Credits   int64
        Enable    bool
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
        C.store_host_api(host) // CRITICAL: store in C global for call_host_api wrapper
        plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
        plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
        plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
        plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)

        // Initialize upstream client + pool + scheduler.
        upstreamClient = upstream.New()
        accountPool = pool.New("") // state file optional; host persists auth
        schedulerCtx, schedulerCancel = context.WithCancel(context.Background())
        sched = scheduler.New(scheduler.Config{
                Pool:         accountPool,
                Upstream:     upstreamClient,
                CheckinHour:  defaultCheckinHour,
                RefreshHours: []int{3},
                RefreshSkew:  defaultRefreshSkew,
        })
        go sched.Run(schedulerCtx)

        // Janitor: sweep abandoned login states every minute to prevent listener leaks.
        go func() {
                ticker := time.NewTicker(time.Minute)
                defer ticker.Stop()
                for {
                        select {
                        case <-schedulerCtx.Done():
                                return
                        case <-ticker.C:
                                now := time.Now()
                                loginStates.Range(func(key, value any) bool {
                                        if lc, ok := value.(*loginCtx); ok && now.After(lc.expires) {
                                                loginStates.Delete(key)
                                                if lc.listener != nil {
                                                        lc.listener.Close()
                                                }
                                        }
                                        return true
                                })
                        }
                }
        }()

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
                return okEnvelope(buildRegistration())

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
        SchemaVersion uint32                  `json:"schema_version"`
        Metadata      pluginapi.Metadata      `json:"metadata"`
        Capabilities  registrationCapability  `json:"capabilities"`
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

func buildRegistration() registrationPayload {
        return registrationPayload{
                SchemaVersion: pluginabi.SchemaVersion,
                Metadata: pluginapi.Metadata{
                        Name:             providerName,
                        Version:          version,
                        Author:           "mmqz (based on traework2api by Sliverkiss)",
                        GitHubRepository: "https://github.com/mmqz/cpa-multi-plugins",
                        Logo:             pluginLogoURL,
                        ConfigFields: []pluginapi.ConfigField{
                                {Name: "checkin_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable daily auto check-in at 09:00 local time (default true)."},
                                {Name: "token_keepalive", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable daily access-token refresh at 03:00 to prevent session expiry (default true)."},
                                {Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional model list. Each item can have id, name, alias, context, max_tokens, enabled."},
                        },
                },
                Capabilities: registrationCapability{
                        ModelProvider:         true,
                        AuthProvider:          true,
                        Executor:              true,
                        ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
                        ExecutorInputFormats:  []string{"chat-completions"},
                        ExecutorOutputFormats: []string{"chat-completions"},
                        Scheduler:             true,
                        ManagementAPI:         true,
                        UsagePlugin:           true,
                },
        }
}

// -----------------------------------------------------------------------------
// Models
// -----------------------------------------------------------------------------

func handleModelStatic(_ []byte) ([]byte, error) {
        // Return static fallback model list (used when no accounts are loaded).
        return okEnvelope(pluginapi.ModelResponse{
                Provider: providerName,
                Models:   staticModels(),
        })
}

func handleModelForAuth(request []byte) ([]byte, error) {
        var req struct {
                Auth pluginapi.AuthData `json:"auth"`
        }
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        // Try dynamic model fetch via the account's auth.
        a, err := parseStoredAuth(req.Auth.StorageJSON)
        if err != nil {
                // Fall back to static list if we can't parse the auth.
                return okEnvelope(pluginapi.ModelResponse{
                        Provider: providerName,
                        Models:   staticModels(),
                })
        }
        dynamic, err := upstreamClient.FetchModels(a)
        if err != nil {
                log.Printf("model.for_auth %s: %v — falling back to static", a.UID, err)
                return okEnvelope(pluginapi.ModelResponse{
                        Provider: providerName,
                        Models:   staticModels(),
                })
        }
        out := make([]pluginapi.ModelInfo, 0, len(dynamic))
        for _, m := range dynamic {
                out = append(out, pluginapi.ModelInfo{
                        ID:            m.ID,
                        Name:          m.Name,
                        ContextLength: m.ContextWindow,
                        MaxCompletionTokens: m.MaxTokens,
                })
        }
        return okEnvelope(pluginapi.ModelResponse{
                Provider: providerName,
                Models:   out,
        })
}

// staticModels returns the known SOLO CN model list (subset).
func staticModels() []pluginapi.ModelInfo {
        known := []string{
                "glm-5.2", "glm-5.3", "DeepSeek-V4-Pro", "DeepSeek-V4-Flash",
                "kimi-k3", "Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Turbo",
                "claude-sonnet-4-5", "claude-opus-4-1", "gpt-5",
        }
        out := make([]pluginapi.ModelInfo, 0, len(known))
        for _, id := range known {
                out = append(out, pluginapi.ModelInfo{
                        ID:      id,
                        Name:    id,
                        OwnedBy: providerName,
                })
        }
        return out
}

// -----------------------------------------------------------------------------
// Auth: parse / login / refresh
// -----------------------------------------------------------------------------

// parseStoredAuth converts pluginapi.AuthData.StorageJSON (JSON bytes) to
// upstream *auth.Auth. StorageJSON is the nested form:
//   {"auth":{...},"account":{...}}
// or the flat form {"accessToken":...,"uid":...}.
func parseStoredAuth(raw []byte) (*auth.Auth, error) {
        a, err := auth.Parse(raw)
        if err != nil {
                return nil, err
        }
        return a, nil
}

func handleParseAuth(request []byte) ([]byte, error) {
        var req struct {
                StorageJSON json.RawMessage `json:"storage_json"`
                FileName    string          `json:"file_name"`
        }
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        a, err := parseStoredAuth(req.StorageJSON)
        if err != nil {
                return errorEnvelope("parse_error", err.Error()), nil
        }
        // Register the account in the pool.
        accountPool.Add(a)
        return okEnvelope(pluginapi.AuthParseResponse{
                Handled: true,
                Auth: pluginapi.AuthData{
                        Provider:    providerName,
                        ID:          a.UID,
                        FileName:    nonEmpty(req.FileName, authFileName),
                        Label:       nonEmpty(a.Nickname, "Trae SOLO CN "+a.UID),
                        StorageJSON: req.StorageJSON,
                        Metadata:    map[string]any{"type": providerName, "uid": a.UID},
                },
        })
}

// handleStartLogin initiates Trae OAuth via GetLoginGuidance + PKCE.
// Mirrors cockpit-tools trae_oauth.rs:1762-1845 (GetLoginGuidance) and
// :1851-1945 (build_verification_uri).
//
// Flow:
//  1. Generate login_trace_id (UUID v4) and PKCE pair (verifier + challenge).
//  2. Allocate local callback port.
//  3. POST GetLoginGuidance to api.trae.cn (body: {loginTraceID, login_trace_id}).
//  4. Parse LoginHost from response (multiple JSON paths checked).
//  5. Build verification URI: {loginHost}/authorization?... with PKCE challenge.
//  6. Persist login state (state = login_trace_id) and start callback server.
//  7. Return AuthLoginStartResponse{URL: verificationURI, State: loginTraceID}.
func handleStartLogin(_ []byte) ([]byte, error) {
        // Step 1: PKCE + login_trace_id.
        loginTraceID := newLoginTraceID()
        codeVerifier, codeChallenge := generatePKCEPair()

        // Step 2: Allocate local callback port.
        ln, err := netListen("tcp", "127.0.0.1:0")
        if err != nil {
                return nil, fmt.Errorf("allocate callback port: %w", err)
        }
        port := ln.Addr().(*netTCPAddr).Port
        cbURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)

        // Step 3: POST GetLoginGuidance.
        // cockpit-tools sends both camelCase and snake_case keys for max compatibility.
        guidanceBody, _ := json.Marshal(map[string]any{
                "loginTraceID":   loginTraceID,
                "login_trace_id": loginTraceID,
        })
        req, err := http.NewRequest(http.MethodPost, oauthLoginGuidanceURL, bytes.NewReader(guidanceBody))
        if err != nil {
                ln.Close()
                return nil, err
        }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "application/json")
        req.Header.Set("User-Agent", "Trae/"+oauthPluginVersion+" antigravity-cockpit-tools")

        resp, err := http.DefaultClient.Do(req)
        if err != nil {
                ln.Close()
                return nil, fmt.Errorf("GetLoginGuidance failed: %w", err)
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        if resp.StatusCode >= 400 {
                ln.Close()
                return nil, fmt.Errorf("GetLoginGuidance upstream %d: %s", resp.StatusCode, truncate(string(raw), 200))
        }

        // Step 4: Parse LoginHost from response (multiple JSON paths checked).
        loginHost := extractLoginHost(raw)
        if loginHost == "" {
                ln.Close()
                return nil, fmt.Errorf("GetLoginGuidance: missing LoginHost (body=%s)", truncate(string(raw), 200))
        }

        // Step 5: Build verification URI with PKCE challenge.
        deviceID := newDeviceID()
        machineID := newMachineID()
        verificationURI := buildVerificationURI(loginHost, verificationURIParams{
                AuthFrom:      oauthAuthFrom,
                PluginVersion: oauthPluginVersion,
                ClientID:      upstream.ClientID,
                LoginTraceID:  loginTraceID,
                CallbackURL:   cbURL,
                MachineID:     machineID,
                DeviceID:      deviceID,
                DeviceBrand:   oauthDeviceBrand,
                DeviceType:    oauthDeviceType,
                OSVersion:     oauthOSVersion,
                Env:           oauthEnv,
                AppVersion:    upstream.IdeVersion,
                AppType:       oauthAppType,
                CodeChallenge: codeChallenge,
                HideSaasLogin: oauthHideSaasLogin,
        })

        // Step 6: Persist login state (state = login_trace_id).
        state := loginTraceID
        loginStates.Store(state, &loginCtx{
                listener:      ln,
                state:         state,
                cbURL:         cbURL,
                expires:       time.Now().Add(loginTTL),
                loginTraceID:  loginTraceID,
                codeVerifier:  codeVerifier,
                codeChallenge: codeChallenge,
                deviceID:      deviceID,
                machineID:     machineID,
        })

        // Step 7: Start callback server goroutine.
        go acceptCallback(state)

        return okEnvelope(pluginapi.AuthLoginStartResponse{
                Provider:  providerName,
                URL:       verificationURI,
                State:     state,
                ExpiresAt: time.Now().Add(loginTTL).UTC(),
                Metadata: map[string]any{
                        "logo":           pluginLogoURL,
                        "callback_url":   cbURL,
                        "login_trace_id": loginTraceID,
                },
        })
}

// loginCtx holds the local callback listener for one in-flight OAuth flow.
type loginCtx struct {
        listener netListener
        state    string
        cbURL    string
        expires  time.Time

        // Set at handleStartLogin (PKCE + device fingerprint).
        loginTraceID  string
        codeVerifier  string
        codeChallenge string
        deviceID      string
        machineID     string

        // Filled by acceptCallback when the user completes login.
        authCode     string
        refreshToken string
        loginHost    string // for ExchangeToken (from callback or fallback)
        err          error
        done         chan struct{}
}

// acceptCallback accepts the OAuth callback GET /authorize?... and parses
// parameters. Mirrors cockpit-tools trae_oauth.rs:1170-1196 callback handling.
//
// Parameter precedence (per cockpit-tools):
//   - error / error_code / err / errorCode → callback error
//   - isRedirect=false → callback error
//   - refreshToken / refresh_token / RefreshToken / refresh-token → use directly
//   - authCode / auth_code / AuthCode / authorization_code / code → ExchangeToken (auth code)
//   - authCodeInfo / auth_code_info / AuthCodeInfo → extract auth code from JSON
//   - loginHost / login_host / LoginHost / host / consoleHost → for ExchangeToken
func acceptCallback(state string) {
        v, ok := loginStates.Load(state)
        if !ok {
                return
        }
        lc := v.(*loginCtx)
        lc.done = make(chan struct{})
        defer close(lc.done)

        ln := lc.listener
        _ = ln.(*netTCPListener).SetDeadline(time.Now().Add(loginTTL))
        conn, err := ln.Accept()
        if err != nil {
                // Accept failed (deadline exceeded or listener closed). Close the listener
                // so it doesn't leak — janitor will also clean up the loginStates entry.
                lc.err = fmt.Errorf("callback accept: %w", err)
                lc.listener.Close()
                return
        }
        defer conn.Close()
        _ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
        buf := make([]byte, 16384)
        n, _ := conn.Read(buf)
        req := string(buf[:n])
        // Parse the first HTTP request line: "GET /authorize?... HTTP/1.1"
        firstLine := req
        if nl := strings.Index(req, "\r\n"); nl >= 0 {
                firstLine = req[:nl]
        }
        sp := strings.Index(firstLine, " ")
        if sp < 0 {
                lc.err = fmt.Errorf("callback: malformed request line")
                writeCallbackHTML(conn, "Login failed", "malformed request")
                return
        }
        rest := firstLine[sp+1:]
        // Trim trailing HTTP version (e.g. " HTTP/1.1").
        if sp2 := strings.LastIndex(rest, " "); sp2 >= 0 {
                rest = rest[:sp2]
        }
        vals := url.Values{}
        if q := strings.Index(rest, "?"); q >= 0 {
                vals, _ = url.ParseQuery(rest[q+1:])
        }

        // Error path.
        for _, k := range []string{"error", "error_code", "err", "errorCode"} {
                if ev := vals.Get(k); ev != "" {
                        lc.err = fmt.Errorf("oauth callback error: %s=%s", k, ev)
                        writeCallbackHTML(conn, "Login failed", lc.err.Error())
                        return
                }
        }
        if ir := vals.Get("isRedirect"); ir == "false" {
                lc.err = fmt.Errorf("oauth callback: isRedirect=false")
                writeCallbackHTML(conn, "Login failed", "isRedirect=false")
                return
        }

        // loginHost (used for ExchangeToken; falls back to oauthDefaultHost).
        for _, k := range []string{"loginHost", "login_host", "LoginHost", "host", "consoleHost"} {
                if v := vals.Get(k); v != "" {
                        lc.loginHost = v
                        break
                }
        }

        // Refresh token (use directly — skip ExchangeToken auth-code path).
        for _, k := range []string{"refreshToken", "refresh_token", "RefreshToken", "refresh-token"} {
                if v := vals.Get(k); v != "" {
                        lc.refreshToken = v
                        break
                }
        }

        // Auth code (multiple names; first non-empty wins).
        for _, k := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
                if v := vals.Get(k); v != "" {
                        lc.authCode = v
                        break
                }
        }

        // authCodeInfo — extract auth code from JSON payload.
        if lc.authCode == "" {
                for _, k := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
                        if v := vals.Get(k); v != "" {
                                if ac := extractAuthCodeFromAuthCodeInfo(v); ac != "" {
                                        lc.authCode = ac
                                        break
                                }
                        }
                }
        }

        // Final sanity check + browser response.
        switch {
        case lc.err != nil:
                writeCallbackHTML(conn, "Login failed", lc.err.Error())
        case lc.refreshToken != "" || lc.authCode != "":
                writeCallbackHTML(conn, "Login successful", "You can close this window now.")
        default:
                lc.err = fmt.Errorf("oauth callback: no authCode/refreshToken in callback params")
                writeCallbackHTML(conn, "Login failed", "no auth code received")
        }
}

// writeCallbackHTML responds to the browser with a simple HTML page.
func writeCallbackHTML(w io.Writer, title, msg string) {
        body := fmt.Sprintf("<html><body><h2>%s</h2><p>%s</p></body></html>", title, msg)
        fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
                len(body), body)
}

// handlePollLogin polls the callback server for completion, then exchanges
// the auth code (or refresh token) for tokens via ExchangeToken.
//
// Flow:
//  1. Look up loginCtx by state.
//  2. If still pending (lc.done not closed), return AuthLoginStatusPending.
//  3. If callback returned an error, return AuthLoginStatusError.
//  4. If callback returned refreshToken: call ExchangeToken (refresh variant)
//     via upstreamClient.RefreshToken to obtain access token.
//  5. If callback returned authCode: call ExchangeToken with auth-code body
//     ({ClientID, AuthCode, CodeVerifier, DeviceInfo, IDEVersion}).
//  6. Parse token response (access/refresh/expires).
//  7. Call GetUserInfo for UID/nickname/enterpriseID.
//  8. Build storage JSON and return AuthLoginStatusSuccess.
func handlePollLogin(request []byte) ([]byte, error) {
        var req pluginapi.AuthLoginPollRequest
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        state := strings.TrimSpace(req.State)
        if state == "" {
                return nil, fmt.Errorf("poll: empty state")
        }
        v, ok := loginStates.Load(state)
        if !ok {
                return nil, fmt.Errorf("poll: unknown state — please restart login")
        }
        lc := v.(*loginCtx)
        if time.Now().After(lc.expires) {
                loginStates.Delete(state)
                lc.listener.Close()
                return nil, fmt.Errorf("poll: login expired (5 min timeout) — please re-initiate")
        }

        // Wait briefly for the callback to complete (non-blocking).
        select {
        case <-lc.done:
                // Callback received.
        default:
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusPending,
                        Message: "waiting for browser login",
                })
        }

        // fail is a helper that cleans up the login state and returns an error envelope.
        fail := func(msg string) ([]byte, error) {
                loginStates.Delete(state)
                lc.listener.Close()
                raw, _ := okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: msg,
                })
                return raw, nil
        }

        if lc.err != nil {
                return fail(lc.err.Error())
        }

        var (
                accessToken  string
                refreshToken string
                expiresAt    int64
        )

        switch {
        case lc.refreshToken != "":
                // Refresh-token path: use the callback's refresh token to obtain an
                // access token via the refresh ExchangeToken flow (refreshLocked).
                // Equivalent to "直接用 + skip auth-code ExchangeToken".
                a := &auth.Auth{
                        RefreshToken: lc.refreshToken,
                        APIHost:       oauthDefaultHost,
                        Domain:        "trae.cn",
                        MachineID:     lc.machineID,
                        DeviceID:      lc.deviceID,
                }
                if err := upstreamClient.RefreshToken(a); err != nil {
                        // Fallback: treat the callback's refreshToken as access token directly.
                        log.Printf("ExchangeToken(refresh) failed: %v — using refreshToken as accessToken", err)
                        accessToken = lc.refreshToken
                        refreshToken = lc.refreshToken
                } else {
                        accessToken = a.AccessToken
                        refreshToken = a.RefreshToken
                        expiresAt = a.ExpiresAt
                }

        case lc.authCode != "":
                // Auth-code path: call ExchangeToken with AuthCode body.
                host := lc.loginHost
                if host == "" {
                        host = oauthDefaultHost
                }
                // Ensure host has scheme; loginHost may be a bare domain (e.g. "www.trae.cn").
                if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
                        host = "https://" + host
                }
                di := buildOfficialDeviceInfo(
                        lc.deviceID, lc.machineID, oauthPlatformCode, oauthDeviceName,
                        oauthDeviceBrand, upstream.IdeVersion, oauthDeviceType, oauthOSVersion,
                )
                body := map[string]any{
                        "ClientID":     upstream.ClientID,
                        "AuthCode":     lc.authCode,
                        "CodeVerifier": lc.codeVerifier,
                        "DeviceInfo":   di,
                        "IDEVersion":   upstream.IdeVersion,
                }
                bodyBytes, _ := json.Marshal(body)
                tokenReq, err := http.NewRequest(http.MethodPost,
                        host+"/trae/api/v3/oauth/ExchangeToken", bytes.NewReader(bodyBytes))
                if err != nil {
                        return fail(fmt.Sprintf("ExchangeToken request: %v", err))
                }
                tokenReq.Header.Set("Content-Type", "application/json")
                tokenReq.Header.Set("Accept", "application/json")
                tokenReq.Header.Set("User-Agent", "Trae/"+oauthPluginVersion+" antigravity-cockpit-tools")
                tokenReq.Header.Set("x-cloudide-token", "")

                tokenResp, err := http.DefaultClient.Do(tokenReq)
                if err != nil {
                        return fail(fmt.Sprintf("ExchangeToken failed: %v", err))
                }
                defer tokenResp.Body.Close()
                tokenRaw, _ := io.ReadAll(io.LimitReader(tokenResp.Body, 1<<20))
                if tokenResp.StatusCode >= 400 {
                        return fail(fmt.Sprintf("ExchangeToken upstream %d: %s",
                                tokenResp.StatusCode, truncate(string(tokenRaw), 200)))
                }
                // Parse token response (multiple field names supported per cockpit-tools).
                accessToken, refreshToken, expiresAt = parseExchangeTokenResponse(tokenRaw)
                if accessToken == "" && refreshToken == "" {
                        return fail(fmt.Sprintf("ExchangeToken: no token in response (body=%s)",
                                truncate(string(tokenRaw), 200)))
                }
                if accessToken == "" {
                        // Some responses only return a refresh token; use it as access token too.
                        accessToken = refreshToken
                }

        default:
                return fail("login completed but no authCode/refreshToken received — please retry")
        }

        // Build partial Auth and call GetUserInfo for UID.
        a := &auth.Auth{
                AccessToken:  accessToken,
                RefreshToken: refreshToken,
                ExpiresAt:    expiresAt,
                APIHost:      oauthDefaultHost,
                Domain:       "trae.cn",
                MachineID:    lc.machineID,
                DeviceID:     lc.deviceID,
        }
        uid, nickname, entID, err := upstreamClient.GetUserInfo(a)
        if err != nil {
                log.Printf("GetUserInfo failed: %v — proceeding with empty UID", err)
        }
        a.UID = uid
        a.Nickname = nickname
        a.EnterpriseID = entID

        // Persist the auth file (nested form: {type, provider, auth:{...}, account:{...}}).
        // CRITICAL: include type+provider so CPA can route this auth to the correct plugin.
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "type":     providerName,
                "provider": providerName,
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
                "disabled": false,
        }, "", "  ")

        // Register in pool.
        accountPool.Add(a)

        // Cleanup login state.
        loginStates.Delete(state)
        lc.listener.Close()

        return okEnvelope(pluginapi.AuthLoginPollResponse{
                Status:  pluginapi.AuthLoginStatusSuccess,
                Message: fmt.Sprintf("login complete (uid=%s)", a.UID),
                Auth: pluginapi.AuthData{
                        Provider:    providerName,
                        ID:          a.UID,
                        FileName:    fmt.Sprintf("%s-%s.json", providerName, a.UID),
                        Label:       nonEmpty(a.Nickname, "Trae SOLO CN "+a.UID),
                        StorageJSON: storageJSON,
                        Metadata:    map[string]any{"type": providerName, "uid": a.UID, "nickname": a.Nickname},
                },
        })
}

// -----------------------------------------------------------------------------
// OAuth helpers (cockpit-tools aligned)
// -----------------------------------------------------------------------------

// verificationURIParams carries the parameters for buildVerificationURI.
type verificationURIParams struct {
        AuthFrom      string // "solo" or "trae"
        PluginVersion string // e.g. "1.0.0"
        ClientID      string // SOLO: en1oxy7wnw8j9n; non-SOLO: ono9krqynydwx5
        LoginTraceID  string
        CallbackURL   string // auth_callback_url (NOT URL-encoded per cockpit-tools)
        MachineID     string
        DeviceID      string
        DeviceBrand   string // e.g. "83DG"
        DeviceType    string // e.g. "windows"
        OSVersion     string // e.g. "Windows 11 Pro"
        Env           string // e.g. "prod"
        AppVersion    string // e.g. "0.1.43" (= upstream.IdeVersion)
        AppType       string // e.g. "trae"
        CodeChallenge string // PKCE challenge (base64url no-pad)
        HideSaasLogin bool   // SOLO only; non-SOLO omits this param
}

// buildVerificationURI builds the user-facing OAuth login URL.
// Mirrors cockpit-tools build_verification_uri (trae_oauth.rs:1851-1945).
// Layout: {loginHost}/authorization?{query}
// Query parameter order is significant (cockpit-tools uses Vec<(K,V)> preserved order).
func buildVerificationURI(loginHost string, p verificationURIParams) string {
        type kv struct {
                k, v   string
                encode bool
        }
        params := []kv{
                {"login_version", "1", false},
                {"auth_from", p.AuthFrom, false},
                {"login_channel", "native_ide", false},
                {"plugin_version", p.PluginVersion, true},
                {"auth_type", "local", false},
                {"client_id", p.ClientID, false},
                {"redirect", "0", false},
                {"login_trace_id", p.LoginTraceID, true},
                {"auth_callback_url", p.CallbackURL, false}, // NOT encoded per cockpit-tools
                {"machine_id", p.MachineID, true},
                {"device_id", p.DeviceID, true},
                {"x_device_id", p.DeviceID, true},
                {"x_machine_id", p.MachineID, true},
                {"x_device_brand", p.DeviceBrand, true},
                {"x_device_type", p.DeviceType, true},
                {"x_os_version", p.OSVersion, true},
                {"x_env", p.Env, true},
                {"x_app_version", p.AppVersion, true},
                {"x_app_type", p.AppType, true},
                {"code_challenge", p.CodeChallenge, true},
                {"code_challenge_method", "S256", false},
        }
        if p.HideSaasLogin {
                params = append(params, kv{"hide_saas_login", "true", false})
        }
        parts := make([]string, 0, len(params))
        for _, e := range params {
                v := e.v
                if e.encode {
                        v = urlEncode(v)
                }
                parts = append(parts, e.k+"="+v)
        }
        return strings.TrimRight(loginHost, "/") + "/authorization?" + strings.Join(parts, "&")
}

// deviceInfo is the DeviceInfo struct sent in the ExchangeToken request body.
// Mirrors cockpit-tools build_official_device_info (trae_oauth.rs:2087-2118).
type deviceInfo struct {
        DeviceID        string `json:"DeviceID"`
        MachineID       string `json:"MachineID"`
        PlatformCode    string `json:"PlatformCode"`    // SOLO_PC | IDE_PC
        DeviceType      string `json:"DeviceType"`      // "PC"
        DeviceName      string `json:"DeviceName"`
        DeviceModel     string `json:"DeviceModel"`     // = DeviceBrand
        ClientVersion   string `json:"ClientVersion"`  // = AppVersion
        DevicePublicKey string `json:"DevicePublicKey"` // empty for now (no device key pair)
        DeviceBrand     string `json:"DeviceBrand"`
        DeviceCPU       string `json:"DeviceCPU"`
        OSInfo          string `json:"OSInfo"` // = DeviceType (e.g. "windows")
        OSVersion       string `json:"OSVersion"`
}

// buildOfficialDeviceInfo builds the DeviceInfo for ExchangeToken.
// Note: DevicePublicKey is sent as empty string for now (matches traework2api's
// behavior — no device key pair). If upstream starts rejecting, generate an
// ECDSA P-256 key pair and send the PEM-encoded public key here.
func buildOfficialDeviceInfo(deviceID, machineID, platformCode, deviceName, deviceBrand, appVersion, deviceType, osVersion string) deviceInfo {
        return deviceInfo{
                DeviceID:        deviceID,
                MachineID:       machineID,
                PlatformCode:    platformCode,
                DeviceType:      "PC",
                DeviceName:      deviceName,
                DeviceModel:     deviceBrand,
                ClientVersion:   appVersion,
                DevicePublicKey: "",
                DeviceBrand:     deviceBrand,
                DeviceCPU:       "",
                OSInfo:          deviceType,
                OSVersion:       osVersion,
        }
}

// extractLoginHost mirrors cockpit-tools extract_login_guidance_host:
// checks multiple JSON paths (Result.LoginHost / Result.loginHost / Result.LoginURL /
// result.* / data.Result.* / data.* / top-level) and returns the first non-empty value.
func extractLoginHost(raw []byte) string {
        var top map[string]any
        if err := json.Unmarshal(raw, &top); err != nil {
                return ""
        }
        candidates := []string{"LoginHost", "loginHost", "LoginURL", "loginUrl", "login_url"}

        // Top-level.
        for _, k := range candidates {
                if s := jsonString(top[k]); s != "" {
                        return s
                }
        }

        // Result.* / result.*
        for _, rk := range []string{"Result", "result"} {
                if sub, ok := top[rk].(map[string]any); ok {
                        for _, k := range candidates {
                                if s := jsonString(sub[k]); s != "" {
                                        return s
                                }
                        }
                }
        }

        // data.* and data.Result.* / data.result.*
        if data, ok := top["data"].(map[string]any); ok {
                for _, k := range candidates {
                        if s := jsonString(data[k]); s != "" {
                                return s
                        }
                }
                for _, rk := range []string{"Result", "result"} {
                        if sub, ok := data[rk].(map[string]any); ok {
                                for _, k := range candidates {
                                        if s := jsonString(sub[k]); s != "" {
                                                return s
                                        }
                                }
                        }
                }
        }
        return ""
}

// jsonString returns v as a string if it is a JSON string; empty otherwise.
func jsonString(v any) string {
        if s, ok := v.(string); ok {
                return s
        }
        return ""
}

// extractAuthCodeFromAuthCodeInfo mirrors cockpit-tools extract_auth_code_from_auth_code_info.
// The input may be JSON-encoded or URL-encoded JSON. Checks multiple keys:
// authCode / auth_code / AuthCode / authorization_code / code.
func extractAuthCodeFromAuthCodeInfo(raw string) string {
        candidates := []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"}
        parse := func(s string) map[string]any {
                var m map[string]any
                if json.Unmarshal([]byte(s), &m) == nil {
                        return m
                }
                return nil
        }
        m := parse(raw)
        if m == nil {
                if decoded, err := url.QueryUnescape(raw); err == nil {
                        m = parse(decoded)
                }
        }
        if m == nil {
                return ""
        }
        for _, k := range candidates {
                if s := jsonString(m[k]); s != "" {
                        return s
                }
        }
        return ""
}

// parseExchangeTokenResponse extracts access/refresh tokens + expiresAt from an
// ExchangeToken response. Mirrors cockpit-tools apply_exchange_token_response
// which checks Result.{AccessToken,accessToken,Token,token} and
// Result.{RefreshToken,refreshToken} (multiple field names supported).
func parseExchangeTokenResponse(raw []byte) (accessToken, refreshToken string, expiresAt int64) {
        var env struct {
                Result map[string]any `json:"Result"`
        }
        if err := json.Unmarshal(raw, &env); err != nil || env.Result == nil {
                return "", "", 0
        }
        r := env.Result
        // Access token candidates (in priority order).
        for _, k := range []string{"AccessToken", "accessToken", "Token", "token"} {
                if s := jsonString(r[k]); s != "" {
                        accessToken = s
                        break
                }
        }
        // Refresh token candidates.
        for _, k := range []string{"RefreshToken", "refreshToken"} {
                if s := jsonString(r[k]); s != "" {
                        refreshToken = s
                        break
                }
        }
        // ExpiresAt: prefer TokenExpireAt (millis → seconds); fall back to duration.
        if n, ok := toInt64(r["TokenExpireAt"]); ok && n > 0 {
                expiresAt = normalizeExpiresAt(n)
        }
        if expiresAt == 0 {
                if n, ok := toInt64(r["TokenExpireDuration"]); ok && n > 0 {
                        expiresAt = time.Now().Add(time.Duration(n) * time.Second).Unix()
                }
        }
        return accessToken, refreshToken, expiresAt
}

// toInt64 converts a JSON number (typically float64 from encoding/json) to int64.
func toInt64(v any) (int64, bool) {
        switch n := v.(type) {
        case float64:
                return int64(n), true
        case int64:
                return n, true
        case int:
                return int64(n), true
        case json.Number:
                if i, err := n.Int64(); err == nil {
                        return i, true
                }
        }
        return 0, false
}

func handleRefreshAuth(request []byte) ([]byte, error) {
        var req pluginapi.AuthRefreshRequest
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        a, err := parseStoredAuth(req.StorageJSON)
        if err != nil {
                return nil, fmt.Errorf("refresh: parse auth: %w", err)
        }
        if err := upstreamClient.RefreshToken(a); err != nil {
                return nil, fmt.Errorf("refresh: ExchangeToken: %w", err)
        }
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "type":     providerName,
                "provider": providerName,
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
                "disabled": false,
        }, "", "  ")
        return okEnvelope(pluginapi.AuthRefreshResponse{
                Auth: pluginapi.AuthData{
                        Provider:    providerName,
                        ID:          a.UID,
                        FileName:    fmt.Sprintf("%s-%s.json", providerName, a.UID),
                        Label:       nonEmpty(a.Nickname, "Trae SOLO CN "+a.UID),
                        StorageJSON: storageJSON,
                        Metadata:    map[string]any{"type": providerName, "uid": a.UID},
                },
                NextRefreshAfter: time.Now().Add(12 * time.Hour).UTC(),
        })
}

// -----------------------------------------------------------------------------
// Executor: execute + execute_stream
// -----------------------------------------------------------------------------

func handleExecExecute(request []byte) ([]byte, error) {
        var req pluginapi.ExecutorRequest
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        a, err := parseStoredAuth(req.StorageJSON)
        if err != nil {
                return nil, fmt.Errorf("execute: parse auth: %w", err)
        }
        // Refresh token if needed (within 24h of expiry).
        refreshed, err := upstreamClient.RefreshTokenIfNeeded(a, defaultRefreshSkew)
        if err != nil {
                return nil, fmt.Errorf("execute: refresh: %w", err)
        }
        if refreshed {
                // Persist the new token back to host auth store so it survives CPA restart.
                persistRefreshedAuth(req, a)
        }

        // Call ChatStream (always stream upstream; aggregate for non-stream).
        rc, status, body, err := upstreamClient.ChatStream(a, req.Payload)
        if err != nil {
                return nil, fmt.Errorf("execute: chat stream: %w", err)
        }
        if rc == nil {
                // Non-2xx: classify and cool the account.
                kind := upstream.Classify(status, string(body))
                applyCooldown(a.UID, kind)
                return nil, fmt.Errorf("upstream %d (%s): %s", status, kind, truncate(string(body), 200))
        }
        defer rc.Close()

        completion, err := upstream.Aggregate(rc)
        if err != nil {
                if se, ok := err.(*upstream.SOLOStreamError); ok {
                        applyCooldown(a.UID, se.Kind())
                }
                return nil, fmt.Errorf("aggregate: %w", err)
        }
        accountPool.NoteSuccess(a.UID)
        out, _ := json.Marshal(completion)
        return okEnvelope(pluginapi.ExecutorResponse{Payload: out})
}

func handleExecStream(request []byte) ([]byte, error) {
        var req pluginapi.ExecutorRequest
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        a, err := parseStoredAuth(req.StorageJSON)
        if err != nil {
                return nil, fmt.Errorf("stream: parse auth: %w", err)
        }
        refreshed, err := upstreamClient.RefreshTokenIfNeeded(a, defaultRefreshSkew)
        if err != nil {
                return nil, fmt.Errorf("stream: refresh: %w", err)
        }
        if refreshed {
                persistRefreshedAuth(req, a)
        }

        rc, status, body, err := upstreamClient.ChatStream(a, req.Payload)
        if err != nil {
                return nil, fmt.Errorf("stream: chat stream: %w", err)
        }
        if rc == nil {
                kind := upstream.Classify(status, string(body))
                applyCooldown(a.UID, kind)
                return nil, fmt.Errorf("upstream %d (%s): %s", status, kind, truncate(string(body), 200))
        }
        defer rc.Close()

        // Real-time SSE conversion: SOLO SSE → OpenAI SSE chunks via channel.
        // Each chunk is a complete "data: {...}\n\n" frame, forwarded to CPA as-is.
        model := ""
        if len(req.Payload) > 0 {
                var peek struct {
                        Model string `json:"model"`
                }
                _ = json.Unmarshal(req.Payload, &peek)
                model = peek.Model
        }
        ch := convertSOLOStreamToOpenAI(rc, model, func(se *upstream.SOLOStreamError) {
                applyCooldown(a.UID, se.Kind())
        })

        // Collect chunks into a slice for the response (CPA expects []ExecutorStreamChunk).
        var chunks []pluginapi.ExecutorStreamChunk
        for chunk := range ch {
                chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: chunk})
        }
        accountPool.NoteSuccess(a.UID)

        return okEnvelope(pluginapi.ExecutorStreamResponse{
                Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
                Chunks:  toChunkChannel(chunks),
        })
}

// -----------------------------------------------------------------------------
// Cooldown / lifecycle
// -----------------------------------------------------------------------------

func applyCooldown(uid string, kind upstream.ErrKind) {
        switch kind {
        case upstream.ErrPlanLimit:
                accountPool.Cooldown(uid, pool.CoolPlan, 12*time.Hour, "plan limit (1005)")
        case upstream.ErrSoftRate:
                accountPool.Cooldown(uid, pool.CoolSoft, 60*time.Second, "soft rate limit (429)")
        case upstream.ErrSessionDead:
                accountPool.Disable(uid, "session dead (401)")
        case upstream.ErrNotFound:
                accountPool.Cooldown(uid, pool.CoolSoft, 60*time.Second, "not found (404)")
        case upstream.ErrServer, upstream.ErrClient:
                accountPool.NoteError(uid, 3, 10*time.Minute)
        }
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func okEnvelope(result any) ([]byte, error) {
        raw, err := json.Marshal(result)
        if err != nil {
                return nil, err
        }
        return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
        raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
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

func truncate(s string, n int) string {
        s = strings.TrimSpace(s)
        if len(s) > n {
                return s[:n]
        }
        return s
}

func nonEmpty(s, fallback string) string {
        if strings.TrimSpace(s) != "" {
                return s
        }
        return fallback
}

func normalizeExpiresAt(v int64) int64 {
        if v > 1e12 {
                return v / 1000
        }
        return v
}

// newDeviceID / newMachineID generate random-looking device identifiers
// for OAuth flows. These do not need to be cryptographically strong; they
// only need to look like real device IDs to the upstream.
func newDeviceID() string {
        return randomHex(16)
}
func newMachineID() string {
        return randomHex(16)
}
