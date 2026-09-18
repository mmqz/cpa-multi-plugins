// Package main implements the workbuddy CLIProxyAPI dynamic plugin.
//
// workbuddy wraps Tencent CodeBuddy (copilot.tencent.com) as a cliproxy
// provider: it performs the CodeBuddy web login flow, refreshes access
// tokens, and forwards OpenAI-compatible chat completion requests to the
// upstream /v2/chat/completions endpoint.
//
// This file is a clean-room reimplementation reconstructed from the public
// workbuddy.so binary (symbol table, string constants and RPC shape) published
// by Sliverkiss. Original credit for the workbuddy plugin goes to Sliverkiss;
// see https://github.com/Sliverkiss/cpa-plugin. Built with -buildmode=c-shared
// and exports the cliproxy C ABI entry points.
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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	providerName  = "workbuddy"
	authFileName  = "workbuddy.json"
	pluginLogoURL = "https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png"
	// CN chat/auth gateway (iss = codebuddy.cn realm).
	upstreamBaseCN = "https://copilot.tencent.com"
	// Global chat/auth gateway (iss = workbuddy.ai realm). APISIX on
	// copilot.tencent.com rejects Global JWTs with 401; must use workbuddy.ai.
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	// Intl chat/auth gateway (merged codebuddy-intl plugin, v0.11.0):
	// the codebuddy.ai realm issues its own JWTs — separate from both
	// copilot.tencent.com and workbuddy.ai.
	upstreamBaseIntl    = "https://www.codebuddy.ai"
	clientUA            = "CLI/2.108.1 CodeBuddy/2.108.1"
	originReferer       = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	originRefererIntl   = "https://www.codebuddy.ai"
	// WorkBuddy desktop client fingerprint (mirrors Sliverkiss/workbuddy2api's
	// injectAttribution + defaultWorkBuddyUAFor). The global gateway's security
	// gate rejects requests that do not carry the official desktop shape with
	// 400 code 11128 "Illegal API invocation from an unapproved channel" — for
	// EVERY model, regardless of reasoning depth or login platform. Replicating
	// the desktop client's UA + header family is the approved-channel identity.
	desktopVersionWB = "5.5.4"
	desktopCLIWB     = "2.137.1"

	// CN endpoint aliases (login / chat / models). upstreamBaseCN is the only
	// CN base; Global has its own upstreamBaseGlobal. No "upstreamBase" legacy
	// alias — removed in v0.6.31 dead-code sweep.
	endpointAuthStateBase = upstreamBaseCN + "/v2/plugin/auth/state?platform="
	endpointLoginAcct     = upstreamBaseCN + "/v2/plugin/login/account?state="
	endpointAuthToken     = upstreamBaseCN + "/v2/plugin/auth/token?state="
	endpointTokenRefresh  = upstreamBaseCN + "/v2/plugin/auth/token/refresh"
	endpointChat          = upstreamBaseCN + "/v2/chat/completions"
	endpointModels        = upstreamBaseCN + "/console/enterprises/personal/models"

	loginTTL = 5 * time.Minute
)

// loginCtx holds the cookie-affined HTTP client for one in-flight login flow.
// CodeBuddy associates the browser login with the state issued at auth/state,
// so we must reuse the same cookie jar across the state request and the polls.
type loginCtx struct {
	client  *http.Client
	region  string // login realm: cn | intl (merged codebuddy-intl)
	expires time.Time
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
	// Intentionally a no-op. The host calls this on its own exit path (after
	// the host Go runtime has started tearing down) and dlclose()es this
	// library immediately afterwards. Touching any Go runtime state here —
	// mutexes, channel close, goroutine synchronization — risks a SIGSEGV in
	// cgo (observed on every docker restart: SIGSEGV in
	// _Cfunc_cliproxy_shutdown_plugin, PC near a freed runtime pointer).
	// The scheduler goroutine and janitor ticker hold no resources that
	// outlive the process; the OS reclaims them on exit.
}

