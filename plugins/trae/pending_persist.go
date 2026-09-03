package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
	intlupstream "github.com/mmqz/cpa-multi-plugins/plugins/trae/intlupstream"
	cnupstream "github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// v0.12.17: disk-persisted pending login + completed-outcome cache.
//
// Why: every in-flight login lives ONLY in process memory (loginStates /
// intlloginStates). A docker restart, host upgrade or any process bounce
// between "click 登录" and "paste the redirect URL" wipes the state and the
// paste box answers the generic "unknown or expired login state" — reported
// by the user as 回调未完成登录 / 粘贴链接也不行. The pending record is
// persisted to <AuthDir>/.trae-pending-login.json at start-login and
// transparently restored on the callback / submit / poll paths, so a paste
// completes even after a restart. The file name is dot-prefixed so the
// auth-file claim logic (adopt.go matches prefixes "trae-cn-", "trae-solo-cn-",
// "trae-intl-") never mistakes it for a credential.
// -----------------------------------------------------------------------------

const pendingLoginFileName = ".trae-pending-login.json"

// loginSelfCompleteGrace is the window a LIVE (non-restored) login gives the
// host's auth.login.poll to consume the completed callback before the plugin
// finishes the login itself (v0.12.23). Var (not const) so tests can shrink it.
var loginSelfCompleteGrace = 15 * time.Second

// spawnSelfCompleteCN arms the one-shot self-completion goroutine for a cn/solo
// login whose callback was just accepted (paste box or browser redirect hit).
// The grace window is snapshotted HERE, in the caller's goroutine, so the
// spawned goroutine never re-reads the (test-mutable) global.
func spawnSelfCompleteCN(lc *loginCtx) {
	lc.selfOnce.Do(func() {
		grace := loginSelfCompleteGrace
		go selfCompleteCNAfterGrace(lc, grace)
	})
}

// selfCompleteCNAfterGrace finishes a cn/solo login in-process. Restored
// logins complete immediately: the host poll channel died with the old
// process, so nobody else can drain the state (v0.12.17 behavior).
//
// Live logins (v0.12.23) first give the host poll a grace window — while the
// CPA login dialog stays open its poll consumes the state within seconds and
// saves the credential through the normal host path. But if the dialog was
// closed / the page navigated away / the host poll loop ended, nobody would
// ever drain the captured authCode: the login silently stalled at
// "回调已收到，正在交换凭证" until the 15-minute TTL with no credential —
// the reported "提交了链接仍然不生成凭证". After the grace, LoadAndDelete
// claims exclusive ownership: a win means no poll consumed the state and the
// plugin exchanges + writes the credential file itself; a loss means the host
// path already finished the login and there is nothing to do.
func selfCompleteCNAfterGrace(lc *loginCtx, grace time.Duration) {
	if lc.restored {
		lc.restored = false // spawn once per restored login
		selfCompleteCN(lc, "restored")
		return
	}
	time.Sleep(grace)
	if _, owned := loginStates.LoadAndDelete(lc.state); !owned {
		return // host poll consumed (or janitor reaped) the state first
	}
	selfCompleteCN(lc, "grace")
}

// spawnSelfCompleteIntl is the intl counterpart of spawnSelfCompleteCN.
func spawnSelfCompleteIntl(lc *intlloginCtx) {
	lc.selfOnce.Do(func() {
		grace := loginSelfCompleteGrace
		go selfCompleteIntlAfterGrace(lc, grace)
	})
}

// selfCompleteIntlAfterGrace is the intl counterpart of selfCompleteCNAfterGrace.
func selfCompleteIntlAfterGrace(lc *intlloginCtx, grace time.Duration) {
	if lc.restored {
		lc.restored = false // spawn once per restored login
		selfCompleteIntl(lc, "restored")
		return
	}
	time.Sleep(grace)
	if _, owned := intlloginStates.LoadAndDelete(lc.state); !owned {
		return // host poll consumed (or janitor reaped) the state first
	}
	selfCompleteIntl(lc, "grace")
}

