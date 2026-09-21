// oauth.go implements zcode's auth flow: the server-mediated CLI login
// mirrored from ZCode 3.12.3 (TriDefender/zcode-api src/auth/oauth.ts).
//
// Flow (both providers, no local callback server):
//
//  1. Generate a client poll token (32 random bytes, hex) — sent as
//     `Authorization: Bearer` on BOTH init and poll.
//  2. `POST {zcodeAPIBase}/oauth/cli/init` body `{"provider":...}` →
//     `{flow_id, poll_token, authorize_url, expires_at, poll_interval_sec}`.
//  3. Open the server-provided authorize_url with the desktop interstitial
//     param appended (`redirect_uri` for zai, `redirect` for bigmodel, value
//     = https://zcode.z.ai/app/oauth/login?redirect=zcode://oauth/callback&app_version=...)
//     — the browser never comes back to localhost; the grant is recorded
//     server-side and the poll flips to ready.
//  4. `GET {zcodeAPIBase}/oauth/cli/poll/{flow_id}` until
//     `data.status == "ready"` → `{token (plan JWT), user.user_id,
//     zai|bigmodel:{access_token}}`.
//
// Poll error semantics mirror the client: 4xx (except 408/429), envelope
// `code !== 0`, or an unknown status are fatal; 5xx / network errors /
// malformed 200 bodies are retried as if pending.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerZai      = "zai"
	providerBigmodel = "bigmodel"

	planCoding = "coding-plan"
	planStart  = "start-plan"

	// zcodeAppVersion mirrors the ZCode desktop release the plugin's identity
	// headers claim to be. Configurable via plugin config (identity_version).
	zcodeAppVersion = "3.14.0"

	// oauthInterstitial is the zcode.z.ai page the desktop client appends to
	// the server-issued authorize_url: it records the authorization
	// server-side (so the poll flips to ready) before bouncing the browser to
	// zcode://oauth/callback.
	oauthInterstitial = "https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback"
)

// cliInitData is the `data` of a successful /oauth/cli/init call.
type cliInitData struct {
	FlowID          string `json:"flow_id"`
	PollToken       string `json:"poll_token"`
	AuthorizeURL    string `json:"authorize_url"`
	ExpiresAt       int64  `json:"expires_at"`        // unix seconds
	PollIntervalSec int64  `json:"poll_interval_sec"` // seconds
}

// cliPollData is the `data` of a /oauth/cli/poll/{flow_id} call: pending or
// ready (with token / user / provider payload), or failed.
type cliPollData struct {
	Status string `json:"status"`
	Token  string `json:"token"`
	User   struct {
		UserID string `json:"user_id"`
	} `json:"user"`
	Zai struct {
		AccessToken string `json:"access_token"`
	} `json:"zai"`
	Bigmodel struct {
		AccessToken string `json:"access_token"`
	} `json:"bigmodel"`
}

// requestZcodeEnvelope POSTs/GETs a zcode.z.ai endpoint and unwraps the
// {code, data, msg} envelope (numeric code required; non-2xx or code !== 0
// surfaces the server msg).
func requestZcodeEnvelope(method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Code == 0 && len(env.Data) == 0 && env.Msg == "" {
		// Not a recognizable envelope.
		return nil, fmt.Errorf("invalid response envelope (status=%d body=%s)", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	if resp.StatusCode >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("failed: status=%d code=%d msg=%s", resp.StatusCode, env.Code, env.Msg)
	}
	return env.Data, nil
}

// normalizeProvider maps any accepted spelling onto zai | bigmodel.
func normalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case providerBigmodel, "bigmodel.cn", "big_model", "zhipu", "glm":
		return providerBigmodel
	default:
		return providerZai
	}
}

// normalizePlan maps any accepted spelling onto coding-plan | start-plan.
func normalizePlan(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case planStart, "start", "start_plan", "trial":
		return planStart
	default:
		return planCoding
	}
}