// -----------------------------------------------------------------------------
// Host calls (async streaming + auth callbacks)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close) and to read the host's auth store (host.auth.list/get).
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
		// Upstream CodeBuddy has no dedicated count_tokens API. Return
		// unhandled-style zero estimate so clients fall back / skip.
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodManagementRegister:
		// Cache host-injected BasePath so handleManagement doesn't hardcode
		// /v0/management (v0.6.31: tolerate future host path changes).
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
	Code    string `json:"code"`
	Message string `json:"message"`
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
var version = "0.9.12"

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          version,
			Author:           "Sliverkiss (based on workbuddy by lovingfish)",
			GitHubRepository: "https://github.com/Sliverkiss/cpa-plugin",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "checkin_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "【自动签到】启用 CN 账号每日自动签到：本地时间 09:00 与 21:00 各一次（默认开启）。"},
				{Name: "lifecycle_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "【自动运营】自动管理生命周期：CN 余额耗尽自动禁用、Global 余额耗尽自动删除；CN 签到恢复积分后自动重新启用（默认开启）。"},
				{Name: "token_keepalive", Type: pluginapi.ConfigFieldTypeBoolean, Description: "【令牌保活】每天 22:00 自动刷新访问令牌，避免 Keycloak 离线会话过期导致失效（默认开启）。"},
				{Name: "login_platform", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"CLI", "ide"}, Description: "【登录形态】新登录采用的客户端形态：CLI（WorkBuddy，默认）或 ide（CodeBuddy IDE）。已存在的账号沿用登录/导入时记录的形态，不受此字段影响。"},
				{Name: "login_region", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"cn", "intl", "global"}, Description: "【账号区域】新登录的账号归属区域：cn（copilot.tencent.com，默认）、intl（codebuddy.ai，IDE 客户端；与 codebuddy-intl 插件合并）或 global（workbuddy.ai）。Global 使用同一套 CLI 登录协议对接 workbuddy.ai；登录后插件会自动完成境外注册激活，并领取一次性试用积分包（幂等）。"},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "【模型列表】可选模型列表。每个条目可包含 id、name、alias、context、max_tokens、enabled、reasoning 字段。注意：要按区域固定模型，请使用下方的 models_cn / models_global / models_intl。"},
				{Name: "models_cn", Type: pluginapi.ConfigFieldTypeString, Description: "【CN 模型】留空（推荐）：每个 CN 账号会按 copilot.tencent.com 对其凭证 Token 实际返回的模型来展示（5 分钟缓存）。也可填入逗号分隔的上游模型 ID，用于固定或覆盖 CN 模型的输出。"},
				{Name: "models_global", Type: pluginapi.ConfigFieldTypeString, Description: "【Global 模型】留空（推荐）：每个 Global 账号会按 workbuddy.ai 对其凭证 Token 实际返回的模型来展示（5 分钟缓存）。也可填入逗号分隔的上游模型 ID，用于固定或覆盖 Global 模型的输出。"},
				{Name: "models_intl", Type: pluginapi.ConfigFieldTypeString, Description: "【Intl 模型】留空（推荐）：每个 Intl 账号会按 codebuddy.ai 对其凭证 Token 实际返回的模型来展示（5 分钟缓存），不预填猜测值，避免把不支持的模型暴露给客户端。也可填入逗号分隔的上游模型 ID，用于配置或覆盖 Intl 模型的输出。"},
				{Name: "free_promos", Type: pluginapi.ConfigFieldTypeString, Description: "【免费模型窗口】覆盖免费模型及其免费截止日（含当天）。格式：逗号分隔的 model=YYYY-MM-DD（也可只写 model 表示无限期免费）。覆盖叠加在内置清单上；缺省时内置 dp4.1-flash / hy4-preview 至 2026-09-25、deepseek-v4-flash 无限期免费。示例：deepseek-v4.1-flash=2026-09-25,hy4-preview=2026-10-01" },
				{Name: "scheduler_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{schedulerModeOff, schedulerModeCredits}, Description: "【调度策略】多账号选择策略：allowed（交给内置默认路由，默认）或 credits（优先选择剩余量最多的账号）。警告：当设为 allowed 且 lifecycle_auto=false 时，仍可能路由到余额已耗尽的账号——请开启 lifecycle_auto 或改用 credits。"},
				{Name: "usage_report_url", Type: pluginapi.ConfigFieldTypeString, Description: "【用量上报地址】用量上报导入地址的可选覆盖值（默认 http://cpa-manager-plus:18317/v0/management/usage/import；也可用环境变量 USAGE_REPORT_URL 指定）。"},
				{Name: "usage_report_key", Type: pluginapi.ConfigFieldTypeString, Description: "【管理密钥】CPAMP 管理员密钥的可选覆盖值的可选覆盖值。优先自动检测环境变量 CPAMP_ADMIN_KEY / USAGE_REPORT_KEY 或密钥文件 /run/secrets/cpamp_admin_key。"},
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

// dynamicModelsCacheTTL bounds how long a fetched model list is reused.
// model.static / model.for_auth are re-invoked by CPA on every config reload
// and on each models query; without caching, every reload fans out to one
// upstream call per account.
const dynamicModelsCacheTTL = 5 * time.Minute

// realmModelsEntry is one realm's cached discovery result plus the v0.9.9
// diagnostics trail: WHERE the advertised list came from (discovery / pin /
// static fallback), when it was fetched, and why discovery failed last time.
// v0.12.18: the cache is keyed by realm (cn|global|intl) — a single shared
// entry let one realm's answer (or CN-flavored static fallback) satisfy
// model.for_auth for accounts on another realm, advertising models their
// gateway never served. Error-only entries (models nil) are cache misses for
// fetch purposes but keep the last failure visible to the panel.
type realmModelsEntry struct {
	models    []pluginapi.ModelInfo
	fetched   time.Time
	source    string // "discovery" | "pin ..." | "static ..."
	srcCount  int    // display count for pin/static states (models stays nil)
	lastErr   string
	lastErrAt time.Time
	lastLogAt time.Time // throttles the discovery-failure log line
}

var dynamicModelsCache = struct {
	sync.RWMutex
	realms map[string]realmModelsEntry
}{realms: map[string]realmModelsEntry{}}

//
// CPA applies oauth-model-alias to the models this plugin registers, so the
// gateway may route a request whose model ID is an alias (e.g.
// "point/deepseek-v4-flash") to this executor. The upstream only knows the
// real model IDs, so the plugin must map the alias back before forwarding.
//
// ExecutorRequest carries no host config, so the alias table is cached from
// the AuthModelRequest.Host summary every time the host asks for models
// (model.static / model.for_auth are re-queried by CPA on config reload,
// keeping this cache in sync with oauth-model-alias changes). Auth-level
// attribute overrides ("model_alias"/"model-alias"/"oauth-model-alias")
// are parsed per request and take precedence over the global table.

var modelAliasCache struct {
	sync.RWMutex
	byAlias map[string]string
}

// ------------------------------------------------------------------------------
// Usage reporting (request monitoring)
// ------------------------------------------------------------------------------
//
// CPA built-in executors publish via host usage.DefaultManager → redisqueue.
// Plugin executors cannot: c-shared .so has its own Go runtime, so
// usage.PublishRecord would hit a separate empty DefaultManager (no sink).
//
// Only effective path: POST NDJSON to CPA-Manager-Plus
// /v0/management/usage/import. Key/URL resolved automatically from
// config → env → docker secret files (see resolveUsageReport).
// usage.Detail is still used as a pure token-counter struct.

// storedAuth is the on-disk shape of a workbuddy credential.
type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
}

type storedTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	// LoginPlatform records which client produced the token: "CLI"
	// (workbuddy login) or "ide" (CodeBuddy IDE login). Empty means legacy
	// CLI-style (no X-IDE-* headers), matching historical workbuddy files.
	LoginPlatform string `json:"loginPlatform,omitempty"`
	// Region records which realm produced the token: "cn" (copilot.tencent.com),
	// "intl" (codebuddy.ai IDE client) or "global" (workbuddy.ai panel import).
	// Empty = legacy file — accountRegion falls back to domain sniffing.
	Region string `json:"region,omitempty"`
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// apiEnvelope is the generic {code,msg,data} wrapper used by every CodeBuddy API.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// Accept both shapes seen in the wild:
	//   nested: {"auth":{"accessToken":...},"account":{"uid":...}} (plugin/oauth output)
	//   flat:   {"accessToken":...,"uid":...,"nickname":...} (CPA-Manager-Plus auths/workbuddy.json)
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
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		sa.Auth = storedTokens{AccessToken: flat.AccessToken, RefreshToken: flat.RefreshToken, ExpiresAt: flat.ExpiresAt, Domain: flat.Domain}
		sa.Account = storedAccount{UID: flat.UID, EnterpriseID: flat.EnterpriseID, Nickname: flat.Nickname}
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &sa, nil
}