// pendingLoginRecord is the serializable subset of loginCtx / intlloginCtx
// needed to resume a login after a process restart. No listener is carried:
// resource flows (the paste path) never need one.
type pendingLoginRecord struct {
	Flow          string `json:"flow"` // "cn" (cn+solo) | "intl"
	State         string `json:"state"`
	Variant       string `json:"variant,omitempty"`
	LoginHost     string `json:"login_host,omitempty"`
	CbURL         string `json:"cb_url,omitempty"`
	CodeVerifier  string `json:"code_verifier"`
	CodeChallenge string `json:"code_challenge,omitempty"`
	DeviceID      string `json:"device_id"`
	MachineID     string `json:"machine_id"`
	AuthDir       string `json:"auth_dir"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"`
}

// authDirCache remembers the last host-provided AuthDir. The resource routes
// (oauth_callback / oauth_submit) carry NO Host context in ManagementRequest,
// so restore uses the most recent dir seen on any RPC that has one
// (start / poll / parse / refresh). After a restart the host parses existing
// credentials at boot, which re-warms the cache before the user can paste.
var authDirCache struct {
	sync.Mutex
	dir string
}

func rememberAuthDir(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	authDirCache.Lock()
	authDirCache.dir = dir
	authDirCache.Unlock()
}

// cachedAuthDir returns the remembered AuthDir, falling back to <cwd>/auths
// (the CLIProxyAPI default layout) as a read-only best effort.
func cachedAuthDir() string {
	authDirCache.Lock()
	dir := authDirCache.dir
	authDirCache.Unlock()
	if dir != "" {
		return dir
	}
	if wd, err := os.Getwd(); err == nil {
		cand := filepath.Join(wd, "auths")
		if st, statErr := os.Stat(cand); statErr == nil && st.IsDir() {
			return cand
		}
	}
	return ""
}

// captureAuthDir extracts Host.AuthDir from any pluginapi RPC payload that
// carries a HostConfigSummary and remembers it for the resource-route restore
// path (ManagementRequest itself has no Host context).
func captureAuthDir(request []byte) {
	if len(request) == 0 {
		return
	}
	var probe struct {
		Host struct {
			AuthDir string `json:"AuthDir"`
		} `json:"Host"`
	}
	if err := json.Unmarshal(request, &probe); err == nil {
		rememberAuthDir(probe.Host.AuthDir)
		// Opportunistic sweep: an expired pending record that nobody ever
		// pasted would otherwise linger in the auth dir (listed by the host
		// as a type-less entry). loadPendingLogin removes it on read.
		if dir := cachedAuthDir(); dir != "" {
			loadPendingLogin(dir)
		}
	}
}

func pendingLoginPath(authDir string) string {
	return filepath.Join(authDir, pendingLoginFileName)
}

func persistPendingLogin(rec pendingLoginRecord) {
	dir := strings.TrimSpace(rec.AuthDir)
	if dir == "" || rec.State == "" {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = os.WriteFile(pendingLoginPath(dir), raw, 0o600)
}

func loadPendingLogin(authDir string) (pendingLoginRecord, bool) {
	if strings.TrimSpace(authDir) == "" {
		return pendingLoginRecord{}, false
	}
	raw, err := os.ReadFile(pendingLoginPath(authDir))
	if err != nil {
		return pendingLoginRecord{}, false
	}
	var rec pendingLoginRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return pendingLoginRecord{}, false
	}
	if rec.State == "" || time.Now().After(time.Unix(rec.ExpiresAt, 0)) {
		_ = os.Remove(pendingLoginPath(authDir))
		return pendingLoginRecord{}, false
	}
	return rec, true
}

func clearPendingLogin(authDir string) {
	if strings.TrimSpace(authDir) == "" {
		return
	}
	_ = os.Remove(pendingLoginPath(authDir))
}

