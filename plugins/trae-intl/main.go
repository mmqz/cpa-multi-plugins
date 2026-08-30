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
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)

	upstreamClient = upstream.New()
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
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

// -----------------------------------------------------------------------------
// Models
// -----------------------------------------------------------------------------

func handleModelStatic(_ []byte) ([]byte, error) {
	return okEnvelope(map[string]any{"models": staticModels()})
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
		return okEnvelope(map[string]any{"models": staticModels()})
	}
	dynamic, err := upstreamClient.FetchModels(a)
	if err != nil {
		log.Printf("model.for_auth %s: %v — falling back to static", a.UID, err)
		return okEnvelope(map[string]any{"models": staticModels()})
	}
	out := make([]pluginapi.ModelInfo, 0, len(dynamic))
	for _, id := range dynamic {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id})
	}
	// Always include "auto" and "work" as virtual models.
	out = append(out,
		pluginapi.ModelInfo{ID: "auto", Name: "auto (server pick)"},
		pluginapi.ModelInfo{ID: "work", Name: "work (fast mode)"},
	)
	return okEnvelope(map[string]any{"models": out})
}

func staticModels() []pluginapi.ModelInfo {
	known := []string{"auto", "work", "gpt-5.2", "gemini-3.1-pro", "kimi-k2.5", "claude-sonnet-4-5"}
	out := make([]pluginapi.ModelInfo, 0, len(known))
	for _, id := range known {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id})
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
			ApiHost      string `json:"apiHost"`
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
		ApiHost      string `json:"apiHost"`
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
			ApiHost:       nested.Auth.ApiHost,
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
		ApiHost:       flat.ApiHost,
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("allocate callback port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	guidanceURL := upstreamClient.OAuthHost + "/cloudide/api/v3/trae/GetLoginGuidance"
	cbURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)
	body := map[string]any{
		"ClientID":    upstreamClient.ClientID,
		"RedirectUri": cbURL,
		"DeviceInfo":  map[string]any{"DeviceId": randomHex(16), "MachineId": randomHex(16)},
		"IDEVersion":  "3.5.66",
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, guidanceURL, bytes.NewReader(bodyBytes))
	if err != nil {
		ln.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae/3.5.66")
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
	state := env.Result.State
	loginStates.Store(state, &loginCtx{
		listener: ln,
		state:    state,
		cbURL:    cbURL,
		expires:  time.Now().Add(loginTTL),
	})
	go acceptCallback(state)
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       env.Result.LoginURL,
		State:     state,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL, "callback_url": cbURL},
	})
}

type loginCtx struct {
	listener    net.Listener
	state       string
	cbURL       string
	expires     time.Time
	authCode    string
	codeVerifier string
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
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(loginTTL))
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
	idx := strings.Index(req, " ")
	if idx < 0 {
		lc.err = fmt.Errorf("callback: malformed request")
		return
	}
	path := req[:idx]
	if q := strings.Index(path, "?"); q >= 0 {
		vals, _ := url.ParseQuery(path[q+1:])
		lc.authCode = vals.Get("authCode")
		lc.codeVerifier = vals.Get("codeVerifier")
	}
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
		ApiHost:      upstreamClient.OAuthHost,
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
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
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
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
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

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = readRand(b)
	return hexEncode(b)
}
