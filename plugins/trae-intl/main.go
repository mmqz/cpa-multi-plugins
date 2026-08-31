// Package main implements the trae-intl CLIProxyAPI dynamic plugin.
//
// trae-intl wraps Trae Intl (api.marscode.com + core-normal.trae.ai) as a
// cliproxy provider: Trae OAuth (GetLoginGuidance + AuthCode ExchangeToken),
// Cloud-IDE-JWT auth, Web SOLO remote chat_sessions + events SSE protocol,
// v1 credit API. Intl has no daily check-in.
//
// Protocol layer based on OmniRoute/open-sse/executors/trae.ts (MIT, by
// diegosouzapw). Translated from TypeScript to Go.
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
        "net"
        "net/http"
        "net/url"
        "strings"
        "sync"
        "time"
        "unsafe"

        "github.com/mmqz/cpa-multi-plugins/plugins/trae-intl/upstream"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
        providerName  = "trae-intl"
        authFileName  = "trae-intl.json"
        pluginLogoURL = ""

        loginTTL = 5 * time.Minute

        // OAuth constants (Intl uses api.marscode.com, not api.trae.cn)
        oauthLoginGuidanceURL = "https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance"
        oauthDefaultHost      = "https://api.marscode.com"
        oauthAuthFrom         = "trae" // Intl non-SOLO uses "trae"
        oauthPluginVersion    = "1.0.0"
        oauthDeviceBrand      = "83DG"
        oauthDeviceType       = "windows"
        oauthOSVersion        = "Windows 11 Pro"
        oauthEnv              = "prod"
        oauthAppVersion       = "3.5.66"
        oauthAppType          = "trae"
        oauthPlatformCode     = "IDE_PC"
        oauthDeviceName       = "DESKTOP-CPAINTL"
)

var version = "0.1.0"

var (
        hostAPI *C.cliproxy_host_api

        loginStates sync.Map

        upstreamClient *upstream.Client
)

func main() {}

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

        upstreamClient = upstream.New()

        // Janitor: sweep abandoned login states every minute to prevent listener leaks.
        go func() {
                ticker := time.NewTicker(time.Minute)
                defer ticker.Stop()
                for range ticker.C {
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
func cliproxyPluginShutdown() {}

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
        Executor              bool                         `json:"executor"`
        ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
        ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
        ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
        ManagementAPI         bool                         `json:"management_api"`
}

func buildRegistration() registrationPayload {
        return registrationPayload{
                SchemaVersion: pluginabi.SchemaVersion,
                Metadata: pluginapi.Metadata{
                        Name:             providerName,
                        Version:          version,
                        Author:           "mmqz (based on OmniRoute trae.ts by diegosouzapw)",
                        GitHubRepository: "https://github.com/mmqz/cpa-multi-plugins",
                        Logo:             pluginLogoURL,
                        ConfigFields: []pluginapi.ConfigField{
                                {Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional model list. Use 'auto' for server-pick, 'work' for fast work mode, or specific model names (gpt-5.2, gemini-3.1-pro, kimi-k2.5, etc)."},
                        },
                },
                Capabilities: registrationCapability{
                        ModelProvider:         true,
                        AuthProvider:          true,
                        Executor:              true,
                        ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
                        ExecutorInputFormats:  []string{"chat-completions"},
                        ExecutorOutputFormats: []string{"chat-completions"},
                        ManagementAPI:         true,
                },
        }
}

// -----------------------------------------------------------------------------
// Models
// -----------------------------------------------------------------------------

func handleModelStatic(_ []byte) ([]byte, error) {
        return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: staticModels()})
}

func handleModelForAuth(request []byte) ([]byte, error) {
        var req struct {
                Auth pluginapi.AuthData `json:"auth"`
        }
        if err := json.Unmarshal(request, &req); err != nil {
                return nil, err
        }
        a, err := parseStoredAuth(req.Auth.StorageJSON)
        if err != nil {
                return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: staticModels()})
        }
        dynamic, err := upstreamClient.FetchModels(a)
        if err != nil {
                log.Printf("model.for_auth %s: %v — falling back to static", a.UID, err)
                return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: staticModels()})
        }
        out := make([]pluginapi.ModelInfo, 0, len(dynamic))
        for _, id := range dynamic {
                out = append(out, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: providerName})
        }
        // Always include "auto" and "work" as virtual models.
        out = append(out,
                pluginapi.ModelInfo{ID: "auto", Name: "auto (server pick)"},
                pluginapi.ModelInfo{ID: "work", Name: "work (fast mode)"},
        )
        return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: out})
}