// stateIsLive reports whether state is a live key in either login map.
func stateIsLive(state string) bool {
	if state == "" {
		return false
	}
	if _, ok := loginStates.Load(state); ok {
		return true
	}
	_, ok := intlloginStates.Load(state)
	return ok
}

// restorePendingLoginState re-materializes a disk-persisted pending login into
// the right in-memory map and returns its state key ("" when nothing restorable).
// With a non-empty state it must match the record's own state — the PKCE pair
// belongs to that login only, so cross-completing a DIFFERENT live login with a
// stale URL is refused (the upstream ExchangeToken would reject it anyway).
// With an empty state (URL carries no state/loginTraceID) the record is adopted
// as the single pending login.
func restorePendingLoginState(state string) string {
	dir := cachedAuthDir()
	if dir == "" {
		return ""
	}
	rec, ok := loadPendingLogin(dir)
	if !ok {
		return ""
	}
	if state != "" && rec.State != state {
		return ""
	}
	if stateIsLive(rec.State) {
		return rec.State // already live — nothing to restore
	}
	expires := time.Unix(rec.ExpiresAt, 0)
	switch rec.Flow {
	case "intl":
		intlloginStates.Store(rec.State, &intlloginCtx{
			listener: nil, state: rec.State, cbURL: rec.CbURL, expires: expires,
			loginTraceID: rec.State, codeVerifier: rec.CodeVerifier,
			codeChallenge: rec.CodeChallenge, deviceID: rec.DeviceID,
			machineID: rec.MachineID, loginHost: rec.LoginHost, authDir: dir,
			restored: true,
		})
		return rec.State
	default: // "cn" — cn + solo share the flow map
		loginStates.Store(rec.State, &loginCtx{
			listener: nil, variant: rec.Variant, state: rec.State, cbURL: rec.CbURL,
			expires: expires, loginTraceID: rec.State, codeVerifier: rec.CodeVerifier,
			codeChallenge: rec.CodeChallenge, deviceID: rec.DeviceID,
			machineID: rec.MachineID, authDir: dir,
			restored: true,
		})
		return rec.State
	}
}

// -----------------------------------------------------------------------------
// completed-outcome cache: a paste arriving after the login already finished
// (auto-callback succeeded, or the panel poll drained the state between two
// pastes) used to get a confusing "unknown or expired login state". Recent
// outcomes are remembered so a re-paste answers "already completed" (success)
// or the original error instead.
// -----------------------------------------------------------------------------

const loginOutcomeTTL = 10 * time.Minute

type loginOutcome struct {
	ok  bool
	msg string
	at  time.Time
}

var outcomeCache struct {
	sync.Mutex
	m map[string]loginOutcome
}

func recordLoginOutcome(state string, ok bool, msg string) {
	if state == "" {
		return
	}
	outcomeCache.Lock()
	if outcomeCache.m == nil {
		outcomeCache.m = make(map[string]loginOutcome)
	}
	outcomeCache.m[state] = loginOutcome{ok: ok, msg: msg, at: time.Now()}
	outcomeCache.Unlock()
}

func lookupLoginOutcome(state string) (loginOutcome, bool) {
	if state == "" {
		return loginOutcome{}, false
	}
	outcomeCache.Lock()
	defer outcomeCache.Unlock()
	o, ok := outcomeCache.m[state]
	if !ok {
		return loginOutcome{}, false
	}
	if time.Since(o.at) > loginOutcomeTTL {
		delete(outcomeCache.m, state)
		return loginOutcome{}, false
	}
	return o, true
}

