// Package main implements the trae-cn CLIProxyAPI dynamic plugin.
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

        "github.com/mmqz/cpa-multi-plugins/plugins/trae-cn/auth"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-cn/pool"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-cn/scheduler"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-cn/upstream"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
        providerName  = "trae-cn"
        authFileName  = "trae-cn.json"
        pluginLogoURL = "https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/Trae.png"

        // OAuth login flow timeout (5 min).
        loginTTL = 5 * time.Minute

        // Scheduler defaults.
        defaultCheckinHour = 9
        defaultRefreshSkew = 24 * time.Hour

        // Account cache TTL for credits/checkin status.
        accountCacheTTL = 5 * time.Minute
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
        AuthProvider          bool                         `json:"auth_provider"`
        Executor              bool                         `json:"executor"`
        ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
        ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
        ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
        Scheduler             bool                         `json:"scheduler"`
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
                        AuthProvider:          true,
                        Executor:              true,
                        ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
                        ExecutorInputFormats:  []string{"chat-completions"},
                        ExecutorOutputFormats: []string{"chat-completions"},
                        Scheduler:             true,
                        UsagePlugin:           true,
                },
        }
}

// -----------------------------------------------------------------------------
// Models
// -----------------------------------------------------------------------------

func handleModelStatic(_ []byte) ([]byte, error) {
        // Return static fallback model list (used when no accounts are loaded).
        models := staticModels()
        return okEnvelope(map[string]any{"models": models})
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
                return okEnvelope(map[string]any{"models": staticModels()})
        }
        dynamic, err := upstreamClient.FetchModels(a)
        if err != nil {
                log.Printf("model.for_auth %s: %v — falling back to static", a.UID, err)
                return okEnvelope(map[string]any{"models": staticModels()})
        }
        out := make([]pluginapi.ModelInfo, 0, len(dynamic))
        for _, m := range dynamic {
                out = append(out, pluginapi.ModelInfo{
                        ID:           m.ID,
                        Name:         m.Name,
                        ContextLength: m.ContextWindow,
                        MaxCompletionTokens: m.MaxTokens,
                })
        }
        return okEnvelope(map[string]any{"models": out})
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
                out = append(out, pluginapi.ModelInfo{ID: id, Name: id})
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

// handleStartLogin initiates Trae OAuth via GetLoginGuidance.
// We spin up a local HTTP listener on a random port to receive the authCode
// callback, then poll the listener from handlePollLogin.
func handleStartLogin(_ []byte) ([]byte, error) {
        // Allocate a random local port for the OAuth callback.
        ln, err := netListen("tcp", "127.0.0.1:0")
        if err != nil {
                return nil, fmt.Errorf("allocate callback port: %w", err)
        }
        port := ln.Addr().(*netTCPAddr).Port

        // Build the GetLoginGuidance request to api.trae.cn.
        guidanceURL := upstream.OAuthHost + "/cloudide/api/v3/trae/GetLoginGuidance"
        cbURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)
        body := map[string]any{
                "ClientID":         upstream.ClientID,
                "RedirectUri":      cbURL,
                "State":            "", // server-generated
                "CodeChallenge":    "",
                "CodeVerifier":     "",
                "DeviceInfo":       map[string]any{"DeviceId": newDeviceID(), "MachineId": newMachineID()},
                "IDEVersion":       upstream.IdeVersion,
        }
        bodyBytes, _ := json.Marshal(body)
        req, err := http.NewRequest(http.MethodPost, guidanceURL, bytes.NewReader(bodyBytes))
        if err != nil {
                ln.Close()
                return nil, err
        }
        upstream.OAuthHeaders(req)
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
        var env struct {
                Result struct {
                        LoginURL string `json:"LoginUrl"`
                        State    string `json:"State"`
                } `json:"Result"`
        }
        if err := json.Unmarshal(raw, &env); err != nil {
                ln.Close()
                return nil, fmt.Errorf("parse GetLoginGuidance: %w", err)
        }
        if env.Result.LoginURL == "" || env.Result.State == "" {
                ln.Close()
                return nil, fmt.Errorf("GetLoginGuidance: missing LoginURL or State")
        }

        // State to track this login flow.
        state := env.Result.State
        loginStates.Store(state, &loginCtx{
                listener: ln,
                state:    state,
                cbURL:    cbURL,
                expires:  time.Now().Add(loginTTL),
        })

        // Start a goroutine to accept the OAuth callback.
        go acceptCallback(state)

        return okEnvelope(pluginapi.AuthLoginStartResponse{
                Provider:  providerName,
                URL:       env.Result.LoginURL,
                State:     state,
                ExpiresAt: time.Now().Add(loginTTL).UTC(),
                Metadata:  map[string]any{"logo": pluginLogoURL, "callback_url": cbURL},
        })
}