func staticModels() []pluginapi.ModelInfo {
        known := []string{"auto", "work", "gpt-5.2", "gemini-3.1-pro", "kimi-k2.5", "claude-sonnet-4-5"}
        out := make([]pluginapi.ModelInfo, 0, len(known))
        for _, id := range known {
                out = append(out, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: providerName})
        }
        return out
}

// -----------------------------------------------------------------------------
// Auth: parse / login / refresh
// -----------------------------------------------------------------------------

func parseStoredAuth(raw []byte) (*upstream.Auth, error) {
        // Support both nested ({"auth":{...},"account":{...}}) and flat shapes.
        var probe map[string]json.RawMessage
        if err := json.Unmarshal(raw, &probe); err != nil {
                return nil, fmt.Errorf("parse auth: %w", err)
        }
        var nested struct {
                Auth struct {
                        AccessToken  string `json:"accessToken"`
                        RefreshToken string `json:"refreshToken"`
                        ExpiresAt    int64  `json:"expiresAt"`
                        Domain       string `json:"domain"`
                        APIHost string `json:"apiHost"`
                        WebID        string `json:"webId"`
                        BizUserID    string `json:"bizUserId"`
                        UserUniqueID string `json:"userUniqueId"`
                        UserIdentity string `json:"userIdentity"`
                        Scope        string `json:"scope"`
                        Tenant       string `json:"tenant"`
                        Region       string `json:"region"`
                        AppLanguage  string `json:"appLanguage"`
                        AppVersion   string `json:"appVersion"`
                } `json:"auth"`
                Account struct {
                        UID          string `json:"uid"`
                        EnterpriseID string `json:"enterpriseId"`
                        Nickname     string `json:"nickname"`
                } `json:"account"`
        }
        var flat struct {
                AccessToken  string `json:"accessToken"`
                RefreshToken string `json:"refreshToken"`
                ExpiresAt    int64  `json:"expiresAt"`
                Domain       string `json:"domain"`
                APIHost string `json:"apiHost"`
                UID          string `json:"uid"`
                EnterpriseID string `json:"enterpriseId"`
                Nickname     string `json:"nickname"`
                WebID        string `json:"webId"`
                BizUserID    string `json:"bizUserId"`
                UserUniqueID string `json:"userUniqueId"`
                UserIdentity string `json:"userIdentity"`
                Scope        string `json:"scope"`
                Tenant       string `json:"tenant"`
                Region       string `json:"region"`
                AppLanguage  string `json:"appLanguage"`
                AppVersion   string `json:"appVersion"`
        }
        if _, ok := probe["auth"]; ok {
                if err := json.Unmarshal(raw, &nested); err != nil {
                        return nil, fmt.Errorf("parse nested auth: %w", err)
                }
                return &upstream.Auth{
                        AccessToken:   nested.Auth.AccessToken,
                        RefreshToken:  nested.Auth.RefreshToken,
                        ExpiresAt:     nested.Auth.ExpiresAt,
                        APIHost:       nested.Auth.APIHost,
                        Domain:        nonEmpty(nested.Auth.Domain, "trae.ai"),
                        UID:           nested.Account.UID,
                        EnterpriseID:  nested.Account.EnterpriseID,
                        Nickname:      nested.Account.Nickname,
                        WebID:         nested.Auth.WebID,
                        BizUserID:     nested.Auth.BizUserID,
                        UserUniqueID:  nested.Auth.UserUniqueID,
                        UserIdentity:  nested.Auth.UserIdentity,
                        Scope:         nested.Auth.Scope,
                        Tenant:        nested.Auth.Tenant,
                        Region:        nested.Auth.Region,
                        AppLanguage:   nested.Auth.AppLanguage,
                        AppVersion:    nested.Auth.AppVersion,
                }, nil
        }
        if err := json.Unmarshal(raw, &flat); err != nil {
                return nil, fmt.Errorf("parse flat auth: %w", err)
        }
        return &upstream.Auth{
                AccessToken:   flat.AccessToken,
                RefreshToken:  flat.RefreshToken,
                ExpiresAt:     flat.ExpiresAt,
                APIHost:       flat.APIHost,
                Domain:        nonEmpty(flat.Domain, "trae.ai"),
                UID:           flat.UID,
                EnterpriseID:  flat.EnterpriseID,
                Nickname:      flat.Nickname,
                WebID:         flat.WebID,
                BizUserID:     flat.BizUserID,
                UserUniqueID:  flat.UserUniqueID,
                UserIdentity:  flat.UserIdentity,
                Scope:         flat.Scope,
                Tenant:        flat.Tenant,
                Region:        flat.Region,
                AppLanguage:   flat.AppLanguage,
                AppVersion:    flat.AppVersion,
        }, nil
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
        return okEnvelope(pluginapi.AuthParseResponse{
                Handled: true,
                Auth: pluginapi.AuthData{
                        Provider:    providerName,
                        ID:          a.UID,
                        FileName:    nonEmpty(req.FileName, authFileName),
                        Label:       nonEmpty(a.Nickname, "Trae Intl "+a.UID),
                        StorageJSON: req.StorageJSON,
                        Metadata:    map[string]any{"type": providerName, "uid": a.UID},
                },
        })
}