// -----------------------------------------------------------------------------
// v0.12.17: self-completion for RESTORED pending logins. After a host restart
// the host's own oauth-session registry is empty, so get-auth-status answers
// "unknown or expired state" and never reaches the plugin's PollLogin — the
// paste would complete the callback but the credential would never be saved.
// Restored logins therefore finish inside the plugin: ExchangeToken +
// GetUserInfo, write the credential file straight into AuthDir (the host
// watcher claims it, same as a manual import), register in the pool and clean
// up. Non-restored logins keep the host-poll path (single completion point).
// -----------------------------------------------------------------------------

// selfCompleteCN finishes a cn/solo login in-process (exchange + GetUserInfo
// + credential file write). source tags the trigger path in logs/outcomes:
// "restored" (post-restart paste) or "grace" (live login the host poll never
// claimed within loginSelfCompleteGrace — v0.12.23).
func selfCompleteCN(lc *loginCtx, source string) {
	state := lc.state
	fail := func(msg string) {
		recordLoginOutcome(state, false, msg)
		clearPendingLogin(lc.authDir)
		loginStates.Delete(state)
		log.Printf("trae self-complete (%s, %s) failed: %s", source, state, msg)
	}
	if lc.authCode == "" && lc.refreshToken == "" {
		fail("no authCode/refreshToken after restore")
		return
	}
	var accessToken, refreshToken string
	var expiresAt int64
	if lc.refreshToken != "" {
		a := &auth.Auth{
			RefreshToken: lc.refreshToken,
			APIHost:      oauthDefaultHost,
			Domain:       "trae.cn",
			MachineID:    lc.machineID,
			DeviceID:     lc.deviceID,
		}
		if err := upstreamClient.RefreshToken(a); err != nil {
			accessToken, refreshToken = lc.refreshToken, lc.refreshToken
		} else {
			accessToken, refreshToken, expiresAt = a.AccessToken, a.RefreshToken, a.ExpiresAt
		}
	} else {
		di := buildOfficialDeviceInfo(
			lc.deviceID, lc.machineID, oauthPlatformCodeFor(lc.variant), oauthDeviceName,
			oauthDeviceBrand, cnupstream.IdeVersion, oauthDeviceType, oauthOSVersion,
		)
		tokenBody := map[string]any{
			"ClientID":     cnupstream.ClientIDFor(lc.variant),
			"AuthCode":     lc.authCode,
			"CodeVerifier": lc.codeVerifier,
			"DeviceInfo":   di,
			"IDEVersion":   cnupstream.IdeVersion,
		}
		bodyBytes, _ := json.Marshal(tokenBody)
		tokenRaw, exErr := exchangeTokenCandidates(
			buildAPIURLs(lc.loginHost, "/trae/api/v3/oauth/ExchangeToken", true), bodyBytes)
		if exErr != nil {
			fail(exErr.Error())
			return
		}
		accessToken, refreshToken, expiresAt = parseExchangeTokenResponse(tokenRaw)
		if accessToken == "" && refreshToken == "" {
			fail("ExchangeToken: no token in response")
			return
		}
		if accessToken == "" {
			accessToken = refreshToken
		}
	}
	a := &auth.Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		APIHost:      oauthDefaultHost,
		Domain:       "trae.cn",
		MachineID:    lc.machineID,
		DeviceID:     lc.deviceID,
		Variant:      lc.variant,
	}
	uid, nickname, entID, err := upstreamClient.GetUserInfo(a)
	if err != nil {
		log.Printf("trae self-complete GetUserInfo: %v — proceeding", err)
	}
	if strings.TrimSpace(uid) == "" {
		uid = lc.loginTraceID // avoid a nameless trae-.json credential file
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
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
			"variant":      a.Variant,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
		"disabled": false,
	}, "", "  ")
	fileName := fmt.Sprintf("%s-%s.json", providerName, uid)
	if lc.authDir == "" {
		fail("no AuthDir to write the credential file")
		return
	}
	if err := os.WriteFile(filepath.Join(lc.authDir, fileName), storageJSON, 0o600); err != nil {
		fail("write credential file: " + err.Error())
		return
	}
	accountPool.Add(a)
	recordLoginOutcome(state, true, "")
	clearPendingLogin(lc.authDir)
	loginStates.Delete(state)
	log.Printf("trae self-complete (%s): saved %s (uid=%s)", source, fileName, uid)
}

