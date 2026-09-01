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

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/intlupstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	intlproviderName = "trae"
	intlauthFileName = "trae-intl.json"
	// OAuth constants (Intl uses api.marscode.com, not api.trae.cn)
	intlOAuthLoginGuidanceURL = "https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance"
	intlOAuthDefaultHost      = "https://api.marscode.com"
	intlOauthPluginVersion    = "1.0.0"
	intlOauthDeviceBrand      = "83DG"
	intlOauthDeviceType       = "windows"
	intlOauthOSVersion        = "Windows 11 Pro"
	intlOauthEnv              = "prod"
	oauthAppVersion           = "3.5.66"
	intlOauthAppType          = "trae"
	intlOauthDeviceName       = "DESKTOP-CPAINTL"
)

var intlversion = "0.1.0"

var (
	intlloginStates sync.Map

	intlupstreamClient *upstream.Client
)

func intlhandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(intlbuildRegistration())

	case pluginabi.MethodModelStatic:
		return intlhandleModelStatic(request)

	case pluginabi.MethodModelForAuth:
		return intlhandleModelForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(intlidentifierResponse{Identifier: intlproviderName})

	case pluginabi.MethodAuthParse:
		return intlhandleParseAuth(request)

	case pluginabi.MethodAuthLoginStart:
		return intlhandleStartLogin(request)

	case pluginabi.MethodAuthLoginPoll:
		return intlhandlePollLogin(request)

	case pluginabi.MethodAuthRefresh:
		return intlhandleRefreshAuth(request)

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(intlidentifierResponse{Identifier: intlproviderName})

	case pluginabi.MethodExecutorExecute:
		return intlhandleExecExecute(request)

	case pluginabi.MethodExecutorExecuteStream:
		return intlhandleExecStream(request)

	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})

	case pluginabi.MethodManagementRegister:
		// Cache host-injected BasePath so intlhandleManagement doesn't hardcode
		// /v0/management (tolerate future host path changes).
		var regReq pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &regReq); err == nil {
			if regReq.BasePath != "" {
				intlsetManagementBasePath(regReq.BasePath)
			}
		}
		return okEnvelope(intlmanagementRegistration())

	case pluginabi.MethodManagementHandle:
		return intlhandleManagement(request)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

type intlidentifierResponse struct {
	Identifier string `json:"identifier"`
}

type intlregistrationPayload struct {
	SchemaVersion uint32                     `json:"schema_version"`
	Metadata      pluginapi.Metadata         `json:"metadata"`
	Capabilities  intlregistrationCapability `json:"capabilities"`
}

type intlregistrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

func intlbuildRegistration() intlregistrationPayload {
	return intlregistrationPayload{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             intlproviderName,
			Version:          intlversion,
			Author:           "mmqz (based on OmniRoute trae.ts by diegosouzapw)",
			GitHubRepository: "https://github.com/mmqz/cpa-multi-plugins",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional model list. Use 'auto' for server-pick, 'work' for fast work mode, or specific model names (gpt-5.2, gemini-3.1-pro, kimi-k2.5, etc)."},
			},
		},
		Capabilities: intlregistrationCapability{
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

func intlhandleModelStatic(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: intlproviderName, Models: intlstaticModels()})
}