// -------------------------------------------------------------------------------
// HTTP plumbing
// -------------------------------------------------------------------------------

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// originRefererFor returns the Origin/Referer base URL appropriate for the
// account's domain. Global accounts use https://www.workbuddy.ai; CN (and
// legacy auth files with empty domain) use the default https://www.codebuddy.cn.
func originRefererFor(sa *storedAuth) string {
	if sa != nil {
		if isGlobalDomain(sa.Auth.Domain) {
			return originRefererGlobal
		}
		if isIntlDomain(sa.Auth.Domain) {
			return originRefererIntl
		}
	}
	return originReferer
}

// upstreamBaseFor returns the chat/auth API host for the account realm.
// Global JWT iss is workbuddy.ai — those tokens only work on www.workbuddy.ai.
// CN tokens work on copilot.tencent.com. Mixing them yields APISIX 401.
func upstreamBaseFor(sa *storedAuth) string {
	if sa != nil {
		if isGlobalDomain(sa.Auth.Domain) {
			return upstreamBaseGlobal
		}
		if isIntlDomain(sa.Auth.Domain) {
			return upstreamBaseIntl
		}
	}
	return upstreamBaseCN
}

func endpointChatFor(sa *storedAuth) string {
	return upstreamBaseFor(sa) + "/v2/chat/completions"
}

// chatEndpointCandidates returns the chat completion paths to try, in order,
// for the request's account.
//
// Global (workbuddy.ai) accounts hold a "console" OAuth session: the web /
// desktop client is authenticated against the console channel, so its chat
// must go to the console-domain endpoint /console/chat/completions — sending
// it to the CLI-channel /v2/chat/completions makes the gate answer 400
// {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}
// for EVERY model (verified live against a workbuddy.global account, 2026-09).
// The old/new API path fork means the console path may 404/405 on some
// gateways; the fallback follows the reference workbuddy2api (PLAN R9):
// [console, v2], stop at any other status.
func chatEndpointCandidates(sa *storedAuth) []string {
	if isGlobalDomain(sa.Auth.Domain) {
		return []string{
			upstreamBaseGlobal + "/console/chat/completions",
			upstreamBaseFor(sa) + "/v2/chat/completions",
		}
	}
	return []string{endpointChatFor(sa)}
}

// chatFallbackStatus reports whether an upstream chat status should try the
// next candidate endpoint (the 404/405 old-vs-new path fork).
func chatFallbackStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed
}

func endpointTokenRefreshFor(sa *storedAuth) string {
	return upstreamBaseFor(sa) + "/v2/plugin/auth/token/refresh"
}