// selfCompleteIntl finishes an intl login in-process; source mirrors
// selfCompleteCN ("restored" post-restart paste, "grace" unclaimed live
// login — v0.12.23).
func selfCompleteIntl(lc *intlloginCtx, source string) {
	state := lc.state
	fail := func(msg string) {
		recordLoginOutcome(state, false, msg)
		clearPendingLogin(lc.authDir)
		intlloginStates.Delete(state)
		log.Printf("trae-intl self-complete (%s, %s) failed: %s", source, state, msg)
	}
	if lc.authCode == "" {
		fail("no authCode after restore")
		return
	}
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
		fail(exErr.Error())
		return
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
		fail(fmt.Sprintf("parse ExchangeToken: %v", err))
		return
	}
	if tokenEnv.Result.AccessToken != "" && tokenEnv.Result.Token == "" {
		tokenEnv.Result.Token = tokenEnv.Result.AccessToken
	}
	if tokenEnv.Result.Token == "" && tokenEnv.Result.RefreshToken == "" {
		fail("ExchangeToken: no token in response")
		return
	}
	a := &intlupstream.Auth{
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
		log.Printf("trae-intl self-complete GetUserInfo: %v — proceeding", err)
	}
	if strings.TrimSpace(uid) == "" {
		uid = lc.loginTraceID
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
			"variant":      "intl",
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
	fileName := fmt.Sprintf("%s-%s.json", intlproviderName, uid)
	if lc.authDir == "" {
		fail("no AuthDir to write the credential file")
		return
	}
	if err := os.WriteFile(filepath.Join(lc.authDir, fileName), storageJSON, 0o600); err != nil {
		fail("write credential file: " + err.Error())
		return
	}
	recordLoginOutcome(state, true, "")
	clearPendingLogin(lc.authDir)
	intlloginStates.Delete(state)
	log.Printf("trae-intl self-complete (%s): saved %s (uid=%s)", source, fileName, uid)
}

// -----------------------------------------------------------------------------
// v0.12.21: GET /v0/resource/plugins/trae/login_status - live login state for
// the panel status line. The host's get-auth-status maps a plugin "pending"
// poll to a bare {"status":"wait"} and drops the message, so manager UIs show
// an unexplained waiting state for the whole 15-minute TTL when the browser
// cannot reach the 127.0.0.1 callback listener (remote / docker deployments).
// This endpoint lets the plugin's own panel explain what is being waited on
// and what to do next. Data only - the panel renders the words. No secrets:
// loopback callback host:port, variant, ages, and the outcome message (the
// same text paste result pages already show unauthenticated).
// -----------------------------------------------------------------------------

type loginStatusOutcome struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	AgeSec  int64  `json:"age_seconds"`
}

type loginStatusPayload struct {
	Pending       bool   `json:"pending"`
	Restored      bool   `json:"restored"`
	Variant       string `json:"variant,omitempty"`
	CallbackHP    string `json:"callback_hostport,omitempty"`
	AgeSec        int64  `json:"age_seconds,omitempty"`
	TTLRemaining  int64  `json:"ttl_seconds_remaining,omitempty"`
	ListenerAlive bool   `json:"listener_alive"`
	// CallbackReceived: the browser redirect already landed (or a paste was
	// accepted) and the token exchange is pending the next poll drain.
	CallbackReceived bool                `json:"callback_received,omitempty"`
	Outcome          *loginStatusOutcome `json:"outcome,omitempty"`
}

func mgmtJSONResourceResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