func intlhandleModelForAuth(request []byte) ([]byte, error) {
	var req struct {
		Auth pluginapi.AuthData `json:"auth"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	a, err := intlparseStoredAuth(req.Auth.StorageJSON)
	if err != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: intlproviderName, Models: intlstaticModels()})
	}
	dynamic, err := intlupstreamClient.FetchModels(a)
	if err != nil {
		log.Printf("model.for_auth %s: %v — falling back to static", a.UID, err)
		return okEnvelope(pluginapi.ModelResponse{Provider: intlproviderName, Models: intlstaticModels()})
	}
	out := make([]pluginapi.ModelInfo, 0, len(dynamic))
	for _, id := range dynamic {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: intlproviderName})
	}
	// Always include "auto" and "work" as virtual models.
	out = append(out,
		pluginapi.ModelInfo{ID: "auto", Name: "auto (server pick)"},
		pluginapi.ModelInfo{ID: "work", Name: "work (fast mode)"},
	)
	return okEnvelope(pluginapi.ModelResponse{Provider: intlproviderName, Models: out})
}

func intlstaticModels() []pluginapi.ModelInfo {
	known := []string{"auto", "work", "gpt-5.2", "gemini-3.1-pro", "kimi-k2.5", "claude-sonnet-4-5"}
	out := make([]pluginapi.ModelInfo, 0, len(known))
	for _, id := range known {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: intlproviderName})
	}
	return out
}

// -----------------------------------------------------------------------------
// Auth: parse / login / refresh
// -----------------------------------------------------------------------------

func intlparseStoredAuth(raw []byte) (*upstream.Auth, error) {
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
			APIHost      string `json:"apiHost"`
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
		APIHost      string `json:"apiHost"`
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
			AccessToken:  nested.Auth.AccessToken,
			RefreshToken: nested.Auth.RefreshToken,
			ExpiresAt:    nested.Auth.ExpiresAt,
			APIHost:      nested.Auth.APIHost,
			Domain:       intlnonEmpty(nested.Auth.Domain, "trae.ai"),
			UID:          nested.Account.UID,
			EnterpriseID: nested.Account.EnterpriseID,
			Nickname:     nested.Account.Nickname,
			WebID:        nested.Auth.WebID,
			BizUserID:    nested.Auth.BizUserID,
			UserUniqueID: nested.Auth.UserUniqueID,
			UserIdentity: nested.Auth.UserIdentity,
			Scope:        nested.Auth.Scope,
			Tenant:       nested.Auth.Tenant,
			Region:       nested.Auth.Region,
			AppLanguage:  nested.Auth.AppLanguage,
			AppVersion:   nested.Auth.AppVersion,
		}, nil
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("parse flat auth: %w", err)
	}
	return &upstream.Auth{
		AccessToken:  flat.AccessToken,
		RefreshToken: flat.RefreshToken,
		ExpiresAt:    flat.ExpiresAt,
		APIHost:      flat.APIHost,
		Domain:       intlnonEmpty(flat.Domain, "trae.ai"),
		UID:          flat.UID,
		EnterpriseID: flat.EnterpriseID,
		Nickname:     flat.Nickname,
		WebID:        flat.WebID,
		BizUserID:    flat.BizUserID,
		UserUniqueID: flat.UserUniqueID,
		UserIdentity: flat.UserIdentity,
		Scope:        flat.Scope,
		Tenant:       flat.Tenant,
		Region:       flat.Region,
		AppLanguage:  flat.AppLanguage,
		AppVersion:   flat.AppVersion,
	}, nil
}

func intlhandleParseAuth(request []byte) ([]byte, error) {
	var req struct {
		StorageJSON json.RawMessage `json:"storage_json"`
		FileName    string          `json:"file_name"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	a, err := intlparseStoredAuth(req.StorageJSON)
	if err != nil {
		return errorEnvelope("parse_error", err.Error()), nil
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    intlproviderName,
			ID:          a.UID,
			FileName:    intlnonEmpty(req.FileName, intlauthFileName),
			Label:       intlnonEmpty(a.Nickname, "Trae Intl "+a.UID),
			StorageJSON: req.StorageJSON,
			Metadata:    map[string]any{"type": intlproviderName, "uid": a.UID},
		},
	})
}

func intlhandleStartLogin(_ []byte) ([]byte, error) {
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

	// Step 3: POST GetLoginGuidance with multi-endpoint fallback
	// (cockpit-tools request_login_guidance). Intl tries api.marscode.com →
	// api.trae.ai → www.trae.ai, so a single-host 404/outage no longer
	// kills the login flow with "GetLoginGuidance upstream 404".
	loginHost, err := intlrequestLoginGuidance(false, loginTraceID)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("GetLoginGuidance failed: %w", err)
	}

	// Step 5: Build verification URI with PKCE.
	deviceID := newDeviceID()
	machineID := newMachineID()
	verificationURI := intlbuildVerificationURI(loginHost, intlverificationURIParams{
		AuthFrom:      oauthAuthFrom,
		PluginVersion: intlOauthPluginVersion,
		ClientID:      intlupstreamClient.ClientID,
		LoginTraceID:  loginTraceID,
		CallbackURL:   cbURL,
		MachineID:     machineID,
		DeviceID:      deviceID,
		DeviceBrand:   intlOauthDeviceBrand,
		DeviceType:    intlOauthDeviceType,
		OSVersion:     intlOauthOSVersion,
		Env:           intlOauthEnv,
		AppVersion:    oauthAppVersion,
		AppType:       intlOauthAppType,
		CodeChallenge: codeChallenge,
		HideSaasLogin: false, // Intl non-SOLO does not hide SaaS login
	})

	// Step 6: Store login state.
	intlloginStates.Store(loginTraceID, &intlloginCtx{
		listener:      ln,
		state:         loginTraceID,
		cbURL:         cbURL,
		expires:       time.Now().Add(loginTTL),
		loginTraceID:  loginTraceID,
		codeVerifier:  codeVerifier,
		codeChallenge: codeChallenge,
		deviceID:      deviceID,
		machineID:     machineID,
	})

	go intlacceptCallback(loginTraceID)

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  intlproviderName,
		URL:       verificationURI,
		State:     loginTraceID,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL, "callback_url": cbURL},
	})
}