func endpointModelsFor(sa *storedAuth) string {
	return upstreamBaseFor(sa) + "/console/enterprises/personal/models"
}

// backendHeaders applies auth-derived headers to a chat completion request.
// Empty fields are signalled via the X-No-* convention used by CodeBuddy.
func backendHeaders(req *http.Request, sa *storedAuth) {
	commonHeaders(req)
	applyPlatformHeaders(req, platformForAuth(sa))
	applyRealmHeaders(req, sa)
	if sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// SECURITY: do NOT send X-Refresh-Token on chat completions. refresh_token is
	// a long-lived credential that can mint new access_tokens; it only belongs on
	// the refresh endpoint (handleRefreshAuth). Sending it to chat upstream leaks
	// the credential into upstream request logs on every chat call.
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
	// Override Origin/Referer for Global accounts so the upstream doesn't
	// reject the request as cross-origin.
	origin := originRefererFor(sa)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	// Global workbuddy.ai chat must present the official desktop identity
	// (11128 channel gate) — see applyGlobalWorkbuddyIdentity below.
	applyGlobalWorkbuddyIdentity(req, sa)
}

// newRequestID32 returns a 32-char lowercase hex string (crypto-rand derived),
// matching the official client's message/request id format used by the
// X-Conversation-* and B3 tracing header family.
func newRequestID32() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure — fall back to a hex-shape id (B3 needs 16/32 hex).
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x%016x", now, uint64(now)^0x9e3779b97f4a7c15)
	}
	return hex.EncodeToString(b[:])
}

// applyGlobalWorkbuddyIdentity stamps GLOBAL (workbuddy.ai) chat requests with
// the official WorkBuddy desktop fingerprint that the upstream business gate
// approves; without it every model — regardless of reasoning depth or login
// platform — is rejected 400 code 11128 "Illegal API invocation from an
// unapproved channel". Only the global realm is rewritten: CN keeps its CLI
// identity and intl its codebuddy-IDE profile, preserving their approvals.
func applyGlobalWorkbuddyIdentity(req *http.Request, sa *storedAuth) {
	if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
		return
	}
	// Desktop client UA (workbuddy2api defaultWorkBuddyUAFor, global form).
	req.Header.Set("User-Agent", "WorkBuddy/"+desktopVersionWB+" WorkBuddy AI/"+desktopVersionWB+" CLI/"+desktopCLIWB)
	// Attribution family (workbuddy2api injectAttribution).
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", desktopVersionWB)
	req.Header.Set("X-Product", "WorkBuddy")
	// Conversation/trace family (official client stamps these per round).
	convReq := newRequestID32()
	msgID := convReq // one message per upstream call (SSE is folded to one completion)
	trace := convReq
	b3Trace := convReq
	if !validB3Hex(b3Trace) {
		b3Trace = msgID
	}
	req.Header.Set("X-Conversation-Request-ID", convReq)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Root-Request-ID", convReq)
	req.Header.Set("X-Trace-ID", trace)
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", msgID[:len(msgID)/2])
	req.Header.Set("X-B3-Sampled", "1")
}

// validB3Hex reports whether s is a B3-compatible trace id (16 or 32 hex).
func validB3Hex(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// logDumpChatOutbound prints a one-line, credential-redacted trace of an
// outbound chat request so we can see which path/headers the plugin actually
// uses. NEVER logs the Authorization header or the message body.
func logDumpChatOutbound(chatEP string, req *http.Request, body []byte) {
	model := "-"
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if s, ok := m["model"].(string); ok {
			model = s
		}
	}
	hk := func(k string) string {
		if v := req.Header.Get(k); v != "" {
			return k + "=" + v
		}
		return k + "=∅"
	}
	log.Printf("wb-chat outbound: url=%s model=%s | %s %s %s %s %s %s %s",
		chatEP, model,
		hk("User-Agent"), hk("Origin"), hk("X-Domain"), hk("X-User-Id"),
		hk("X-Agent-Purpose"), hk("X-IDE-Type"), hk("X-Product"))
}