// callbackHostPort reduces a callback URL to its host:port - the only part
// the user needs to recognize the browser's "cannot connect" error page.
func callbackHostPort(cbURL string) string {
	cbURL = strings.TrimSpace(cbURL)
	if cbURL == "" {
		return ""
	}
	if u, err := url.Parse(cbURL); err == nil && u.Host != "" {
		return u.Host
	}
	return ""
}

// latestLoginOutcome returns the most recent recorded login outcome across
// states (the cache is keyed by state; the status line is stateless).
func latestLoginOutcome() (loginOutcome, bool) {
	outcomeCache.Lock()
	defer outcomeCache.Unlock()
	var best loginOutcome
	found := false
	for _, o := range outcomeCache.m {
		if !found || o.at.After(best.at) {
			best, found = o, true
		}
	}
	return best, found
}

// pendingSnapshot is the display-relevant slice of a live or restored login.
type pendingSnapshot struct {
	variant          string
	cbURL            string
	expires          time.Time
	restored         bool
	listenerAlive    bool
	callbackReceived bool
}

// snapshotPending picks the pending login with the latest expiry across both
// flow maps (cn/solo share loginStates; intl has its own). Expired entries
// the janitor has not reaped yet are ignored.
func snapshotPending() (pendingSnapshot, bool) {
	now := time.Now()
	var best pendingSnapshot
	found := false
	consider := func(variant, cbURL string, expires time.Time, restored, listenerAlive, cbRecv bool) {
		if !now.Before(expires) {
			return
		}
		if !found || expires.After(best.expires) {
			best = pendingSnapshot{
				variant: variant, cbURL: cbURL, expires: expires,
				restored: restored, listenerAlive: listenerAlive, callbackReceived: cbRecv,
			}
			found = true
		}
	}
	loginStates.Range(func(_, v any) bool {
		if lc, ok := v.(*loginCtx); ok {
			got := lc.authCode != "" || lc.refreshToken != "" || lc.err != nil
			consider(lc.variant, lc.cbURL, lc.expires, lc.restored, lc.listener != nil, got)
		}
		return true
	})
	intlloginStates.Range(func(_, v any) bool {
		if lc, ok := v.(*intlloginCtx); ok {
			got := lc.authCode != "" || lc.err != nil
			consider("intl", lc.cbURL, lc.expires, lc.restored, lc.listener != nil, got)
		}
		return true
	})
	return best, found
}

// handleLoginStatusResource answers the panel's status poll. Priority: the
// in-memory pending login (live listener or restored-into-memory) wins; the
// disk record is reported only when nothing is in memory (post-restart, the
// paste can still complete it). The newest outcome is always attached.
func handleLoginStatusResource() []byte {
	now := time.Now()
	payload := loginStatusPayload{}
	if snap, ok := snapshotPending(); ok {
		payload.Pending = true
		payload.Restored = snap.restored
		payload.Variant = snap.variant
		payload.CallbackHP = callbackHostPort(snap.cbURL)
		payload.AgeSec = int64((loginTTL - time.Until(snap.expires)).Seconds())
		payload.TTLRemaining = int64(time.Until(snap.expires).Seconds())
		payload.ListenerAlive = snap.listenerAlive
		payload.CallbackReceived = snap.callbackReceived
	} else if dir := cachedAuthDir(); dir != "" {
		// loadPendingLogin also sweeps an expired record (cleanup side effect).
		if rec, ok := loadPendingLogin(dir); ok {
			payload.Pending = true
			payload.Restored = true // not in memory -> only the disk record serves it
			payload.Variant = rec.Variant
			payload.CallbackHP = callbackHostPort(rec.CbURL)
			payload.AgeSec = now.Unix() - rec.CreatedAt
			payload.TTLRemaining = rec.ExpiresAt - now.Unix()
		}
	}
	if o, ok := latestLoginOutcome(); ok {
		payload.Outcome = &loginStatusOutcome{OK: o.ok, Message: o.msg, AgeSec: int64(time.Since(o.at).Seconds())}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"pending":false}`)
	}
	return raw
}