func handleStartLogin(_ []byte) ([]byte, error) {
	// Step 1: PKCE + login_trace_id.
	loginTraceID := newLoginTraceID()
	codeVerifier, codeChallenge := generatePKCEPair()

	// Step 2: Allocate local callback port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("allocate callback port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cbURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)

	// Step 3: POST GetLoginGuidance (Intl uses api.marscode.com).
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

	// Step 4: Parse LoginHost.
	loginHost := extractLoginHost(raw)
	if loginHost == "" {
		ln.Close()
		return nil, fmt.Errorf("GetLoginGuidance: missing LoginHost (body=%s)", truncate(string(raw), 200))
	}

	// Step 5: Build verification URI with PKCE.
	deviceID := newDeviceID()
	machineID := newMachineID()
	verificationURI := buildVerificationURI(loginHost, verificationURIParams{
		AuthFrom:      oauthAuthFrom,
		PluginVersion: oauthPluginVersion,
		ClientID:      upstreamClient.ClientID,
		LoginTraceID:  loginTraceID,
		CallbackURL:   cbURL,
		MachineID:     machineID,
		DeviceID:      deviceID,
		DeviceBrand:   oauthDeviceBrand,
		DeviceType:    oauthDeviceType,
		OSVersion:     oauthOSVersion,
		Env:           oauthEnv,
		AppVersion:    oauthAppVersion,
		AppType:       oauthAppType,
		CodeChallenge: codeChallenge,
		HideSaasLogin: false, // Intl non-SOLO does not hide SaaS login
	})

	// Step 6: Store login state.
	loginStates.Store(loginTraceID, &loginCtx{
		listener:     ln,
		state:        loginTraceID,
		cbURL:        cbURL,
		expires:      time.Now().Add(loginTTL),
		loginTraceID: loginTraceID,
		codeVerifier: codeVerifier,
		codeChallenge: codeChallenge,
		deviceID:     deviceID,
		machineID:    machineID,
	})

	go acceptCallback(loginTraceID)

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       verificationURI,
		State:     loginTraceID,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL, "callback_url": cbURL},
	})
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
        select {
        case <-lc.done:
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
                loginStates.Delete(state)
                lc.listener.Close()
                return okEnvelope(pluginapi.AuthLoginPollResponse{
                        Status:  pluginapi.AuthLoginStatusError,
                        Message: "login completed but no authCode received — please retry",
                })
        }
        // Exchange authCode for tokens.
        tokenBody := map[string]any{
                "ClientID":     upstreamClient.ClientID,
                "AuthCode":     lc.authCode,
                "CodeVerifier": lc.codeVerifier,
                "ClientSecret": "-",
                "UserID":       "",
                "DeviceInfo":   map[string]any{"DeviceId": randomHex(16), "MachineId": randomHex(16)},
                "IDEVersion":   "3.5.66",
        }
        tokenBytes, _ := json.Marshal(tokenBody)
        tokenReq, _ := http.NewRequest(http.MethodPost,
                upstreamClient.OAuthHost+"/trae/api/v3/oauth/ExchangeToken",
                bytes.NewReader(tokenBytes))
        tokenReq.Header.Set("Content-Type", "application/json")
        tokenReq.Header.Set("User-Agent", "Trae/3.5.66")
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
                        Token               string `json:"Token"`
                        TokenExpireAt       int64  `json:"TokenExpireAt"`
                        RefreshToken        string `json:"RefreshToken"`
                        RefreshExpireAt     int64  `json:"RefreshExpireAt"`
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
        a := &upstream.Auth{
                AccessToken:  tokenEnv.Result.Token,
                RefreshToken: tokenEnv.Result.RefreshToken,
                ExpiresAt:    normalizeExpiresAt(tokenEnv.Result.TokenExpireAt),
                APIHost:      upstreamClient.OAuthHost,
                Domain:       "trae.ai",
                Region:       "US-East",
                Scope:        "marscode-us",
                Tenant:       "marscode",
                UserIdentity: "Free",
                AppLanguage:  "en",
                AppVersion:   "1.0.0.1229",
        }
        uid, nickname, entID, err := upstreamClient.GetUserInfo(a)
        if err != nil {
                log.Printf("GetUserInfo failed: %v — proceeding with empty UID", err)
        }
        a.UID = uid
        a.Nickname = nickname
        a.EnterpriseID = entID
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "type":     providerName,
                "provider": providerName,
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "region":       a.Region,
                        "scope":        a.Scope,
                        "tenant":       a.Tenant,
                        "appLanguage":  a.AppLanguage,
                        "appVersion":   a.AppVersion,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
                "disabled": false,
        }, "", "  ")
        loginStates.Delete(state)
        lc.listener.Close()
        return okEnvelope(pluginapi.AuthLoginPollResponse{
                Status:  pluginapi.AuthLoginStatusSuccess,
                Message: fmt.Sprintf("login complete (uid=%s)", a.UID),
                Auth: pluginapi.AuthData{
                        Provider:    providerName,
                        ID:          a.UID,
                        FileName:    fmt.Sprintf("%s-%s.json", providerName, a.UID),
                        Label:       nonEmpty(a.Nickname, "Trae Intl "+a.UID),
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
                "type":     providerName,
                "provider": providerName,
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "region":       a.Region,
                        "scope":        a.Scope,
                        "tenant":       a.Tenant,
                        "appLanguage":  a.AppLanguage,
                        "appVersion":   a.AppVersion,
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
                        Label:       nonEmpty(a.Nickname, "Trae Intl "+a.UID),
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
        refreshed, err := upstreamClient.RefreshTokenIfNeeded(a, 24*time.Hour)
        if err != nil {
                return nil, fmt.Errorf("execute: refresh: %w", err)
        }
        if refreshed {
                persistRefreshedAuth(req, a)
        }
        // Parse OpenAI messages from payload.
        var openaiReq struct {
                Model    string           `json:"model"`
                Messages []map[string]any `json:"messages"`
        }
        if err := json.Unmarshal(req.Payload, &openaiReq); err != nil {
                return nil, fmt.Errorf("execute: parse payload: %w", err)
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        completion, err := upstreamClient.Execute(ctx, a, openaiReq.Model, openaiReq.Messages)
        if err != nil {
                return nil, fmt.Errorf("execute: %w", err)
        }
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
        // Refresh token if needed (within 24h of expiry).
        refreshed, err := upstreamClient.RefreshTokenIfNeeded(a, 24*time.Hour)
        if err != nil {
                return nil, fmt.Errorf("stream: refresh: %w", err)
        }
        if refreshed {
                persistRefreshedAuth(req, a)
        }
        var openaiReq struct {
                Model    string           `json:"model"`
                Messages []map[string]any `json:"messages"`
        }
        if err := json.Unmarshal(req.Payload, &openaiReq); err != nil {
                return nil, fmt.Errorf("stream: parse payload: %w", err)
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        reader, err := upstreamClient.ExecuteStream(ctx, a, openaiReq.Model, openaiReq.Messages)
        if err != nil {
                return nil, fmt.Errorf("stream: %w", err)
        }
        // Read all chunks and emit as a single ExecutorStreamChunk (simpler than
        // real-time piping through the host's chunk channel).
        raw, err := io.ReadAll(reader)
        if err != nil {
                return nil, fmt.Errorf("stream: read: %w", err)
        }
        ch := make(chan pluginapi.ExecutorStreamChunk, 1)
        ch <- pluginapi.ExecutorStreamChunk{Payload: raw}
        close(ch)
        return okEnvelope(pluginapi.ExecutorStreamResponse{
                Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
                Chunks:  ch,
        })
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


// extractLoginHost extracts LoginHost from GetLoginGuidance response.
func extractLoginHost(raw []byte) string {
	var probe struct {
		Result struct {
			LoginHost string `json:"LoginHost"`
			LoginURL  string `json:"LoginURL"`
		} `json:"Result"`
		Result2 struct {
			LoginHost string `json:"loginHost"`
			LoginURL  string `json:"loginUrl"`
		} `json:"result"`
		Data struct {
			Result struct {
				LoginHost string `json:"LoginHost"`
			} `json:"Result"`
		} `json:"data"`
		LoginHost string `json:"LoginHost"`
		LoginURL  string `json:"loginUrl"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	if probe.Result.LoginHost != "" {
		return probe.Result.LoginHost
	}
	if probe.Result2.LoginHost != "" {
		return probe.Result2.LoginHost
	}
	if probe.Result.LoginURL != "" {
		return probe.Result.LoginURL
	}
	if probe.Result2.LoginURL != "" {
		return probe.Result2.LoginURL
	}
	if probe.Data.Result.LoginHost != "" {
		return probe.Data.Result.LoginHost
	}
	if probe.LoginHost != "" {
		return probe.LoginHost
	}
	return probe.LoginURL
}

// verificationURIParams holds parameters for building the OAuth verification URL.
type verificationURIParams struct {
	AuthFrom       string
	PluginVersion  string
	ClientID       string
	LoginTraceID   string
	CallbackURL    string
	MachineID      string
	DeviceID       string
	DeviceBrand    string
	DeviceType     string
	OSVersion      string
	Env            string
	AppVersion     string
	AppType        string
	CodeChallenge  string
	HideSaasLogin  bool
}

// buildVerificationURI constructs the browser-facing OAuth URL.
func buildVerificationURI(loginHost string, p verificationURIParams) string {
	if !strings.HasPrefix(loginHost, "http") {
		loginHost = "https://" + loginHost
	}
	u, err := url.Parse(loginHost)
	if err != nil {
		return loginHost
	}
	u.Path = "/authorization"
	q := u.Query()
	q.Set("login_version", "1")
	q.Set("auth_from", p.AuthFrom)
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", p.PluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", p.ClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", p.LoginTraceID)
	q.Set("auth_callback_url", p.CallbackURL)
	q.Set("machine_id", p.MachineID)
	q.Set("device_id", p.DeviceID)
	q.Set("x_device_id", p.DeviceID)
	q.Set("x_machine_id", p.MachineID)
	q.Set("x_device_brand", p.DeviceBrand)
	q.Set("x_device_type", p.DeviceType)
	q.Set("x_os_version", p.OSVersion)
	q.Set("x_env", p.Env)
	q.Set("x_app_version", p.AppVersion)
	q.Set("x_app_type", p.AppType)
	q.Set("code_challenge", p.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	if p.HideSaasLogin {
		q.Set("hide_saas_login", "true")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// writeCallbackHTML writes a success/failure HTML page to the HTTP response.
func writeCallbackHTML(conn net.Conn, title, msg string) {
	body := fmt.Sprintf("<html><body><h2>%s</h2><p>%s</p></body></html>", title, msg)
	fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
}

// loginCtx holds the local callback listener for one in-flight OAuth flow.
type loginCtx struct {
	listener     net.Listener
	state        string
	cbURL        string
	expires      time.Time
	loginTraceID string
	codeVerifier string
	codeChallenge string
	deviceID     string
	machineID    string
	loginHost    string
	authCode     string
	err          error
	done         chan struct{}
}

// acceptCallback accepts the OAuth callback from the browser.
func acceptCallback(state string) {
	v, ok := loginStates.Load(state)
	if !ok {
		return
	}
	lc := v.(*loginCtx)
	lc.done = make(chan struct{})
	defer close(lc.done)

	ln := lc.listener
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(loginTTL))
	conn, err := ln.Accept()
	if err != nil {
		lc.err = fmt.Errorf("callback accept: %w", err)
		lc.listener.Close()
		return
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 16384)
	n, _ := conn.Read(buf)
	req := string(buf[:n])
	// Parse "GET /authorize?... HTTP/1.1"
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
	if sp2 := strings.LastIndex(rest, " "); sp2 >= 0 {
		rest = rest[:sp2]
	}
	vals := url.Values{}
	if q := strings.Index(rest, "?"); q >= 0 {
		vals, _ = url.ParseQuery(rest[q+1:])
	}
	lc.authCode = vals.Get("authCode")
	lc.loginHost = vals.Get("loginHost")
	body := "<html><body><h2>Login successful</h2><p>You can close this window now.</p></body></html>"
	fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
}