// loginCtx holds the local callback listener for one in-flight OAuth flow.
type loginCtx struct {
        listener netListener
        state    string
        cbURL    string
        expires  time.Time

        // Filled by acceptCallback when the user completes login.
        authCode    string
        codeVerifier string
        userJWT     string
        err         error
        done        chan struct{}
}

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
                lc.err = fmt.Errorf("callback accept: %w", err)
                return
        }
        defer conn.Close()
        _ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
        buf := make([]byte, 8192)
        n, _ := conn.Read(buf)
        req := string(buf[:n])
        // Parse "GET /authorize?... HTTP/1.1"
        idx := strings.Index(req, " ")
        if idx < 0 {
                lc.err = fmt.Errorf("callback: malformed request")
                return
        }
        path := req[:idx]
        if q := strings.Index(path, "?"); q >= 0 {
                query := path[q+1:]
                vals, _ := url.ParseQuery(query)
                lc.authCode = vals.Get("authCode")
                lc.codeVerifier = vals.Get("codeVerifier")
                lc.userJWT = vals.Get("userJwt")
        }
        // Respond with a success page so the browser shows something useful.
        body := "<html><body><h2>Login successful</h2><p>You can close this window now.</p></body></html>"
        fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
                len(body), body)
}

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

        if lc.err != nil {
                loginStates.Delete(state)
                lc.listener.Close()
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: lc.err.Error(),
                })
        }
        if lc.authCode == "" {
                // User completed login but no authCode came back (e.g., denied).
                loginStates.Delete(state)
                lc.listener.Close()
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: "login completed but no authCode received — please retry",
                })
        }

        // Exchange authCode for tokens via ExchangeToken.
        tokenBody := map[string]any{
                "ClientID":     upstream.ClientID,
                "AuthCode":     lc.authCode,
                "CodeVerifier": lc.codeVerifier,
                "ClientSecret": "-",
                "UserID":       "",
                "DeviceInfo":   map[string]any{"DeviceId": newDeviceID(), "MachineId": newMachineID()},
                "IDEVersion":   upstream.IdeVersion,
        }
        tokenBytes, _ := json.Marshal(tokenBody)
        tokenReq, _ := http.NewRequest(http.MethodPost,
                upstream.OAuthHost+"/trae/api/v3/oauth/ExchangeToken",
                bytes.NewReader(tokenBytes))
        upstream.OAuthHeaders(tokenReq)
        tokenResp, err := http.DefaultClient.Do(tokenReq)
        if err != nil {
                loginStates.Delete(state)
                lc.listener.Close()
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: fmt.Sprintf("ExchangeToken failed: %v", err),
                })
        }
        defer tokenResp.Body.Close()
        tokenRaw, _ := io.ReadAll(io.LimitReader(tokenResp.Body, 1<<20))
        if tokenResp.StatusCode >= 400 {
                loginStates.Delete(state)
                lc.listener.Close()
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: fmt.Sprintf("ExchangeToken upstream %d: %s", tokenResp.StatusCode, truncate(string(tokenRaw), 200)),
                })
        }
        var tokenEnv struct {
                Result struct {
                        Token                string `json:"Token"`
                        TokenExpireAt        int64  `json:"TokenExpireAt"`
                        TokenExpireDuration  int64  `json:"TokenExpireDuration"`
                        RefreshToken         string `json:"RefreshToken"`
                        RefreshExpireAt      int64  `json:"RefreshExpireAt"`
                } `json:"Result"`
        }
        if err := json.Unmarshal(tokenRaw, &tokenEnv); err != nil {
                loginStates.Delete(state)
                lc.listener.Close()
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: fmt.Sprintf("parse ExchangeToken: %v", err),
                })
        }

        // Build a partial Auth and call GetUserInfo to get UID.
        a := &auth.Auth{
                AccessToken:  tokenEnv.Result.Token,
                RefreshToken: tokenEnv.Result.RefreshToken,
                ExpiresAt:    normalizeExpiresAt(tokenEnv.Result.TokenExpireAt),
                ApiHost:      upstream.OAuthHost,
                Domain:       "trae.cn",
                MachineID:    newMachineID(),
                DeviceID:     newDeviceID(),
        }
        uid, nickname, entID, err := upstreamClient.GetUserInfo(a)
        if err != nil {
                log.Printf("GetUserInfo failed: %v — proceeding with empty UID", err)
        }
        a.UID = uid
        a.Nickname = nickname
        a.EnterpriseID = entID

        // Persist the auth file.
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.ApiHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
        }, "", "  ")

        // Register in pool.
        accountPool.Add(a)

        // Cleanup login state.
        loginStates.Delete(state)
        lc.listener.Close()

        return okEnvelope(pluginapi.AuthLoginPollResponse{
                Status: pluginapi.AuthLoginStatusSuccess,
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
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.ApiHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
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
        _ = refreshed // host re-persists on next refresh RPC

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
        _ = refreshed

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

        // Aggregate SOLO SSE → OpenAI completion, then emit it as a single chunk.
        // This is simpler and more robust than translating SSE in real-time (which
        // would require a fake http.ResponseWriter). The host receives one chunk
        // and forwards it to the client; if the client requested stream=true, the
        // host will stream the bytes through.
        completion, err := upstream.Aggregate(rc)
        if err != nil {
                if se, ok := err.(*upstream.SOLOStreamError); ok {
                        applyCooldown(a.UID, se.Kind())
                }
                return nil, fmt.Errorf("aggregate: %w", err)
        }
        accountPool.NoteSuccess(a.UID)
        out, _ := json.Marshal(completion)
        ch := make(chan pluginapi.ExecutorStreamChunk, 1)
        ch <- pluginapi.ExecutorStreamChunk{Payload: out}
        close(ch)
        return okEnvelope(pluginapi.ExecutorStreamResponse{
                Headers: http.Header{"Content-Type": []string{"application/json"}},
                Chunks:  ch,
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