func intlhandlePollLogin(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := intlloginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state — please restart login")
	}
	lc := v.(*intlloginCtx)
	if time.Now().After(lc.expires) {
		intlloginStates.Delete(state)
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
		intlloginStates.Delete(state)
		lc.listener.Close()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: lc.err.Error(),
		})
	}
	if lc.authCode == "" {
		intlloginStates.Delete(state)
		lc.listener.Close()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "login completed but no authCode received — please retry",
		})
	}
	// Exchange authCode for tokens via multi-origin fallback
	// (cockpit-tools candidate_account_api_origins): derive origins from
	// the callback loginHost (www.→api. rewrite) and append the Intl
	// account-API defaults. The full DeviceInfo mirrors cockpit-tools
	// build_official_device_info and reuses the SAME machine/device ids
	// embedded in the login URL (previously random hex ids were sent).
	di := intlbuildOfficialDeviceInfo(
		lc.deviceID, lc.machineID, oauthPlatformCode, intlOauthDeviceName,
		intlOauthDeviceBrand, oauthAppVersion, intlOauthDeviceType, intlOauthOSVersion,
	)
	tokenBody := map[string]any{
		"ClientID":     intlupstreamClient.ClientID,
		"AuthCode":     lc.authCode,
		"CodeVerifier": lc.codeVerifier,
		"DeviceInfo":   di,
		"IDEVersion":   oauthAppVersion,
	}
	tokenBytes, _ := json.Marshal(tokenBody)
	tokenRaw, exErr := intlexchangeTokenCandidates(
		intlbuildAPIURLs(lc.loginHost, "/trae/api/v3/oauth/ExchangeToken", false), tokenBytes)
	if exErr != nil {
		intlloginStates.Delete(state)
		lc.listener.Close()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: exErr.Error(),
		})
	}
	var tokenEnv struct {
		Result struct {
			Token           string `json:"Token"`
			AccessToken     string `json:"AccessToken"`
			TokenExpireAt   int64  `json:"TokenExpireAt"`
			RefreshToken    string `json:"RefreshToken"`
			RefreshExpireAt int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(tokenRaw, &tokenEnv); err != nil {
		intlloginStates.Delete(state)
		lc.listener.Close()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("parse ExchangeToken: %v", err),
		})
	}
	if tokenEnv.Result.AccessToken != "" && tokenEnv.Result.Token == "" {
		tokenEnv.Result.Token = tokenEnv.Result.AccessToken
	}
	a := &upstream.Auth{
		AccessToken:  tokenEnv.Result.Token,
		RefreshToken: tokenEnv.Result.RefreshToken,
		ExpiresAt:    intlnormalizeExpiresAt(tokenEnv.Result.TokenExpireAt),
		APIHost:      intlupstreamClient.OAuthHost,
		Domain:       "trae.ai",
		Region:       "US-East",
		Scope:        "marscode-us",
		Tenant:       "marscode",
		UserIdentity: "Free",
		AppLanguage:  "en",
		AppVersion:   "1.0.0.1229",
	}
	uid, nickname, entID, err := intlupstreamClient.GetUserInfo(a)
	if err != nil {
		log.Printf("GetUserInfo failed: %v — proceeding with empty UID", err)
	}
	a.UID = uid
	a.Nickname = nickname
	a.EnterpriseID = entID
	storageJSON, _ := json.MarshalIndent(map[string]any{
		"type":     intlproviderName,
		"provider": intlproviderName,
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
	intlloginStates.Delete(state)
	lc.listener.Close()
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: fmt.Sprintf("login complete (uid=%s)", a.UID),
		Auth: pluginapi.AuthData{
			Provider:    intlproviderName,
			ID:          a.UID,
			FileName:    fmt.Sprintf("%s-%s.json", intlproviderName, a.UID),
			Label:       intlnonEmpty(a.Nickname, "Trae Intl "+a.UID),
			StorageJSON: storageJSON,
			Metadata:    map[string]any{"type": intlproviderName, "uid": a.UID, "nickname": a.Nickname},
		},
	})
}

func intlhandleRefreshAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	a, err := intlparseStoredAuth(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: parse auth: %w", err)
	}
	if err := intlupstreamClient.RefreshToken(a); err != nil {
		return nil, fmt.Errorf("refresh: ExchangeToken: %w", err)
	}
	storageJSON, _ := json.MarshalIndent(map[string]any{
		"type":     intlproviderName,
		"provider": intlproviderName,
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
			Provider:    intlproviderName,
			ID:          a.UID,
			FileName:    fmt.Sprintf("%s-%s.json", intlproviderName, a.UID),
			Label:       intlnonEmpty(a.Nickname, "Trae Intl "+a.UID),
			StorageJSON: storageJSON,
			Metadata:    map[string]any{"type": intlproviderName, "uid": a.UID},
		},
		NextRefreshAfter: time.Now().Add(12 * time.Hour).UTC(),
	})
}

// -----------------------------------------------------------------------------
// Executor: execute + execute_stream
// -----------------------------------------------------------------------------

func intlhandleExecExecute(request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	a, err := intlparseStoredAuth(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("execute: parse auth: %w", err)
	}
	// Refresh token if needed (within 24h of expiry).
	refreshed, err := intlupstreamClient.RefreshTokenIfNeeded(a, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("execute: refresh: %w", err)
	}
	if refreshed {
		intlpersistRefreshedAuth(req, a)
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
	completion, err := intlupstreamClient.Execute(ctx, a, openaiReq.Model, openaiReq.Messages)
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	out, _ := json.Marshal(completion)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: out})
}

func intlhandleExecStream(request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	a, err := intlparseStoredAuth(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("stream: parse auth: %w", err)
	}
	// Refresh token if needed (within 24h of expiry).
	refreshed, err := intlupstreamClient.RefreshTokenIfNeeded(a, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("stream: refresh: %w", err)
	}
	if refreshed {
		intlpersistRefreshedAuth(req, a)
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
	reader, err := intlupstreamClient.ExecuteStream(ctx, a, openaiReq.Model, openaiReq.Messages)
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

func intltruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func intlnonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func intlnormalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// intlextractLoginHost extracts LoginHost from GetLoginGuidance response.
func intlextractLoginHost(raw []byte) string {
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

// intlverificationURIParams holds parameters for building the OAuth verification URL.
type intlverificationURIParams struct {
	AuthFrom      string
	PluginVersion string
	ClientID      string
	LoginTraceID  string
	CallbackURL   string
	MachineID     string
	DeviceID      string
	DeviceBrand   string
	DeviceType    string
	OSVersion     string
	Env           string
	AppVersion    string
	AppType       string
	CodeChallenge string
	HideSaasLogin bool
}

// intlbuildVerificationURI constructs the browser-facing OAuth URL.
func intlbuildVerificationURI(loginHost string, p intlverificationURIParams) string {
	// intlensureHTTPSScheme mirrors cockpit-tools ensure_https_url: a scheme-less
	// URL would be resolved as a RELATIVE path by the panel browser and hit
	// the CPA server itself → "404 page not found".
	loginHost = intlensureHTTPSScheme(loginHost)
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

// intlloginCtx holds the local callback listener for one in-flight OAuth flow.
type intlloginCtx struct {
	listener      net.Listener
	state         string
	cbURL         string
	expires       time.Time
	loginTraceID  string
	codeVerifier  string
	codeChallenge string
	deviceID      string
	machineID     string
	loginHost     string
	authCode      string
	err           error
	done          chan struct{}
}

// intlacceptCallback accepts the OAuth callback from the browser.
func intlacceptCallback(state string) {
	v, ok := intlloginStates.Load(state)
	if !ok {
		return
	}
	lc := v.(*intlloginCtx)
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