// applyInterstitial appends the desktop interstitial param to the
// server-provided authorize_url (param name differs per provider).
func applyInterstitial(authorizeURL, provider string) string {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return authorizeURL
	}
	q := u.Query()
	if provider == providerBigmodel {
		q.Set("redirect", oauthInterstitial)
	} else {
		q.Set("redirect_uri", oauthInterstitial)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// initCliLogin performs POST /oauth/cli/init with a freshly generated poll
// token and returns the parsed init payload.
func initCliLogin(provider string) (*cliInitData, string, error) {
	pollBytes := make([]byte, 32)
	if _, err := rand.Read(pollBytes); err != nil {
		return nil, "", fmt.Errorf("poll token: %w", err)
	}
	pollToken := hex.EncodeToString(pollBytes)
	body, _ := json.Marshal(map[string]string{"provider": provider})
	data, err := requestZcodeEnvelope(http.MethodPost, zcodeAPIBase+"/oauth/cli/init", func(req *http.Request) {
		commonHeaders(req)
		req.Header.Set("Authorization", "Bearer "+pollToken)
	}, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("%s login init: %w", provider, err)
	}
	var init cliInitData
	if err := json.Unmarshal(data, &init); err != nil {
		return nil, "", fmt.Errorf("%s login init: invalid response data: %w", provider, err)
	}
	if init.FlowID == "" || init.AuthorizeURL == "" {
		return nil, "", fmt.Errorf("%s login init: missing flow_id/authorize_url", provider)
	}
	return &init, pollToken, nil
}

// pollCliLogin performs one GET /oauth/cli/poll/{flow_id} round.
// Returns (data, retry, error): retry=true means poll again later (pending
// semantics); error is fatal.
func pollCliLogin(flowID, pollToken, provider string) (*cliPollData, bool, error) {
	req, err := http.NewRequest(http.MethodGet, zcodeAPIBase+"/oauth/cli/poll/"+url.PathEscape(flowID), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Accept", "application/json")
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, true, nil // network error → retry as pending
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		return nil, false, fmt.Errorf("%s login poll failed: status=%d body=%s", provider, resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	if resp.StatusCode >= 300 {
		return nil, true, nil // 5xx/3xx → retry as pending
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, true, nil // malformed 200 body → retry as pending
	}
	if env.Code != 0 {
		return nil, false, fmt.Errorf("%s login poll failed: code=%d msg=%s", provider, env.Code, env.Msg)
	}
	var data cliPollData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, true, nil
	}
	switch data.Status {
	case "ready":
		return &data, false, nil
	case "pending":
		return nil, true, nil
	case "failed":
		return nil, false, fmt.Errorf("authorization failed. Please retry login")
	default:
		return nil, false, fmt.Errorf("%s login poll: unexpected status %q", provider, data.Status)
	}
}

// accessTokenFromPoll picks data.{provider}.access_token from a ready poll.
func accessTokenFromPoll(data *cliPollData, provider string) string {
	if provider == providerBigmodel {
		return strings.TrimSpace(data.Bigmodel.AccessToken)
	}
	return strings.TrimSpace(data.Zai.AccessToken)
}

// buildStoredAuthFromPoll maps a ready poll payload onto storedAuth. The
// nickname is fetched lazily by the panel (no blocking upstream call before
// the auth file lands).
func buildStoredAuthFromPoll(data *cliPollData, provider string) *storedAuth {
	uid := strings.TrimSpace(data.User.UserID)
	accessToken := accessTokenFromPoll(data, provider)
	if uid == "" {
		uid = "u-" + sha256hex8(accessToken)
	}
	return &storedAuth{
		Auth: zcodeTokens{
			AccessToken: accessToken,
			JWT:         strings.TrimSpace(data.Token),
			Provider:    provider,
			Plan:        planCoding,
			DeviceMid:   uuid.NewString(),
		},
		Account: zcodeAccount{
			UID:      uid,
			Nickname: "",
		},
	}
}

// handleStartLogin implements AuthProvider.StartLogin: POST cli/init and
// stash the flow under the returned state.
func handleStartLogin(raw []byte) ([]byte, error) {
	return startLoginWithProvider(raw, loadedLoginProvider())
}

// startLoginWithProvider starts a CLI-poll login pinned to provider. The
// OAuth entry point stays single per plugin; which upstream it targets is
// chosen in the plugin config (login_provider dropdown) and is STICKY.
func startLoginWithProvider(raw []byte, provider string) ([]byte, error) {
	provider = normalizeProvider(provider)
	initData, pollToken, err := initCliLogin(provider)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ttl := loginTTL
	if initData.ExpiresAt > 0 {
		if t := time.Unix(initData.ExpiresAt, 0); t.After(now) {
			ttl = time.Until(t)
		}
	}
	state := fmt.Sprintf("zc-%d", now.UnixNano())
	loginStates.Store(state, &loginCtx{
		flowID:    initData.FlowID,
		pollToken: pollToken,
		provider:  provider,
		expires:   now.Add(ttl),
		startedAt: now.UnixNano(),
	})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       applyInterstitial(initData.AuthorizeURL, provider),
		State:     state,
		ExpiresAt: now.Add(ttl).UTC(),
		Metadata: map[string]any{
			"logo":   pluginLogoURL,
			"prompt": "在打开的页面中登录并授权 Z.AI / 智谱编码套餐（授权记录在服务端完成，无需回调）。完成后此窗口会自动关闭。",
		},
	})
}

// handlePollLogin implements AuthProvider.PollLogin: poll the flow until
// ready. The plugin-side loop is one round per call — the host drives the
// cadence.
func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}
	data, retry, err := pollCliLogin(lc.flowID, lc.pollToken, lc.provider)
	if err != nil {
		return nil, err
	}
	if retry {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待浏览器完成授权",
		})
	}
	accessToken := accessTokenFromPoll(data, lc.provider)
	if accessToken == "" {
		return nil, fmt.Errorf("%s login poll: response missing %s.access_token", lc.provider, lc.provider)
	}
	sa := buildStoredAuthFromPoll(data, lc.provider)
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

// handleRefreshAuth implements AuthProvider.Refresh. ZCode credentials do not
// refresh: the provider access token is a permanent API key and the plan JWT
// has no exp (an 8-day-old JWT still serves billing; only a 401/3012 from the
// gateway means re-login). Return the stored credential unchanged so the host
// refreshes its metadata without altering tokens.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: toAuthDataOpts(sa, nil, parseDisabledFromAuthJSON(req.StorageJSON)),
	})
}