// clampGlobalReasoningEffort aligns the chat body for the global
// (workbuddy.ai) console channel, mirroring workbuddy2api's effort clamp:
// the realm only accepts reasoning_effort="high" (no low/max tier — sending
// low/max upstream returns 4xx), so any non-"high" value is clamped to "high".
// The global console-channel system-message guarantee is handled separately by
// ensureSystemMessageInPlace in prepareUpstreamBody (the same guard as
// wb2api's ensureConsoleSystem). Non-global / unparseable bodies return as-is.
func clampGlobalReasoningEffort(body []byte, sa *storedAuth) []byte {
	if len(body) == 0 || sa == nil || !isGlobalDomain(sa.Auth.Domain) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if e, ok := obj["reasoning_effort"]; ok {
		if s, ok2 := e.(string); !ok2 || (s != "" && !strings.EqualFold(s, "high")) {
			obj["reasoning_effort"] = "high"
		}
	}
	// Drop thinking-level styles not valid on this realm.
	for _, k := range []string{"thinking_level", "budget_tokens"} {
		delete(obj, k)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

// isOurFamilyFileName reports whether a type-less auth file belongs to this
// plugin by name: our canonical prefix, a pre-merge family prefix, or a
// legacy single-file name. The filename is the only trustworthy discriminator
// for files without an explicit type (see the ownership note in
// handleParseAuth).
func isOurFamilyFileName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{providerName + "-", "codebuddy-cn-", "codebuddy-intl-"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	switch lower {
	case "workbuddy.json", "codebuddy.json", "codebuddy-cn.json", "codebuddy-intl.json":
		return true
	}
	return false
}

// isOurDeclaredType reports whether an explicitly declared auth "type"
// belongs to this plugin's family (current name plus pre-merge names).
func isOurDeclaredType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "workbuddy", "workbuddy-cn", "workbuddy-global", "workbuddy-intl",
		"codebuddy", "codebuddy-cn", "codebuddy-intl":
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
	// top-level "type" field (synthesizer/file.go). Files without a type fall
	// back to polling every plugin — first Handled=true wins. Only claim files
	// whose declared type matches us — or, for type-less legacy files, when the
	// host already routed this to us or the filename carries our prefix.
	// Symmetric with the qoderwork plugin's guard (commit 7b776a9).
	var probeType struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.RawJSON, &probeType)
	declared := strings.ToLower(strings.TrimSpace(probeType.Type))
	if declared != "" && !isOurDeclaredType(declared) {
		// Explicitly another provider's file — never claim it. Family-wide:
		// pre-merge names (codebuddy/codebuddy-cn/codebuddy-intl) remain ours.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if declared == "" {
		// No type declared: claim ONLY when the filename carries our family.
		// NOTE: req.Provider cannot prove ownership here — the host's
		// callParseAuths rewrites an empty Provider to the POLLED plugin's own
		// identifier, so EqualFold(req.Provider, "workbuddy") is always true
		// while polling us regardless of the file's origin. Symmetric with the
		// qoder plugin's fix (repo v0.12.9).
		if !isOurFamilyFileName(req.FileName) {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a workbuddy credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	// CRITICAL: echo back the host-provided FileName AND leave ID empty.
	//
	// CPA uses ID for auth record identity (upsert key). If we set ID=uid
	// while the host's watcher initially registered with ID=filename,
	// upsertAuthRecord can't find the existing record → creates a NEW one
	// → duplicate auth entries (same file, different IDs).
	//
	// By leaving ID empty, CPA falls back to authIDForPath(path) which
	// derives ID from the file path → always matches the watcher's key.
	// FileName is also echoed back to avoid rename-based duplicates.
	ad := toAuthDataOpts(sa, nil, false)
	ad.ID = "" // let host compute from path (prevents ID mismatch dupes)
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
	storage, _ := json.Marshal(sa)
	id := providerName
	fileName := authFileName
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			id = uid
			fileName = "workbuddy-" + uid + ".json"
		}
	}
	label := labelForAuth(sa)
	meta := enrichAuthMetadata(sa, cr, disabled)
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		Disabled:    disabled,
		StorageJSON: storage,
		// Standardized auth metadata. `type` is required by the host for
		// auth-file classification; `logo`/`note`/`disabled` surface on auth rows.
		Metadata: meta,
	}
}

// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	// Resolve oauth-model-alias (e.g. "point/deepseek-v4-flash") back to the
	// real upstream model ID; the upstream rejects unknown alias IDs.
	upstreamModel := resolveUpstreamModel(req.Model, req.AuthAttributes)
	started := time.Now()
	authUID := ""
	if sa.Account.UID != "" {
		authUID = sa.Account.UID
	}
	// CodeBuddy rejects non-stream requests (code 11101), so always stream
	// upstream and fold the chunks into a single chat.completion object.
	// prepareUpstreamBody does forceStream + normalizeTools + rewriteSystem +
	// ensureSystemMessage + rewriteModel in ONE unmarshal/marshal pass.
	body := prepareUpstreamBody(req.Payload, req.OriginalRequest, sa, upstreamModel)
	body = clampGlobalReasoningEffort(body, sa)
	// Global (console-channel) accounts must call /console/chat/completions;
	// 404/405 falls back to /v2/chat/completions. Other realms use /v2 only.
	var completion []byte
	for attempt, chatEP := range chatEndpointCandidates(sa) {
		httpReq, err := http.NewRequest(http.MethodPost, chatEP, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		backendHeaders(httpReq, sa)
		logDumpChatOutbound(chatEP, httpReq, body)
		// Compliance: route via host.http.do_stream so request-log captures the
		// outbound call. Read entire body via the bridge, then fold SSE → completion.
		stream, statusCode, _, err := hostHTTPDoStream(httpReq)
		if err != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
			return nil, fmt.Errorf("http_error: %w", err)
		}
		reader := newHostStreamReader(stream)
		if statusCode >= 400 {
			payload, _ := io.ReadAll(reader)
			stream.Close()
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(payload))
			if attempt < len(chatEndpointCandidates(sa))-1 && chatFallbackStatus(statusCode) {
				log.Printf("workbuddy chat: %s %d -> next endpoint", chatEP, statusCode)
				continue
			}
			reconcileAfterExecutorError(req.AuthID, statusCode, string(payload))
			// v0.12.18: 11102 model-catalog rejections become a bilingual,
			// realm-aware actionable error; other failures keep the raw shape.
			return nil, translateChatUpstreamError(statusCode, string(payload), sa)
		}
		completion, err = aggregateCompletion(reader, req.Model)
		stream.Close()
		if err != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
			return nil, err
		}
		break
	}
	publishUsage(req.Model, upstreamModel, authUID, started, usageDetailFromCompletion(completion), false, 0, "")
	invalidateAccountCredits(req.AuthID, authUID)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
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
	upstreamModel := resolveUpstreamModel(req.Model, req.AuthAttributes)
	started := time.Now()
	authUID := ""
	if sa.Account.UID != "" {
		authUID = sa.Account.UID
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	// Single-pass JSON rewrite (see handleExecExecute for the non-stream path).
	body = prepareUpstreamBody(body, nil, sa, upstreamModel)
	body = clampGlobalReasoningEffort(body, sa)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		collector := &sseUsageCollector{}
		chunks, statusCode, errCollect := collectUpstreamStream(body, sa, sseFramed, collector)
		if errCollect != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, errCollect.Error())
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
	_ = ctx // pump builds its own per-endpoint requests; cancel below only guards it
	go pumpUpstreamStream(body, chatEndpointCandidates(sa), cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, sa)
	return okEnvelope(streamResponse{Headers: headers})
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
